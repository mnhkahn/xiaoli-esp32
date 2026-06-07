// xiaoli-mock-server is a minimal local stand-in for the real
// xiaoli-admin WebSocket server. It speaks the same wire protocol
// (hello handshake, listen start/stop, OPUS audio frames, STT/LLM/
// TTS text responses) and is intended for audio-path debugging only.
//
// Echo mode (the default and only mode right now):
//
//  1. Client connects, sends "hello" → server replies with hello +
//     audio_params (opus / 16kHz / mono / 60ms).
//  2. Client sends "listen start" + binary OPUS frames → server
//     tees each frame into mic_input_<ts>_turn<N>.opus and a small
//     in-memory ring.
//  3. Client sends "listen stop" → server closes the mic file and
//     starts a canned TTS turn: tts.start → stt "我听到了" →
//     llm emotion "happy" → tts.sentence_start "这是回放" →
//     (echo the buffered OPUS frames back as binary, paced at
//     ~60ms per frame) → tts.stop. The same bytes are written to
//     tts_output_<ts>_turn<N>.opus.
//
// Diffing server's tts_output_*.opus against the client's
// --record-tts file isolates WS / encoder corruption end-to-end.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var (
	addr      = flag.String("addr", ":8080", "HTTP listen address")
	outDir    = flag.String("out", "./recordings", "directory for mic/tts .opus dumps")
	path      = flag.String("path", "/xiaozhi/v1/", "WebSocket mount path")
	paceMs    = flag.Int("pace-ms", 60, "delay between echoed OPUS frames (ms); 0 = no delay")
	cannedSTT = flag.String("stt", "我听到了你说的话", "STT text sent in the canned response")
	cannedTTS = flag.String("tts", "这是回放", "TTS sentence_start text in the canned response")
	cannedEm  = flag.String("emotion", "happy", "LLM emotion in the canned response")

	// preloadOpusDir, if non-empty, makes the server send pre-loaded
	// OPUS frames (000.opus, 001.opus, …) from this directory as the
	// canned TTS payload, instead of echoing the client's mic frames
	// back. Used to byte-diff the client's TTS receive path against
	// known-good fixtures (e.g. server/internal/admin/testdata/
	// mac_test_input/).
	preloadOpusDir = flag.String("preload-opus-dir", "",
		"directory of 000.opus..NNN.opus to send as canned TTS "+
			"(overrides mic echo; empty = echo mode)")
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

func main() {
	flag.Parse()

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		log.Fatalf("mkdir %s: %v", *outDir, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc(*path, handle)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	go func() {
		<-ctx.Done()
		log.Printf("[mock] shutting down")
		_ = srv.Close()
	}()

	log.Printf("[mock] listening on ws://%s%s, output -> %s", *addr, *path, *outDir)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("listen: %v", err)
	}
}

func handle(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[mock] upgrade: %v", err)
		return
	}
	defer conn.Close()
	s := newSession(conn, r.Header.Get("Device-Id"))
	s.run()
}

// session is the per-connection state. One session per WebSocket;
// mic and tts files are timestamped so concurrent turns (or two
// devices) don't clobber each other.
type session struct {
	conn   *websocket.Conn
	device string

	mu             sync.Mutex
	turn           int
	listening      bool
	micFile        *os.File
	ttsPath        string
	pending        [][]byte
	lastMicAt      time.Time
	currentTSLabel string
}

func newSession(conn *websocket.Conn, device string) *session {
	return &session{conn: conn, device: device}
}

func (s *session) run() {
	log.Printf("[mock] connected device=%s", s.device)
	defer log.Printf("[mock] disconnected device=%s", s.device)
	for {
		mt, data, err := s.conn.ReadMessage()
		if err != nil {
			if !isExpectedClose(err) {
				log.Printf("[mock] read: %v", err)
			}
			return
		}
		switch mt {
		case websocket.TextMessage:
			s.handleText(data)
		case websocket.BinaryMessage:
			s.handleBinary(data)
		default:
			// ping/pong/close are handled by gorilla.
		}
	}
}

func isExpectedClose(err error) bool {
	return websocket.IsCloseError(err,
		websocket.CloseNormalClosure,
		websocket.CloseGoingAway,
		websocket.CloseAbnormalClosure)
}

