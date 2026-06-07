// mac-recv is a headless e2e receiver used to byte-diff the Mac
// client's TTS receive path against a known-good fixture, without
// needing the Fyne GUI. It uses the same xiaoli/mac/protocol/client
// package the production app uses, so the receive path is the real
// one (not a re-implementation).
//
// Usage:
//
//	xiaoli-mock-server -preload-opus-dir <fixture> &
//	mac-recv -server ws://localhost:8080 -out /tmp/mac-recv.opus
//
//	# then SHA256-compare:
//	sha256sum /tmp/mac-recv.opus   <-- the 18 frames concatenated
//	cat <fixture>/*.opus | sha256sum
//
// Or use -compare-dir to do the comparison in-process and exit
// non-zero on mismatch.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"time"

	"xiaoli/mac/protocol/client"
	"xiaoli/mac/protocol/transport"
)

func main() {
	server := flag.String("server", "ws://localhost:8080", "server base URL (path auto-appended)")
	deviceID := flag.String("device-id", "mac-recv-001", "Device-Id header")
	out := flag.String("out", "/tmp/mac-recv.opus", "raw concatenated OPUS output file")
	compareDir := flag.String("compare-dir", "", "if set, byte-compare output against 000.opus..NNN.opus in this dir")
	timeoutSec := flag.Int("timeout", 15, "exit after this many seconds (network is otherwise idle)")
	listenMic := flag.Bool("listen-mic", true, "send listen start + a few silence mic frames before listen stop (recommended for mock server)")
	flag.Parse()

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(*timeoutSec)*time.Second)
	defer cancel()

	outFile, err := os.Create(*out)
	if err != nil {
		log.Fatalf("create %s: %v", *out, err)
	}
	defer outFile.Close()

	// Counters
	var (
		binaryCount int
		totalBytes  int
		hash        = sha256.New()
		allText     []string
		gotTTSStop  bool
	)
	allText = append(allText, "")

	onFrame := func(f transport.Frame) {
		if !f.Binary {
			allText = append(allText, fmt.Sprintf("  t=%s text: %s", time.Now().Format("15:04:05.000"), truncate(string(f.Data), 100)))
			if containsTTSStop(f.Data) {
				log.Printf("[recv] got tts.stop after %d binary frames", binaryCount)
				gotTTSStop = true
			}
			return
		}
		binaryCount++
		totalBytes += len(f.Data)
		hash.Write(f.Data)
		if _, err := outFile.Write(f.Data); err != nil {
			log.Printf("[recv] write frame %d: %v", binaryCount, err)
		}
	}

	u, _ := url.Parse(*server)
	host := u.Host
	if host == "" {
		// ws://localhost:8080 → "localhost:8080"
		host = u.Opaque
		if host == "" {
			host = u.Path
		}
	}
	log.Printf("[recv] connecting to %s device=%s", *server, *deviceID)
	c := client.New(client.Config{
		URL:      *server,
		DeviceID: *deviceID,
	})

	netCtx, netCancel := context.WithCancel(ctx)
	defer netCancel()
	netDone := make(chan error, 1)
	go func() {
		netDone <- c.Connect(netCtx, onFrame)
	}()

	// Wait for hello to complete, then drive a listen cycle if
	// requested. The mock server's `listen stop` triggers the
	// canned TTS playback (echo or preload).
	select {
	case <-c.HelloAcked():
		log.Printf("[recv] hello acked")
	case <-time.After(10 * time.Second):
		log.Fatalf("[recv] timeout waiting for server hello")
	}

	if *listenMic {
		if err := c.SendListenStart("auto"); err != nil {
			log.Fatalf("[recv] listen start: %v", err)
		}
		log.Printf("[recv] sent listen start")

		// Send a few silence Opus frames (30 bytes each is the
		// minimum valid Opus packet). Mock server's echo mode
		// would just bounce these back, but with -preload-opus-dir
		// set on the server these are irrelevant — the canned
		// frames are what's sent.
		silence := makeSilenceOpus(30)
		for i := 0; i < 3; i++ {
			if err := c.SendAudio(silence); err != nil {
				log.Printf("[recv] send silence #%d: %v", i, err)
			}
			time.Sleep(60 * time.Millisecond)
		}

		// Brief pause before stop so server's per-frame pacing
		// has time to settle (mock uses 60ms between echo frames).
		time.Sleep(200 * time.Millisecond)
		if err := c.SendListenStop(); err != nil {
			log.Fatalf("[recv] listen stop: %v", err)
		}
		log.Printf("[recv] sent listen stop, waiting for TTS playback")
	}

	// Wait for tts.stop (or the ctx timeout).
