package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

// capturingSender records every payload pushed to it.
type capturingSender struct {
	mu    sync.Mutex
	sent  []any
	failN int32 // fail the next N sends
}

func (c *capturingSender) Send(payload any) error {
	if atomic.LoadInt32(&c.failN) > 0 {
		atomic.AddInt32(&c.failN, -1)
		return errors.New("simulated send failure")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, payload)
	return nil
}

func (c *capturingSender) Last() (Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return Response{}, false
	}
	r, _ := c.sent[len(c.sent)-1].(Response)
	return r, true
}

func (c *capturingSender) All() []Response {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Response, 0, len(c.sent))
	for _, s := range c.sent {
		if r, ok := s.(Response); ok {
			out = append(out, r)
		}
	}
	return out
}

func (c *capturingSender) Reset() {
	c.mu.Lock()
	c.sent = nil
	c.mu.Unlock()
}

func mustJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// unwrapContent extracts the JSON-encoded text from an MCP content
// block ({"content":[{"type":"text","text":"..."}]}) and unmarshals
// it into v.
func unwrapContent(raw json.RawMessage, v any) error {
	var wrapper struct {
		Content []struct{ Type, Text string } `json:"content"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return err
	}
	if len(wrapper.Content) == 0 {
		return errors.New("empty content block")
	}
	return json.Unmarshal([]byte(wrapper.Content[0].Text), v)
}

// ----- dispatch: standard methods -----

func TestPing(t *testing.T) {
	s := NewServer(nil)
	// nil sender should not panic; ping returns an empty object.
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "ping",
	}))
}

func TestInitialize(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
		"params": map[string]any{
			"protocolVersion": "2024-11-05",
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	}))
	r, ok := cap.Last()
	if !ok || r.Error != nil {
		t.Fatalf("initialize: %+v %v", r, ok)
	}
	var init struct {
		ProtocolVersion string `json:"protocolVersion"`
		ServerInfo      struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"serverInfo"`
	}
	if err := json.Unmarshal(r.Result, &init); err != nil {
		t.Fatal(err)
	}
	if init.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion: %s", init.ProtocolVersion)
	}
	if init.ServerInfo.Name == "" {
		t.Error("serverInfo.name missing")
	}
}

func TestToolsListPagination(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.SetPageSize(2) // force pagination
	for i := 0; i < 5; i++ {
		s.Register(&Tool{
			Name:    fmt.Sprintf("self.tool_%d", i),
			Desc:    fmt.Sprintf("tool %d", i),
			Schema:  map[string]any{"type": "object"},
			Handler: func(ctx context.Context, args json.RawMessage) (any, error) { return nil, nil },
		})
	}

	seen := map[string]bool{}
	cursor := ""
	for i := 0; i < 8; i++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		s.Handle(context.Background(), mustJSON(t, map[string]any{
			"jsonrpc": "2.0", "id": i, "method": "tools/list",
			"params": params,
		}))
		r, _ := cap.Last()
		var resp struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(r.Result, &resp); err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		for _, tool := range resp.Tools {
			if seen[tool.Name] {
				t.Errorf("duplicate tool %s across pages", tool.Name)
			}
			seen[tool.Name] = true
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("expected 5 tools, got %d", len(seen))
	}
}

func TestToolsListUserOnlyFilter(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Register(&Tool{Name: "self.normal", Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }})
	s.Register(&Tool{Name: "self.admin", UserOnly: true, Handler: func(context.Context, json.RawMessage) (any, error) { return nil, nil }})

	// Without withUserTools, only normal is returned.
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}))
	r, _ := cap.Last()
	var resp struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	json.Unmarshal(r.Result, &resp)
	if len(resp.Tools) != 1 || resp.Tools[0].Name != "self.normal" {
		t.Errorf("filter failed: %+v", resp.Tools)
	}

	// With withUserTools=true, both are returned.
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
		"params": map[string]any{"withUserTools": true},
	}))
	r, _ = cap.Last()
	resp.Tools = nil
	json.Unmarshal(r.Result, &resp)
	if len(resp.Tools) != 2 {
		t.Errorf("expected 2 tools, got %d", len(resp.Tools))
	}
}

// ----- dispatch: tools/call -----

func TestToolsCallSuccess(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Register(&Tool{
		Name:   "self.echo",
		Schema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"echoed": string(args)}, nil
		},
	})
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "self.echo",
			"arguments": map[string]any{"x": 1},
		},
	}))
	r, _ := cap.Last()
	if r.Error != nil {
		t.Fatalf("tools/call: %+v", r.Error)
	}
	// Result is wrapped in MCP content block.
	var content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	wrapper := struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}{}
	if err := json.Unmarshal(r.Result, &wrapper); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	content = wrapper.Content
	if len(content) != 1 || content[0].Type != "text" {
		t.Errorf("content shape: %+v", content)
	}
}

func TestToolsCallUnknown(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "self.does_not_exist"},
	}))
	r, _ := cap.Last()
	if r.Error == nil {
		t.Fatal("expected error")
	}
	if r.Error.Code != -32601 {
		t.Errorf("expected method-not-found (-32601), got %d", r.Error.Code)
	}
}

