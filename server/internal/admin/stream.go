package admin

import (
	"sync"
	"time"
)

type StreamEvent struct {
	Type        string  `json:"type"`
	DeviceID    string  `json:"device_id"`
	ContentType string  `json:"content_type"`
	Image       string  `json:"image"`
	Size        int     `json:"size"`
	TS          float64 `json:"ts"`
	StreamID    string  `json:"stream_id"`
	Seq         string  `json:"seq"`
	TimestampMS string  `json:"timestamp_ms"`
}

type streamHub struct {
	mu         sync.Mutex
	latestByID map[string]StreamEvent
	subs       map[string]map[chan StreamEvent]struct{}
}

func newStreamHub() *streamHub {
	return &streamHub{
		latestByID: map[string]StreamEvent{},
		subs:       map[string]map[chan StreamEvent]struct{}{},
	}
}

func (h *streamHub) latest(deviceID string) *StreamEvent {
	h.mu.Lock()
	defer h.mu.Unlock()
	event, ok := h.latestByID[deviceID]
	if !ok {
		return nil
	}
	return &event
}

func (h *streamHub) publish(event StreamEvent) StreamEvent {
	if event.Type == "" {
		event.Type = "frame"
	}
	if event.TS == 0 {
		event.TS = float64(time.Now().UnixNano()) / 1e9
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.latestByID[event.DeviceID] = event
	for ch := range h.subs[event.DeviceID] {
		select {
		case ch <- event:
		default:
			select {
			case <-ch:
			default:
			}
			select {
			case ch <- event:
			default:
			}
		}
	}
	return event
}

func (h *streamHub) subscribe(deviceID string) (chan StreamEvent, func()) {
	ch := make(chan StreamEvent, 1)
	h.mu.Lock()
	if h.subs[deviceID] == nil {
		h.subs[deviceID] = map[chan StreamEvent]struct{}{}
	}
	h.subs[deviceID][ch] = struct{}{}
	if latest, ok := h.latestByID[deviceID]; ok {
		ch <- latest
	}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.subs[deviceID] != nil {
			delete(h.subs[deviceID], ch)
			if len(h.subs[deviceID]) == 0 {
				delete(h.subs, deviceID)
			}
		}
		close(ch)
	}
}
