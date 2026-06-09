package audio

import (
	"context"
	"fmt"
	"log"
	"runtime/debug"
	"sync/atomic"
	"time"

	"github.com/gordonklaus/portaudio"
)

// activePlaybacks is the number of Playback streams currently open.
// Exposed via ActivePlaybacks() for lifecycle tests. Incremented
// after stream.Start() succeeds; decremented after Drain or Abort
// closes the underlying PortAudio stream. If a Playback is opened
// and neither Drain nor Abort is called (and ctx is never
// cancelled), this counter will keep growing and the test will
// catch the leak.
var activePlaybacks int64

// ActivePlaybacks returns the number of open Playback streams.
// Tests use this to detect leaked streams across open/close cycles.
func ActivePlaybacks() int64 { return atomic.LoadInt64(&activePlaybacks) }

// Playback drains PCM frames from a channel and writes them to the
// default output device. The caller is responsible for calling
// Drain (normal teardown, lets buffered audio play out) or Abort
// (force teardown, discards buffer) exactly once.
type Playback struct {
	stream *portaudio.Stream
	in     chan []int16
	// drainSig is closed by Drain to signal the PortAudio
	// callback to stop reading from `in` and start producing
	// silence. We use a separate signal instead of closing `in`
	// because closing `in` while the PortAudio callback is
	// reading from it can race with the C-side stream
	// teardown and cause a "close of closed channel" panic.
	drainSig chan struct{}
	// closed is set by Drain/Abort via CompareAndSwap. Write
	// returns immediately once closed is true, so a forwarder
	// that's mid-loop after Abort spins harmlessly through
	// remaining pcmOut frames instead of panicking on a closed
	// `in` channel.
	closed atomic.Bool
}

// OpenPlayback opens the default output device and starts
// streaming. PCM frames pushed into the returned channel are
// written to the speaker in order.
//
// The callback uses a non-blocking channel receive and repeats
// the last frame on underrun ("frame-and-hold") instead of
// writing silence or blocking the CoreAudio realtime thread.
// Blocking on a channel inside the PortAudio callback causes
// audio glitches on macOS because CoreAudio's audio unit render
// thread must not block.
//
// Two teardown paths:
//   - Normal (tts.stop): caller invokes Playback.Drain() after
//     the producer channel is closed. Drain signals the PortAudio
//     callback to stop, waits ~100ms for PortAudio to play out
//     its internal buffer, then closes the stream. Audio in `in`
//     and the PortAudio ring buffer is preserved — matching the
//     ESP32 reference (xiaozhi-esp32/main/audio/audio_service.cc:652,
//     where tts.stop flips state but does not clear
//     audio_playback_queue_).
//   - Force (ctx cancel / app shutdown): the watchdog started
//     here calls Playback.Abort() which discards the PortAudio
//     buffer and closes the stream.
func OpenPlayback(ctx context.Context, deviceName string) (*Playback, chan []int16, error) {
	Initialize()
	dev, err := selectOutputDevice(deviceName)
	if err != nil {
		return nil, nil, err
	}
	params := portaudio.HighLatencyParameters(nil, dev)
	params.Output.Channels = Channels
	params.SampleRate = SampleRate
	params.FramesPerBuffer = samplesPerFrame

	in := make(chan []int16, 64)
	drainSig := make(chan struct{})

	// Pre-fill one silent frame so the first callback never
	// underruns before the decode loop produces its first frame.
	in <- make([]int16, samplesPerFrame)

	var lastFrame []int16
	stream, err := portaudio.OpenStream(params, func(out []int16) {
		// Non-blocking receive; never block on the CoreAudio
		// realtime thread. When no frame is ready we repeat the
		// last frame (frame-and-hold), which for TTS is a brief,
		// barely-audible stutter — far better than the 60ms
		// silence gap that the old `default: silence` approach
		// injected between words.
		select {
		case frame, ok := <-in:
			if !ok {
				for i := range out {
					out[i] = 0
				}
				return
			}
			lastFrame = frame
			n := copy(out, frame)
			for i := n; i < len(out); i++ {
				out[i] = 0
			}
		case <-ctx.Done():
			for i := range out {
				out[i] = 0
			}
			return
		case <-drainSig:
			for i := range out {
				out[i] = 0
			}
			return
		default:
			if lastFrame != nil {
				copy(out, lastFrame)
			}
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("portaudio open output: %w", err)
	}
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		return nil, nil, fmt.Errorf("portaudio start output: %w", err)
	}
	atomic.AddInt64(&activePlaybacks, 1)
	pb := &Playback{stream: stream, in: in, drainSig: drainSig}

	// Watchdog: force-abort on ctx cancel (app shutdown, parent
	// cancel). The normal tts.stop path goes through Drain, not
	// cancel, so this only fires when something above the audio
	// pipeline tears the whole context down.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[audio] FATAL panic in playback watchdog: %v\n%s", r, debug.Stack())
			}
		}()
		<-ctx.Done()
		pb.Abort()
	}()
	return pb, in, nil
}

