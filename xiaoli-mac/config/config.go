package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// Config is the full device configuration. Mirrors the keys in
// xiaozhi-esp32 Kconfig (selected fields only).
type Config struct {
	// Server is the xiaoli-admin WebSocket URL, e.g.
	//   wss://xiaoli.cyeam.com/ws
	Server string `json:"server"`

	// Token is the activation token, equivalent to
	// CONFIG_ACTIVATION_TOKEN in the ESP32 menuconfig.
	Token string `json:"token,omitempty"`

	// DeviceID is the unique device identifier. On ESP32 this is the
	// MAC; on Mac we use the hardware UUID.
	DeviceID string `json:"device_id,omitempty"`

	// Auth is the SERVER_AUTH_KEY, sent in the "auth" field of hello.
	Auth string `json:"auth,omitempty"`

	// WakeWord selects the wake-word engine. "off" disables it.
	WakeWord WakeWordConfig `json:"wake_word"`

	// Audio configures mic/speaker.
	Audio AudioConfig `json:"audio"`
}

type WakeWordConfig struct {
	// Engine: "off", "porcupine", or "openwakeword".
	Engine string `json:"engine"`
	// Keyword is the wake word keyword name (Porcupine) or label
	// (openWakeWord).
	Keyword string `json:"keyword,omitempty"`
	// AccessKey is the Picovoice access key (Porcupine only).
	AccessKey string `json:"access_key,omitempty"`
}

type AudioConfig struct {
	// InputDevice is the PortAudio device name. Empty = default.
	InputDevice string `json:"input_device,omitempty"`
	// OutputDevice is the PortAudio device name. Empty = default.
	OutputDevice string `json:"output_device,omitempty"`
	// SampleRate is the PCM sample rate. 16000 matches the server.
	SampleRate int `json:"sample_rate"`
	// FrameDurationMs is the OPUS frame duration. 60 matches server.
	FrameDurationMs int `json:"frame_duration_ms"`
}

// Default returns a config populated with sensible defaults.
func Default() *Config {
	return &Config{
		WakeWord: WakeWordConfig{Engine: "off"},
		Audio: AudioConfig{
			SampleRate:     16000,
			FrameDurationMs: 60,
		},
	}
}

// Load reads a config from path, falling back to defaults for any
// missing field. Path is JSON; YAML is intentionally not supported to
// keep the dependency surface small.
func Load(path string) (*Config, error) {
	if path == "" {
		return nil, errors.New("config: empty path")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if cfg.Server == "" {
		return nil, errors.New("config: server url is required")
	}
	return cfg, nil
}
