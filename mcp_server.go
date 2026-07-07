package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	appName    = "puppet-leonardo"
	appVersion = "1.0.0"
)

// defaultInstructions is what the server tells an MCP client about itself when
// mcp.instructions is not set.
const defaultInstructions = `Generates images with Leonardo.ai by driving a real browser session.

Call generate_image_with_leonardo with a self-contained scene description in "prompt" and, optionally, a "size" of 2:3 (posters/portraits), 1:1 (avatars/thumbnails) or 16:9 (banners/covers). If the image is ready within the call's wait (wait_seconds), the result text is the URL of the generated .jpg on Leonardo's CDN, which can be embedded or downloaded directly.

A generation usually takes longer than that - one to a few minutes, more if others are queued ahead - so expect the call to come back with the task queued or running and a task_id (the SHA-256 hex digest of the prompt). Poll get_image_task with that task_id every 20-30 seconds: status completed means its uri is the image; failed carries the error. The task carries on whether or not you are waiting.

Generations run one at a time from a queue. Do not resubmit a prompt to hurry it: a call for a prompt that is already queued or running joins that task and returns the same task_id. Only a full queue rejects a call, with a "generation queue is full" error; retry later.

Each task carries requested_at (queued), started_at (left the queue and began signing in to Leonardo), submitted_at (prompt submitted to Leonardo), finished_at and attempts; a queued task also reports queue_position (1 = next). A generation itself typically takes about 40 seconds after submitted_at.

get_image_tasks returns every task - queued, running, completed and failed - prompts included; it grows with every image, so use get_image_task to follow a single task.`

// ToolHandler runs one tool call. Returning an error is reserved for protocol
// failures; problems the model could correct belong in the result with IsError
// set, which is what toolError produces.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*CallToolResult, *RPCError)

type registeredTool struct {
	def     Tool
	handler ToolHandler
}

// mcpServer dispatches MCP requests. It holds no per-connection state: the
// legacy sessions belong to the HTTP transport.
type mcpServer struct {
	name         string
	version      string
	instructions string
	// legacy is whether the initialize-based revisions are served alongside the
	// current one.
	legacy bool
	tools  map[string]registeredTool
}

func newMCPServer(instructions string, legacy bool) *mcpServer {
	if instructions == "" {
		instructions = defaultInstructions
	}
	s := &mcpServer{
		name:         appName,
		version:      appVersion,
		instructions: instructions,
		legacy:       legacy,
		tools:        map[string]registeredTool{},
	}
	s.registerTool(generateImageTool())
	s.registerTool(getImageTasksTool())
	s.registerTool(getImageTaskTool())
	return s
}

func (s *mcpServer) registerTool(def Tool, handler ToolHandler) {
	s.tools[def.Name] = registeredTool{def: def, handler: handler}
}

// versionKey carries the protocol version negotiated for the request being
// handled. A legacy session fixes it at initialize; a modern request declares
// it in _meta every time.
type versionKey struct{}

func withVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, versionKey{}, version)
}

func versionFrom(ctx context.Context) string {
	version, _ := ctx.Value(versionKey{}).(string)
	return version
}

// completeResult builds the result envelope. Against a legacy client it is
// empty: resultType and the cache hints mean nothing to an earlier revision.
func (s *mcpServer) completeResult(ctx context.Context) Result {
	if !isModernVersion(versionFrom(ctx)) {
		return Result{}
	}
	return Result{
		ResultType: ResultComplete,
		Meta:       map[string]any{MetaServerInfo: Implementation{Name: s.name, Version: s.version}},
	}
}

func (s *mcpServer) capabilities() ServerCapabilities {
	return ServerCapabilities{Tools: &ToolsCapability{}}
}

// supportedVersions is what this server actually serves, newest first.
func (s *mcpServer) supportedVersions() []string {
	if !s.legacy {
		return ModernVersions
	}
	return SupportedVersions
}

func (s *mcpServer) supports(version string) bool { return contains(s.supportedVersions(), version) }

