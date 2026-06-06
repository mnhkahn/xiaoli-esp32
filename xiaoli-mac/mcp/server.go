// Package mcp implements the device side of the MCP (Model Context
// Protocol) JSON-RPC 2.0 channel that the server uses to invoke
// tools on the device.
//
// Wire format (from server direct_device.go:602):
//
//	{"type":"mcp","payload":{"jsonrpc":"2.0","id":N,"method":"tools/list","params":{...}}}
//	{"type":"mcp","payload":{"jsonrpc":"2.0","id":N,"method":"tools/call","params":{"name":"X","arguments":{...}}}}
//
// We respond with the standard JSON-RPC envelope. Only tool-call
// results (tools/call and direct method invocations) are wrapped in
// MCP content blocks:
//
//	{"result":{"content":[{"type":"text","text":"..."}]}}
//
// initialize, tools/list, and ping return the structured value
// directly so the server can read fields like payload.tools without
// unwrapping (see server direct_device.go:585 for tools/list and
// direct_device.go:560 for initialize).
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
)

// Sender pushes an MCP response back to the server.
type Sender func(payload any) error

// Tool is the unit of work the server can request. Name is the
// unique tool identifier (e.g. "self.get_device_status"). The
// handler returns a result value (map, string, number, etc.) or
// an error. Tool-call results are wrapped in a text content block
// before being sent; structured methods (initialize, tools/list)
// return the value as-is.
type Tool struct {
	Name    string
	Desc    string
	Schema  map[string]any // JSON Schema for arguments
	Handler func(ctx context.Context, args json.RawMessage) (any, error)

	// UserOnly mirrors ESP32's AddUserOnlyTool flag. The server may
	// pass params.withUserTools=true to filter accordingly. We keep
	// the flag for parity even though we do not enforce auth yet.
	UserOnly bool
}

// Server holds registered tools and dispatches incoming requests.
type Server struct {
	mu    sync.RWMutex
	tools map[string]*Tool
	send  Sender

	// pageSize caps the number of tools returned per tools/list
	// call. The server loops up to 8 times to collect all pages
	// (see direct_device.go:572). Zero means no pagination.
	pageSize int

	deviceVersion string
	deviceState   string
}

// NewServer creates a server that will use send to reply. send is
// expected to wrap the payload in the {"type":"mcp", "payload":...}
// envelope.
func NewServer(send Sender) *Server {
	return &Server{
		tools:         map[string]*Tool{},
		send:          send,
		pageSize:      50,
		deviceVersion: "xiaoli-mac/0.1.0",
		deviceState:   "unknown",
	}
}

// SetPageSize overrides the per-page tool count. Pass 0 to disable
// pagination (return everything in one call).
func (s *Server) SetPageSize(n int) { s.pageSize = n }

// Register adds a tool.
func (s *Server) Register(t *Tool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tools[t.Name] = t
}

// SetDeviceState updates the state string returned by
// self.get_device_status. The state machine calls this.
func (s *Server) SetDeviceState(state string) {
	s.mu.Lock()
	s.deviceState = state
	s.mu.Unlock()
}

// Handle is called for every MCP payload received from the server.
// It dispatches the request, builds the response, and sends it.
func (s *Server) Handle(ctx context.Context, payload json.RawMessage) {
	var req Request
	if err := json.Unmarshal(payload, &req); err != nil {
		log.Printf("[mcp] unmarshal: %v", err)
		return
	}

	// Notifications (no "id") get no reply.
	if req.ID == nil {
		s.handleNotification(ctx, &req)
		return
	}

	resp := Response{
		JSONRPC: "2.0",
		ID:      *req.ID,
	}
	result, err := s.dispatch(ctx, &req)
	if err != nil {
		resp.Error = &ErrorObj{
			Code:    errorCode(err),
			Message: err.Error(),
		}
	} else {
		raw, mErr := encodeResult(req.Method, result)
		if mErr != nil {
			resp.Error = &ErrorObj{Code: -32000, Message: mErr.Error()}
		} else {
			resp.Result = raw
		}
	}

	if s.send == nil {
		return
	}
	if err := s.send(resp); err != nil {
		log.Printf("[mcp] send reply: %v", err)
	}
}

func (s *Server) handleNotification(_ context.Context, req *Request) {
	switch req.Method {
	case "notifications/initialized":
		log.Printf("[mcp] client initialized")
	case "notifications/cancelled":
		log.Printf("[mcp] cancelled: %s", string(req.Params))
	case "notifications/progress":
		// No-op; clients send these for long-running ops.
	default:
		log.Printf("[mcp] unhandled notification: %s", req.Method)
	}
}

func (s *Server) dispatch(ctx context.Context, req *Request) (any, error) {
	switch req.Method {
	case "initialize":
		return s.handleInitialize(req.Params)
	case "tools/list":
		return s.handleToolsList(req.Params)
	case "ping":
		return map[string]any{}, nil
	case "tools/call":
		return s.handleToolsCall(ctx, req.Params)
	}
	// Look up a registered tool by method name (the ESP32 firmware
	// supports both `tools/call` and direct method invocation).
	s.mu.RLock()
	t, ok := s.tools[req.Method]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("method not found: %s", req.Method)
	}
	return t.Handler(ctx, req.Params)
}

