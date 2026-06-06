package app

import (
	"context"
	"errors"
)

// audioChan is a thread-safe FIFO of OPUS packets used as the
// FrameSource for the playback decode loop. Push is non-blocking;
// Next blocks until a packet is available or Close is called.
type audioChan struct {
	ch chan []byte
}

func newAudioChan() *audioChan {
	return &audioChan{ch: make(chan []byte, 256)}
}

func (a *audioChan) Push(pkt []byte) {
	if a == nil || a.ch == nil {
		return
	}
	select {
	case a.ch <- pkt:
	default:
		// Drop the packet if the decode loop is far behind.
	}
}

func (a *audioChan) Next(ctx context.Context) ([]byte, error) {
	if a == nil || a.ch == nil {
		return nil, errors.New("audioChan closed")
	}
	select {
	case pkt, ok := <-a.ch:
		if !ok {
			return nil, errors.New("audioChan closed")
		}
		return pkt, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (a *audioChan) Close() {
	if a == nil || a.ch == nil {
		return
	}
	close(a.ch)
}