// Handle dispatches a single request and returns its result, or an error. It
// returns (nil, nil) for a notification that needs no response.
func (s *mcpServer) Handle(ctx context.Context, req *Request) (any, *RPCError) {
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return nil, Errorf(CodeInvalidRequest, "unsupported jsonrpc version %q", req.JSONRPC)
	}
	if versionFrom(ctx) == "" {
		ctx = withVersion(ctx, req.MetaString(MetaProtocolVersion))
	}
	version := versionFrom(ctx)

	// server/discover is how a client finds out which versions are accepted,
	// so it tolerates an absent declaration.
	if req.Method != "server/discover" && !s.supports(version) {
		return nil, UnsupportedVersionError(version, s.supportedVersions())
	}

	switch req.Method {
	case "server/discover":
		return &DiscoverResult{
			Result:            s.completeResult(ctx).cacheable(3600000, CacheScopePublic),
			SupportedVersions: s.supportedVersions(),
			Capabilities:      s.capabilities(),
			Instructions:      s.instructions,
		}, nil
	case "tools/list":
		return s.listTools(ctx), nil
	case "tools/call":
		return s.callTool(ctx, req)
	case "ping":
		return emptyResult, nil
	case "initialize":
		// The transport answers initialize itself, because the handshake is what
		// creates the session.
		return nil, Errorf(CodeInvalidRequest, "initialize is handled by the transport")
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, Errorf(CodeMethodNotFound, "unknown method %q", req.Method)
}

// initialize answers a legacy initialize request. A version this server
// implements is echoed back; anything else gets the newest legacy version,
// which lets the client decide for itself whether it can proceed.
func (s *mcpServer) initialize(req *Request) (*InitializeResult, Implementation, string, *RPCError) {
	var params InitializeParams
	if err := req.Bind(&params); err != nil {
		return nil, Implementation{}, "", err
	}
	version := params.ProtocolVersion
	if !isLegacyVersion(version) {
		version = LatestLegacyVersion
	}
	return &InitializeResult{
		ProtocolVersion: version,
		Capabilities:    s.capabilities(),
		ServerInfo:      Implementation{Name: s.name, Version: s.version},
		Instructions:    s.instructions,
	}, params.ClientInfo, version, nil
}

func (s *mcpServer) listTools(ctx context.Context) *ListToolsResult {
	// Sorted so the list is deterministic and clients can cache it.
	names := make([]string, 0, len(s.tools))
	for name := range s.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, s.tools[name].def)
	}
	return &ListToolsResult{
		Result: s.completeResult(ctx).cacheable(300000, CacheScopePrivate),
		Tools:  tools,
	}
}

func (s *mcpServer) callTool(ctx context.Context, req *Request) (any, *RPCError) {
	var params CallToolParams
	if err := req.Bind(&params); err != nil {
		return nil, err
	}
	tool, ok := s.tools[params.Name]
	if !ok {
		return nil, Errorf(CodeInvalidParams, "unknown tool: %s", params.Name)
	}
	res, err := tool.handler(ctx, params.Arguments)
	if err != nil {
		return nil, err
	}
	res.Result = s.completeResult(ctx)
	return res, nil
}

// toolError reports a failure the model can act on and retry. Per MCP, tool
// failures are reported in-band with isError rather than as a protocol error.
func toolError(format string, args ...any) *CallToolResult {
	return &CallToolResult{Content: textContent(fmt.Sprintf(format, args...)), IsError: true}
}

// --- Tools ---

const mcpToolName = "generate_image_with_leonardo"