func (s *Server) handleInitialize(params json.RawMessage) (any, error) {
	var p struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      map[string]any `json:"clientInfo"`
	}
	_ = json.Unmarshal(params, &p)
	return map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    "xiaoli-mac",
			"version": s.deviceVersion,
		},
	}, nil
}

func (s *Server) handleToolsList(params json.RawMessage) (any, error) {
	var p struct {
		WithUserTools bool   `json:"withUserTools"`
		Cursor        string `json:"cursor"`
	}
	_ = json.Unmarshal(params, &p)

	s.mu.RLock()
	all := make([]*Tool, 0, len(s.tools))
	for _, t := range s.tools {
		if !p.WithUserTools && t.UserOnly {
			continue
		}
		all = append(all, t)
	}
	s.mu.RUnlock()

	// Sort for stable pagination.
	sortTools(all)

	pageSize := s.pageSize
	if pageSize <= 0 || pageSize > len(all) {
		pageSize = len(all)
	}
	start := 0
	if p.Cursor != "" {
		fmt.Sscanf(p.Cursor, "%d", &start)
		if start < 0 || start > len(all) {
			start = 0
		}
	}
	end := start + pageSize
	if end > len(all) {
		end = len(all)
	}
	page := all[start:end]

	out := make([]map[string]any, 0, len(page))
	for _, t := range page {
		out = append(out, map[string]any{
			"name":        t.Name,
			"description": t.Desc,
			"inputSchema": t.Schema,
		})
	}
	resp := map[string]any{"tools": out}
	if end < len(all) {
		resp["nextCursor"] = fmt.Sprintf("%d", end)
	}
	return resp, nil
}

func (s *Server) handleToolsCall(ctx context.Context, params json.RawMessage) (any, error) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return nil, fmt.Errorf("tools/call: invalid params: %w", err)
	}
	if p.Name == "" {
		return nil, errors.New("tools/call: missing name")
	}
	s.mu.RLock()
	t, ok := s.tools[p.Name]
	s.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("tools/call: unknown tool: %s", p.Name)
	}
	return t.Handler(ctx, p.Arguments)
}

func sortTools(t []*Tool) {
	// Stable insertion sort; tool lists are small.
	for i := 1; i < len(t); i++ {
		for j := i; j > 0 && t[j-1].Name > t[j].Name; j-- {
			t[j-1], t[j] = t[j], t[j-1]
		}
	}
}

// encodeResult serialises a dispatch result. Tool-call results
// (tools/call and direct method invocations like
// self.audio_speaker.set_volume) are wrapped in MCP content blocks
// so the server's extractMCPText (direct_device.go:885) can parse
// them. Structured methods (initialize, tools/list, ping) return
// the value as-is so the server can read fields like payload.tools
// without an extra unwrap step.
func encodeResult(method string, v any) (json.RawMessage, error) {
	switch method {
	case "initialize", "tools/list", "ping":
		return json.Marshal(v)
	}
	return wrapResult(v)
}

// wrapResult wraps a tool's return value in the standard MCP
// {"content":[{"type":"text","text":"..."}]} envelope. If the
// value is already a map with a "content" key, it is returned
// unchanged.
func wrapResult(v any) (json.RawMessage, error) {
	// String shortcut.
	if s, ok := v.(string); ok {
		return json.Marshal(map[string]any{
			"content": []map[string]any{{"type": "text", "text": s}},
		})
	}
	// If the tool already returned a content map, pass through.
	if m, ok := v.(map[string]any); ok {
		if _, has := m["content"]; has {
			return json.Marshal(m)
		}
	}
	// Otherwise, JSON-encode v and wrap it as text.
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{
		"content": []map[string]any{{"type": "text", "text": string(b)}},
	})
}

// errorCode maps a Go error to a JSON-RPC 2.0 error code. Special
// values of -32601 (method not found) and -32602 (invalid params)
// are detected from the error message to mirror the MCP convention.
func errorCode(err error) int {
	if err == nil {
		return 0
	}
	msg := err.Error()
	switch {
	case contains(msg, "method not found"), contains(msg, "unknown tool"):
		return -32601
	case contains(msg, "invalid params"), contains(msg, "missing"):
		return -32602
	default:
		return -32000 // server error
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ----- JSON-RPC 2.0 types -----

// Request is a single JSON-RPC 2.0 call. ID is nil for notifications.
type Request struct {
	JSONRPC string           `json:"jsonrpc"`
	ID      *json.RawMessage `json:"id,omitempty"`
	Method  string           `json:"method"`
	Params  json.RawMessage  `json:"params,omitempty"`
}

// Response is a single JSON-RPC 2.0 reply. Only one of Result or
// Error is set.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *ErrorObj       `json:"error,omitempty"`
}

// ErrorObj is a JSON-RPC 2.0 error.
type ErrorObj struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}
