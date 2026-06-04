package admin

import (
	"bytes"
	"testing"
	"time"

	"gopkg.in/hraban/opus.v2"
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

func TestReencodeOpusFrames20To60(t *testing.T) {
	const sampleRate = 16000
	const srcFrameMs = 20
	const targetFrameMs = 60

	enc, _ := opus.NewEncoder(sampleRate, 1, opus.AppVoIP)
	dec, _ := opus.NewDecoder(sampleRate, 1)

	// Encode 6 frames of 20ms
	var packets [][]byte
	for i := 0; i < 6; i++ {
		pcm := make([]int16, sampleRate/1000*srcFrameMs)
		for j := range pcm {
			pcm[j] = int16(float64(8000) * float64(j%50) / 50.0)
		}
		buf := make([]byte, 1024)
		n, _ := enc.Encode(pcm, buf)
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		packets = append(packets, pkt)
	}

	reencoded, frameDur, err := reencodeOpusFrames(packets, sampleRate, time.Duration(srcFrameMs)*time.Millisecond, targetFrameMs)
	if err != nil {
		t.Fatalf("reencodeOpusFrames: %v", err)
	}

	if frameDur != time.Duration(targetFrameMs)*time.Millisecond {
		t.Errorf("frameDuration: got %v want %v", frameDur, time.Duration(targetFrameMs)*time.Millisecond)
	}

	// 6 x 20ms = 120ms => 2 x 60ms
	if len(reencoded) != 2 {
		t.Fatalf("reencoded count: got %d want 2", len(reencoded))
	}

	// Each reencoded packet should decode to 60ms (960 samples at 16kHz)
	for i, pkt := range reencoded {
		pcm := make([]int16, 960)
		n, err := dec.Decode(pkt, pcm)
		if err != nil {
			t.Fatalf("Decode packet %d: %v", i, err)
		}
		if n != 960 {
			t.Errorf("packet %d: decoded %d samples, want 960", i, n)
		}
	}
}
