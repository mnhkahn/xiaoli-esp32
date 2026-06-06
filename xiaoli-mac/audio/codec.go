package audio

import (
	"fmt"

	"github.com/hraban/opus"
)

// NewEncoder returns an Opus encoder configured for 16kHz, mono, VoIP.
func NewEncoder() (*opus.Encoder, error) {
	enc, err := opus.NewEncoder(SampleRate, Channels, opus.AppVoIP)
	if err != nil {
		return nil, fmt.Errorf("opus encoder: %w", err)
	}
	// 60ms frame at 16kHz = 960 samples.
	if err := enc.SetBitrate(32000); err != nil {
		return nil, fmt.Errorf("opus bitrate: %w", err)
	}
	return enc, nil
}

// NewDecoder returns an Opus decoder configured for 16kHz, mono.
func NewDecoder() (*opus.Decoder, error) {
	dec, err := opus.NewDecoder(SampleRate, Channels)
	if err != nil {
		return nil, fmt.Errorf("opus decoder: %w", err)
	}
	return dec, nil
}
