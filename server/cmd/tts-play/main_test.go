// tts-play_test.go spins up an in-process WebSocket server that
// pretends to be xiaoli-admin, sends a fake TTS stream of two 60ms
// Opus packets, and verifies tts-play decodes them to a 16kHz mono
// WAV. This is a sanity check that the decoder wiring is correct
// without needing the real admin server.
package main

import (
	"encoding/binary"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/hraban/opus.v2"
)

func TestTTSPlayDecodesAndDumpsWAV(t *testing.T) {
	// Compile tts-play into a temp dir.
	binDir := t.TempDir()
	bin := filepath.Join(binDir, "tts-play")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = "."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build tts-play: %v\n%s", err, out)
	}

	// Encode 2 frames of a 440Hz sine wave, 60ms / 16kHz / VoIP —
	// same params as the production server.
	const (
		sampleRate = 16000
		frameDur   = 60
		frameSize  = sampleRate * frameDur / 1000 // 960
	)
	enc, err := opus.NewEncoder(sampleRate, 1, opus.AppVoIP)
	if err != nil {
		t.Fatalf("encoder: %v", err)
	}
	var sine [frameSize]int16
	for i := range sine {
		sine[i] = int16(8000.0 * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
	}
	pkt1 := make([]byte, 4000)
	n1, err := enc.Encode(sine[:], pkt1)
	if err != nil {
		t.Fatalf("encode frame 1: %v", err)
	}
	pkt1 = pkt1[:n1]
	pkt2 := make([]byte, 4000)
	n2, err := enc.Encode(sine[:], pkt2)
	if err != nil {
		t.Fatalf("encode frame 2: %v", err)
	}
	pkt2 = pkt2[:n2]

	upgrader := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/xiaozhi/v1/" {
			http.NotFound(w, r)
			return
		}
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, _, _ = c.ReadMessage() // eat device hello
		send := func(payload []byte) {
			_ = c.SetWriteDeadline(time.Now().Add(2 * time.Second))
			_ = c.WriteMessage(websocket.TextMessage, payload)
		}
		send([]byte(`{"type":"tts","state":"start"}`))
		send([]byte(`{"type":"tts","state":"sentence_start","text":"beep"}`))
		_ = c.WriteMessage(websocket.BinaryMessage, pkt1)
		_ = c.WriteMessage(websocket.BinaryMessage, pkt2)
		send([]byte(`{"type":"tts","state":"stop"}`))
		time.Sleep(200 * time.Millisecond) // give client time to drain
	}))
	defer srv.Close()

	u, _ := url.Parse(srv.URL)
	host, port, _ := net.SplitHostPort(u.Host)
	wsURL := "ws://" + net.JoinHostPort(host, port) + "/xiaozhi/v1/"

	out := filepath.Join(t.TempDir(), "cap.wav")
	cmd := exec.Command(bin,
		"-url", wsURL,
		"-device-id", "test-001",
		"-out", out,
		"-idle-timeout", "500ms",
		"-max-run", "10s",
	)
	outb, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("tts-play exit: %v\n%s", err, outb)
	}
	if !strings.Contains(string(outb), "captured 2 frames") {
		t.Fatalf("expected 'captured 2 frames' in output, got:\n%s", outb)
	}

	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read %s: %v", out, err)
	}
	if len(raw) < 44 {
		t.Fatalf("WAV too short: %d bytes", len(raw))
	}
	dataSize := binary.LittleEndian.Uint32(raw[40:44])
	if dataSize != 2*frameSize*2 {
		t.Fatalf("WAV data size = %d, want %d (2 frames * 960 samples * 2 bytes)",
			dataSize, 2*frameSize*2)
	}
	samples := int(dataSize / 2)
	var energy int64
	for i := 0; i < samples; i++ {
		v := int16(binary.LittleEndian.Uint16(raw[44+i*2 : 46+i*2]))
		energy += int64(v) * int64(v)
	}
	rms := math.Sqrt(float64(energy / int64(samples)))
	if rms < 100 {
		t.Fatalf("decoded PCM is silent (RMS=%.1f), opus decode broken", rms)
	}
	t.Logf("OK: 2 frames decoded, %d samples, RMS=%.1f", samples, rms)
}