// generateImageTool exposes browserSession.generate, the same operation as
// POST /api/generate.
func generateImageTool() (Tool, ToolHandler) {
	def := Tool{
		Name:  mcpToolName,
		Title: "Generate image with Leonardo.ai",
		Description: "Generate an image with Leonardo.ai from a text prompt. Generation takes one to a few minutes. " +
			"If the image is ready within wait_seconds the result is its URL; otherwise the result says the task is " +
			"still running and gives its task_id - call get_image_task with that id every 20-30 seconds until its " +
			"status is completed (then use its uri) or failed. The generation carries on either way.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"prompt": map[string]any{
					"type":        "string",
					"description": "The text prompt describing the image to generate.",
				},
				"size": map[string]any{
					"type":        "string",
					"enum":        []string{"2:3", "1:1", "16:9"},
					"description": "Aspect ratio of the image. Defaults to the server's configured default (" + cfg.DefaultSize + ") if omitted.",
				},
				"wait_seconds": map[string]any{
					"type":    "integer",
					"minimum": 0,
					"description": fmt.Sprintf("How long to wait for the image before returning the task as running "+
						"(default %s). Keep it below your own tool-call timeout; 0 returns immediately.", waitDefaultText()),
				},
			},
			"required": []string{"prompt"},
		},
		// Each call spends Leonardo tokens and yields a new image, so it is
		// neither read-only nor idempotent, and it reaches an outside service.
		Annotations: &ToolAnnotations{Title: "Generate image with Leonardo.ai", OpenWorldHint: true},
	}
	handler := func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Prompt      string `json:"prompt"`
			Size        string `json:"size"`
			WaitSeconds *int   `json:"wait_seconds"`
		}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &args); err != nil {
				return toolError("invalid arguments: %v", err), nil
			}
		}
		prompt := strings.TrimSpace(args.Prompt)
		if prompt == "" {
			return toolError("prompt is required"), nil
		}
		size, err := resolveSize(args.Size)
		if err != nil {
			return toolError("%v", err), nil
		}

		wait := time.Duration(cfg.MCP.GenerateWaitSeconds) * time.Second
		if args.WaitSeconds != nil {
			if *args.WaitSeconds <= 0 {
				// Zero means "don't wait"; generate treats 0 as "wait for ever".
				wait = time.Nanosecond
			} else {
				wait = time.Duration(*args.WaitSeconds) * time.Second
			}
		} else if wait < 0 {
			wait = 0
		}

		log.Printf("[mcp] generating (size %s): %s", size, prompt)
		id, url, pending, err := generate(prompt, size, wait)
		if err != nil {
			return toolError("%v", err), nil
		}
		if pending {
			info, _ := lookupTask(id)
			where := "is still being generated"
			if info.Status == TaskQueued {
				where = fmt.Sprintf("is queued at position %d", info.QueuePosition)
			}
			return &CallToolResult{
				Content: textContent(fmt.Sprintf("The image %s (task_id: %s). Do not call "+
					"generate_image_with_leonardo again for it; call get_image_task with this task_id in "+
					"20-30 seconds. When its status is completed, its uri is the image URL.", where, id)),
				StructuredContent: info,
			}, nil
		}
		// The text stays the bare URL, as before; the task id rides along in
		// structured content and a second text block.
		return &CallToolResult{
			Content:           append(textContent(url), Content{Type: "text", Text: "task_id: " + id}),
			StructuredContent: map[string]string{"task_id": id, "status": TaskCompleted, "url": url},
		}, nil
	}
	return def, handler
}

// waitDefaultText describes the configured default wait for the tool schema.
func waitDefaultText() string {
	if cfg.MCP.GenerateWaitSeconds < 0 {
		return "until the image is ready"
	}
	return fmt.Sprintf("%d seconds", cfg.MCP.GenerateWaitSeconds)
}

