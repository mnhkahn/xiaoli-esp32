// playback_lifecycle_test.go verifies that a tts.start → tts.stop
// cycle in the App actually releases the PortAudio stream. The
// production bug (xiaoli-mac/app/app.go: onTTSStart passing
// context.Background() to audio.OpenPlayback) caused the stream to
// leak across TTS turns. This test uses the audio package's
// ActivePlaybacks() counter to detect the leak.
//
// The test constructs an App manually (no fyne, no network) so it
// can be run in CI without a display server.
package app

import (
	"testing"
	"time"

	"xiaoli/mac/audio"
	"xiaoli/mac/config"
)

func TestTTSLifecycleReleasesPlaybackStream(t *testing.T) {
	pipe, err := audio.NewPipeline()
	if err != nil {
		t.Skipf("opus not available: %v", err)
	}
	a := &App{
		Cfg:    &config.Config{},
		audio:  pipe,
		events: make(chan func(), 16),
	}

	waitFor := func(want int64, label string) {
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if got := audio.ActivePlaybacks(); got == want {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Errorf("%s: activePlaybacks=%d, want %d", label,
			audio.ActivePlaybacks(), want)
	}

	baseline := audio.ActivePlaybacks()

	// Round 1: open + close.
	a.onTTSStart()
	waitFor(baseline+1, "after 1st tts.start")
	a.onTTSStop()
	waitFor(baseline, "after 1st tts.stop")

	// Round 2: must NOT pile up a second stream.
	a.onTTSStart()
	if got := audio.ActivePlaybacks(); got != baseline+1 {
		t.Errorf("after 2nd tts.start: activePlaybacks=%d, want %d (old stream leaked)",
			got, baseline+1)
	}
	a.onTTSStop()
	waitFor(baseline, "after 2nd tts.stop")

	// Round 3: still tight.
	a.onTTSStart()
	if got := audio.ActivePlaybacks(); got != baseline+1 {
		t.Errorf("after 3rd tts.start: activePlaybacks=%d, want %d", got, baseline+1)
	}
	a.onTTSStop()
	waitFor(baseline, "after 3rd tts.stop")
}
