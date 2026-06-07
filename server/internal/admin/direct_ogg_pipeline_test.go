package admin

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gopkg.in/hraban/opus.v2"
)

func computeSNR(a, b []int16) float64 {
	var sig, noise float64
	for i := range a {
		s := float64(a[i])
		n := float64(a[i] - b[i])
		sig += s * s
		noise += n * n
	}
	if noise == 0 {
		return math.Inf(1)
	}
	return 10 * math.Log10(sig/noise)
}

func TestTTSReencodePipelineFromSample(t *testing.T) {
	body, err := os.ReadFile("testdata/tts_sample.ogg")
	if err != nil {
		t.Skip("testdata/tts_sample.ogg missing — run TestTTSSynthesizeSavesSampleForLocalPlayback first")
	}
	if !bytes.HasPrefix(body, []byte("OggS")) {
		t.Fatalf("sample is not Ogg (first 4 bytes = %q)", body[:4])
	}

	inPackets, srcFrameDur := extractOpusPackets(body)
	if len(inPackets) == 0 {
		t.Fatal("extractOpusPackets returned 0 packets")
	}
	t.Logf("INPUT : %d packets, srcFrameDur=%v, totalDur=%v",
		len(inPackets), srcFrameDur,
		time.Duration(float64(time.Second)*float64(len(inPackets))*srcFrameDur.Seconds()))

	outPackets, _, err := reencodeOpusFrames(inPackets, 16000, srcFrameDur, 60)
	if err != nil {
		t.Fatalf("reencodeOpusFrames: %v", err)
	}
	t.Logf("OUTPUT: %d packets × 60ms = %v total",
		len(outPackets), time.Duration(len(outPackets))*60*time.Millisecond)

	for i, p := range inPackets {
		if len(p) >= 8 && (string(p[:8]) == "OpusHead" || string(p[:8]) == "OpusTags") {
			t.Errorf("input packet %d is a header: %x", i, p[:8])
		}
	}
	for i, p := range outPackets {
		if len(p) >= 8 && (string(p[:8]) == "OpusHead" || string(p[:8]) == "OpusTags") {
			t.Errorf("output packet %d is a header: %x", i, p[:8])
		}
	}

	dec, err := opus.NewDecoder(16000, 1)
	if err != nil {
		t.Fatal(err)
	}

	var inSamples []int
	for i, p := range inPackets {
		pcm := make([]int16, 5760)
		n, err := dec.Decode(p, pcm)
		if err != nil {
			t.Fatalf("decode input[%d]: %v", i, err)
		}
		inSamples = append(inSamples, n)
	}

	outSamples := make([]int, len(outPackets))
	var outPCM []int16
	for i, p := range outPackets {
		pcm := make([]int16, 960)
		n, err := dec.Decode(p, pcm)
		if err != nil {
			t.Fatalf("decode output[%d]: %v", i, err)
		}
		outSamples[i] = n
		outPCM = append(outPCM, pcm[:n]...)
	}

	bad := 0
	for i, n := range outSamples {
		if n != 960 {
			t.Errorf("output[%d] decoded to %d samples, want 960", i, n)
			bad++
		}
	}
	if bad == 0 {
		t.Logf("FRAME : all %d outputs decode to 960 samples (60ms @ 16kHz) ✓", len(outPackets))
	}

	var energy float64
	for _, s := range outPCM {
		energy += float64(s) * float64(s)
	}
	rms := math.Sqrt(energy / float64(len(outPCM)))
	t.Logf("ENERGY: RMS=%.1f (out of 32768) over %d samples", rms, len(outPCM))
	if rms < 100 {
		t.Errorf("output audio is near-silent (RMS=%.1f) — pipeline may be broken", rms)
	}

	if os.Getenv("DUMP_TTS") != "" {
		if err := os.MkdirAll("testdata/frames_in", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll("testdata/frames_out", 0o755); err != nil {
			t.Fatal(err)
		}
		writeWAVs(t, "testdata/frames_in", inPackets, inSamples)
		writeWAVs(t, "testdata/frames_out", outPackets, outSamples)
		stitched, err := buildOggOpus(outPackets, 16000, 1, 60)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile("testdata/stitched.ogg", stitched, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("DUMPED: testdata/frames_in/ (%d files), testdata/frames_out/ (%d files), testdata/stitched.ogg",
			len(inPackets), len(outPackets))
	}
}

func writeWAVs(t *testing.T, dir string, packets [][]byte, samples []int) {
	dec, _ := opus.NewDecoder(16000, 1)
	for i, p := range packets {
		pcm := make([]int16, 5760)
		n, _ := dec.Decode(p, pcm)
		if n == 0 {
			n = samples[i]
		}
		path := filepath.Join(dir, fmt.Sprintf("%03d.wav", i))
		if err := os.WriteFile(path, wavWrap(pcm[:n], 16000), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func wavWrap(pcm []int16, sampleRate int) []byte {
	const headerSize = 44
	buf := make([]byte, headerSize+len(pcm)*2)
	copy(buf[0:4], "RIFF")
	binary.LittleEndian.PutUint32(buf[4:8], uint32(len(buf)-8))
	copy(buf[8:12], "WAVE")
	copy(buf[12:16], "fmt ")
	binary.LittleEndian.PutUint32(buf[16:20], 16)
	binary.LittleEndian.PutUint16(buf[20:22], 1)
	binary.LittleEndian.PutUint16(buf[22:24], 1)
	binary.LittleEndian.PutUint32(buf[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:32], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(buf[32:34], 2)
	binary.LittleEndian.PutUint16(buf[34:36], 16)
	copy(buf[36:40], "data")
	binary.LittleEndian.PutUint32(buf[40:44], uint32(len(pcm)*2))
	for i, s := range pcm {
		binary.LittleEndian.PutUint16(buf[headerSize+i*2:headerSize+i*2+2], uint16(s))
	}
	return buf
}
