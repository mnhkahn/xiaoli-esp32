// direct_device_e2e_test.go is the §3 end-to-end test: spin up the
// real deviceHub with a fixture-backed TTS, open a WebSocket client
// exactly like the device would, call deviceHub.Speak("你好宝宝"),
// and verify the client receives 18 audio frames in the right order
// with the right pacing. The decoded PCM is written to
// testdata/cap-go.wav so the human reviewer can afplay it and decide
// whether the audio is intelligible.
//
// This is what §1+§2+§3 looks like from the wire. If cap-go.wav
// sounds like "你好宝宝" the whole Go TTS pipeline (TTS API → Ogg
// re-encode → 60ms re-encode → WS frames → opus decode → PCM) is
// correct on the server side. The remaining unknown is the device
// (Xiaozhi firmware, OLED + speaker) which we cannot test from here.
package admin

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/hraban/opus.v2"
)

// fixtureTTS is a SpeechSynthesizer that always returns the same
// Ogg bytes regardless of input text. Used so the test doesn't hit
// the real SiliconFlow API.
type fixtureTTS struct {
	ogg []byte
}

func (f *fixtureTTS) Synthesize(_ context.Context, _ string) (string, []byte, error) {
	return "audio/ogg", f.ogg, nil
}

func TestE2ETTSCaptureFromGoServer(t *testing.T) {
	// 1. Load the upstream TTS fixture (a real 你好宝宝 response
	//    from a previous TestTTSSynthesizeSavesSampleForLocalPlayback
	//    run, 18,123 bytes of Ogg Opus).
	oggPath := "testdata/tts_sample.ogg"
	ogg, err := os.ReadFile(oggPath)
	if err != nil {
		t.Skipf("fixture missing: %s (run TestTTSSynthesizeSavesSampleForLocalPlayback once with SILICONFLOW_API_KEY to seed it)", oggPath)
	}
	if !bytes.HasPrefix(ogg, []byte("OggS")) {
		t.Fatalf("%s is not an Ogg container (first 4 bytes = %q)", oggPath, ogg[:4])
	}

	// 2. Build the admin server with our fixture TTS wired in.
	cfg := testConfig()
	cfg.DirectDeviceServer = true
	cfg.DeviceAuthEnabled = false
	srv := NewServer(cfg)
	srv.deviceHub.tts = &fixtureTTS{ogg: ogg} // override real SiliconFlow

	httpSrv := httptest.NewServer(srv)
	defer httpSrv.Close()

	// 3. Open a real WebSocket client to /xiaozhi/v1/.
	u, _ := url.Parse(httpSrv.URL)
	wsURL := "ws://" + u.Host + "/xiaozhi/v1/"
	deviceID := "e2e-test-001"
	headers := http.Header{}
	headers.Set("Device-Id", deviceID)
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, headers)
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	defer conn.Close()

	// 4. Send hello, wait for the server's hello response.
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`)); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err != nil {
		t.Fatalf("read server hello: %v", err)
	}

	// 5. Trigger TTS in a goroutine, capturing all frames on the
	//    client side as they arrive. We use a buffered channel for
	//    events so the read loop never blocks the server.
	type event struct {
		kind     string // "text" or "binary"
		text     string
		binary   []byte
		recvAtMs int64 // ms since tts.start
	}
	events := make(chan event, 64)
	var (
		startSeenAt time.Time
		wg          sync.WaitGroup
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(events)
		_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
		for {
			mt, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			now := time.Now()
			ts := int64(0)
			if !startSeenAt.IsZero() {
				ts = now.Sub(startSeenAt).Milliseconds()
			}
			switch mt {
			case websocket.TextMessage:
				ev := event{kind: "text", text: string(data), recvAtMs: ts}
				if bytes.Contains(data, []byte(`"tts"`)) && bytes.Contains(data, []byte(`"start"`)) && startSeenAt.IsZero() {
					startSeenAt = now
					ev.recvAtMs = 0
				}
				events <- ev
				if bytes.Contains(data, []byte(`"tts"`)) && bytes.Contains(data, []byte(`"stop"`)) {
					return
				}
			case websocket.BinaryMessage:
				events <- event{kind: "binary", binary: data, recvAtMs: ts}
			}
		}
	}()

	// Speak blocks until the whole TTS stream is sent, including
	// the tts.stop frame.
	speakDone := make(chan error, 1)
	go func() {
		_, err := srv.deviceHub.Speak(context.Background(), deviceID, "你好宝宝")
		speakDone <- err
	}()

	// 6. Drain the event stream, count frames, decode audio.
	dec, err := opus.NewDecoder(16000, 1)
	if err != nil {
		t.Fatalf("opus decoder: %v", err)
	}
	var (
		allPCM   []int16
		binTimes []int64
		binaryCount int
		textEvents  []event
		rawFrames   [][]byte // raw 18 Opus packets, for the Mac client e2e test
	)
	for ev := range events {
		if ev.kind == "text" {
			textEvents = append(textEvents, ev)
			continue
		}
		// binary
		binaryCount++
		binTimes = append(binTimes, ev.recvAtMs)
		// Copy out of the live conn buffer; the gorilla/websocket
		// library reuses the slice between reads.
		pkt := make([]byte, len(ev.binary))
		copy(pkt, ev.binary)
		rawFrames = append(rawFrames, pkt)
		pcm := make([]int16, 960)
		n, derr := dec.Decode(ev.binary, pcm)
		if derr != nil {
			t.Errorf("opus decode frame %d: %v", binaryCount, derr)
			continue
		}
		if n != 960 {
			t.Errorf("frame %d decoded to %d samples, want 960", binaryCount, n)
		}
		allPCM = append(allPCM, pcm[:n]...)
	}

	if err := <-speakDone; err != nil {
		t.Fatalf("Speak returned: %v", err)
	}
	wg.Wait()

	// 7. Verify the contract.
	if binaryCount != 18 {
		t.Errorf("binary frame count = %d, want 18 (§2 + §3 contract)", binaryCount)
	}
	if len(textEvents) < 3 {
		t.Errorf("expected at least 3 text events (start, sentence_start, stop), got %d", len(textEvents))
	}

	// Timing: first 5 frames should arrive in a tight burst
	// (prebuffer), then pacing should be ~60ms.
	if len(binTimes) >= 6 {
		// Frames 0..4 all within 100ms of tts.start.
		for i := 0; i < 5; i++ {
			if binTimes[i] > 100 {
				t.Errorf("prebuffer frame %d arrived at t=%dms, expected within 100ms of tts.start", i, binTimes[i])
			}
		}
		// Frames 5..N should be spaced ~60ms apart.
		for i := 5; i < len(binTimes)-1; i++ {
			gap := binTimes[i+1] - binTimes[i]
			if gap < 40 || gap > 100 {
				t.Errorf("paced gap frame %d→%d = %dms, want ~60ms", i, i+1, gap)
			}
		}
	}

	// 8. Write the captured PCM to testdata/cap-go.wav.
	outPath := "testdata/cap-go.wav"
	if custom := os.Getenv("CAP_GO_WAV"); custom != "" {
		outPath = custom
	}
	if err := writeWAVFile(outPath, allPCM, 16000, 1); err != nil {
		t.Fatalf("write %s: %v", outPath, err)
	}

	// 9. Save the 18 raw Opus frames for the Mac client e2e test
	//    (xiaoli-mac/audio/pipeline_mac_e2e_test.go loads these and
	//    runs them through the Mac pipeline's decoder). Frames go
	//    to testdata/mac_test_input/000.opus .. 017.opus, three-digit
	//    zero-padded to match Go's natural sort.
	rawDir := "testdata/mac_test_input"
	if custom := os.Getenv("CAP_MAC_RAW_DIR"); custom != "" {
		rawDir = custom
	}
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rawDir, err)
	}
	for i, pkt := range rawFrames {
		path := fmt.Sprintf("%s/%03d.opus", rawDir, i)
		if err := os.WriteFile(path, pkt, 0o644); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
	t.Logf("wrote %d raw opus frames to %s/", len(rawFrames), rawDir)

	dur := float64(len(allPCM)) / 16000.0
	t.Logf("OK: %d binary frames, %d PCM samples, %.3fs @ 16kHz, wrote %s",
		binaryCount, len(allPCM), dur, outPath)
	t.Logf("first 5 frame times (ms): %v", binTimes[:min(5, len(binTimes))])
	t.Logf("paced gaps (ms): %v", gapDeltas(binTimes[5:]))
	t.Logf("text events (%d):", len(textEvents))
	for _, ev := range textEvents {
		t.Logf("  t=%dms: %s", ev.recvAtMs, ev.text)
	}
}

func gapDeltas(times []int64) []int64 {
	if len(times) < 2 {
		return nil
	}
	out := make([]int64, 0, len(times)-1)
	for i := 0; i < len(times)-1; i++ {
		out = append(out, times[i+1]-times[i])
	}
	return out
}

func writeWAVFile(path string, samples []int16, sampleRate, channels int) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dataSize := uint32(len(samples) * 2)
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], 36+dataSize)
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], uint16(channels))
	binary.LittleEndian.PutUint32(hdr[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(sampleRate*channels*2))
	binary.LittleEndian.PutUint16(hdr[32:], uint16(channels*2))
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], dataSize)
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	return binary.Write(f, binary.LittleEndian, samples)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
