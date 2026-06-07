// opusdump reads a file of raw concatenated OPUS packets (one
// self-contained packet after another, no OGG container — what
// xiaoli-mac writes via record-tts and xiaoli-mock-server writes as
// tts_output_*.opus) and writes a 16kHz mono 16-bit PCM WAV.
//
// Packet boundaries are found by reading the TOC byte at each
// cursor and asking libopus how many samples a packet starting
// with that TOC should yield. The smallest prefix that decodes to
// exactly that count is the next packet.
package main

/*
#cgo CFLAGS: -I/usr/local/include/opus
#cgo LDFLAGS: -L/usr/local/lib -lopus
#include <opus.h>
*/
import "C"

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"os"
	"unsafe"

	"github.com/hraban/opus"
)

func main() {
	in := flag.String("in", "", "input .opus file (raw OPUS packet stream)")
	out := flag.String("out", "", "output .wav file")
	sampleRate := flag.Int("sr", 16000, "decoder sample rate")
	channels := flag.Int("ch", 1, "decoder channels (1=mono)")
	flag.Parse()
	if *in == "" || *out == "" {
		log.Fatalf("usage: opusdump -in file.opus -out file.wav")
	}

	raw, err := os.ReadFile(*in)
	if err != nil {
		log.Fatalf("read: %v", err)
	}
	log.Printf("input: %d bytes", len(raw))

	dec, err := opus.NewDecoder(*sampleRate, *channels)
	if err != nil {
		log.Fatalf("decoder: %v", err)
	}

	pcm := make([]int16, 5760) // 120ms at 48kHz, max OPUS frame
	var all []int16

	cursor := 0
	packets := 0
	hist := map[int]int{}
	for cursor < len(raw) {
		toc := raw[cursor]
		expected := samplesPerFrame(toc, *sampleRate)
		if expected <= 0 {
			// Garbage byte at the end; trim and stop.
			log.Printf("garbage TOC=0x%02x at cursor=%d, trimming %d bytes",
				toc, cursor, len(raw)-cursor)
			break
		}
		// Find the shortest prefix that decodes to exactly
		// `expected` samples.
		bound, ok := findPacketBoundary(dec, raw[cursor:], expected, pcm)
		if !ok {
			log.Printf("could not find boundary at cursor=%d (TOC=0x%02x, expected=%d samples)",
				cursor, toc, expected)
			break
		}
		all = append(all, pcm[:expected]...)
		hist[expected]++
		cursor += bound
		packets++
	}
	log.Printf("decoded %d packets, %d samples (%.2fs @ %dHz, ch=%d)",
		packets, len(all), float64(len(all))/float64(*sampleRate), *sampleRate, *channels)
	log.Printf("per-packet sample-count histogram: %v", hist)

	writeWAV(*out, all, *sampleRate, *channels)
	fmt.Printf("wrote %s\n", *out)
}

// samplesPerFrame calls libopus's TOC parser to get the frame size
// in samples for a packet starting with TOC byte `b`. Returns 0
// for unrecognised TOCs.
func samplesPerFrame(b byte, fs int) int {
	c := C.opus_packet_get_samples_per_frame((*C.uchar)(unsafe.Pointer(&b)), C.opus_int32(fs))
	return int(c)
}

// findPacketBoundary brute-forces the smallest prefix length n
// (1..len(buf)) such that opus_decode(buf[:n], ...) succeeds and
// returns exactly `expected` samples.
func findPacketBoundary(dec *opus.Decoder, buf []byte, expected int, pcm []int16) (int, bool) {
	max := len(buf)
	if max > 4000 {
		max = 4000
	}
	for n := 1; n <= max; n++ {
		got, err := dec.Decode(buf[:n], pcm)
		if err != nil {
			continue
		}
		if got == expected {
			return n, true
		}
	}
	return 0, false
}

func writeWAV(path string, samples []int16, sampleRate, channels int) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatalf("create: %v", err)
	}
	defer f.Close()

	dataSize := uint32(len(samples) * 2)
	f.Write([]byte("RIFF"))
	binary.Write(f, binary.LittleEndian, uint32(36+dataSize))
	f.Write([]byte("WAVE"))
	f.Write([]byte("fmt "))
	binary.Write(f, binary.LittleEndian, uint32(16))
	binary.Write(f, binary.LittleEndian, uint16(1))
	binary.Write(f, binary.LittleEndian, uint16(channels))
	binary.Write(f, binary.LittleEndian, uint32(sampleRate))
	binary.Write(f, binary.LittleEndian, uint32(sampleRate*channels*2))
	binary.Write(f, binary.LittleEndian, uint16(channels*2))
	binary.Write(f, binary.LittleEndian, uint16(16))
	f.Write([]byte("data"))
	binary.Write(f, binary.LittleEndian, dataSize)
	for _, s := range samples {
		binary.Write(f, binary.LittleEndian, s)
	}
}
