// Package app wires every component together and runs the single
// main goroutine. The pattern matches the ESP32 application task:
// every event (network, state, audio) is funneled through one
// goroutine that calls Display methods serially.
package app

import (
	"context"
	"encoding/json"
	"log"
	"sync"

	"xiaoli/mac/assets"
	"xiaoli/mac/audio"
	"xiaoli/mac/config"
	"xiaoli/mac/display"
	"xiaoli/mac/display/fynegui"
	"xiaoli/mac/mcp"
	"xiaoli/mac/mcp/tools"
	"xiaoli/mac/protocol"
	"xiaoli/mac/protocol/client"
	"xiaoli/mac/protocol/transport"
	"xiaoli/mac/state"
	"xiaoli/mac/wakeword"
)

// hardwareUUID is defined in uuid_darwin.go.

// App is the root process object. Lifetime matches the program.
type App struct {
	Cfg     *config.Config
	Display *fynegui.Display
	Machine *state.Machine
	Client  *client.Client

	dispatcher *protocol.Dispatcher
	events     chan func()

	// Audio is the encode/decode pipeline. It is created on demand
	// so a missing PortAudio install does not abort startup.
	audio *audio.Pipeline

	// ttsSource is a channel of OPUS packets currently being
	// decoded. New packets are pushed by the network goroutine on
	// tts.start; the decode loop drains them.
	ttsMu      sync.Mutex
	ttsSource  *audioChan
	ttsPlaying bool

	// capture is the live mic; nil if PortAudio is unavailable.
	capture *audio.Capture

	// MCP server and the device-state bag it exposes.
	mcp      *mcp.Server
	deviceDS *tools.DeviceState
}

// New builds an App from cfg. The Display and Machine are ready to
// use, but the network stack is not connected yet — call Start.
func New(cfg *config.Config) *App {
	if cfg.DeviceID == "" {
		cfg.DeviceID = hardwareUUID()
	}
	a := &App{
		Cfg:     cfg,
		Display: fynegui.New(),
		Machine: state.New(),
		events:  make(chan func(), 64),
	}
	a.dispatcher = &protocol.Dispatcher{
		Display: a.Display,
		Machine: a.Machine,
		Lang:    "zh-CN",
		OnTTSStart: a.onTTSStart,
		OnTTSStop:  a.onTTSStop,
		OnMCP:      a.onMCP,
		OnAlert: func(status, message, emotion string) {
			log.Printf("[alert] %s / %s / %s", status, message, emotion)
		},
	}
	a.mcp = mcp.NewServer(func(payload any) error {
		return a.Client.SendMCP(payload)
	})
	a.deviceDS = tools.RegisterDeviceTools(a.mcp)
	a.Client = client.New(client.Config{
		URL:      cfg.Server,
		DeviceID: cfg.DeviceID,
		AuthKey:  cfg.Auth,
		OnHelloAck: func(env protocol.Envelope) {
			a.Submit(func() {
				log.Printf("[hello] session=%s audio=%v", env.SessionID, env.AudioParams)
				if a.Machine.Current() == state.Connecting {
					a.Machine.TransitionTo(state.Idle)
				}
			})
		},
	})
	a.Display.SetOnPressListen(a.onPressListen)
	return a
}

// Submit funnels a callback onto the main event loop. Mirrors the
// C++ Application::Schedule() helper.
func (a *App) Submit(fn func()) {
	select {
	case a.events <- fn:
	default:
		log.Printf("[app] event queue full, dropping")
	}
}

// Run blocks until ctx is cancelled or the Fyne window is closed,
// whichever happens first.
func (a *App) Run(ctx context.Context) error {
	a.bootstrap()
	a.Display.Show()

	a.startAudio(ctx)

	// Start the WebSocket client. It reconnects automatically.
	go a.runNetwork(ctx)

	// Drain the event queue on a goroutine; exit when the Fyne
	// loop returns (window closed) or ctx is cancelled.
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case fn := <-a.events:
				a.safeRun(fn)
			}
		}
	}()

	// fyneApp.Run() blocks until the window is closed.
	a.Display.Run()
	return ctx.Err()
}

