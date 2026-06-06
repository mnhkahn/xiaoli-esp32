package state

import (
	"sync"
	"testing"
)

// TestValidTransitions enumerates every (from, to) pair and asserts
// that the result matches the C++ table.
func TestValidTransitions(t *testing.T) {
	tests := []struct {
		from, to State
		want     bool
	}{
		// Same-state self-loop is always allowed.
		{Idle, Idle, true},

		// Unknown -> Starting only.
		{Unknown, Starting, true},
		{Unknown, Idle, false},

		// Starting -> WifiConfiguring or Activating.
		{Starting, WifiConfiguring, true},
		{Starting, Activating, true},
		{Starting, Idle, false},

		// WifiConfiguring -> Activating or AudioTesting.
		{WifiConfiguring, Activating, true},
		{WifiConfiguring, AudioTesting, true},
		{WifiConfiguring, Idle, false},

		// AudioTesting -> WifiConfiguring.
		{AudioTesting, WifiConfiguring, true},
		{AudioTesting, Idle, false},

		// Activating -> Upgrading, Idle, WifiConfiguring.
		{Activating, Upgrading, true},
		{Activating, Idle, true},
		{Activating, WifiConfiguring, true},
		{Activating, Listening, false},

		// Upgrading -> Idle, Activating.
		{Upgrading, Idle, true},
		{Upgrading, Activating, true},
		{Upgrading, Listening, false},

		// Idle -> many.
		{Idle, Connecting, true},
		{Idle, Listening, true},
		{Idle, Speaking, true},
		{Idle, Activating, true},
		{Idle, Upgrading, true},
		{Idle, WifiConfiguring, true},
		{Idle, FatalError, false},

		// Connecting -> Idle, Listening.
		{Connecting, Idle, true},
		{Connecting, Listening, true},
		{Connecting, Speaking, false},

		// Listening -> Speaking, Idle.
		{Listening, Speaking, true},
		{Listening, Idle, true},
		{Listening, Activating, false},

		// Speaking -> Listening, Idle.
		{Speaking, Listening, true},
		{Speaking, Idle, true},
		{Speaking, Connecting, false},

		// FatalError is terminal.
		{FatalError, Idle, false},
		{FatalError, Listening, false},
	}
	m := New()
	for _, tc := range tests {
		m.current.Store(int32(tc.from))
		got := m.TransitionTo(tc.to)
		if got != tc.want {
			t.Errorf("TransitionTo(%s -> %s) = %v, want %v",
				tc.from, tc.to, got, tc.want)
		}
	}
}

func TestListenerFires(t *testing.T) {
	m := New()
	m.TransitionTo(Starting)
	m.TransitionTo(Activating)

	var (
		mu   sync.Mutex
		seen [][2]State
	)
	id := m.AddListener(func(old, new State) {
		mu.Lock()
		seen = append(seen, [2]State{old, new})
		mu.Unlock()
	})
	defer m.RemoveListener(id)

	m.TransitionTo(Idle)
	m.TransitionTo(Connecting)
	m.TransitionTo(Listening)

	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 3 {
		t.Fatalf("listener fired %d times, want 3", len(seen))
	}
	if seen[0] != [2]State{Activating, Idle} ||
		seen[1] != [2]State{Idle, Connecting} ||
		seen[2] != [2]State{Connecting, Listening} {
		t.Errorf("listener order/content wrong: %+v", seen)
	}
}
