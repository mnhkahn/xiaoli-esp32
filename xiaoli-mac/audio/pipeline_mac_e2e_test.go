// pipeline_mac_e2e_test.go runs the 18 raw Opus frames the Go server
// sent (captured by server/internal/admin/direct_device_e2e_test.go)
// through the Mac client's audio.Pipeline decoder and verifies the
// output PCM is byte-identical to server/internal/admin/testdata/
// cap-go.wav. If this passes, the Mac decoder is correct end-to-end.
// If the bytes diverge, the bug is in this package's encoder/decoder
// (hraban/opus wrapper, frame size, sample rate). If they match but
// the real xiaoli-mac binary still sounds garbled, the bug is in
// playback lifecycle (stream never closed) or in the state machine.
package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// pathToServerTestdata is the path from xiaoli-mac/audio/ up to the
// Go server test fixtures. Kept as a variable so a future layout
// change only touches this constant.
const serverTestdata = "../../server/internal/admin/testdata"

func TestMacDecoderE2EMatchesGoServer(t *testing.T) {
	opusDir := filepath.Join(serverTestdata, "mac_test_input")
	capGo := filepath.Join(serverTestdata, "cap-go.wav")

	// Load 18 raw Opus frames.
	var frames [][]byte
	for i := 0; i < 100; i++ {
		path := fmt.Sprintf("%s/%03d.opus", opusDir, i)
		pkt, err := os.ReadFile(path)
		if err != nil {
			break
		}
		frames = append(frames, pkt)
	}
	if len(frames) == 0 {
		t.Skipf("no mac_test_input frames at %s (run the Go server e2e test first to seed them)", opusDir)
	}
	t.Logf("loaded %d raw opus frames from %s", len(frames), opusDir)

	// Drive a bytesSource (FrameSource impl) through the Mac
	// pipeline's DecodeLoop, just like a real tts.start would.
	p, err := NewPipeline()
	if err != nil {
		t.Skipf("opus not available: %v", err)
	}
	pcmOut := make(chan []int16, len(frames))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	src := &bytesSource{frames: frames}
	go p.DecodeLoop(ctx, src, pcmOut)

	var allPCM []int16
	for i := 0; i < len(frames); i++ {
		select {
		case frame, ok := <-pcmOut:
			if !ok {
				t.Fatalf("decode loop closed channel at frame %d", i)
			}
			allPCM = append(allPCM, frame...)
		case <-ctx.Done():
			t.Fatalf("decode loop timeout at frame %d", i)
		}
	}
	t.Logf("decoded %d frames → %d PCM samples (%.3fs @ 16kHz)",
		len(frames), len(allPCM), float64(len(allPCM))/16000.0)

	// Write /tmp/mac-decoded.wav for human listening.
	macOut := "/tmp/mac-decoded.wav"
	if err := writeWAV16Mono(macOut, allPCM, 16000); err != nil {
		t.Fatalf("write %s: %v", macOut, err)
	}
	t.Logf("wrote %s (afplay it to hear what the Mac client would play)", macOut)

	// Compare with cap-go.wav.
	want, err := readWAV16Mono(capGo)
	if err != nil {
		t.Skipf("cap-go.wav missing at %s: %v", capGo, err)
	}
	if len(want) != len(allPCM) {
		t.Errorf("PCM length mismatch: mac=%d, cap-go=%d (diff=%d samples = %.1f ms)",
			len(allPCM), len(want), len(allPCM)-len(want),
			float64(len(allPCM)-len(want))/16.0)
	} else {
		var maxDiff int16
		for i := range allPCM {
			d := allPCM[i] - want[i]
			if d < 0 {
				d = -d
			}
			if d > maxDiff {
				maxDiff = d
			}
		}
		t.Logf("max PCM delta vs cap-go.wav = %d (int16)", maxDiff)
		// libopus's resampler/state is deterministic across runs of
		// the same code on the same arch, so bytes should be
		// identical. Allow a tiny tolerance for the off-by-one LSB
		// some builds show on the leading samples of frame 0.
		if maxDiff > 2 {
			t.Errorf("PCM diverges from cap-go.wav (max delta %d); Mac decoder is misconfigured", maxDiff)
		} else {
			t.Logf("OK: Mac decoder output matches cap-go.wav (max delta ≤ 2)")
		}
	}

	// Sanity: total RMS is well above silence. (cap-go measures
	// -28 dB mean; that corresponds to RMS ≈ 1000.)
	var sumSq float64
	for _, s := range allPCM {
		sumSq += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSq / float64(len(allPCM)))
	if rms < 100 {
		t.Errorf("decoded signal RMS=%.1f, expected > 100 (signal is silent?)", rms)
	}
}

// bytesSource is a FrameSource backed by a slice of pre-loaded
// Opus packets. Returns io.EOF (via context.Canceled) when exhausted.
type bytesSource struct {
	frames [][]byte
	idx    int
}

func (b *bytesSource) Next(ctx context.Context) ([]byte, error) {
	if b.idx >= len(b.frames) {
		return nil, context.Canceled
	}
	pkt := b.frames[b.idx]
	b.idx++
	return pkt, nil
}

// writeWAV16Mono writes a 16-bit mono PCM WAV.
func writeWAV16Mono(path string, samples []int16, sampleRate int) error {
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
	binary.LittleEndian.PutUint16(hdr[22:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(hdr[32:], 2)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], dataSize)
	if _, err := f.Write(hdr); err != nil {
		return err
	}
	return binary.Write(f, binary.LittleEndian, samples)
}

// readWAV16Mono reads a 16-bit mono PCM WAV, validating the header.
func readWAV16Mono(path string) ([]int16, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 44 {
		return nil, fmt.Errorf("WAV too short: %d bytes", len(data))
	}
	if !bytes.HasPrefix(data, []byte("RIFF")) || !bytes.Contains(data[8:12], []byte("WAVE")) {
		return nil, fmt.Errorf("not a WAV file (header=%q)", data[:12])
	}
	if binary.LittleEndian.Uint16(data[20:22]) != 1 {
		return nil, fmt.Errorf("not PCM (format=%d)", binary.LittleEndian.Uint16(data[20:22]))
	}
	if binary.LittleEndian.Uint16(data[22:24]) != 1 {
		return nil, fmt.Errorf("not mono (channels=%d)", binary.LittleEndian.Uint16(data[22:24]))
	}
	if binary.LittleEndian.Uint16(data[34:36]) != 16 {
		return nil, fmt.Errorf("not 16-bit (bits=%d)", binary.LittleEndian.Uint16(data[34:36]))
	}
	dataSize := binary.LittleEndian.Uint32(data[40:44])
	pcm := data[44 : 44+int(dataSize)]
	samples := make([]int16, len(pcm)/2)
	if err := binary.Read(bytes.NewReader(pcm), binary.LittleEndian, &samples); err != nil {
		return nil, err
	}
	return samples, nil
}
