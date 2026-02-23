package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"
)

// TestToolReplayIntegration verifies that tool_call and tool_call_update messages
// are correctly relayed through the WebSocket bridge, with proper title, _meta,
// rawInput, and content fields preserved.
//
// This is a full integration test that starts a fake acp-multiplex socket,
// an HTTP server with WebSocket handler, and connects as a browser client.
//
// Skip by default: run in a separate VM to avoid disrupting work.
//
//	go test -run TestToolReplayIntegration -count=1
func TestToolReplayIntegration(t *testing.T) {
	t.Skip("Integration test: run in a separate VM to avoid disrupting work. Use: go test -run TestToolReplayIntegration -count=1 -v")

	tmpDir := t.TempDir()
	sockDir := filepath.Join(tmpDir, "acp-multiplex")
	os.MkdirAll(sockDir, 0700)
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	// Build replay messages
	messages := buildTestReplay("test-session-1")

	// Start fake acp-multiplex socket server
	sockPath := filepath.Join(sockDir, "99999.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			for _, m := range messages {
				conn.Write(m)
				conn.Write([]byte("\n"))
			}
			// Keep connection open for bridging
			buf := make([]byte, 4096)
			for {
				conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				_, err := conn.Read(buf)
				if err != nil {
					conn.Close()
					return
				}
			}
		}
	}()

	// Start HTTP server with WebSocket handler
	testMode = true
	mux := http.NewServeMux()
	mux.Handle("/ws", &websocket.Server{
		Handler: func(ws *websocket.Conn) {
			bridgeWebSocket(ws, sockPath)
		},
		Handshake: func(config *websocket.Config, r *http.Request) error {
			return nil // skip origin check in test
		},
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// Connect as WebSocket client
	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws?sock=99999"
	ws, err := websocket.Dial(wsURL, "", srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	// Collect all messages
	var received []json.RawMessage
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var msg string
		err := websocket.Message.Receive(ws, &msg)
		if err != nil {
			break
		}
		received = append(received, json.RawMessage(msg))
	}

	if len(received) == 0 {
		t.Fatal("received no messages")
	}
	t.Logf("received %d messages", len(received))

	// Parse and verify tool_call messages
	type toolCall struct {
		ID       string
		Title    string
		ToolName string // from _meta.claudeCode.toolName
		Status   string
		Content  string
	}
	tools := make(map[string]*toolCall)

	for _, raw := range received {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Update json.RawMessage `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Method != "session/update" {
			continue
		}
		var update struct {
			SessionUpdate string          `json:"sessionUpdate"`
			ToolCallId    string          `json:"toolCallId"`
			Title         string          `json:"title"`
			Status        string          `json:"status"`
			Meta          json.RawMessage `json:"_meta"`
			RawInput      json.RawMessage `json:"rawInput"`
			Content       json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(msg.Params.Update, &update); err != nil {
			continue
		}

		switch update.SessionUpdate {
		case "tool_call":
			tc := tools[update.ToolCallId]
			if tc == nil {
				tc = &toolCall{ID: update.ToolCallId}
				tools[update.ToolCallId] = tc
			}
			if update.Title != "" {
				tc.Title = update.Title
			}
			// Extract _meta.claudeCode.toolName
			if len(update.Meta) > 0 {
				var meta struct {
					ClaudeCode struct {
						ToolName string `json:"toolName"`
					} `json:"claudeCode"`
				}
				json.Unmarshal(update.Meta, &meta)
				if meta.ClaudeCode.ToolName != "" {
					tc.ToolName = meta.ClaudeCode.ToolName
				}
			}
		case "tool_call_update":
			tc := tools[update.ToolCallId]
			if tc == nil {
				tc = &toolCall{ID: update.ToolCallId}
				tools[update.ToolCallId] = tc
			}
			tc.Status = update.Status
			// Extract content.text (object form)
			if len(update.Content) > 0 {
				var obj struct {
					Text string `json:"text"`
				}
				if json.Unmarshal(update.Content, &obj) == nil && obj.Text != "" {
					tc.Content = obj.Text
				}
			}
		}
	}

	// Expected tools with their properties
	expected := []struct {
		id       string
		title    string
		toolName string
		status   string
		content  string
	}{
		{"tc1", `grep "authenticate" src/`, "Grep", "completed", "Found 5 matches in src/auth/"},
		{"tc2", "Read src/auth/handler.go", "Read", "completed", "package auth\n\nfunc HandleLogin(...)"},
		{"tc3", "Read src/auth/middleware.go", "Read", "completed", "package auth\n\nfunc AuthMiddleware(...)"},
		{"tc4", "Read src/auth/handler.go", "Read", "completed", "package auth\n\nfunc HandleLogin(...)"},
		{"tc5", "Edit src/auth/handler.go", "Edit", "completed", "File edited successfully"},
		{"tc6", "Run tests", "Bash", "completed", "ok  src/auth  0.5s"},
		{"tc7", "Build project", "Bash", "completed", "Build successful"},
		{"tc8", "Read src/auth/handler_test.go", "Read", "completed", "package auth\n\nfunc TestHandleLogin(t *testing.T) {"},
		{"tc9", "Edit src/auth/handler_test.go", "Edit", "completed", "File edited successfully"},
		{"tc10", "Run tests with verbose output", "Bash", "completed", "=== RUN TestHandleLoginError\n--- PASS: TestHandleLoginError\nPASS"},
	}

	for _, exp := range expected {
		tc, ok := tools[exp.id]
		if !ok {
			t.Errorf("tool %s: not found in replay", exp.id)
			continue
		}
		if tc.Title != exp.title {
			t.Errorf("tool %s title: got %q, want %q", exp.id, tc.Title, exp.title)
		}
		if tc.ToolName != exp.toolName {
			t.Errorf("tool %s toolName (_meta.claudeCode): got %q, want %q", exp.id, tc.ToolName, exp.toolName)
		}
		if tc.Status != exp.status {
			t.Errorf("tool %s status: got %q, want %q", exp.id, tc.Status, exp.status)
		}
		if tc.Content != exp.content {
			t.Errorf("tool %s content: got %q, want %q", exp.id, tc.Content, exp.content)
		}
	}

	// Verify no tools have empty title or toolName (the "?" bug)
	for id, tc := range tools {
		if tc.Title == "" && tc.ToolName == "" {
			t.Errorf("tool %s: both title and toolName empty — would render as '?'", id)
		}
		if tc.Content == "" {
			t.Errorf("tool %s (%s): content empty — output would not display", id, tc.ToolName)
		}
	}
}

// buildTestReplay constructs a realistic set of replay messages simulating
// a multi-turn conversation with various tool calls (Grep, Read, Edit, Bash).
func buildTestReplay(sessionID string) [][]byte {
	msg := func(v interface{}) []byte {
		b, _ := json.Marshal(v)
		return b
	}

	update := func(kind string, extra map[string]interface{}) []byte {
		u := map[string]interface{}{"sessionUpdate": kind}
		for k, v := range extra {
			u[k] = v
		}
		return msg(map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "session/update",
			"params": map[string]interface{}{
				"sessionId": sessionID,
				"update":    u,
			},
		})
	}

	toolCall := func(id, title, toolName string, rawInput map[string]string) []byte {
		return update("tool_call", map[string]interface{}{
			"toolCallId": id,
			"title":      title,
			"_meta":      map[string]interface{}{"claudeCode": map[string]string{"toolName": toolName}},
			"rawInput":   rawInput,
		})
	}

	toolResult := func(id, toolName, text string) []byte {
		return update("tool_call_update", map[string]interface{}{
			"toolCallId": id,
			"status":     "completed",
			"_meta":      map[string]interface{}{"claudeCode": map[string]string{"toolName": toolName}},
			"content":    map[string]string{"type": "text", "text": text},
		})
	}

	agentMsg := func(text string) []byte {
		return update("agent_message_chunk", map[string]interface{}{
			"content": map[string]string{"type": "text", "text": text},
		})
	}

	userMsg := func(text string) []byte {
		return update("user_message_chunk", map[string]interface{}{
			"content": map[string]string{"type": "text", "text": text},
		})
	}

	turnComplete := func() []byte { return update("turn_complete", nil) }

	var messages [][]byte

	// Meta
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "method": "acp-multiplex/meta",
		"params": map[string]interface{}{"name": "TEST: Tool Replay"},
	}))

	// Init + session/new responses
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{
			"protocolVersion": 1,
			"agentInfo":       map[string]string{"name": "test-agent", "title": "Test Agent"},
		},
	}))
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{"sessionId": sessionID},
	}))

	// Turn 1: search + read
	messages = append(messages, userMsg("Search the codebase for authentication handling"))
	messages = append(messages, agentMsg("Let me search for authentication code."))
	messages = append(messages, toolCall("tc1", `grep "authenticate" src/`, "Grep", map[string]string{"pattern": "authenticate", "path": "src/"}))
	messages = append(messages, toolResult("tc1", "Grep", "Found 5 matches in src/auth/"))
	messages = append(messages, toolCall("tc2", "Read src/auth/handler.go", "Read", map[string]string{"file_path": "src/auth/handler.go"}))
	messages = append(messages, toolResult("tc2", "Read", "package auth\n\nfunc HandleLogin(...)"))
	messages = append(messages, toolCall("tc3", "Read src/auth/middleware.go", "Read", map[string]string{"file_path": "src/auth/middleware.go"}))
	messages = append(messages, toolResult("tc3", "Read", "package auth\n\nfunc AuthMiddleware(...)"))
	messages = append(messages, agentMsg("I found the authentication code. Here's what I see..."))
	messages = append(messages, turnComplete())

	// Turn 2: fix bug with edit + bash
	messages = append(messages, userMsg("Now fix the bug in the login handler"))
	messages = append(messages, agentMsg("I'll fix the login handler bug."))
	messages = append(messages, toolCall("tc4", "Read src/auth/handler.go", "Read", map[string]string{"file_path": "src/auth/handler.go"}))
	messages = append(messages, toolResult("tc4", "Read", "package auth\n\nfunc HandleLogin(...)"))
	messages = append(messages, toolCall("tc5", "Edit src/auth/handler.go", "Edit", map[string]string{
		"file_path": "src/auth/handler.go", "old_string": "if err != nil {", "new_string": "if err != nil {\n\tlog.Error(err)",
	}))
	messages = append(messages, toolResult("tc5", "Edit", "File edited successfully"))
	messages = append(messages, toolCall("tc6", "Run tests", "Bash", map[string]string{"command": "go test ./src/auth/..."}))
	messages = append(messages, toolResult("tc6", "Bash", "ok  src/auth  0.5s"))
	messages = append(messages, toolCall("tc7", "Build project", "Bash", map[string]string{"command": "go build ./..."}))
	messages = append(messages, toolResult("tc7", "Bash", "Build successful"))
	messages = append(messages, agentMsg("The bug is fixed. Tests pass and the build succeeds."))
	messages = append(messages, turnComplete())

	// Turn 3: add test
	messages = append(messages, userMsg("Now add a test for the fix"))
	messages = append(messages, agentMsg("I'll add a test for the login fix."))
	messages = append(messages, toolCall("tc8", "Read src/auth/handler_test.go", "Read", map[string]string{"file_path": "src/auth/handler_test.go"}))
	messages = append(messages, toolResult("tc8", "Read", "package auth\n\nfunc TestHandleLogin(t *testing.T) {"))
	messages = append(messages, toolCall("tc9", "Edit src/auth/handler_test.go", "Edit", map[string]string{
		"file_path": "src/auth/handler_test.go", "old_string": "func TestHandleLogin", "new_string": "func TestHandleLoginError",
	}))
	messages = append(messages, toolResult("tc9", "Edit", "File edited successfully"))
	messages = append(messages, toolCall("tc10", "Run tests with verbose output", "Bash", map[string]string{"command": "go test -v ./src/auth/..."}))
	messages = append(messages, toolResult("tc10", "Bash", "=== RUN TestHandleLoginError\n--- PASS: TestHandleLoginError\nPASS"))
	messages = append(messages, agentMsg("Test added and passing. All done."))
	messages = append(messages, turnComplete())

	return messages
}

// TestToolReplayMessageFormat verifies the structure of replay messages
// without starting servers. This is a fast unit test.
func TestToolReplayMessageFormat(t *testing.T) {
	messages := buildTestReplay("test-session-1")

	type toolInfo struct {
		id       string
		title    string
		toolName string
		content  string
	}
	var tools []toolInfo

	for _, raw := range messages {
		var msg struct {
			Method string `json:"method"`
			Params struct {
				Update struct {
					SessionUpdate string          `json:"sessionUpdate"`
					ToolCallId    string          `json:"toolCallId"`
					Title         string          `json:"title"`
					Meta          json.RawMessage `json:"_meta"`
					Content       json.RawMessage `json:"content"`
				} `json:"update"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &msg); err != nil || msg.Method != "session/update" {
			continue
		}
		u := msg.Params.Update

		switch u.SessionUpdate {
		case "tool_call":
			if u.Title == "" {
				t.Errorf("tool_call %s: missing title field", u.ToolCallId)
			}
			// Check _meta.claudeCode.toolName exists
			if len(u.Meta) > 0 {
				var meta struct {
					ClaudeCode struct {
						ToolName string `json:"toolName"`
					} `json:"claudeCode"`
				}
				json.Unmarshal(u.Meta, &meta)
				if meta.ClaudeCode.ToolName == "" {
					t.Errorf("tool_call %s: missing _meta.claudeCode.toolName", u.ToolCallId)
				}
				tools = append(tools, toolInfo{
					id: u.ToolCallId, title: u.Title, toolName: meta.ClaudeCode.ToolName,
				})
			}

		case "tool_call_update":
			// Verify content is extractable (object with .text)
			if len(u.Content) > 0 {
				var obj struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(u.Content, &obj); err != nil || obj.Text == "" {
					t.Errorf("tool_call_update %s: content not extractable as {text}: %s", u.ToolCallId, string(u.Content))
				}
				// Find matching tool and record content
				for i := range tools {
					if tools[i].id == u.ToolCallId {
						tools[i].content = obj.Text
					}
				}
			}
		}
	}

	// Every tool should have title, toolName, and content
	for _, tc := range tools {
		if tc.title == "" {
			t.Errorf("tool %s: no title (would show toolName as fallback)", tc.id)
		}
		if tc.toolName == "" {
			t.Errorf("tool %s: no toolName (icon selection and Edit/Write detection would fail)", tc.id)
		}
		if tc.content == "" {
			t.Errorf("tool %s (%s): no content (output would not display)", tc.id, tc.toolName)
		}
	}

	// Verify tool types used
	typeCount := map[string]int{}
	for _, tc := range tools {
		typeCount[tc.toolName]++
	}
	if typeCount["Grep"] != 1 {
		t.Errorf("expected 1 Grep tool, got %d", typeCount["Grep"])
	}
	if typeCount["Read"] != 4 {
		t.Errorf("expected 4 Read tools, got %d", typeCount["Read"])
	}
	if typeCount["Edit"] != 2 {
		t.Errorf("expected 2 Edit tools, got %d", typeCount["Edit"])
	}
	if typeCount["Bash"] != 3 {
		t.Errorf("expected 3 Bash tools, got %d", typeCount["Bash"])
	}

	t.Logf("verified %d tools: %v", len(tools), typeCount)
}