func (s *session) handleText(data []byte) {
	var env struct {
		Type  string `json:"type"`
		State string `json:"state"`
		Mode  string `json:"mode"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		log.Printf("[mock] json: %v", err)
		return
	}
	switch env.Type {
	case "hello":
		s.respondHello()
	case "listen":
		switch env.State {
		case "start":
			s.startListening(env.Mode)
		case "stop":
			s.stopListening()
		case "detect":
			log.Printf("[mock] listen detect (realtime interim)")
		default:
			log.Printf("[mock] listen state=%q", env.State)
		}
	case "abort":
		log.Printf("[mock] abort")
	default:
		log.Printf("[mock] unhandled text type=%s payload=%s", env.Type, string(data))
	}
}

func (s *session) respondHello() {
	resp := map[string]any{
		"type":        "hello",
		"session_id":  "mock-" + strconv.FormatInt(time.Now().UnixNano(), 36),
		"transport":   "websocket",
		"audio_params": map[string]any{
			"format":         "opus",
			"sample_rate":    16000,
			"channels":       1,
			"frame_duration": 60,
		},
	}
	if err := s.writeJSON(resp); err != nil {
		log.Printf("[mock] hello write: %v", err)
		return
	}
	log.Printf("[mock] hello sent")
}

func (s *session) startListening(mode string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.listening {
		return
	}
	s.turn++
	s.listening = true
	s.pending = s.pending[:0]
	s.lastMicAt = time.Now()
	s.currentTSLabel = time.Now().Format("20060102-150405")
	micPath := filepath.Join(*outDir,
		fmt.Sprintf("mic_input_%s_turn%02d.opus", s.currentTSLabel, s.turn))
	f, err := os.Create(micPath)
	if err != nil {
		log.Printf("[mock] create %s: %v", micPath, err)
		return
	}
	s.micFile = f
	s.ttsPath = filepath.Join(*outDir,
		fmt.Sprintf("tts_output_%s_turn%02d.opus", s.currentTSLabel, s.turn))
	log.Printf("[mock] listen start (mode=%s) -> %s", mode, micPath)
}

func (s *session) handleBinary(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.listening {
		log.Printf("[mock] stray OPUS frame (%d bytes) outside listen window", len(data))
		return
	}
	if s.micFile != nil {
		if _, err := s.micFile.Write(data); err != nil {
			log.Printf("[mock] mic write: %v", err)
		}
	}
	// Copy out of the WS read buffer; the network layer recycles it.
	cp := make([]byte, len(data))
	copy(cp, data)
	s.pending = append(s.pending, cp)
	s.lastMicAt = time.Now()
}

func (s *session) stopListening() {
	s.mu.Lock()
	if !s.listening {
		s.mu.Unlock()
		return
	}
	s.listening = false
	if s.micFile != nil {
		_ = s.micFile.Close()
		s.micFile = nil
	}
	frames := s.pending
	s.pending = nil
	ttsPath := s.ttsPath
	s.mu.Unlock()

	// If preload mode is on, replace the (typically empty for a
	// silent test) mic frames with the canned fixture.
	if *preloadOpusDir != "" {
		loaded, err := loadPreloadedOpus(*preloadOpusDir)
		if err != nil {
			log.Printf("[mock] preload from %s: %v", *preloadOpusDir, err)
			return
		}
		frames = loaded
		log.Printf("[mock] preload mode: %d OPUS frames from %s", len(frames), *preloadOpusDir)
	} else {
		log.Printf("[mock] listen stop, %d OPUS frames buffered, echoing", len(frames))
	}
	go s.echoTTS(frames, ttsPath)
}

// loadPreloadedOpus reads 000.opus, 001.opus, … from dir, stopping
// at the first missing index. Returns the frames in numeric order.
func loadPreloadedOpus(dir string) ([][]byte, error) {
	var out [][]byte
	for i := 0; i < 1000; i++ {
		path := filepath.Join(dir, fmt.Sprintf("%03d.opus", i))
		data, err := os.ReadFile(path)
		if err != nil {
			if i == 0 {
				return nil, fmt.Errorf("read %s: %w", path, err)
			}
			break
		}
		out = append(out, data)
	}
	return out, nil
}

func (s *session) echoTTS(frames [][]byte, ttsPath string) {
	ttsFile, err := os.Create(ttsPath)
	if err != nil {
		log.Printf("[mock] create tts file: %v", err)
		return
	}
	defer ttsFile.Close()

	mustSend := func(v any, label string) {
		if err := s.writeJSON(v); err != nil {
			log.Printf("[mock] %s: %v", label, err)
		}
	}

	mustSend(map[string]any{"type": "tts", "state": "start"}, "tts.start")
	mustSend(map[string]any{"type": "stt", "text": *cannedSTT}, "stt")
	mustSend(map[string]any{"type": "llm", "emotion": *cannedEm}, "llm")
	mustSend(map[string]any{
		"type":  "tts",
		"state": "sentence_start",
		"text":  *cannedTTS,
	}, "tts.sentence_start")

	// Echo buffered OPUS frames, paced at the server's frame
	// interval. With paceMs=0 we send as fast as possible (useful
	// for the diff, not for human ears).
	pacer := time.NewTicker(time.Duration(*paceMs) * time.Millisecond)
	defer pacer.Stop()
	for i, frame := range frames {
		if *paceMs > 0 {
			<-pacer.C
		}
		if err := s.conn.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			log.Printf("[mock] echo binary #%d: %v", i, err)
			return
		}
		if _, err := ttsFile.Write(frame); err != nil {
			log.Printf("[mock] tts file write: %v", err)
		}
	}

	mustSend(map[string]any{"type": "tts", "state": "stop"}, "tts.stop")
	log.Printf("[mock] echo done (%d frames) -> %s", len(frames), ttsPath)
}

func (s *session) writeJSON(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}
