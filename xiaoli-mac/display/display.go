// Package display mirrors xiaozhi-esp32/main/display/display.h.
//
// The interface matches the C++ Display class methods so the protocol
// dispatcher can call them in the same way.
package display

import "time"

// Role is the chat message role string, used by SetChatMessage.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleSystem    Role = "system"
)

// Theme mirrors the C++ Theme class.
type Theme struct {
	Name string
}

// Display is the abstract display interface. All methods are safe to
// call from the main event loop goroutine only (matching the C++
// contract that display calls are scheduled onto a single task).
type Display interface {
	// SetStatus updates the top status text, e.g. "待命" / "聆听中..." / "说话中..."
	SetStatus(status string)

	// ShowNotification shows a transient toast/notification. durationMs
	// is the timeout; 0 means default (3000ms on the C++ side).
	ShowNotification(text string, durationMs int)

	// SetEmotion changes the face/emoji. Common values:
	// "neutral", "happy", "sad", "laughing", "angry", "crying", "loving",
	// "embarrassed", "surprised", "shocked", "thinking", "winking",
	// "cool", "relaxed", "delicious", "kissy", "confident", "sleepy",
	// "silly", "confused".
	SetEmotion(emotion string)

	// SetChatMessage shows a single message. In the C++ code this is
	// called for the "subtitle" (captions) display style; the bubble
	// style instead uses SetChatMessages.
	SetChatMessage(role Role, content string)

	// SetChatMessages sets the full visible chat list. The C++ bubble
	// mode calls this with a fresh slice on every server update.
	SetChatMessages(messages []ChatMessage)

	// ClearChatMessages removes the chat history from the display.
	ClearChatMessages()

	// SetTheme swaps to a named theme; the C++ version persists the
	// choice via Settings.
	SetTheme(theme *Theme)

	// SetPowerSaveMode dims the display. Mac web display maps this to
	// a CSS class toggle.
	SetPowerSaveMode(on bool)

	// SetOnPressListen registers a callback invoked when the user
	// presses the in-window "按住说话" button. Mac-only: ESP32 has
	// no equivalent in the display (its wake word + boot button are
	// wired through the application task, not the display).
	SetOnPressListen(fn func())

	// SetListenButtonState updates the listen button label and
	// enabled/disabled flag. The label is shown on the button; the
	// disabled flag prevents accidental presses during non-Idle
	// states (matching the C++ board, which has no listen button).
	SetListenButtonState(label string, enabled bool)
}

// ChatMessage is a single chat bubble item.
type ChatMessage struct {
	Role      Role
	Content   string
	Timestamp time.Time
}

// NoDisplay is a null-object implementation. Useful for unit tests and
// for the Windows client, which does not own a display.
type NoDisplay struct{}

func (NoDisplay) SetStatus(string)               {}
func (NoDisplay) ShowNotification(string, int)   {}
func (NoDisplay) SetEmotion(string)              {}
func (NoDisplay) SetChatMessage(Role, string)    {}
func (NoDisplay) SetChatMessages([]ChatMessage)  {}
func (NoDisplay) ClearChatMessages()             {}
func (NoDisplay) SetTheme(*Theme)                {}
func (NoDisplay) SetPowerSaveMode(bool)          {}
func (NoDisplay) SetOnPressListen(func())        {}
func (NoDisplay) SetListenButtonState(string, bool) {}
