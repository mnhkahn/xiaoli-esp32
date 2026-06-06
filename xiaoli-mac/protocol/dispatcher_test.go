package protocol

import (
	"encoding/json"
	"testing"

	"xiaoli/mac/display"
	"xiaoli/mac/state"
)

func TestDispatcherTTSStartStop(t *testing.T) {
	d := display.NoDisplay{}
	m := state.New()
	m.TransitionTo(state.Starting)
	m.TransitionTo(state.Activating)
	m.TransitionTo(state.Idle)

	var started, stopped bool
	disp := &Dispatcher{
		Display:    d,
		Machine:    m,
		Lang:       "zh-CN",
		OnTTSStart: func() { started = true },
		OnTTSStop:  func() { stopped = true },
	}

	disp.Handle(json.RawMessage(`{"type":"tts","state":"start"}`))
	if m.Current() != state.Speaking {
		t.Fatalf("want Speaking, got %s", m.Current())
	}
	if !started {
		t.Error("OnTTSStart not called")
	}

	disp.Handle(json.RawMessage(`{"type":"tts","state":"stop"}`))
	if m.Current() != state.Idle {
		t.Fatalf("want Idle, got %s", m.Current())
	}
	if !stopped {
		t.Error("OnTTSStop not called")
	}
}

func TestDispatcherSTT(t *testing.T) {
	disp := &Dispatcher{Display: display.NoDisplay{}, Machine: state.New(), Lang: "zh-CN"}
	disp.Handle(json.RawMessage(`{"type":"stt","text":"你好"}`))
}

func TestDispatcherLLMEmotion(t *testing.T) {
	disp := &Dispatcher{Display: display.NoDisplay{}, Machine: state.New(), Lang: "zh-CN"}
	disp.Handle(json.RawMessage(`{"type":"llm","emotion":"happy"}`))
}

func TestDispatcherAlert(t *testing.T) {
	called := false
	disp := &Dispatcher{
		Display: display.NoDisplay{},
		Machine: state.New(),
		Lang:    "zh-CN",
		OnAlert: func(s, msg, e string) { called = true },
	}
	disp.Handle(json.RawMessage(`{"type":"alert","status":"info","message":"hi","emotion":"happy"}`))
	if !called {
		t.Error("OnAlert not called")
	}
	// Missing fields -> no panic.
	disp.Handle(json.RawMessage(`{"type":"alert","status":"info"}`))
}

func TestDispatcherCustom(t *testing.T) {
	disp := &Dispatcher{Display: display.NoDisplay{}, Machine: state.New(), Lang: "zh-CN"}
	disp.Handle(json.RawMessage(`{"type":"custom","payload":{"k":"v"}}`))
}

func TestDispatcherUnknownType(t *testing.T) {
	disp := &Dispatcher{Display: display.NoDisplay{}, Machine: state.New(), Lang: "zh-CN"}
	disp.Handle(json.RawMessage(`{"type":"garbage"}`))
}

func TestDispatcherSystemReboot(t *testing.T) {
	called := false
	disp := &Dispatcher{
		Display:   display.NoDisplay{},
		Machine:   state.New(),
		Lang:      "zh-CN",
		OnReboot:  func() { called = true },
	}
	disp.Handle(json.RawMessage(`{"type":"system","command":"reboot"}`))
	if !called {
		t.Error("OnReboot not called")
	}
}
