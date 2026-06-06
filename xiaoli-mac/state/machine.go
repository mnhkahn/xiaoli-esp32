package state

import (
	"log"
	"sync"
	"sync/atomic"
)

// Callback receives old and new state. It runs in the goroutine that
// triggered the transition (typically the main event loop).
type Callback func(old, new State)

// Machine enforces the same transition table as
// xiaozhi-esp32/main/device_state_machine.cc:IsValidTransition.
type Machine struct {
	current atomic.Int32

	mu        sync.Mutex
	listeners []callback
	nextID    int
}

type callback struct {
	id int
	fn Callback
}

func New() *Machine {
	// Mac devices start directly in Idle — there is no WiFi
	// configuration or activation phase, so we collapse the ESP32
	// Unknown → Starting → Activating → Idle sequence. The Unknown
	// state is still part of the State enum (parity with the C++
	// board) but is no longer reachable from a fresh machine.
	m := &Machine{}
	m.current.Store(int32(Idle))
	return m
}

func (m *Machine) Current() State {
	return State(m.current.Load())
}

// TransitionTo validates and (on success) commits the transition, then
// notifies all listeners. Returns false on invalid transition.
func (m *Machine) TransitionTo(target State) bool {
	old := State(m.current.Load())
	if old == target {
		return true
	}
	if !isValidTransition(old, target) {
		log.Printf("[StateMachine] invalid transition: %s -> %s", old, target)
		return false
	}
	m.current.Store(int32(target))
	log.Printf("[StateMachine] state: %s -> %s", old, target)
	m.notify(old, target)
	return true
}

func (m *Machine) CanTransitionTo(target State) bool {
	return isValidTransition(m.Current(), target)
}

func (m *Machine) AddListener(fn Callback) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := m.nextID
	m.nextID++
	m.listeners = append(m.listeners, callback{id: id, fn: fn})
	return id
}

func (m *Machine) RemoveListener(id int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, c := range m.listeners {
		if c.id == id {
			m.listeners = append(m.listeners[:i], m.listeners[i+1:]...)
			return
		}
	}
}

func (m *Machine) notify(old, new State) {
	m.mu.Lock()
	copies := make([]Callback, 0, len(m.listeners))
	for _, c := range m.listeners {
		copies = append(copies, c.fn)
	}
	m.mu.Unlock()
	for _, fn := range copies {
		fn(old, new)
	}
}

// isValidTransition mirrors the C++ switch in device_state_machine.cc:34-102.
func isValidTransition(from, to State) bool {
	switch from {
	case Unknown:
		return to == Starting
	case Starting:
		return to == WifiConfiguring || to == Activating
	case WifiConfiguring:
		return to == Activating || to == AudioTesting
	case AudioTesting:
		return to == WifiConfiguring
	case Activating:
		return to == Upgrading || to == Idle || to == WifiConfiguring
	case Upgrading:
		return to == Idle || to == Activating
	case Idle:
		return to == Connecting || to == Listening || to == Speaking ||
			to == Activating || to == Upgrading || to == WifiConfiguring
	case Connecting:
		return to == Idle || to == Listening
	case Listening:
		return to == Speaking || to == Idle
	case Speaking:
		return to == Listening || to == Idle
	case FatalError:
		return false
	default:
		return false
	}
}
