// Package protocol mirrors the message types handled by
// xiaozhi-esp32/main/application.cc:OnIncomingJson (line 635-747).
//
// Only the inbound message shapes are modelled here. Outbound messages
// (hello, audio frames, etc.) are defined alongside the WebSocket
// transport.
package protocol

import (
	"encoding/json"

	"xiaoli/mac/display"
)

// TTSState is the value of the "state" field in a "tts" message.
type TTSState string

const (
	TTSStart         TTSState = "start"
	TTSStop          TTSState = "stop"
	TTSSentenceStart TTSState = "sentence_start"
)

// TTSMessage is {"type":"tts", "state":"...", "text":"..."}.
type TTSMessage struct {
	State TTSState `json:"state"`
	Text  string   `json:"text,omitempty"`
}

// STTMessage is {"type":"stt", "text":"..."}.
type STTMessage struct {
	Text string `json:"text"`
}

// LLMMessage is {"type":"llm", "emotion":"..."}.
type LLMMessage struct {
	Emotion string `json:"emotion"`
}

// MCPMessage is {"type":"mcp", "payload":{...}}.
type MCPMessage struct {
	Payload json.RawMessage `json:"payload"`
}

// SystemMessage is {"type":"system", "command":"reboot"}.
type SystemMessage struct {
	Command string `json:"command"`
}

// AlertMessage is {"type":"alert", "status":"...", "message":"...",
// "emotion":"..."}. Mirrors application.cc:723-731.
type AlertMessage struct {
	Status  string `json:"status"`
	Message string `json:"message"`
	Emotion string `json:"emotion"`
}

// CustomMessage is {"type":"custom", "payload":{...}}. Matches the
// CONFIG_RECEIVE_CUSTOM_MESSAGE branch in application.cc:732-743.
type CustomMessage struct {
	Payload json.RawMessage `json:"payload"`
}

// Envelope is the generic wrapper. We use this to dispatch by "type"
// without losing the raw body.
type Envelope struct {
	Type        string          `json:"type"`
	State       string          `json:"state,omitempty"`
	Text        string          `json:"text,omitempty"`
	Emotion     string          `json:"emotion,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Command     string          `json:"command,omitempty"`
	Status      string          `json:"status,omitempty"`
	Message     string          `json:"message,omitempty"`
	SessionID   string          `json:"session_id,omitempty"`
	Transport   string          `json:"transport,omitempty"`
	Version     int             `json:"version,omitempty"`
	AudioParams *AudioParams    `json:"audio_params,omitempty"`
}

// AudioParams mirrors the server's audio_params block in the hello.
type AudioParams struct {
	Format        string `json:"format"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	FrameDuration int    `json:"frame_duration"`
}

// Re-export for convenience.
type ChatMessage = display.ChatMessage
type Role = display.Role

const (
	RoleUser      = display.RoleUser
	RoleAssistant = display.RoleAssistant
	RoleSystem    = display.RoleSystem
)
