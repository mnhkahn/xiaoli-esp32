package wakeword

import (
	"context"
	"log"
)

// EnergyDetector is a placeholder wake word detector. It computes
// the RMS of every 60ms frame and fires the callback after the
// signal has been loud for MinFrames consecutive frames. A
// Cooldown prevents rapid retriggering.
type EnergyDetector struct {
	// Threshold is the RMS level (0..32767) above which a frame
	// is considered "voice". Default 1500, tune for the room.
	Threshold int
	// MinFrames is the number of consecutive voice frames required
	// before firing. 3 frames = ~180ms.
	MinFrames int
	// Cooldown is the number of frames to wait after a fire
	// before the detector can fire again. 30 frames = ~1.8s.
	Cooldown int
}

// Run consumes frames until ctx is done.
func (e *EnergyDetector) Run(ctx context.Context, frames <-chan []int16, onWake Callback) error {
	if e.Threshold == 0 {
		e.Threshold = 1500
	}
	if e.MinFrames == 0 {
		e.MinFrames = 3
	}
	if e.Cooldown == 0 {
		e.Cooldown = 30
	}
	voiceRun := 0
	cooldown := 0
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case frame, ok := <-frames:
			if !ok {
				return nil
			}
			if cooldown > 0 {
				cooldown--
				continue
			}
			rms := int(rmsInt16(frame))
			if rms >= e.Threshold {
				voiceRun++
				if voiceRun >= e.MinFrames {
					log.Printf("[wakeword] energy trigger (rms=%d)", rms)
					if onWake != nil {
						onWake()
					}
					cooldown = e.Cooldown
					voiceRun = 0
				}
			} else {
				voiceRun = 0
			}
		}
	}
}

// rmsInt16 returns the root-mean-square of the samples.
func rmsInt16(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum int64
	for _, v := range pcm {
		sum += int64(v) * int64(v)
	}
	// Integer math then convert.
	return sqrtFloat(float64(sum) / float64(len(pcm)))
}

// sqrtFloat is a tiny wrapper around the standard library so the
// hot loop doesn't depend on math on every file.
func sqrtFloat(x float64) float64 {
	return sqrtImpl(x)
}

var sqrtImpl = func(x float64) float64 { return 0 }
