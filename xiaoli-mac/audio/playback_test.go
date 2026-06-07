// playback_test.go verifies the OpenPlayback lifecycle: a context
// that is cancelled should result in the underlying PortAudio stream
// being closed and the activePlaybacks counter decrementing. This
// is the contract the App layer relies on when a tts.stop frame
// arrives.
package audio

import (
	"context"
	"testing"
	"time"
)

func TestOpenPlaybackClosesOnContextCancel(t *testing.T) {
	before := ActivePlaybacks()

	ctx, cancel := context.WithCancel(context.Background())
	pb, pcmOut, err := OpenPlayback(ctx, "")
	if err != nil {
		t.Skipf("portaudio not available: %v", err)
	}
	if pb == nil || pcmOut == nil {
		t.Fatal("OpenPlayback returned nil")
	}

	if got := ActivePlaybacks(); got != before+1 {
		t.Errorf("after open: activePlaybacks=%d, want %d", got, before+1)
	}

	cancel()

	// The cleanup goroutine runs asynchronously after ctx.Done.
	// Poll for up to 2s.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ActivePlaybacks() == before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("after ctx cancel: activePlaybacks=%d, want %d (stream leaked)",
		ActivePlaybacks(), before)
}

func TestOpenPlaybackClosesOnBackgroundContextDoesNotLeakWithinTest(t *testing.T) {
	// Sanity check: even with a Background context (which is the
	// buggy pattern that caused the production leak), the
	// counter accurately reports the open stream. This documents
	// the leak rather than the fix; the fix lives in the App
	// layer (passing a cancellable context).
	before := ActivePlaybacks()

	_, pcmOut, err := OpenPlayback(context.Background(), "")
	if err != nil {
		t.Skipf("portaudio not available: %v", err)
	}
	if pcmOut == nil {
		t.Fatal("nil channel")
	}
	defer func() {
		// We can't cancel a Background context, so the cleanup
		// goroutine will never run in this test. The test process
		// will exit with the stream still open; that's a
		// process-level cleanup concern, not this test's problem.
		// (The whole point of the test is to demonstrate the leak
		// exists when ctx is non-cancellable.)
		if got := ActivePlaybacks(); got != before+1 {
			t.Errorf("Background ctx should keep stream open: activePlaybacks=%d, want %d", got, before+1)
		}
	}()
}