func TestToolsCallMissingName(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{},
	}))
	r, _ := cap.Last()
	if r.Error == nil {
		t.Fatal("expected error for missing name")
	}
	if r.Error.Code != -32602 {
		t.Errorf("expected invalid-params (-32602), got %d", r.Error.Code)
	}
}

func TestToolsCallInvalidParams(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	// params must be an object; sending a string is malformed.
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": "not an object",
	}))
	r, _ := cap.Last()
	if r.Error == nil {
		t.Fatal("expected error for invalid params")
	}
}

// ----- dispatch: direct method invocation -----

func TestDirectMethodInvocation(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Register(&Tool{
		Name:   "self.direct",
		Schema: map[string]any{"type": "object"},
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return "ok", nil
		},
	})
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "self.direct",
	}))
	r, _ := cap.Last()
	if r.Error != nil {
		t.Fatalf("direct: %+v", r.Error)
	}
}

// ----- notifications -----

func TestNotificationInitialized(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/initialized",
	}))
	if len(cap.sent) != 0 {
		t.Errorf("notification should not reply, got %d replies", len(cap.sent))
	}
}

func TestNotificationCancelled(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/cancelled",
		"params": map[string]any{"requestId": 7, "reason": "user abort"},
	}))
	if len(cap.sent) != 0 {
		t.Errorf("notification should not reply, got %d replies", len(cap.sent))
	}
}

func TestNotificationUnknown(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	// Unknown notifications are logged and ignored — no reply.
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "method": "notifications/something_new",
	}))
	if len(cap.sent) != 0 {
		t.Error("expected no reply for unknown notification")
	}
}

// ----- error format -----

func TestErrorResponseFormat(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": 42, "method": "no.such.method",
	}))
	r, _ := cap.Last()
	if r.JSONRPC != "2.0" {
		t.Errorf("jsonrpc: %s", r.JSONRPC)
	}
	if string(r.ID) != "42" {
		t.Errorf("id: %s", r.ID)
	}
	if r.Error == nil {
		t.Fatal("expected error")
	}
	if r.Result != nil {
		t.Errorf("result should be nil on error")
	}
}

func TestStringID(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), mustJSON(t, map[string]any{
		"jsonrpc": "2.0", "id": "abc-123", "method": "ping",
	}))
	r, _ := cap.Last()
	if string(r.ID) != `"abc-123"` {
		t.Errorf("string id: %s", r.ID)
	}
}

// ----- edge cases -----

func TestMalformedJSON(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	// Garbage input must not panic.
	s.Handle(context.Background(), json.RawMessage(`{garbage`))
	if len(cap.sent) != 0 {
		t.Error("malformed JSON should not produce a reply")
	}
}

func TestEmptyPayload(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	s.Handle(context.Background(), json.RawMessage(``))
	if len(cap.sent) != 0 {
		t.Error("empty payload should not produce a reply")
	}
}

func TestSenderErrorDoesNotBlock(t *testing.T) {
	cap := &capturingSender{failN: 3}
	s := NewServer(cap.Send)
	// 3 failed sends + 1 success.
	for i := 0; i < 4; i++ {
		s.Handle(context.Background(), mustJSON(t, map[string]any{
			"jsonrpc": "2.0", "id": i, "method": "ping",
		}))
	}
	if atomic.LoadInt32(&cap.failN) != 0 {
		t.Errorf("failN should be 0, got %d", cap.failN)
	}
	// Exactly the 4th reply should be captured.
	if len(cap.sent) != 1 {
		t.Errorf("expected 1 captured reply, got %d", len(cap.sent))
	}
}

func TestConcurrentDispatch(t *testing.T) {
	cap := &capturingSender{}
	s := NewServer(cap.Send)
	const N = 50
	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s.Handle(context.Background(), mustJSON(t, map[string]any{
				"jsonrpc": "2.0", "id": id, "method": "ping",
			}))
		}(i)
	}
	wg.Wait()
	if got := len(cap.All()); got != N {
		t.Errorf("expected %d replies, got %d", N, got)
	}
}

// ----- result wrapping -----

func TestWrapResultString(t *testing.T) {
	raw, err := wrapResult("hello")
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Content []struct{ Type, Text string } `json:"content"`
	}
	json.Unmarshal(raw, &w)
	if len(w.Content) != 1 || w.Content[0].Type != "text" || w.Content[0].Text != "hello" {
		t.Errorf("string wrap: %+v", w)
	}
}

func TestWrapResultPassThrough(t *testing.T) {
	in := map[string]any{"content": []any{map[string]any{"type": "text", "text": "x"}}}
	raw, err := wrapResult(in)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"content":[{"text":"x","type":"text"}]}` {
		t.Errorf("pass-through: %s", raw)
	}
}

func TestWrapResultGeneric(t *testing.T) {
	raw, err := wrapResult(map[string]any{"x": 1})
	if err != nil {
		t.Fatal(err)
	}
	var w struct {
		Content []struct{ Type, Text string } `json:"content"`
	}
	json.Unmarshal(raw, &w)
	if len(w.Content) != 1 || w.Content[0].Type != "text" {
		t.Errorf("generic wrap: %+v", w)
	}
	// The text should be a valid JSON object.
	var m map[string]int
	if err := json.Unmarshal([]byte(w.Content[0].Text), &m); err != nil {
		t.Errorf("wrapped text not valid JSON: %v", err)
	}
}