// getImageTasksTool exposes the task dictionary, the same data as GET /api/tasks.
func getImageTasksTool() (Tool, ToolHandler) {
	def := Tool{
		Name:  "get_image_tasks",
		Title: "List image tasks",
		Description: "Return every task - queued, running, completed and failed - as a JSON object keyed by task id " +
			"(the SHA-256 of the prompt), each value holding its status, prompt, image uri (once there is one) and error " +
			"(if it failed). It grows with every image; to follow one generation, use get_image_task with its task_id instead.",
		InputSchema: map[string]any{"type": "object", "additionalProperties": false},
		Annotations: &ToolAnnotations{Title: "List image tasks", ReadOnlyHint: true, IdempotentHint: true},
	}
	handler := func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		all := tasks.All()
		pretty, err := json.MarshalIndent(all, "", "  ")
		if err != nil {
			return toolError("could not serialise tasks: %v", err), nil
		}
		return &CallToolResult{Content: textContent(string(pretty)), StructuredContent: all}, nil
	}
	return def, handler
}

// getImageTaskTool exposes one task, the same data as GET /api/tasks/{id}.
func getImageTaskTool() (Tool, ToolHandler) {
	def := Tool{
		Name:  "get_image_task",
		Title: "Get an image task",
		Description: "Return everything known about one task by its task id (the SHA-256 hex digest of the prompt): " +
			"its status (queued, running, completed or failed), queue_position while queued (1 = next), the prompt, " +
			"the image uri once there is one, the error if it failed, and requested_at / started_at / submitted_at / " +
			"finished_at / attempts. Use it to poll a generation that generate_image_with_leonardo returned before it finished.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{
					"type":        "string",
					"description": "The task id returned by generate_image_with_leonardo, or the SHA-256 hex digest of the prompt.",
				},
			},
			"required": []string{"task_id"},
		},
		Annotations: &ToolAnnotations{Title: "Get an image task", ReadOnlyHint: true},
	}
	handler := func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			TaskID string `json:"task_id"`
		}
		if len(raw) > 0 && string(raw) != "null" {
			if err := json.Unmarshal(raw, &args); err != nil {
				return toolError("invalid arguments: %v", err), nil
			}
		}
		if strings.TrimSpace(args.TaskID) == "" {
			return toolError("task_id is required"), nil
		}
		info, ok := lookupTask(args.TaskID)
		if !ok {
			return toolError("unknown task %s: no such task has been requested", args.TaskID), nil
		}
		pretty, err := json.MarshalIndent(info, "", "  ")
		if err != nil {
			return toolError("could not serialise task: %v", err), nil
		}
		return &CallToolResult{Content: textContent(string(pretty)), StructuredContent: info}, nil
	}
	return def, handler
}

// --- Legacy sessions ---

// mcpSession is one legacy Streamable HTTP session, created by initialize and
// addressed afterwards by the Mcp-Session-Id header.
type mcpSession struct {
	ID       string
	Version  string
	Client   Implementation
	LastSeen time.Time
}

// sessionStore holds the legacy sessions. Modern requests never touch it.
// Sessions idle for longer than ttl are dropped, so a client that disappears
// without a DELETE does not leak one.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*mcpSession
	ttl      time.Duration
}

func newSessionStore(ttl time.Duration) *sessionStore {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	return &sessionStore{sessions: map[string]*mcpSession{}, ttl: ttl}
}

func (s *sessionStore) Create(version string, client Implementation) *mcpSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	sess := &mcpSession{ID: newSessionID(), Version: version, Client: client, LastSeen: time.Now()}
	s.sessions[sess.ID] = sess
	return sess
}

// Get returns a live session and marks it as used.
func (s *sessionStore) Get(id string) (*mcpSession, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictLocked()
	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	sess.LastSeen = time.Now()
	return sess, true
}

func (s *sessionStore) Delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[id]; !ok {
		return false
	}
	delete(s.sessions, id)
	return true
}

func (s *sessionStore) evictLocked() {
	cutoff := time.Now().Add(-s.ttl)
	for id, sess := range s.sessions {
		if sess.LastSeen.Before(cutoff) {
			delete(s.sessions, id)
		}
	}
}

// newSessionID returns a cryptographically random id, which is what the legacy
// specification requires of a session id.
func newSessionID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "mcp-" + time.Now().UTC().Format("20060102T150405.000000000")
	}
	return "mcp-" + hex.EncodeToString(buf[:])
}
