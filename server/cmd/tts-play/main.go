// tts-play is a minimal WebSocket client for the xiaoli-admin direct
// device endpoint. It connects as a fake device, sends the protocol
// "hello" frame, and captures every inbound binary TTS audio frame.
// Each frame is decoded with libopus (16kHz mono, 60ms = 960 samples)
// and appended to a single PCM buffer; on exit the buffer is written
// out as a 16kHz mono 16-bit WAV file the user can play with afplay,
// ffplay, or any standard tool.
//
// Why a separate client? The Mac reference client (xiaoli-mac) brings
// fyne, PortAudio, MCP, wake-word and a state machine. When a §3
// (send order, pacing) or §4 (frame timing on the wire) bug shows up
// it's hard to tell which layer is at fault. tts-play is the
// thinnest possible "real client" — one goroutine, one decoder, one
// WAV writer — so a clean capture here pins §3 as the suspect
// instead of the Mac app.
//
// Usage:
//
//	# 1. start the tool (blocks until SIGINT or idle timeout)
//	go run ./cmd/tts-play -device-id tts-play-001 -out /tmp/cap.wav
//
//	# 2. in another shell, trigger TTS via the admin API
//	curl -X POST http://localhost:8004/admin/api/speak \
//	    -H "Content-Type: application/json" \
//	    -d '{"device_id":"tts-play-001","text":"你好宝宝"}'
//
//	# 3. wait for tts.stop, Ctrl-C, or the idle timeout
//	afplay /tmp/cap.wav
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	"gopkg.in/hraban/opus.v2"
)

const (
	clientSampleRate    = 16000
	clientChannels      = 1
	samplesPerFrame60ms = 960 // 16kHz * 60ms
)

func main() {
	url := flag.String("url", "ws://localhost:8004/xiaozhi/v1/", "xiaozhi WS endpoint")
	deviceID := flag.String("device-id", "tts-play-001", "Device-Id header value")
	auth := flag.String("auth", "", "Authorization header value (optional)")
	out := flag.String("out", "/tmp/tts-capture.wav", "output WAV path")
	verbose := flag.Bool("v", false, "log every binary frame's metadata to stderr")
	timeout := flag.Duration("idle-timeout", 5*time.Second, "exit after this much silence following the last frame")
	maxRun := flag.Duration("max-run", 60*time.Second, "hard cap on how long the tool will stay connected")
	flag.Parse()

	headers := http.Header{}
	headers.Set("Device-Id", *deviceID)
	if *auth != "" {
		headers.Set("Authorization", *auth)
	}

	conn, resp, err := websocket.DefaultDialer.Dial(*url, headers)
	if err != nil {
		if resp != nil {
			log.Fatalf("dial %s: %v (status %d)", *url, err, resp.StatusCode)
		}
		log.Fatalf("dial %s: %v", *url, err)
	}
	defer conn.Close()
	log.Printf("[tts-play] connected to %s as device %s", *url, *deviceID)

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"hello"}`)); err != nil {
		log.Fatalf("send hello: %v", err)
	}
	log.Printf("[tts-play] hello sent; waiting for tts.start")

	dec, err := opus.NewDecoder(clientSampleRate, clientChannels)
	if err != nil {
		log.Fatalf("opus decoder: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *maxRun)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigCh:
			log.Printf("[tts-play] interrupted")
		case <-ctx.Done():
		}
		cancel()
		_ = conn.Close()
	}()

	var (
		allPCM        []int16
		frameIdx      int
		ttsStartSeen  bool
		ttsStopSeenAt time.Time
		lastFrameAt   time.Time
	)
	pcmBuf := make([]int16, samplesPerFrame60ms)

	readLoop:
	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		mt, data, err := conn.ReadMessage()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				if ctx.Err() != nil {
					log.Printf("[tts-play] max-run reached, exiting")
					break
				}
				if ttsStopSeenAt.IsZero() {
					continue
				}
				if time.Since(lastFrameAt) >= *timeout {
					log.Printf("[tts-play] idle %.1fs after tts.stop, exiting",
						time.Since(lastFrameAt).Seconds())
					break
				}
				continue
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				log.Printf("[tts-play] server closed: %v", err)
				break
			}
			log.Printf("[tts-play] read error: %v", err)
			break
		}

		switch mt {
		case websocket.TextMessage:
			log.Printf("[tts-play] << text: %s", string(data))
			if bytes.Contains(data, []byte(`"tts"`)) && bytes.Contains(data, []byte(`"start"`)) {
				ttsStartSeen = true
				log.Printf("[tts-play] >>> tts.start seen; capturing binary frames")
			}
			if bytes.Contains(data, []byte(`"tts"`)) && bytes.Contains(data, []byte(`"stop"`)) {
				ttsStopSeenAt = time.Now()
				log.Printf("[tts-play] >>> tts.stop seen; will exit after %.1fs of silence",
					timeout.Seconds())
			}

		case websocket.BinaryMessage:
			if !ttsStartSeen {
				log.Printf("[tts-play] WARNING: binary frame received before tts.start "+
					"(len=%d) — dropping", len(data))
				continue
			}
			n, err := dec.Decode(data, pcmBuf)
			if err != nil {
				log.Printf("[tts-play] opus decode error on frame %d (bytes=%d): %v",
					frameIdx, len(data), err)
				continue
			}
			if n != samplesPerFrame60ms {
				log.Printf("[tts-play] WARNING: frame %d decoded to %d samples "+
					"(expected %d) — §3 / device contract violation",
					frameIdx, n, samplesPerFrame60ms)
			}
			allPCM = append(allPCM, pcmBuf[:n]...)
			frameIdx++
			lastFrameAt = time.Now()
			if *verbose {
				rms := computeRMS(pcmBuf[:n])
				fmt.Fprintf(os.Stderr,
					"[tts-play] binary frame=%d bytes=%d samples=%d rms=%.1f\n",
					frameIdx-1, len(data), n, rms)
			}

		case websocket.CloseMessage:
			log.Printf("[tts-play] server sent close")
			break readLoop
		}
	}

	dur := time.Duration(len(allPCM)) * time.Second / time.Duration(clientSampleRate)
	log.Printf("[tts-play] captured %d frames, %d samples (%.3fs @ %dHz, ch=%d)",
		frameIdx, len(allPCM), dur.Seconds(), clientSampleRate, clientChannels)
	if err := writeWAV(*out, allPCM, clientSampleRate, clientChannels); err != nil {
		log.Fatalf("write WAV %s: %v", *out, err)
	}
	log.Printf("[tts-play] wrote %s — afplay %s", *out, *out)
}

func writeWAV(path string, samples []int16, sampleRate, channels int) error {
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
	binary.LittleEndian.PutUint16(hdr[20:], 1) // PCM format
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

func computeRMS(samples []int16) float64 {
	if len(samples) == 0 {
		return 0
	}
	var sum float64
	for _, s := range samples {
		x := float64(s)
		sum += x * x
	}
	return sqrt(sum / float64(len(samples)))
}

// sqrt is hand-rolled so this binary doesn't pull in math just for
// one RMS print. 16 Newton iterations converges well past 24-bit
// audio precision.
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 16; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}
