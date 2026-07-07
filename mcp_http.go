package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// sessionHeader is the legacy session identifier. Revision 2026-07-28 removed
// it; earlier revisions mint it during initialize and send it on every request.
const sessionHeader = "Mcp-Session-Id"

// corsAllowHeaders is the fallback set advertised on a preflight that does not
// say which headers it wants.
const corsAllowHeaders = "Content-Type, Authorization, Accept, MCP-Protocol-Version, " +
	"Mcp-Method, Mcp-Name, Mcp-Session-Id, Last-Event-ID"

// mcpTransport serves the Streamable HTTP transport at /mcp. The 2026-07-28
// shape is a single POST endpoint with no sessions; with legacy compatibility
// on it also serves the 2025-03-26 through 2025-11-25 shape, which negotiates
// through initialize and carries a session.
type mcpTransport struct {
	server   *mcpServer
	cfg      MCPConfig
	sessions *sessionStore
}

func newMCPTransport(s *mcpServer, c MCPConfig) *mcpTransport {
	return &mcpTransport{
		server:   s,
		cfg:      c,
		sessions: newSessionStore(time.Duration(c.SessionTimeoutSeconds) * time.Second),
	}
}

// Handler returns the /mcp handler, wrapped in the CORS and origin checks so
// that every response carries the headers a browser needs.
func (t *mcpTransport) Handler() http.Handler {
	return t.withCORS(http.HandlerFunc(t.handle))
}