// TestToolOutputTextExtraction tests the various content formats that
// toolOutputText() in index.html must handle. This documents the contract
// between acp-multiplex messages and the JS rendering code.
func TestToolOutputTextExtraction(t *testing.T) {
	// These are the content formats we've seen in the wild.
	// The JS toolOutputText() function must handle all of them.
	cases := []struct {
		name    string
		content json.RawMessage
		want    string
	}{
		{
			name:    "object with text field",
			content: json.RawMessage(`{"type":"text","text":"ok src/auth 0.5s"}`),
			want:    "ok src/auth 0.5s",
		},
		{
			name:    "plain string",
			content: json.RawMessage(`"Build successful"`),
			want:    "Build successful",
		},
		{
			name:    "array of content objects",
			content: json.RawMessage(`[{"type":"text","text":"line 1"},{"type":"text","text":"line 2"}]`),
			want:    "line 1",
		},
		{
			name:    "nested content wrapper",
			content: json.RawMessage(`[{"content":{"type":"text","text":"nested text"}}]`),
			want:    "nested text",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Simulate the JS toolOutputText logic in Go
			got := extractToolOutputText(tc.content)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// extractToolOutputText mirrors the JS toolOutputText() function.
// Used to verify the extraction logic matches what the browser does.
func extractToolOutputText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}

	// Try as array
	var arr []json.RawMessage
	if json.Unmarshal(content, &arr) == nil && len(arr) > 0 {
		for _, item := range arr {
			// Try item.content.text (nested wrapper)
			var wrapper struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if json.Unmarshal(item, &wrapper) == nil && wrapper.Content.Text != "" {
				return wrapper.Content.Text
			}
			// Try item.text directly
			var obj struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(item, &obj) == nil && obj.Text != "" {
				return obj.Text
			}
		}
	}

	// Try as string
	var s string
	if json.Unmarshal(content, &s) == nil && s != "" {
		return s
	}

	// Try as object with .text
	var obj struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &obj) == nil && obj.Text != "" {
		return obj.Text
	}

	return ""
}

