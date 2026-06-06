// Package wakeword runs a small local detector on the mic stream
// and fires a callback when the wake phrase is heard.
//
// Two backends are provided:
//
//   - EnergyDetector: a zero-dependency RMS-threshold detector.
//     Useful as a placeholder and on CI; it will fire on any loud
//     sound above the configured threshold, so it is not a real
//     "wake word" but a "tap to talk" trigger.
//
//   - PorcupineDetector: a real keyword spotter backed by Picovoice
//     Porcupine. Requires a paid access key and a `.ppn` model file.
//     See https://picovoice.ai/docs/porcupine/.
//
// The Detector interface lets the app stay agnostic.
package wakeword

import (
	"context"
	"log"
)

// Callback is invoked from the detector's run loop when the wake
// phrase is heard. It must be non-blocking; the detector's loop will
// continue regardless.
type Callback func()

// Detector consumes PCM frames and fires a callback on detection.
type Detector interface {
	// Run blocks until ctx is done or an unrecoverable error occurs.
	// frames is the live mic channel, 16kHz mono int16.
	Run(ctx context.Context, frames <-chan []int16, onWake Callback) error
}

// Engine returns the Detector configured by the engine name. Unknown
// names return nil so the caller can skip wake-word handling.
func Engine(name string, keyword, accessKey string) Detector {
	switch name {
	case "", "off":
		return nil
	case "energy":
		return &EnergyDetector{
			Threshold: 1500,
			MinFrames: 3, // ~180ms of voice
			Cooldown:  30,
		}
	case "porcupine":
		p, err := NewPorcupine(keyword, accessKey)
		if err != nil {
			log.Printf("[wakeword] porcupine init failed: %v (falling back to energy)", err)
			return Engine("energy", "", "")
		}
		return p
	default:
		log.Printf("[wakeword] unknown engine %q, disabled", name)
		return nil
	}
}