// withCORS validates the request origin and answers preflights. An Origin the
// configuration does not allow is refused outright: that check is the DNS
// rebinding defence the specification requires.
func (t *mcpTransport) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" && !t.originAllowed(origin) {
			writeJSONRPCError(w, http.StatusForbidden, nil,
				Errorf(CodeInvalidRequest, "origin %q is not allowed", origin))
			return
		}
		t.writeCORSHeaders(w, origin)

		if r.Method == http.MethodOptions {
			// A preflight carries no credentials and no body, so it is answered
			// here rather than falling through to the auth and method checks.
			h := w.Header()
			h.Set("Access-Control-Allow-Methods", "POST, GET, DELETE, OPTIONS")
			h.Add("Vary", "Access-Control-Request-Headers")
			h.Set("Access-Control-Allow-Headers", t.allowHeadersFor(r.Header.Get("Access-Control-Request-Headers")))
			// Chrome's Private Network Access check: a page on a public address
			// reaching this server on a private one must be granted it explicitly.
			if t.cfg.AllowPrivateNetwork && r.Header.Get("Access-Control-Request-Private-Network") == "true" {
				h.Set("Access-Control-Allow-Private-Network", "true")
			}
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (t *mcpTransport) writeCORSHeaders(w http.ResponseWriter, origin string) {
	h := w.Header()
	h.Add("Vary", "Origin")
	switch {
	case origin == "":
		if t.allowAnyOrigin() {
			h.Set("Access-Control-Allow-Origin", "*")
		}
	case t.allowAnyOrigin():
		// Echoed rather than starred so a client using withCredentials can read
		// it; credentials themselves are not granted, since any origin qualifies.
		h.Set("Access-Control-Allow-Origin", origin)
	default:
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	// Mcp-Session-Id must be readable or a legacy client in a page cannot pick
	// up the session its initialize just created.
	h.Set("Access-Control-Expose-Headers",
		strings.Join([]string{sessionHeader, "MCP-Protocol-Version", "Mcp-Method", "Mcp-Name", "WWW-Authenticate"}, ", "))
}

// allowHeadersFor decides what a preflight is told it may send. An unset
// allowed_headers echoes what the browser asked for; ["*"] allows anything;
// any other list restricts the answer to it.
func (t *mcpTransport) allowHeadersFor(requested string) string {
	if contains(t.cfg.AllowedHeaders, "*") {
		if requested != "" {
			return requested
		}
		return "*"
	}
	if len(t.cfg.AllowedHeaders) > 0 {
		return strings.Join(t.cfg.AllowedHeaders, ", ")
	}
	if requested != "" {
		return requested
	}
	return corsAllowHeaders
}

func (t *mcpTransport) allowAnyOrigin() bool { return contains(t.cfg.AllowedOrigins, "*") }

// originAllowed matches an Origin against mcp.allowed_origins. An entry without
// a port matches any port on that host, so "http://localhost" is useful as is.
func (t *mcpTransport) originAllowed(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	for _, allowed := range t.cfg.AllowedOrigins {
		if allowed == "*" || allowed == origin {
			return true
		}
		a, err := url.Parse(allowed)
		if err != nil {
			continue
		}
		if a.Scheme == u.Scheme && a.Hostname() == u.Hostname() && (a.Port() == "" || a.Port() == u.Port()) {
			return true
		}
	}
	return false
}

func (t *mcpTransport) handle(w http.ResponseWriter, r *http.Request) {
	if t.cfg.AuthToken != "" {
		if !strings.EqualFold(strings.TrimSpace(r.Header.Get("Authorization")), "bearer "+t.cfg.AuthToken) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			writeJSONRPCError(w, http.StatusUnauthorized, nil, Errorf(CodeInvalidRequest, "authentication required"))
			return
		}
	}
	// Revision 2026-07-28 removed the GET stream and the DELETE session
	// teardown, so both are 405 unless a legacy client still expects them.
	if r.Method != http.MethodPost {
		if t.server.legacy && t.serveLegacyNonPost(w, r) {
			return
		}
		w.Header().Set("Allow", http.MethodPost)
		writeJSONRPCError(w, http.StatusMethodNotAllowed, nil,
			Errorf(CodeInvalidRequest, "the MCP endpoint accepts POST only in protocol version "+ProtocolVersion))
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, Errorf(CodeParseError, "could not read the request body: %v", err))
		return
	}
	var req Request
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONRPCError(w, http.StatusBadRequest, nil, Errorf(CodeParseError, "invalid JSON: %v", err))
		return
	}

	// Which era this request belongs to decides what must be validated, what
	// the result envelope looks like, and whether a session is involved.
	version, sess, ok := t.resolveVersion(w, r, &req)
	if !ok {
		return
	}
	ctx := withVersion(r.Context(), version)

	if req.IsNotification() {
		t.server.Handle(ctx, &req)
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if req.Method == "initialize" {
		t.serveInitialize(w, &req)
		return
	}

	if isModernVersion(version) {
		// The mirrored request headers arrived with 2026-07-28; a legacy client
		// neither sends them nor should be judged against them.
		if rpcErr := validateHeaders(r, &req); rpcErr != nil {
			writeJSONRPCError(w, http.StatusBadRequest, req.ID, rpcErr)
			return
		}
	}
	if req.Method != "server/discover" && !t.server.supports(version) {
		writeJSONRPCError(w, http.StatusBadRequest, req.ID, UnsupportedVersionError(version, t.server.supportedVersions()))
		return
	}
	if sess != nil {
		w.Header().Set(sessionHeader, sess.ID)
	}

	if req.Method == "subscriptions/listen" {
		if !isModernVersion(version) {
			writeJSONRPCError(w, http.StatusOK, req.ID, Errorf(CodeMethodNotFound,
				"subscriptions/listen was introduced in protocol version %s; open the GET stream instead", ProtocolVersion))
			return
		}
		serveListen(w, r, &req)
		return
	}

	result, rpcErr := t.server.Handle(ctx, &req)
	if rpcErr != nil {
		status := http.StatusOK
		switch rpcErr.Code {
		case CodeMethodNotFound:
			// The modern revision answers an unknown method with 404. A legacy
			// client reads 404 as "your session is gone" and would re-initialize
			// in a loop, so it gets the error in a 200 instead.
			if isModernVersion(version) {
				status = http.StatusNotFound
			}
		case CodeUnsupportedProtocolVersion, CodeHeaderMismatch, CodeMissingRequiredClientCapability:
			status = http.StatusBadRequest
		}
		writeJSONRPCError(w, status, req.ID, rpcErr)
		return
	}
	writeJSON(w, http.StatusOK, newResponse(req.ID, result))
}

