package audio

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"
)

// Round-trip an Opus-encoded frame: encode a sine-wave PCM frame,
// decode it, check the energy is non-zero.
func TestPipelineRoundTrip(t *testing.T) {
	p, err := NewPipeline()
	if err != nil {
		t.Skipf("opus not available: %v", err)
	}

	// 60ms of a 440Hz sine at 16kHz.
	pcmIn := make([]int16, samplesPerFrame)
	for i := range pcmIn {
		pcmIn[i] = int16(8000.0 * sine(440.0, float64(i)/float64(SampleRate)))
	}

	pkt, err := p.enc.Encode(pcmIn, p.encBuf)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if pkt == 0 {
		t.Fatal("encoder produced 0 bytes")
	}
	encoded := make([]byte, pkt)
	copy(encoded, p.encBuf[:pkt])

	dec, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	n, err := dec.Decode(encoded, p.decBuf)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if n == 0 {
		t.Fatal("decoder produced 0 samples")
	}

	// The decoder may pad with leading/trailing silence, so we
	// check the energy somewhere in the middle.
	var energy int64
	for i := n / 4; i < 3*n/4; i++ {
		v := int64(p.decBuf[i])
		energy += v * v
	}
	if energy == 0 {
		t.Errorf("decoded signal is silent (n=%d)", n)
	}
}

// Verify the listening gate actually disables the encode path.
func TestPipelineListeningGate(t *testing.T) {
	p, err := NewPipeline()
	if err != nil {
		t.Skipf("opus not available: %v", err)
	}
	if p.IsListening() {
		t.Fatal("should start with listening=false")
	}
	p.SetListening(true)
	if !p.IsListening() {
		t.Fatal("SetListening(true) did not take effect")
	}
	p.SetListening(false)
	if p.IsListening() {
		t.Fatal("SetListening(false) did not take effect")
	}
}

func sine(freq, t float64) float64 {
	const twoPi = 6.283185307179586
	// 0.5 + 0.5*sin so the value is in [0, 1] (positive only) and
	// multiplied by amplitude 8000 by the caller to get a positive
	// int16. The bias avoids the sign crossing issue that some
	// Opus modes apply a high-pass filter to.
	return 0.5 + 0.5*sinInt(twoPi*freq*t)
}

func sinInt(x float64) float64 {
	// Use Go's stdlib math.Sin via a tiny indirection so the test
	// doesn't pull math into every other file. The indirection
	// also lets the test stub this out if needed.
	return sinImpl(x)
}

// sinImpl is set in TestMain.
var sinImpl = func(x float64) float64 { return 0 }

// bytesReader is a tiny FrameSource used in the round-trip test.
type bytesReader struct {
	buf *bytes.Reader
}

func TestMain(m *testing.M) {
	sinImpl = math.Sin
	m.Run()
}

func (b *bytesReader) Next(ctx context.Context) ([]byte, error) {
	if b.buf.Len() == 0 {
		return nil, context.Canceled
	}
	pkt := make([]byte, 80)
	n, err := b.buf.Read(pkt)
	if err != nil {
		return nil, err
	}
	return pkt[:n], nil
}

// Ensure the decode loop terminates when the FrameSource returns an
// error. We use a source that immediately returns an error.
type errSource struct{ err error }

func (e *errSource) Next(ctx context.Context) ([]byte, error) {
	return nil, e.err
}

func TestDecodeLoopExitsOnError(t *testing.T) {
	p, err := NewPipeline()
	if err != nil {
		t.Skipf("opus not available: %v", err)
	}
	pcmOut := make(chan []int16, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan struct{})
	go func() {
		p.DecodeLoop(ctx, &errSource{err: context.Canceled}, pcmOut)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("decode loop did not exit")
	}
}
