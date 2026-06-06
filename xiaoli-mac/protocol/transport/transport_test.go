package transport

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
)

// echoServer accepts a WS connection, reads each frame, and replies
// with the same payload (text echoed as JSON, binary echoed as-is).
type echoServer struct {
	upgrader websocket.Upgrader
	server   *httptest.Server
	frames   []Frame
	mu       sync.Mutex
}

func newEchoServer(t *testing.T) *echoServer {
	t.Helper()
	es := &echoServer{
		upgrader: websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }},
	}
	es.server = httptest.NewServer(http.HandlerFunc(es.serve))
	return es
}

func (es *echoServer) URL() string {
	ws := strings.Replace(es.server.URL, "http://", "ws://", 1)
	return ws + "/ws"
}

func (es *echoServer) close() { es.server.Close() }

func (es *echoServer) serve(w http.ResponseWriter, r *http.Request) {
	conn, err := es.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		es.mu.Lock()
		es.frames = append(es.frames, Frame{Binary: mt == websocket.BinaryMessage, Data: data})
		es.mu.Unlock()
		var reply []byte
		switch mt {
		case websocket.TextMessage:
			// Wrap in {"echo":...} so the client can verify it
			// came through transport intact.
			wrapper, _ := json.Marshal(map[string]any{"echo": string(data)})
			reply = wrapper
		case websocket.BinaryMessage:
			reply = data
		}
		_ = conn.WriteMessage(mt, reply)
	}
}

func TestClientEcho(t *testing.T) {
	es := newEchoServer(t)
	defer es.close()

	c := New(Config{URL: es.URL()})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	received := make(chan Frame, 4)
	go func() {
		_ = c.Connect(ctx, func(f Frame) { received <- f })
	}()

	// Wait for connection to establish.
	time.Sleep(50 * time.Millisecond)

	if err := c.SendText(map[string]any{"type": "hello"}); err != nil {
		t.Fatalf("SendText: %v", err)
	}
	if err := c.SendBinary([]byte{0x01, 0x02, 0x03, 0x04}); err != nil {
		t.Fatalf("SendBinary: %v", err)
	}

	// Expect two echo replies.
	for i := 0; i < 2; i++ {
		select {
		case f := <-received:
			if !f.Binary && !strings.Contains(string(f.Data), "echo") {
				t.Errorf("text frame missing echo wrapper: %s", f.Data)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("missing reply %d", i)
		}
	}
}

func TestClientOnConnect(t *testing.T) {
	es := newEchoServer(t)
	defer es.close()

	called := make(chan struct{}, 1)
	c := New(Config{
		URL: es.URL(),
		OnConnect: func() {
			select {
			case called <- struct{}{}:
			default:
			}
		},
	})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go c.Connect(ctx, func(Frame) {})

	select {
	case <-called:
		// good
	case <-time.After(2 * time.Second):
		t.Fatal("OnConnect was not called")
	}
}
