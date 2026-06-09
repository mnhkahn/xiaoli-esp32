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

// TestClosingOnePlaybackDoesNotAffectAnother is a regression test
// for the production bug where each stream's cleanup goroutine
// called portaudio.Terminate() (a process-wide teardown that closes
// ALL open PortAudio streams). The next OpenStream() then raced
// with the prior Terminate() and the new stream's audio came out
// garbled. With the fix, stream A's cleanup must not touch stream B.
//
// Procedure:
//  1. Open stream A.
//  2. Open stream B (counter: baseline+2).
//  3. Cancel A; wait for its counter slot to be released.
//  4. Assert B is still open (counter == baseline+1) and we can
//     still write to it without panicking.
//  5. Cancel B; wait for cleanup.
func TestClosingOnePlaybackDoesNotAffectAnother(t *testing.T) {
	before := ActivePlaybacks()

	ctxA, cancelA := context.WithCancel(context.Background())
	pbA, pcmA, err := OpenPlayback(ctxA, "")
	if err != nil {
		t.Skipf("portaudio not available: %v", err)
	}
	if pbA == nil || pcmA == nil {
		t.Fatal("OpenPlayback A returned nil")
	}
	_ = pbA // alive for the duration; prevent lint complaint

	ctxB, cancelB := context.WithCancel(context.Background())
	pbB, pcmB, err := OpenPlayback(ctxB, "")
	if err != nil {
		cancelA()
		t.Fatalf("OpenPlayback B failed: %v", err)
	}
	if pbB == nil || pcmB == nil {
		cancelA()
		t.Fatal("OpenPlayback B returned nil")
	}

	if got := ActivePlaybacks(); got != before+2 {
		t.Errorf("after 2 opens: activePlaybacks=%d, want %d", got, before+2)
	}

	// Close A. A's cleanup goroutine MUST NOT touch B.
	cancelA()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ActivePlaybacks() == before+1 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := ActivePlaybacks(); got != before+1 {
		cancelB()
		t.Fatalf("after cancel A: activePlaybacks=%d, want %d (cleanup of A killed B)",
			got, before+1)
	}

	// B must still be writable. We send a silent frame to a buffered
	// channel; if B is dead this would block forever, so we wrap in
	// a select with a timeout to keep the test bounded.
	select {
	case pcmB <- make([]int16, samplesPerFrame):
	case <-time.After(time.Second):
		cancelB()
		t.Fatal("writing to stream B after A closed: timed out (B was killed by A's cleanup)")
	}

	// Clean up B.
	cancelB()
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ActivePlaybacks() == before {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("after cancel B: activePlaybacks=%d, want %d (stream B leaked)",
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
