package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"time"

	"xiaoli/mac/audio"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "usage: tts-play <recording.opus>\n")
		fmt.Fprintf(os.Stderr, "reads a recording with [2-byte big-endian length][frame data] format\n")
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "read: %v\n", err)
		os.Exit(1)
	}

	// Parse length-prefixed frames
	var frames [][]byte
	offset := 0
	for offset+2 <= len(data) {
		n := int(data[offset])<<8 | int(data[offset+1])
		offset += 2
		if n <= 0 || offset+n > len(data) {
			fmt.Fprintf(os.Stderr, "bad frame at offset %d: size=%d, remaining=%d\n", offset-2, n, len(data)-offset)
			break
		}
		frames = append(frames, data[offset:offset+n])
		offset += n
	}

	fmt.Printf("file: %s (%d bytes), %d frames\n", os.Args[1], len(data), len(frames))
	for i, f := range frames {
		fmt.Printf("  frame %2d: %4d bytes\n", i, len(f))
	}

	if len(frames) == 0 {
		return
	}

	// Decode with the production pipeline
	p, err := audio.NewPipeline()
	if err != nil {
		fmt.Fprintf(os.Stderr, "pipeline: %v\n", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	src := &frameSrc{frames: frames}
	pcmOut := make(chan []int16, len(frames)+5)

	go p.DecodeLoop(ctx, src, pcmOut)

	var allPCM []int16
	for i := 0; i < len(frames); i++ {
		select {
		case frame, ok := <-pcmOut:
			if !ok {
				fmt.Fprintf(os.Stderr, "decode loop closed early at frame %d\n", i)
				os.Exit(1)
			}
			allPCM = append(allPCM, frame...)
		case <-ctx.Done():
			fmt.Fprintf(os.Stderr, "decode timeout at frame %d\n", i)
			os.Exit(1)
		}
	}

	secs := float64(len(allPCM)) / 16000.0
	var sumSq float64
	for _, s := range allPCM {
		sumSq += float64(s) * float64(s)
	}
	rms := math.Sqrt(sumSq / float64(len(allPCM)))

	fmt.Printf("decoded: %d PCM samples (%.3fs @ 16kHz), RMS=%.1f\n", len(allPCM), secs, rms)
	if rms < 100 {
		fmt.Println("  ⚠️  near silence — audio content may be empty")
	} else {
		fmt.Println("  ✅  signal present — audio content looks valid")
	}
}

type frameSrc struct {
	frames [][]byte
	idx    int
}

func (s *frameSrc) Next(ctx context.Context) ([]byte, error) {
	if s.idx >= len(s.frames) {
		return nil, context.Canceled
	}
	pkt := s.frames[s.idx]
	s.idx++
	return pkt, nil
}