// startAudio opens the mic capture and starts the encode loop. If
// PortAudio is unavailable, the device runs without audio (e.g. for
// development on a CI runner).
func (a *App) startAudio(ctx context.Context) {
	pipe, err := audio.NewPipeline()
	if err != nil {
		log.Printf("[audio] pipeline init failed: %v (continuing without audio)", err)
		return
	}
	a.audio = pipe

	capture, pcmIn, err := audio.OpenCapture(ctx, a.Cfg.Audio.InputDevice)
	if err != nil {
		log.Printf("[audio] capture init failed: %v (continuing without mic)", err)
		// Surface a visible hint — PortAudio errors are almost
		// always TCC permission denials on a freshly built bundle.
		a.Submit(func() {
			a.Display.ShowNotification("麦克风未授权：请到 系统设置 → 隐私与安全性 → 麦克风 给\"小李\"打开权限，然后重启", 8000)
		})
		return
	}
	a.capture = capture

	// The encode loop drains pcmIn and forwards encoded frames to
	// the WebSocket client.
	go pipe.EncodeLoop(ctx, pcmIn, func(opus []byte) {
		if err := a.Client.SendAudio(opus); err != nil {
			log.Printf("[audio] send: %v", err)
		}
	})

	// Wake word detector (if configured). It reads the same mic
	// stream and triggers a state transition to Listening.
	if det := wakeword.Engine(a.Cfg.WakeWord.Engine, a.Cfg.WakeWord.Keyword, a.Cfg.WakeWord.AccessKey); det != nil {
		go func() {
			if err := det.Run(ctx, pcmIn, a.onWakeWord); err != nil {
				log.Printf("[wakeword] detector exited: %v", err)
			}
		}()
	}
}

// onWakeWord is invoked by the wake word detector. It submits a
// state transition to the main loop so the rest of the app sees a
// consistent view.
func (a *App) onWakeWord() {
	a.Submit(func() {
		if a.Machine.Current() == state.Idle {
			log.Printf("[wakeword] -> Listening")
			a.Machine.TransitionTo(state.Listening)
		}
	})
}

// onPressListen is invoked from the Fyne "按住说话" button. Mirrors
// the wake word path: only acts in Idle (the state machine refuses
// illegal transitions anyway, but checking here keeps the log clean).
func (a *App) onPressListen() {
	a.Submit(func() {
		if a.Machine.Current() == state.Idle {
			log.Printf("[ui] press -> Listening")
			a.Machine.TransitionTo(state.Listening)
		}
	})
}

// onTTSStart begins a new playback session: open the speaker stream
// and start a decode loop fed by the ttsSource channel.
func (a *App) onTTSStart() {
	if a.audio == nil {
		return
	}
	a.ttsMu.Lock()
	if a.ttsPlaying {
		// Already playing: stop the previous session first.
		a.ttsMu.Unlock()
		a.onTTSStop()
		a.ttsMu.Lock()
	}
	src := newAudioChan()
	a.ttsSource = src
	a.ttsPlaying = true
	a.ttsMu.Unlock()

	// Open a fresh speaker stream per TTS turn.
	playback, pcmOut, err := audio.OpenPlayback(context.Background(), a.Cfg.Audio.OutputDevice)
	if err != nil {
		log.Printf("[audio] playback open failed: %v", err)
		return
	}

	go a.audio.DecodeLoop(context.Background(), src, pcmOut)
	go func() {
		for frame := range pcmOut {
			playback.Write(frame)
		}
	}()
}

// onTTSStop closes the current playback source, which terminates
// the decode loop. The speaker device is closed when the program
// exits.
func (a *App) onTTSStop() {
	a.ttsMu.Lock()
	src := a.ttsSource
	a.ttsSource = nil
	a.ttsPlaying = false
	a.ttsMu.Unlock()
	if src != nil {
		src.Close()
	}
}

