package protocol

import (
	"encoding/json"
	"log"

	"xiaoli/mac/assets"
	"xiaoli/mac/display"
	"xiaoli/mac/state"
)

// Dispatcher mirrors the C++ OnIncomingJson closure registered in
// xiaozhi-esp32/main/application.cc:635-747. All callbacks are
// already running on the main goroutine; the caller (network
// goroutine) is responsible for hopping to the main loop.
type Dispatcher struct {
	Display display.Display
	Machine *state.Machine

	// OnTTSStart / OnTTSStop let the audio pipeline react to the
	// matching server events.
	OnTTSStart func()
	OnTTSStop  func()
	OnSTTText  func(text string)
	OnAlert    func(status, message, emotion string)
	OnReboot   func()
	OnCustom   func(payload json.RawMessage)
	OnMCP      func(payload json.RawMessage)

	// Lang controls the status-bar localization.
	Lang string
}

// Handle parses msg (a single JSON object) and invokes the matching
// handler, just like the C++ switch on "type".
func (d *Dispatcher) Handle(msg json.RawMessage) {
	var env Envelope
	if err := json.Unmarshal(msg, &env); err != nil {
		log.Printf("[protocol] unmarshal: %v", err)
		return
	}
	switch env.Type {
	case "tts":
		d.handleTTS(env)
	case "stt":
		d.handleSTT(env)
	case "llm":
		d.handleLLM(env)
	case "mcp":
		d.handleMCP(env)
	case "system":
		d.handleSystem(env)
	case "alert":
		d.handleAlert(env)
	case "custom":
		d.handleCustom(env)
	default:
		log.Printf("[protocol] unknown message type: %s", env.Type)
	}
}

// handleTTS maps tts.state to SetDeviceState + SetChatMessage, same
// as application.cc:638-676.
func (d *Dispatcher) handleTTS(env Envelope) {
	switch TTSState(env.State) {
	case TTSStart:
		log.Printf("[protocol] tts start")
		d.Machine.TransitionTo(state.Speaking)
		d.Display.SetStatus(d.localize("SPEAKING"))
		if d.OnTTSStart != nil {
			d.OnTTSStart()
		}
	case TTSStop:
		log.Printf("[protocol] tts stop")
		if d.Machine.Current() == state.Speaking {
			// C++: idle if manual-stop mode, else back to listening.
			// We have no listening-mode flag here, so go to idle
			// (the wake-word goroutine will re-enter listening).
			d.Machine.TransitionTo(state.Idle)
			d.Display.SetStatus(d.localize("STANDBY"))
		}
		if d.OnTTSStop != nil {
			d.OnTTSStop()
		}
	case TTSSentenceStart:
		if env.Text == "" {
			return
		}
		log.Printf("[protocol] << %s", env.Text)
		d.Display.SetChatMessage(display.RoleAssistant, env.Text)
	}
}

// handleSTT mirrors application.cc:677-694.
func (d *Dispatcher) handleSTT(env Envelope) {
	if env.Text == "" {
		return
	}
	log.Printf("[protocol] >> %s", env.Text)
	d.Display.SetChatMessage(display.RoleUser, env.Text)
	if d.OnSTTText != nil {
		d.OnSTTText(env.Text)
	}
}

// handleLLM mirrors application.cc:695-704.
func (d *Dispatcher) handleLLM(env Envelope) {
	if env.Emotion == "" {
		return
	}
	d.Display.SetEmotion(env.Emotion)
}

// handleMCP forwards payload to the local MCP server (wired later).
func (d *Dispatcher) handleMCP(env Envelope) {
	if len(env.Payload) == 0 {
		return
	}
	if d.OnMCP != nil {
		d.OnMCP(env.Payload)
	}
}

// handleSystem mirrors application.cc:710-722.
func (d *Dispatcher) handleSystem(env Envelope) {
	switch env.Command {
	case "reboot":
		if d.OnReboot != nil {
			d.OnReboot()
		}
	default:
		log.Printf("[protocol] unknown system command: %s", env.Command)
	}
}

// handleAlert mirrors application.cc:723-731.
func (d *Dispatcher) handleAlert(env Envelope) {
	if env.Status == "" || env.Message == "" || env.Emotion == "" {
		log.Printf("[protocol] alert: missing field(s)")
		return
	}
	d.Display.SetEmotion(env.Emotion)
	d.Display.ShowNotification(env.Message, 3000)
	if d.OnAlert != nil {
		d.OnAlert(env.Status, env.Message, env.Emotion)
	}
}

// handleCustom mirrors application.cc:732-743.
func (d *Dispatcher) handleCustom(env Envelope) {
	if len(env.Payload) == 0 {
		return
	}
	d.Display.SetChatMessage(display.RoleSystem, string(env.Payload))
	if d.OnCustom != nil {
		d.OnCustom(env.Payload)
	}
}

func (d *Dispatcher) localize(key string) string {
	return assets.Locale(d.Lang, key)
}