// resolveVersion works out which protocol version answers this request, and
// which session it belongs to if any. When it returns ok=false it has already
// written the response.
//
// A dual-era server selects behaviour from how the client opens: per-request
// _meta means modern, an initialize handshake means legacy.
func (t *mcpTransport) resolveVersion(w http.ResponseWriter, r *http.Request, req *Request) (string, *mcpSession, bool) {
	if declared := req.MetaString(MetaProtocolVersion); declared != "" {
		return declared, nil, true
	}
	if !t.server.legacy {
		return r.Header.Get("MCP-Protocol-Version"), nil, true
	}
	if req.Method == "initialize" {
		return LatestLegacyVersion, nil, true
	}
	if id := r.Header.Get(sessionHeader); id != "" {
		sess, ok := t.sessions.Get(id)
		if !ok {
			// The legacy specification answers 404 for an unknown session, which
			// tells the client to start a new one.
			writeJSONRPCError(w, http.StatusNotFound, req.ID,
				Errorf(CodeInvalidRequest, "unknown or expired session %q; send initialize to start a new one", id))
			return "", nil, false
		}
		return sess.Version, sess, true
	}
	// 2025-06-18 and later send the version in a header even without a session;
	// 2025-03-26 sends nothing at all, and is assumed by the specification. That
	// is also what lets a bare curl call tools/list without a handshake.
	header := r.Header.Get("MCP-Protocol-Version")
	if isLegacyVersion(header) {
		return header, nil, true
	}
	if header == "" && !hasModernMetadata(req) {
		return "2025-03-26", nil, true
	}
	return header, nil, true
}

// hasModernMetadata reports whether the request carries the per-request
// metadata that identifies a modern client.
func hasModernMetadata(req *Request) bool {
	meta := req.Meta()
	for _, key := range []string{MetaProtocolVersion, MetaClientInfo, MetaClientCapabilities} {
		if _, ok := meta[key]; ok {
			return true
		}
	}
	return false
}

// serveInitialize answers the legacy handshake and mints a session.
func (t *mcpTransport) serveInitialize(w http.ResponseWriter, req *Request) {
	if !t.server.legacy {
		writeJSONRPCError(w, http.StatusNotFound, req.ID, Errorf(CodeMethodNotFound,
			"initialize was removed in protocol version %s, and legacy compatibility is disabled on this server",
			ProtocolVersion))
		return
	}
	result, client, version, rpcErr := t.server.initialize(req)
	if rpcErr != nil {
		writeJSONRPCError(w, http.StatusBadRequest, req.ID, rpcErr)
		return
	}
	sess := t.sessions.Create(version, client)
	w.Header().Set(sessionHeader, sess.ID)
	writeJSON(w, http.StatusOK, newResponse(req.ID, result))
}

