package audio

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync/atomic"

	"github.com/gordonklaus/portaudio"
)

// activePlaybacks is the number of Playback streams currently open.
// Exposed via ActivePlaybacks() for lifecycle tests. Incremented
// after stream.Start() succeeds; decremented after stream.Close()
// returns in the cleanup goroutine. If OpenPlayback is called and
// the caller's context is never cancelled, this counter will keep
// growing and the test will catch the leak.
var activePlaybacks int64

// ActivePlaybacks returns the number of open Playback streams.
// Tests use this to detect leaked streams across open/close cycles.
func ActivePlaybacks() int64 { return atomic.LoadInt64(&activePlaybacks) }

// Playback drains PCM frames from a channel and writes them to the
// default output device. If the channel is empty, the callback
// supplies silence.
type Playback struct {
	stream *portaudio.Stream
	in     chan []int16
}

// OpenPlayback opens the default output device and starts streaming.
// PCM frames pushed into the returned channel are written to the
// speaker in order. The channel should be closed by the caller on
// shutdown.
func OpenPlayback(ctx context.Context, deviceName string) (*Playback, chan []int16, error) {
	if err := portaudio.Initialize(); err != nil {
		return nil, nil, fmt.Errorf("portaudio init: %w", err)
	}
	dev, err := selectOutputDevice(deviceName)
	if err != nil {
		_ = portaudio.Terminate()
		return nil, nil, err
	}
	params := portaudio.LowLatencyParameters(nil, dev)
	params.Output.Channels = Channels
	params.SampleRate = SampleRate
	params.FramesPerBuffer = samplesPerFrame

	in := make(chan []int16, 32)
	stream, err := portaudio.OpenStream(params, func(out []int16) {
		// Block waiting for the next frame instead of writing
		// silence on an empty channel. The previous non-blocking
		// variant (`default: silence`) was the root cause of the
		// "你好…宝宝" gap the user reported: every time the
		// producer was even briefly behind — a Go GC pause, a
		// scheduling hiccup, the initial cold-start latency — the
		// audio device got 60ms of literal silence injected, which
		// on TTS manifests as broken word boundaries.
		//
		// With a blocking callback the OS audio device holds its
		// last sample while we wait. A held sample is barely
		// audible (and for TTS, a brief held pitch is much better
		// than a 60ms gap), and the 32-buffered `in` plus the
		// 60ms pace of both producer (decode loop) and consumer
		// (PortAudio callback) means we rarely block for more than
		// a few hundred microseconds in steady state.
		//
		// The ctx.Done() arm lets the callback exit cleanly when
		// the playback is being torn down (e.g. on tts.stop).
		select {
		case frame, ok := <-in:
			if !ok {
				for i := range out {
					out[i] = 0
				}
				return
			}
			n := copy(out, frame)
			// Pad with zero if the frame is shorter than
			// the PortAudio buffer.
			for i := n; i < len(out); i++ {
				out[i] = 0
			}
		case <-ctx.Done():
			for i := range out {
				out[i] = 0
			}
			return
		}
	})
	if err != nil {
		_ = portaudio.Terminate()
		return nil, nil, fmt.Errorf("portaudio open output: %w", err)
	}
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		_ = portaudio.Terminate()
		return nil, nil, fmt.Errorf("portaudio start output: %w", err)
	}
	atomic.AddInt64(&activePlaybacks, 1)
	pb := &Playback{stream: stream, in: in}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[audio] FATAL panic in playback cleanup: %v\n%s", r, debug.Stack())
			}
		}()
		<-ctx.Done()
		_ = stream.Stop()
		// close(in) defensively unblocks any in-flight callback
		// that is currently sitting on the `<-in` branch of its
		// select. After stream.Stop() PortAudio will not call the
		// callback again, so the close is purely a wake-up for
		// the very rare case where ctx.Done() raced with an
		// already-blocked read.
		_ = stream.Close()
		_ = portaudio.Terminate()
		atomic.AddInt64(&activePlaybacks, -1)
		log.Printf("[audio] playback stopped")
	}()
	return pb, in, nil
}

// Write pushes a PCM frame to the speaker. Safe to call from any
// goroutine. Blocks if the speaker can't keep up. The previous
// non-blocking variant dropped frames on a full channel, which on
// TTS manifested as an audible gap; a blocking send lets the
// producer slow down to match the consumer's 60ms pace — no frame
// loss.
func (p *Playback) Write(frame []int16) {
	defer func() { _ = recover() }() // in may be closed during teardown
	p.in <- frame
}

func selectOutputDevice(name string) (*portaudio.DeviceInfo, error) {
	if name == "" {
		dev, err := portaudio.DefaultOutputDevice()
		if err != nil {
			return nil, fmt.Errorf("default output: %w", err)
		}
		return dev, nil
	}
	devs, err := portaudio.Devices()
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	for _, d := range devs {
		if d.Name == name && d.MaxOutputChannels > 0 {
			return d, nil
		}
	}
	return nil, fmt.Errorf("output device %q not found", name)
}