// FeedTTSAudio is invoked by the network layer on every binary
// frame received between tts.start and tts.stop.
func (a *App) FeedTTSAudio(opus []byte) {
	a.ttsMu.Lock()
	src := a.ttsSource
	a.ttsMu.Unlock()
	if src == nil {
		return
	}
	src.Push(opus)
}

// onMCP hands an inbound MCP payload to the local MCP server. The
// server builds the response and pushes it back via the Sender
// closure we registered in NewServer.
func (a *App) onMCP(payload json.RawMessage) {
	a.mcp.Handle(context.Background(), payload)
}

// runNetwork connects to the server, performs the hello handshake,
// and dispatches inbound frames onto the main event loop.
func (a *App) runNetwork(ctx context.Context) {
	// First: drive the state machine to "connecting" (Idle -> Connecting
	// is always valid; the initial state is Idle, see state.New).
	a.Submit(func() {
		a.Machine.TransitionTo(state.Connecting)
	})

	err := a.Client.Connect(ctx, a.handleFrame)
	if err != nil && err != context.Canceled {
		log.Printf("[network] exited: %v", err)
	}
}

// handleFrame is invoked on the transport's read goroutine. We hop
// to the main loop before doing anything state-mutating.
func (a *App) handleFrame(f transport.Frame) {
	a.Submit(func() {
		if f.Binary {
			// Audio frame from server. Forwarded to the active
			// TTS decode loop, if any.
			a.FeedTTSAudio(f.Data)
			return
		}
		a.dispatcher.Handle(f.Data)
	})
}

// silence unused-import warnings for tools when audio is disabled.
var _ = tools.RegisterDeviceTools

func (a *App) safeRun(fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[app] panic in event: %v", r)
		}
	}()
	fn()
}

// bootstrap sets up state machine listeners and the initial UI.
func (a *App) bootstrap() {
	a.Machine.AddListener(func(old, new state.State) {
		log.Printf("[state] %s -> %s", old, new)
		switch new {
		case state.Idle:
			a.Display.SetStatus(assets.Locale(a.dispatcher.Lang, "STANDBY"))
			a.Display.SetEmotion("neutral")
			a.Display.SetListenButtonState("按住说话", true)
			a.setMicEnabled(false)
		case state.Listening:
			a.Display.SetStatus(assets.Locale(a.dispatcher.Lang, "LISTENING"))
			a.Display.SetListenButtonState("聆听中…", false)
			a.setMicEnabled(true)
		case state.Connecting:
			a.Display.SetStatus(assets.Locale(a.dispatcher.Lang, "CONNECTING"))
			a.Display.SetListenButtonState("按住说话", false)
			a.setMicEnabled(false)
		case state.Upgrading:
			a.Display.SetStatus(assets.Locale(a.dispatcher.Lang, "UPGRADING"))
			a.Display.SetListenButtonState("按住说话", false)
		case state.Activating:
			a.Display.SetStatus(assets.Locale(a.dispatcher.Lang, "ACTIVATING"))
			a.Display.SetListenButtonState("按住说话", false)
		}
	})
	a.Display.SetStatus(assets.Locale(a.dispatcher.Lang, "STANDBY"))
	a.Display.SetListenButtonState("按住说话", true)
}

// setMicEnabled toggles the audio encode gate and tells the server
// to start/stop voice recognition. Safe to call from the main
// goroutine only.
func (a *App) setMicEnabled(on bool) {
	if a.audio != nil {
		a.audio.SetListening(on)
	}
	if a.Client == nil {
		return
	}
	if on {
		if err := a.Client.SendListenStart("auto"); err != nil {
			log.Printf("[audio] listen start: %v", err)
		}
	} else {
		if err := a.Client.SendListenStop(); err != nil {
			log.Printf("[audio] listen stop: %v", err)
		}
	}
}

// GetDisplay returns the Display, satisfying the use case where a
// subsystem only needs the display interface (e.g. tests).
func (a *App) GetDisplay() display.Display { return a.Display }

// GetDispatcher exposes the protocol dispatcher for the network layer.
func (a *App) GetDispatcher() *protocol.Dispatcher { return a.dispatcher }

// hardwareUUID is defined in uuid_darwin.go.