// serveLegacyNonPost answers the GET and DELETE requests the earlier revisions
// define. It reports whether it handled the request.
func (t *mcpTransport) serveLegacyNonPost(w http.ResponseWriter, r *http.Request) bool {
	id := r.Header.Get(sessionHeader)
	switch r.Method {
	case http.MethodDelete:
		if id == "" {
			return false
		}
		if !t.sessions.Delete(id) {
			http.Error(w, "unknown session", http.StatusNotFound)
			return true
		}
		w.WriteHeader(http.StatusNoContent)
		return true

	case http.MethodGet:
		// The standalone SSE stream on which a legacy server could push
		// messages. This server has nothing to push, so the stream is opened
		// and kept alive, so a client that waits on it is not left failing.
		if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			return false
		}
		if id != "" {
			if _, ok := t.sessions.Get(id); !ok {
				http.Error(w, "unknown session", http.StatusNotFound)
				return true
			}
		}
		flusher, ok := w.(http.Flusher)
		if !ok {
			return false
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if id != "" {
			w.Header().Set(sessionHeader, id)
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		keepAlive(r.Context(), w, flusher)
		return true
	}
	return false
}

// serveListen answers subscriptions/listen with a long-lived SSE stream. This
// server publishes no list-changed notifications, so the stream acknowledges
// the subscription and then only keeps itself alive.
func serveListen(w http.ResponseWriter, r *http.Request, req *Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSONRPCError(w, http.StatusInternalServerError, req.ID, Errorf(CodeInternalError, "streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	data, _ := json.Marshal(Notification{
		JSONRPC: "2.0",
		Method:  "notifications/subscriptions/acknowledged",
		Params: map[string]any{
			"_meta": map[string]any{MetaSubscriptionID: fmt.Sprintf("sub-%d", time.Now().UnixNano())},
		},
	})
	fmt.Fprintf(w, "data: %s\n\n", data)
	flusher.Flush()
	keepAlive(r.Context(), w, flusher)
}

// keepAlive holds an SSE stream open with periodic comment lines until the
// client goes away.
func keepAlive(ctx context.Context, w io.Writer, flusher http.Flusher) {
	ticker := time.NewTicker(20 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			fmt.Fprint(w, ":\r\n")
			flusher.Flush()
		}
	}
}

// validateHeaders enforces the request-metadata mirroring revision 2026-07-28
// requires: the headers an intermediary routes on must match the body.
func validateHeaders(r *http.Request, req *Request) *RPCError {
	pv := r.Header.Get("MCP-Protocol-Version")
	if pv == "" {
		return Errorf(CodeHeaderMismatch, "the MCP-Protocol-Version header is required")
	}
	if declared := req.MetaString(MetaProtocolVersion); declared != "" && declared != pv {
		return Errorf(CodeHeaderMismatch, "MCP-Protocol-Version header %q does not match the body value %q", pv, declared)
	}
	method := r.Header.Get("Mcp-Method")
	if method == "" {
		return Errorf(CodeHeaderMismatch, "the Mcp-Method header is required")
	}
	if method != req.Method {
		return Errorf(CodeHeaderMismatch, "Mcp-Method header %q does not match the body method %q", method, req.Method)
	}
	// Mcp-Name mirrors params.name for the methods that address a specific tool.
	if req.Method != "tools/call" {
		return nil
	}
	var p struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(req.Params, &p)
	raw := r.Header.Get("Mcp-Name")
	if raw == "" {
		return Errorf(CodeHeaderMismatch, "the Mcp-Name header is required for %s", req.Method)
	}
	got, err := decodeHeaderValue(raw)
	if err != nil {
		return Errorf(CodeHeaderMismatch, "Mcp-Name: %v", err)
	}
	if got != p.Name {
		return Errorf(CodeHeaderMismatch, "Mcp-Name header %q does not match the body value %q", got, p.Name)
	}
	return nil
}

// decodeHeaderValue unwraps the =?base64?...?= sentinel the spec defines for
// values that cannot be carried as plain ASCII.
func decodeHeaderValue(value string) (string, error) {
	const prefix, suffix = "=?base64?", "?="
	if !strings.HasPrefix(value, prefix) || !strings.HasSuffix(value, suffix) {
		return value, nil
	}
	decoded, err := base64.StdEncoding.DecodeString(value[len(prefix) : len(value)-len(suffix)])
	if err != nil {
		return "", errors.New("malformed base64 sentinel value")
	}
	return string(decoded), nil
}

func writeJSONRPCError(w http.ResponseWriter, status int, id json.RawMessage, rpcErr *RPCError) {
	writeJSON(w, status, newErrorResponse(id, rpcErr))
}
