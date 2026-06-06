package tools

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"xiaoli/mac/mcp"
)

type capturingSender struct {
	mu   sync.Mutex
	sent []any
}

func (c *capturingSender) Send(payload any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, payload)
	return nil
}

func (c *capturingSender) Last() (mcp.Response, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.sent) == 0 {
		return mcp.Response{}, false
	}
	r, _ := c.sent[len(c.sent)-1].(mcp.Response)
	return r, true
}

func mustJ(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// findTool returns the tool with the given name from a tools/list
// response, or nil if absent.
func findTool(listResp []byte, name string) (map[string]any, bool) {
	var resp struct {
		Tools []map[string]any `json:"tools"`
	}
	if err := json.Unmarshal(listResp, &resp); err != nil {
		return nil, false
	}
	for _, t := range resp.Tools {
		if t["name"] == name {
			return t, true
		}
	}
	return nil, false
}

func TestDeviceToolsRegistered(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	RegisterDeviceTools(s)

	// Default listing excludes user-only tools.
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}))
	r, _ := cap.Last()
	for _, name := range []string{
		"self.get_device_status",
		"self.audio_speaker.set_volume",
		"self.audio_speaker.play_sound",
		"self.screen.set_theme",
	} {
		if _, ok := findTool(r.Result, name); !ok {
			t.Errorf("tool %q not registered", name)
		}
	}

	// User-only tools appear with withUserTools=true.
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
		"params": map[string]any{"withUserTools": true},
	}))
	r, _ = cap.Last()
	if _, ok := findTool(r.Result, "self.reboot"); !ok {
		t.Error("self.reboot should be listed with withUserTools=true")
	}
}

func TestGetDeviceStatus(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	ds := RegisterDeviceTools(s)
	ds.SetState("idle")
	ds.mu.Lock()
	ds.Volume = 42
	ds.mu.Unlock()

	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "self.get_device_status",
	}))
	r, _ := cap.Last()
	if r.Error != nil {
		t.Fatalf("status: %+v", r.Error)
	}

	// Response is wrapped in {content:[{type:text, text:...}]}.
	var wrapper struct {
		Content []struct {
			Type, Text string
		} `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &wrapper); err != nil {
		t.Fatal(err)
	}
	if len(wrapper.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(wrapper.Content))
	}
	var status map[string]any
	if err := json.Unmarshal([]byte(wrapper.Content[0].Text), &status); err != nil {
		t.Fatal(err)
	}
	if status["state"] != "idle" {
		t.Errorf("state: %v", status["state"])
	}
	if status["volume"].(float64) != 42 {
		t.Errorf("volume: %v", status["volume"])
	}
}

func TestSetVolume(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"normal", 50, 50},
		{"over", 999, 100},
		{"under", -5, 0},
		{"zero", 0, 0},
		{"hundred", 100, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cap := &capturingSender{}
			s := mcp.NewServer(cap.Send)
			ds := RegisterDeviceTools(s)

			s.Handle(context.Background(), mustJ(t, map[string]any{
				"jsonrpc": "2.0", "id": 1, "method": "self.audio_speaker.set_volume",
				"params": map[string]any{"volume": tc.in},
			}))
			r, _ := cap.Last()
			if r.Error != nil {
				t.Fatalf("set_volume: %+v", r.Error)
			}
			ds.mu.Lock()
			got := ds.Volume
			ds.mu.Unlock()
			if got != tc.want {
				t.Errorf("volume = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestSetVolumeMissingArg(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	RegisterDeviceTools(s)
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "self.audio_speaker.set_volume",
		"params": map[string]any{},
	}))
	r, _ := cap.Last()
	// Missing required arg -> handler returns an unmarshal error
	// (Volume stays 0, the default). This is acceptable; the
	// contract is that the volume is left unchanged.
	if r.Error == nil {
		t.Error("expected error for missing volume arg")
	}
}

func TestPlaySound(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	RegisterDeviceTools(s)
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "self.audio_speaker.play_sound",
		"params": map[string]any{"sound": "success"},
	}))
	r, _ := cap.Last()
	if r.Error != nil {
		t.Fatalf("play_sound: %+v", r.Error)
	}
}

func TestScreenSetTheme(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	RegisterDeviceTools(s)
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "self.screen.set_theme",
		"params": map[string]any{"theme": "dark"},
	}))
	r, _ := cap.Last()
	if r.Error != nil {
		t.Fatalf("set_theme: %+v", r.Error)
	}
}

func TestRebootNoop(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	RegisterDeviceTools(s)
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "self.reboot",
	}))
	r, _ := cap.Last()
	if r.Error != nil {
		t.Fatalf("reboot: %+v", r.Error)
	}
}

func TestToolsCallWrapsResult(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	RegisterDeviceTools(s)
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{
			"name":      "self.audio_speaker.set_volume",
			"arguments": map[string]any{"volume": 80},
		},
	}))
	r, _ := cap.Last()
	if r.Error != nil {
		t.Fatalf("tools/call: %+v", r.Error)
	}
	// Result should be wrapped in MCP content block.
	var wrapper struct {
		Content []struct{ Type, Text string } `json:"content"`
	}
	if err := json.Unmarshal(r.Result, &wrapper); err != nil {
		t.Fatal(err)
	}
	if len(wrapper.Content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(wrapper.Content))
	}
}

func TestUserOnlyToolFiltered(t *testing.T) {
	cap := &capturingSender{}
	s := mcp.NewServer(cap.Send)
	RegisterDeviceTools(s)

	// Without withUserTools, self.reboot should not appear.
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/list",
	}))
	r, _ := cap.Last()
	if _, ok := findTool(r.Result, "self.reboot"); ok {
		t.Error("self.reboot should be filtered out without withUserTools")
	}

	// With withUserTools=true, it should appear.
	s.Handle(context.Background(), mustJ(t, map[string]any{
		"jsonrpc": "2.0", "id": 2, "method": "tools/list",
		"params": map[string]any{"withUserTools": true},
	}))
	r, _ = cap.Last()
	if _, ok := findTool(r.Result, "self.reboot"); !ok {
		t.Error("self.reboot should appear with withUserTools=true")
	}
}
