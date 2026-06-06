package fynegui

import (
	"sync"
	"time"
)

// timerIDs are 1-based; 0 means "no timer pending".
var (
	timerMu sync.Mutex
	timers  = map[int]*time.Timer{}
	nextID  = 1
)

// scheduleAfter runs fn after ms milliseconds and returns a handle
// that can be passed to cancelTimer. If the handle has already fired
// or is unknown, cancelTimer is a no-op.
func scheduleAfter(ms int, fn func()) int {
	timerMu.Lock()
	id := nextID
	nextID++
	timerMu.Unlock()
	t := time.AfterFunc(time.Duration(ms)*time.Millisecond, func() {
		timerMu.Lock()
		delete(timers, id)
		timerMu.Unlock()
		fn()
	})
	timerMu.Lock()
	timers[id] = t
	timerMu.Unlock()
	return id
}

func cancelTimer(id int) {
	if id == 0 {
		return
	}
	timerMu.Lock()
	t, ok := timers[id]
	if ok {
		delete(timers, id)
	}
	timerMu.Unlock()
	if ok {
		t.Stop()
	}
}