// TestPermissionReplayInteractive verifies that a pending permission
// at the end of replay would be found by the backward search in flushReplay.
// This is a regression test for the fix in commit 13d8e95.
func TestPermissionReplayInteractive(t *testing.T) {
	t.Skip("Integration test: run in a separate VM to avoid disrupting work. Use: go test -run TestPermissionReplayInteractive -count=1 -v")

	tmpDir := t.TempDir()
	sockDir := filepath.Join(tmpDir, "acp-multiplex")
	os.MkdirAll(sockDir, 0700)
	t.Setenv("XDG_RUNTIME_DIR", tmpDir)

	sid := "test-perm-session"
	msg := func(v interface{}) []byte {
		b, _ := json.Marshal(v)
		return b
	}
	update := func(kind string, extra map[string]interface{}) []byte {
		u := map[string]interface{}{"sessionUpdate": kind}
		for k, v := range extra {
			u[k] = v
		}
		return msg(map[string]interface{}{
			"jsonrpc": "2.0", "method": "session/update",
			"params": map[string]interface{}{"sessionId": sid, "update": u},
		})
	}

	var messages [][]byte
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "method": "acp-multiplex/meta",
		"params": map[string]interface{}{"name": "TEST: Permission"},
	}))
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{
			"protocolVersion": 1,
			"agentInfo":       map[string]string{"name": "test", "title": "Test"},
		},
	}))
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`0`),
		"result": map[string]interface{}{"sessionId": sid},
	}))

	// Some turns before the permission
	messages = append(messages, update("user_message_chunk", map[string]interface{}{
		"content": map[string]string{"type": "text", "text": "Do something"},
	}))
	messages = append(messages, update("agent_message_chunk", map[string]interface{}{
		"content": map[string]string{"type": "text", "text": "I need permission to proceed."},
	}))
	messages = append(messages, update("turn_complete", nil))

	// Permission request (simulating ExitPlanMode or tool permission)
	messages = append(messages, msg(map[string]interface{}{
		"jsonrpc": "2.0", "id": json.RawMessage(`42`),
		"method": "session/request_permission",
		"params": map[string]interface{}{
			"sessionId": sid,
			"permission": map[string]interface{}{
				"type":    "plan",
				"title":   "Ready to code?",
				"message": "I have a plan to implement the feature.",
			},
		},
	}))

	// Start fake socket + server
	sockPath := filepath.Join(sockDir, "88888.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		for _, m := range messages {
			conn.Write(m)
			conn.Write([]byte("\n"))
		}
		buf := make([]byte, 4096)
		for {
			conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			if _, err := conn.Read(buf); err != nil {
				conn.Close()
				return
			}
		}
	}()

	testMode = true
	mux := http.NewServeMux()
	mux.Handle("/ws", &websocket.Server{
		Handler:   func(ws *websocket.Conn) { bridgeWebSocket(ws, sockPath) },
		Handshake: func(*websocket.Config, *http.Request) error { return nil },
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	wsURL := strings.Replace(srv.URL, "http://", "ws://", 1) + "/ws?sock=88888"
	ws, err := websocket.Dial(wsURL, "", srv.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()

	var received []string
	ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var msg string
		if err := websocket.Message.Receive(ws, &msg); err != nil {
			break
		}
		received = append(received, msg)
	}

	// The permission message should be present in the replay
	foundPermission := false
	permIdx := -1
	for i, raw := range received {
		if strings.Contains(raw, "request_permission") {
			foundPermission = true
			permIdx = i
		}
	}

	if !foundPermission {
		t.Fatal("permission message not found in replay")
	}

	// Permission should be the last message (or near-last)
	t.Logf("permission at index %d of %d messages", permIdx, len(received))
	if permIdx < len(received)-2 {
		t.Errorf("permission at index %d but expected near end (total %d)", permIdx, len(received))
		for i := permIdx; i < len(received); i++ {
			t.Logf("  msg[%d]: %.100s", i, received[i])
		}
	}

	_ = fmt.Sprintf // avoid unused import
}
