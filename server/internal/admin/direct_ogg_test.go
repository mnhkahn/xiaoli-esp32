package admin

import (
	"bytes"
	"testing"
	"time"
)

func TestBuildOggOpusWrapsRawFrames(t *testing.T) {
	body, err := buildOggOpus([][]byte{
		{0x11, 0x22, 0x33},
		{0x44, 0x55},
	}, 16000, 1, 60)
	if err != nil {
		t.Fatalf("buildOggOpus returned error: %v", err)
	}
	if !bytes.HasPrefix(body, []byte("OggS")) {
		t.Fatalf("ogg body does not start with OggS: %x", body[:4])
	}
	if !bytes.Contains(body, []byte("OpusHead")) || !bytes.Contains(body, []byte("OpusTags")) {
		t.Fatal("ogg body is missing Opus headers")
	}
	if got := oggOpusDuration(body); got != 120*time.Millisecond {
		t.Fatalf("duration = %s, want 120ms", got)
	}
}

func TestExtractOpusPacketsRoundTrip(t *testing.T) {
	frames := [][]byte{
		make([]byte, 80),
		make([]byte, 90),
		make([]byte, 85),
		make([]byte, 80),
		make([]byte, 75),
	}
	for i, f := range frames {
		for j := range f {
			f[j] = byte(i + 1)
		}
	}
	ogg, err := buildOggOpus(frames, 16000, 1, 60)
	if err != nil {
		t.Fatalf("buildOggOpus: %v", err)
	}
	got, frameDur := extractOpusPackets(ogg)
	if len(got) != len(frames) {
		t.Fatalf("packet count: got %d want %d", len(got), len(frames))
	}
	for i, f := range frames {
		if !bytes.Equal(got[i], f) {
			t.Fatalf("packet %d mismatch: got %x want %x", i, got[i], f)
		}
	}
	if frameDur < 50*time.Millisecond || frameDur > 70*time.Millisecond {
		t.Errorf("frameDuration: got %v want ~60ms", frameDur)
	}
}
