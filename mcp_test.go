package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestMCP(t *testing.T, mc MCPConfig) http.Handler {
	t.Helper()
	cfg.DefaultSize = "16:9"
	if mc.AllowedOrigins == nil {
		mc.AllowedOrigins = []string{"http://localhost"}
	}
	return newMCPTransport(newMCPServer("", mc.LegacyCompatibility), mc).Handler()
}

func post(h http.Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestLegacyHandshakeAndToolsList(t *testing.T) {
	h := newTestMCP(t, MCPConfig{LegacyCompatibility: true})

	rec := post(h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","clientInfo":{"name":"test","version":"1"}}}`, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("initialize: status %d: %s", rec.Code, rec.Body)
	}
	sid := rec.Header().Get(sessionHeader)
	if sid == "" {
		t.Fatal("initialize did not mint a session")
	}
	res := decode(t, rec)["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("negotiated %v", res["protocolVersion"])
	}

	rec = post(h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, map[string]string{sessionHeader: sid})
	tools := decode(t, rec)["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 3 || tools[0].(map[string]any)["name"] != mcpToolName ||
		tools[1].(map[string]any)["name"] != "get_image_task" || tools[2].(map[string]any)["name"] != "get_image_tasks" {
		t.Fatalf("tools/list: %s", rec.Body)
	}

	if rec := post(h, `{"jsonrpc":"2.0","id":3,"method":"tools/list"}`, map[string]string{sessionHeader: "nope"}); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown session: status %d", rec.Code)
	}
}

func TestBareToolsListWithoutHandshake(t *testing.T) {
	// The README's curl examples call tools/list with no initialize at all.
	h := newTestMCP(t, MCPConfig{LegacyCompatibility: true})
	rec := post(h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), mcpToolName) {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
}

func TestModernRequest(t *testing.T) {
	h := newTestMCP(t, MCPConfig{})
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"2026-07-28"}}}`

	if rec := post(h, body, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing mirrored headers: status %d", rec.Code)
	}
	rec := post(h, body, map[string]string{"MCP-Protocol-Version": "2026-07-28", "Mcp-Method": "tools/list"})
	res := decode(t, rec)["result"].(map[string]any)
	if res["resultType"] != ResultComplete {
		t.Fatalf("result: %s", rec.Body)
	}

	// With legacy compatibility off, initialize is gone.
	if rec := post(h, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{}}`, nil); rec.Code != http.StatusNotFound {
		t.Fatalf("initialize without legacy: status %d", rec.Code)
	}
}

func TestToolArgumentErrorsAreInBand(t *testing.T) {
	h := newTestMCP(t, MCPConfig{LegacyCompatibility: true})
	for _, args := range []string{`{}`, `{"prompt":"x","size":"4:3"}`} {
		rec := post(h, `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"`+mcpToolName+`","arguments":`+args+`}}`, nil)
		res := decode(t, rec)["result"].(map[string]any)
		if res["isError"] != true {
			t.Fatalf("args %s: %s", args, rec.Body)
		}
	}
}

func TestAuthTokenAndOrigin(t *testing.T) {
	h := newTestMCP(t, MCPConfig{LegacyCompatibility: true, AuthToken: "s3cret"})
	body := `{"jsonrpc":"2.0","id":1,"method":"ping"}`
	if rec := post(h, body, nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: status %d", rec.Code)
	}
	if rec := post(h, body, map[string]string{"Authorization": "Bearer s3cret"}); rec.Code != http.StatusOK {
		t.Fatalf("with token: status %d", rec.Code)
	}
	if rec := post(h, body, map[string]string{"Authorization": "Bearer s3cret", "Origin": "http://evil.example"}); rec.Code != http.StatusForbidden {
		t.Fatalf("bad origin: status %d", rec.Code)
	}
	if rec := post(h, body, map[string]string{"Authorization": "Bearer s3cret", "Origin": "http://localhost:8080"}); rec.Code != http.StatusOK {
		t.Fatalf("allowed origin: status %d", rec.Code)
	}
}
