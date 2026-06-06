// Package tools provides built-in MCP tool implementations for the
// Mac device. They mirror the device-side tools exposed by the ESP32
// firmware (see xiaozhi-esp32/main/mcp_server.cc).
//
// Tool names are kept identical to the ESP32 set so the server can
// use the same routing table:
//
//   - self.get_device_status
//   - self.audio_speaker.set_volume
//   - self.audio_speaker.play_sound
//   - self.screen.set_theme  (Fyne display only)
//   - self.reboot            (user-only)
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"sync"

	"xiaoli/mac/mcp"
)

// RegisterDeviceTools wires the standard set of device tools into s.
func RegisterDeviceTools(s *mcp.Server) *DeviceState {
	ds := &DeviceState{
		Volume:   70,
		Muted:    false,
		Battery:  100,
		Charging: true,
	}

	s.Register(&mcp.Tool{
		Name: "self.get_device_status",
		Desc: "Return the current state of the device: state, version, battery, volume.",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			ds.mu.Lock()
			defer ds.mu.Unlock()
			return map[string]any{
				"state":    ds.State,
				"version":  ds.Version,
				"battery":  ds.Battery,
				"charging": ds.Charging,
				"volume":   ds.Volume,
				"muted":    ds.Muted,
			}, nil
		},
	})

	s.Register(&mcp.Tool{
		Name: "self.audio_speaker.set_volume",
		Desc: "Set the output volume (0-100).",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"volume": map[string]any{
					"type":    "integer",
					"minimum": 0,
					"maximum": 100,
				},
			},
			"required": []string{"volume"},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var p struct {
				Volume *int `json:"volume"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, err
			}
			if p.Volume == nil {
				return nil, errors.New("volume is required")
			}
			ds.mu.Lock()
			ds.Volume = clamp(*p.Volume, 0, 100)
			v := ds.Volume
			ds.mu.Unlock()
			return map[string]any{"volume": v}, nil
		},
	})

	s.Register(&mcp.Tool{
		Name: "self.audio_speaker.play_sound",
		Desc: "Play a short sound effect on the speaker. The 'sound' argument is a URL or a built-in name (e.g. 'success', 'popup', 'vibration').",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sound": map[string]any{"type": "string"},
			},
			"required": []string{"sound"},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var p struct {
				Sound string `json:"sound"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, err
			}
			// Phase 2 stub: log only. Future phases stream the
			// downloaded audio to the same decoder the TTS uses.
			return map[string]any{"played": p.Sound}, nil
		},
	})

	s.Register(&mcp.Tool{
		Name: "self.screen.set_theme",
		Desc: "Switch the display theme ('light' or 'dark').",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"theme": map[string]any{"type": "string"},
			},
			"required": []string{"theme"},
		},
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			var p struct {
				Theme string `json:"theme"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return nil, err
			}
			return map[string]any{"theme": p.Theme}, nil
		},
	})

	s.Register(&mcp.Tool{
		Name:    "self.reboot",
		Desc:    "Reboot the device. No-op on Mac.",
		Schema:  map[string]any{"type": "object", "properties": map[string]any{}},
		UserOnly: true,
		Handler: func(ctx context.Context, args json.RawMessage) (any, error) {
			return map[string]any{"status": "noop on mac"}, nil
		},
	})

	return ds
}

// DeviceState is the small mutable state bag exposed by
// self.get_device_status. The main app updates State and Battery
// from the main goroutine.
type DeviceState struct {
	mu       sync.Mutex
	State    string
	Version  string
	Battery  int
	Charging bool
	Volume   int
	Muted    bool
}

// SetState updates the device state string.
func (d *DeviceState) SetState(state string) {
	d.mu.Lock()
	d.State = state
	d.mu.Unlock()
}

func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