waitLoop:
	for {
		if gotTTSStop {
			break waitLoop
		}
		select {
		case <-ctx.Done():
			log.Printf("[recv] ctx timeout waiting for tts.stop; have %d frames", binaryCount)
			break waitLoop
		case <-time.After(100 * time.Millisecond):
			// poll
		}
	}

	// Give the writer a moment to flush.
	time.Sleep(200 * time.Millisecond)
	netCancel()
	<-netDone

	sum := hex.EncodeToString(hash.Sum(nil))
	log.Printf("[recv] done: %d binary frames, %d bytes, sha256=%s",
		binaryCount, totalBytes, sum)
	log.Printf("[recv] text events received:")
	for _, t := range allText {
		if t != "" {
			log.Print(t)
		}
	}

	if *compareDir != "" {
		ok, info := compareWithDir(*out, *compareDir)
		if !ok {
			log.Fatalf("[recv] MISMATCH: %s", info)
		}
		log.Printf("[recv] OK: %s", info)
	}
}

// makeSilenceOpus returns a minimal valid 1-frame CELT-silence Opus
// packet. The contents don't matter for the round-trip test; the
// goal is to give the mock server something to record as a "mic
// frame" so its listen window is non-empty.
func makeSilenceOpus(n int) []byte {
	// 30 bytes of zeros is the minimum Opus packet (CN/voice
	// switch at low bitrate); libopus decodes it to silence.
	out := make([]byte, n)
	return out
}

func containsTTSStop(data []byte) bool {
	// Cheap string match; the JSON is small.
	return binaryContains(data, []byte(`"tts"`)) && binaryContains(data, []byte(`"stop"`))
}

func binaryContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// compareWithDir reads 000.opus..NNN.opus from dir, sorts by name,
// concatenates their bytes, and SHA256+length compares against the
// receiver's output. Returns (true, summary) on match.
func compareWithDir(outPath, dir string) (bool, string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, fmt.Sprintf("read dir %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".opus" {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return false, fmt.Sprintf("no .opus files in %s", dir)
	}

	// SHA256 of the receiver output for length+byte check.
	recvData, err := os.ReadFile(outPath)
	if err != nil {
		return false, fmt.Sprintf("read %s: %v", outPath, err)
	}
	recvHash := sha256.Sum256(recvData)

	// SHA256 of the concatenated expected frames.
	wantHash := sha256.New()
	wantTotal := 0
	for _, n := range names {
		data, err := os.ReadFile(filepath.Join(dir, n))
		if err != nil {
			return false, fmt.Sprintf("read %s: %v", n, err)
		}
		wantHash.Write(data)
		wantTotal += len(data)
	}
	wantSum := hex.EncodeToString(wantHash.Sum(nil))

	if len(recvData) != wantTotal {
		return false, fmt.Sprintf("byte count: got %d, want %d (sha256 got=%s want=%s)",
			len(recvData), wantTotal, hex.EncodeToString(recvHash[:]), wantSum)
	}
	if recvHash != [32]byte(wantHash.Sum(nil)) {
		return false, fmt.Sprintf("sha256: got=%s want=%s", hex.EncodeToString(recvHash[:]), wantSum)
	}
	return true, fmt.Sprintf("%d bytes, sha256=%s, matches %d frames in %s",
		wantTotal, wantSum, len(names), dir)
}
