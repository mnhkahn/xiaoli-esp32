package audio

import (
	"context"
	"errors"
	"log"
	"sync"

	"github.com/hraban/opus"
)

// FrameSink is the callback invoked for each mic OPUS packet. The
// packet is one 60ms frame at 16kHz mono.
type FrameSink func(opus []byte)

// FrameSource supplies encoded OPUS packets for the speaker. It
// returns io.EOF or ctx.Err() to terminate the decoder loop.
type FrameSource interface {
	// Next blocks until the next packet is available, or returns
	// an error to terminate the loop.
	Next(ctx context.Context) ([]byte, error)
}

// Pipeline owns the encode and decode loops. The capture/playback
// devices are managed separately so the same pipeline can be reused
// across reconnects.
type Pipeline struct {
	enc *opus.Encoder
	dec *opus.Decoder

	mu      sync.Mutex
	listen  bool
	decBuf  []int16
	encBuf  []byte
}

// NewPipeline returns a pipeline with Opus encoder and decoder ready.
func NewPipeline() (*Pipeline, error) {
	enc, err := NewEncoder()
	if err != nil {
		return nil, err
	}
	dec, err := NewDecoder()
	if err != nil {
		return nil, err
	}
	return &Pipeline{
		enc:    enc,
		dec:    dec,
		decBuf: make([]int16, 5760), // 120ms max
		encBuf: make([]byte, 4000),
	}, nil
}

// SetListening toggles the encode path. While true, pcmIn frames
// are encoded and forwarded to sink. While false, frames are
// discarded.
func (p *Pipeline) SetListening(on bool) {
	p.mu.Lock()
	p.listen = on
	p.mu.Unlock()
}

// IsListening reports the current encode gate.
func (p *Pipeline) IsListening() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.listen
}

// EncodeLoop reads PCM frames, encodes them when listening is on,
// and forwards them to sink. The loop returns when ctx is done or
// pcmIn is closed.
func (p *Pipeline) EncodeLoop(ctx context.Context, pcmIn <-chan []int16, sink FrameSink) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame, ok := <-pcmIn:
			if !ok {
				return
			}
			if !p.IsListening() {
				continue
			}
			pkt, err := p.enc.Encode(frame, p.encBuf)
			if err != nil {
				log.Printf("[audio] encode: %v", err)
				continue
			}
			// Copy out of the shared buffer before forwarding.
			out := make([]byte, pkt)
			copy(out, p.encBuf[:pkt])
			sink(out)
		}
	}
}

// DecodeLoop reads OPUS packets from src, decodes them, and writes
// the resulting PCM frames to pcmOut. The loop returns when src
// signals EOF, or when ctx is done. On exit it closes pcmOut so
// downstream consumers (e.g. the playback forwarder) can drain
// remaining frames and observe EOF via a `for range` loop.
//
// The drain-on-close semantics mirror the ESP32 reference
// (xiaozhi-esp32/main/audio/audio_service.cc): tts.stop flips state
// but does not clear audio_decode_queue_/audio_playback_queue_.
// Closing pcmOut here is the "no more packets" signal — the
// forwarder keeps writing buffered frames to PortAudio and then
// calls Playback.Drain() to let the speaker play out the rest.
func (p *Pipeline) DecodeLoop(ctx context.Context, src FrameSource, pcmOut chan<- []int16) {
	defer close(pcmOut)
	for {
		pkt, err := src.Next(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[audio] decode loop exit: %v", err)
			}
			return
		}
		n, err := p.dec.Decode(pkt, p.decBuf)
		if err != nil {
			log.Printf("[audio] decode: %v", err)
			continue
		}
		frame := make([]int16, n)
		copy(frame, p.decBuf[:n])
		select {
		case pcmOut <- frame:
		case <-ctx.Done():
			return
		}
	}
}
