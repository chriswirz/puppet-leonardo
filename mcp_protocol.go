package main

import (
	"encoding/json"
	"fmt"
)

// --- MCP (Model Context Protocol) wire types ---
//
// This server is "dual-era": it serves the stateless 2026-07-28 revision, where
// every request carries its own version in _meta, and (when
// mcp.legacy_compatibility is on) the earlier initialize-and-session revisions.
// Which one answers is decided by how the client opens.

// ProtocolVersion is the MCP revision this server prefers.
const ProtocolVersion = "2026-07-28"

var (
	// ModernVersions are the stateless revisions, newest first.
	ModernVersions = []string{"2026-07-28"}

	// LegacyVersions are the handshake-based revisions this server still
	// answers, newest first. All use the Streamable HTTP transport with
	// sessions; the 2024-11-05 HTTP+SSE transport is not implemented.
	LegacyVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26"}
)

// SupportedVersions is every version this server speaks, newest first.
var SupportedVersions = append(append([]string{}, ModernVersions...), LegacyVersions...)

// LatestLegacyVersion is what an initialize handshake falls back to when the
// client asks for a version this server does not implement.
const LatestLegacyVersion = "2025-11-25"

func isModernVersion(version string) bool { return contains(ModernVersions, version) }
func isLegacyVersion(version string) bool { return contains(LegacyVersions, version) }

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// Well-known _meta keys defined by the specification.
const (
	MetaProtocolVersion    = "io.modelcontextprotocol/protocolVersion"
	MetaClientInfo         = "io.modelcontextprotocol/clientInfo"
	MetaClientCapabilities = "io.modelcontextprotocol/clientCapabilities"
	MetaServerInfo         = "io.modelcontextprotocol/serverInfo"
	MetaSubscriptionID     = "io.modelcontextprotocol/subscriptionId"
)

// JSON-RPC and MCP error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603

	CodeHeaderMismatch                  = -32020
	CodeMissingRequiredClientCapability = -32021
	CodeUnsupportedProtocolVersion      = -32022
)

// Implementation is the identity of a client or server.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// Request is an incoming JSON-RPC request or notification.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the message carries no id and therefore
// expects no response.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// Meta returns the _meta object of the request params, or nil.
func (r *Request) Meta() map[string]json.RawMessage {
	if len(r.Params) == 0 {
		return nil
	}
	var p struct {
		Meta map[string]json.RawMessage `json:"_meta"`
	}
	if err := json.Unmarshal(r.Params, &p); err != nil {
		return nil
	}
	return p.Meta
}

// MetaString returns a string-valued _meta entry.
func (r *Request) MetaString(key string) string {
	raw, ok := r.Meta()[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// Bind decodes the request params into v.
func (r *Request) Bind(v any) *RPCError {
	if len(r.Params) == 0 {
		return nil
	}
	if err := json.Unmarshal(r.Params, v); err != nil {
		return Errorf(CodeInvalidParams, "invalid params: %v", err)
	}
	return nil
}

// Response is an outgoing JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// Notification is an outgoing JSON-RPC notification.
type Notification struct {
	JSONRPC string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCError is a JSON-RPC error object.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

// Errorf builds an RPCError with a formatted message.
func Errorf(code int, format string, args ...any) *RPCError {
	return &RPCError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// UnsupportedVersionError is returned when a request declares a protocol
// version this server does not implement.
func UnsupportedVersionError(requested string, supported []string) *RPCError {
	return &RPCError{
		Code:    CodeUnsupportedProtocolVersion,
		Message: "Unsupported protocol version",
		Data:    map[string]any{"supported": supported, "requested": requested},
	}
}

func newResponse(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Result: result}
}

func newErrorResponse(id json.RawMessage, err *RPCError) *Response {
	return &Response{JSONRPC: "2.0", ID: id, Error: err}
}

// Result is the envelope every successful result carries. Revision 2026-07-28
// requires resultType on all results and cache hints on list-shaped ones. The
// fields are omitted when empty so the same structs serve a legacy client.
type Result struct {
	ResultType string         `json:"resultType,omitempty"`
	Meta       map[string]any `json:"_meta,omitempty"`
	TTLMs      int64          `json:"ttlMs,omitempty"`
	CacheScope string         `json:"cacheScope,omitempty"`
}

const (
	ResultComplete = "complete"

	CacheScopePublic  = "public"
	CacheScopePrivate = "private"
)

// cacheable adds the freshness hints required on list results. A legacy
// client gets an empty envelope, which is left alone.
func (r Result) cacheable(ttlMs int64, scope string) Result {
	if r.ResultType == "" {
		return r
	}
	r.TTLMs = ttlMs
	r.CacheScope = scope
	return r
}

// DiscoverResult answers server/discover.
type DiscoverResult struct {
	Result
	SupportedVersions []string           `json:"supportedVersions"`
	Capabilities      ServerCapabilities `json:"capabilities"`
	Instructions      string             `json:"instructions,omitempty"`
}

// ServerCapabilities describes the optional features this server implements.
// Only tools: the service has nothing to offer as resources or prompts.
type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

type ToolsCapability struct {
	ListChanged bool `json:"listChanged,omitempty"`
}

// Tool is a tool definition as returned by tools/list.
type Tool struct {
	Name        string           `json:"name"`
	Title       string           `json:"title,omitempty"`
	Description string           `json:"description,omitempty"`
	InputSchema map[string]any   `json:"inputSchema"`
	Annotations *ToolAnnotations `json:"annotations,omitempty"`
}

// ToolAnnotations are behavioural hints; clients treat them as untrusted.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    bool   `json:"readOnlyHint,omitempty"`
	DestructiveHint bool   `json:"destructiveHint,omitempty"`
	IdempotentHint  bool   `json:"idempotentHint,omitempty"`
	OpenWorldHint   bool   `json:"openWorldHint,omitempty"`
}

// ListToolsResult answers tools/list.
type ListToolsResult struct {
	Result
	Tools []Tool `json:"tools"`
}

// CallToolParams are the params of tools/call.
type CallToolParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments,omitempty"`
}

// CallToolResult answers tools/call.
type CallToolResult struct {
	Result
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// Content is one unstructured content block of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

func textContent(text string) []Content {
	return []Content{{Type: "text", Text: text}}
}

// InitializeParams are the params of a legacy initialize request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities,omitempty"`
	ClientInfo      Implementation `json:"clientInfo"`
}

// InitializeResult answers a legacy initialize request. It deliberately does
// not embed Result: a legacy client knows nothing of resultType or cache hints.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// emptyResult is the {} that ping answers with.
var emptyResult = json.RawMessage(`{}`)
