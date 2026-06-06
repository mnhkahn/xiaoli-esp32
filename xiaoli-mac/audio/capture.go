package audio

import (
	"context"
	"fmt"
	"log"

	"github.com/gordonklaus/portaudio"
)

// Capture is a PortAudio input stream that pushes fixed-size PCM
// frames onto the given channel. The channel is closed when ctx is
// done.
type Capture struct {
	stream *portaudio.Stream
}

// OpenCapture opens the default input device and starts streaming.
// The frames channel receives slices of length samplesPerFrame
// (960 int16 samples = 60ms at 16kHz).
func OpenCapture(ctx context.Context, deviceName string) (*Capture, <-chan []int16, error) {
	if err := portaudio.Initialize(); err != nil {
		return nil, nil, fmt.Errorf("portaudio init: %w", err)
	}
	dev, err := selectInputDevice(deviceName)
	if err != nil {
		_ = portaudio.Terminate()
		return nil, nil, err
	}
	params := portaudio.LowLatencyParameters(dev, nil)
	params.Input.Channels = Channels
	params.SampleRate = SampleRate
	params.FramesPerBuffer = samplesPerFrame

	out := make(chan []int16, 8)
	stream, err := portaudio.OpenStream(params, func(in []int16) {
		// Copy out of the PortAudio buffer; the next call will
		// overwrite it.
		buf := make([]int16, len(in))
		copy(buf, in)
		select {
		case out <- buf:
		case <-ctx.Done():
		default:
			// Drop frame if the consumer is slow.
		}
	})
	if err != nil {
		_ = portaudio.Terminate()
		return nil, nil, fmt.Errorf("portaudio open input: %w", err)
	}
	if err := stream.Start(); err != nil {
		_ = stream.Close()
		_ = portaudio.Terminate()
		return nil, nil, fmt.Errorf("portaudio start input: %w", err)
	}
	go func() {
		<-ctx.Done()
		_ = stream.Stop()
		_ = stream.Close()
		_ = portaudio.Terminate()
		close(out)
		log.Printf("[audio] capture stopped")
	}()
	return &Capture{stream: stream}, out, nil
}

func selectInputDevice(name string) (*portaudio.DeviceInfo, error) {
	if name == "" {
		dev, err := portaudio.DefaultInputDevice()
		if err != nil {
			return nil, fmt.Errorf("default input: %w", err)
		}
		return dev, nil
	}
	devs, err := portaudio.Devices()
	if err != nil {
		return nil, fmt.Errorf("list devices: %w", err)
	}
	for _, d := range devs {
		if d.Name == name && d.MaxInputChannels > 0 {
			return d, nil
		}
	}
	return nil, fmt.Errorf("input device %q not found", name)
}