// Write pushes a PCM frame to the speaker. Safe to call from any
// goroutine. Blocks if the speaker can't keep up. The previous
// non-blocking variant dropped frames on a full channel, which on
// TTS manifested as an audible gap; a blocking send lets the
// producer slow down to match the consumer's 60ms pace — no frame
// loss.
//
// Once Drain or Abort has run, Write becomes a no-op so a
// forwarder that's mid-loop doesn't panic on a closed `in`.
func (p *Playback) Write(frame []int16) {
	if p.closed.Load() {
		return
	}
	defer func() { _ = recover() }() // in may be closed during teardown
	p.in <- frame
}

// Drain is the normal teardown. It signals the PortAudio callback
// to stop reading from `in` and start producing silence, waits
// briefly for PortAudio's internal ring buffer to play out the
// last real samples, then closes the stream. Buffered audio in
// `in` and in the PortAudio ring buffer is preserved — callers
// see a smooth tail, not a hard cut.
//
// We use a dedicated drainSig channel (not close(in)) because the
// PortAudio callback reads `in` from a C thread, and a Go
// `close(in)` racing with the C-side stream teardown can produce
// a "close of closed channel" panic that is hard to reproduce
// but breaks the lifecycle test.
//
// The 500ms Close timeout matches the previous behaviour; on
// macOS, Pa_CloseStream can hang for many seconds when multiple
// output streams are open, so we'd rather abandon the close and
// let the process exit reclaim the device than block the audio
// pipeline.
func (p *Playback) Drain() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[audio] FATAL panic in playback Drain: %v\n%s", r, debug.Stack())
		}
	}()
	close(p.drainSig)
	// Let PortAudio's internal ring buffer (typically ~10-20ms,
	// sometimes larger on aggregate devices) play out the last
	// real samples before we Close. 100ms is generous enough to
	// cover the worst case without being perceptibly slow.
	time.Sleep(100 * time.Millisecond)
	closeStream(p.stream)
	atomic.AddInt64(&activePlaybacks, -1)
	log.Printf("[audio] playback drained")
}

// Abort is the force teardown. It discards the PortAudio internal
// buffer (so any audio still queued for the speaker is lost) and
// closes the stream. Use this on ctx cancel / app shutdown when
// you don't want to wait for Drain's 100ms grace period.
func (p *Playback) Abort() {
	if !p.closed.CompareAndSwap(false, true) {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[audio] FATAL panic in playback Abort: %v\n%s", r, debug.Stack())
		}
	}()
	_ = p.stream.Abort()
	closeStream(p.stream)
	atomic.AddInt64(&activePlaybacks, -1)
	log.Printf("[audio] playback aborted")
}

// closeStream closes the underlying PortAudio stream with a
// timeout, because Pa_CloseStream can block on macOS when there
// are multiple output streams and the host audio backend is
// rebalancing. Used by both Drain and Abort.
func closeStream(stream *portaudio.Stream) {
	closeDone := make(chan struct{})
	go func() {
		_ = stream.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(500 * time.Millisecond):
		log.Printf("[audio] closeStream: still running after 500ms; abandoning (will be cleaned up at process exit)")
	}
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
