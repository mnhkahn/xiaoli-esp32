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
