package client

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"xiaoli/mac/protocol"
	"xiaoli/mac/protocol/transport"
)

// fakeServer plays back a fixed sequence of server-to-client messages
// on connect: hello, tts.start, tts.sentence_start, tts.stop. It
// also captures every message the client sends.
type fakeServer struct {
	upgrader websocket.Upgrader
	server   *httptest.Server

	mu       sync.Mutex
	sent     [][]byte
	received [][]byte
	playOn   bool
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	fs := &fakeServer{
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
	fs.server = httptest.NewServer(http.HandlerFunc(fs.serve))
	return fs
}

func (fs *fakeServer) URL() string {
	ws := strings.Replace(fs.server.URL, "http://", "ws://", 1)
	return ws + "/ws"
}

func (fs *fakeServer) close() { fs.server.Close() }

func (fs *fakeServer) play() {
	fs.mu.Lock()
	fs.playOn = true
	fs.mu.Unlock()
}

func (fs *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := fs.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Check Device-Id header.
	if r.Header.Get("Device-Id") == "" {
		return
	}

	// Read until we see a hello from the client, then play the
	// canned sequence.
	helloed := false
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		fs.mu.Lock()
		fs.received = append(fs.received, data)
		fs.mu.Unlock()
		if !helloed && mt == websocket.TextMessage {
			var env protocol.Envelope
			if json.Unmarshal(data, &env) == nil && env.Type == "hello" {
				helloed = true
				reply := map[string]any{
					"type":       "hello",
					"transport":  "websocket",
					"version":    1,
					"session_id": "test-session",
					"audio_params": map[string]any{
						"format":         "opus",
						"sample_rate":    16000,
						"channels":       1,
						"frame_duration": 60,
					},
				}
				body, _ := json.Marshal(reply)
				_ = conn.WriteMessage(websocket.TextMessage, body)
			}
		}
	}
}

func TestClientHelloHandshake(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.close()

	helloAcked := make(chan protocol.Envelope, 1)
	c := New(Config{
		URL:      fs.URL(),
		DeviceID: "test-device",
		AuthKey:  "test-auth",
		OnHelloAck: func(env protocol.Envelope) {
			helloAcked <- env
		},
	})

	frames := make(chan transport.Frame, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go c.Connect(ctx, func(f transport.Frame) { frames <- f })

	select {
	case env := <-helloAcked:
		if env.SessionID != "test-session" {
			t.Errorf("session id: %q", env.SessionID)
		}
		if env.AudioParams == nil || env.AudioParams.SampleRate != 16000 {
			t.Errorf("audio params: %+v", env.AudioParams)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("hello not acked")
	}
}

func TestClientSendListen(t *testing.T) {
	fs := newFakeServer(t)
	defer fs.close()

	acked := make(chan struct{}, 1)
	c := New(Config{
		URL:      fs.URL(),
		DeviceID: "test-device",
		OnHelloAck: func(protocol.Envelope) {
			select {
			case acked <- struct{}{}:
			default:
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Connect(ctx, func(transport.Frame) {})

	// Wait for the hello round-trip so the WS is definitely open.
	select {
	case <-acked:
	case <-time.After(2 * time.Second):
		t.Fatal("hello not acked")
	}

	if err := c.SendListenStart("auto"); err != nil {
		t.Fatalf("SendListenStart: %v", err)
	}
	if err := c.SendListenStop(); err != nil {
		t.Fatalf("SendListenStop: %v", err)
	}
	if err := c.SendAbort(); err != nil {
		t.Fatalf("SendAbort: %v", err)
	}
	if err := c.SendAudio([]byte{0xff, 0xfe}); err != nil {
		t.Fatalf("SendAudio: %v", err)
	}
	// Empty audio is rejected.
	if err := c.SendAudio(nil); err == nil {
		t.Error("expected error on empty audio")
	}

	// Allow the goroutine to deliver the last frame to the fake server.
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		fs.mu.Lock()
		n := len(fs.received)
		fs.mu.Unlock()
		if n >= 5 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	fs.mu.Lock()
	defer fs.mu.Unlock()
	if got := len(fs.received); got < 5 {
		t.Errorf("server received %d frames, want >= 5", got)
	}
}

func TestNormalizeServerURL(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"wss://xiaoli.cyeam.com", "wss://xiaoli.cyeam.com/xiaozhi/v1/"},
		{"wss://xiaoli.cyeam.com/", "wss://xiaoli.cyeam.com/xiaozhi/v1/"},
		{"ws://127.0.0.1:8004", "ws://127.0.0.1:8004/xiaozhi/v1/"},
		{"wss://example.com/xiaozhi/v1", "wss://example.com/xiaozhi/v1"},
		{"wss://example.com/xiaozhi/v1/", "wss://example.com/xiaozhi/v1"},
		{"wss://example.com/ws", "wss://example.com/ws"},
		{"http://not-ws", "http://not-ws"},
	}
	for _, tc := range cases {
		if got := normalizeServerURL(tc.in); got != tc.want {
			t.Errorf("normalizeServerURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
