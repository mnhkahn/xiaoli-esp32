package wakeword

import (
	"context"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func TestEnergyDetectorTriggersOnLoudFrame(t *testing.T) {
	d := &EnergyDetector{Threshold: 1000, MinFrames: 2, Cooldown: 5}
	frames := make(chan []int16, 8)
	var fires int32
	done := make(chan struct{})

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	go func() {
		_ = d.Run(ctx, frames, func() {
			atomic.AddInt32(&fires, 1)
		})
		close(done)
	}()

	// Push 4 loud frames (above 1000 RMS).
	for i := 0; i < 4; i++ {
		frames <- []int16{2000, 2000, 2000, 2000}
	}

	// Wait for the trigger.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if atomic.LoadInt32(&fires) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&fires) == 0 {
		t.Error("expected at least one trigger")
	}

	cancel()
	<-done
}

func TestEnergyDetectorSilentOnQuietInput(t *testing.T) {
	d := &EnergyDetector{Threshold: 5000, MinFrames: 2, Cooldown: 5}
	frames := make(chan []int16, 8)
	var fires int32
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	go func() {
		_ = d.Run(ctx, frames, func() {
			atomic.AddInt32(&fires, 1)
		})
	}()

	for i := 0; i < 10; i++ {
		frames <- []int16{50, 50, 50, 50}
		time.Sleep(10 * time.Millisecond)
	}
	if atomic.LoadInt32(&fires) != 0 {
		t.Errorf("expected no triggers, got %d", fires)
	}
}

func TestEngineRoutesCorrectly(t *testing.T) {
	if d := Engine("off", "", ""); d != nil {
		t.Error("off should return nil")
	}
	if d := Engine("", "", ""); d != nil {
		t.Error("empty should return nil")
	}
	if d := Engine("garbage", "", ""); d != nil {
		t.Error("unknown should return nil")
	}
	if d := Engine("energy", "", ""); d == nil {
		t.Error("energy should return a Detector")
	}
}

func TestRmsInt16(t *testing.T) {
	// All zeros -> 0.
	if got := rmsInt16([]int16{0, 0, 0}); got != 0 {
		t.Errorf("rms of zeros: %v", got)
	}
	// All 1000 -> 1000.
	if got := rmsInt16([]int16{1000, -1000, 1000, -1000}); math.Abs(got-1000) > 0.01 {
		t.Errorf("rms of 1000: %v", got)
	}
}
