package audio

import (
	"context"
	"fmt"
	"log"

	"github.com/gordonklaus/portaudio"
)

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
		// Try to grab one frame; on miss, emit silence.
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
		default:
			for i := range out {
				out[i] = 0
			}
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
	pb := &Playback{stream: stream, in: in}
	go func() {
		<-ctx.Done()
		_ = stream.Stop()
		_ = stream.Close()
		_ = portaudio.Terminate()
		log.Printf("[audio] playback stopped")
	}()
	return pb, in, nil
}

// Write pushes a PCM frame to the speaker. Safe to call from any
// goroutine.
func (p *Playback) Write(frame []int16) {
	select {
	case p.in <- frame:
	default:
		// Drop the frame if the speaker is already overwhelmed.
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
