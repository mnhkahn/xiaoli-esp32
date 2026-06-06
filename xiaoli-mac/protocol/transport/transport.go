// Package transport is a thin WebSocket client. It is independent of
// the higher-level device protocol: callers pass text frames as JSON
// and binary frames as raw bytes. The transport handles the WS
// handshake, ping/pong, masking, and reconnection.
package transport

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Frame is one WebSocket message: either a Text JSON value or raw
// Binary payload.
type Frame struct {
	Binary bool
	Data   []byte // JSON-encoded for Text, raw bytes for Binary
}

// SendFunc is invoked by the transport for inbound frames. The
// callback is expected to be non-blocking: if the consumer needs to
// process the message slowly, it should hand it off to its own queue.
type SendFunc func(Frame)

// Config controls connection behaviour.
type Config struct {
	// URL is the full ws:// or wss:// endpoint, e.g.
	//   wss://xiaoli.cyeam.com/xiaozhi/v1/
	URL string

	// Headers are added to the upgrade request. The standard
	// Sec-WebSocket-* headers are set automatically.
	Headers http.Header

	// HandshakeTimeout caps the dial time.
	HandshakeTimeout time.Duration

	// PingInterval drives outbound pings. The server replies with a
	// pong; on miss, the connection is torn down and re-dialled.
	PingInterval time.Duration

	// PongTimeout is the maximum gap between ping and pong.
	PongTimeout time.Duration

	// ReconnectBackoff is the initial wait between reconnects. The
	// wait doubles on every failure up to ReconnectMax.
	ReconnectBackoff time.Duration
	ReconnectMax     time.Duration

	// OnConnect is called from a dedicated goroutine after each
	// successful dial, before the read loop starts. It is the right
	// place to send the protocol-level "hello" frame. Errors are
	// logged but do not abort the connection.
	OnConnect func()
}

// DefaultConfig fills in sensible defaults for any zero fields.
func DefaultConfig(c Config) Config {
	if c.HandshakeTimeout == 0 {
		c.HandshakeTimeout = 10 * time.Second
	}
	if c.PingInterval == 0 {
		c.PingInterval = 30 * time.Second
	}
	if c.PongTimeout == 0 {
		c.PongTimeout = 60 * time.Second
	}
	if c.ReconnectBackoff == 0 {
		c.ReconnectBackoff = time.Second
	}
	if c.ReconnectMax == 0 {
		c.ReconnectMax = 30 * time.Second
	}
	return c
}

// Client owns a single WebSocket connection. The Connect method runs
// the read loop in a background goroutine and reconnects on failure.
type Client struct {
	cfg Config

	mu      sync.Mutex
	conn    *websocket.Conn
	closed  bool
	writeMu sync.Mutex // gorilla/websocket requires serialized writes
}

// New returns a Client. Call Connect to actually open the socket.
func New(cfg Config) *Client {
	return &Client{cfg: DefaultConfig(cfg)}
}

// Connect blocks until ctx is cancelled, dialing the server and
// reconnecting on failure. Each inbound frame is delivered to send.
//
// This is the only entry point the caller needs: everything else
// (SendText, SendBinary, Close) is safe to call from any goroutine
// once Connect is running.
func (c *Client) Connect(ctx context.Context, send SendFunc) error {
	backoff := c.cfg.ReconnectBackoff
	for {
		if err := c.dialAndServe(ctx, send); err != nil {
			if errors.Is(err, context.Canceled) || c.isClosed() {
				return err
			}
			log.Printf("[ws] dial failed: %v (retry in %s)", err, backoff)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
			if backoff > c.cfg.ReconnectMax {
				backoff = c.cfg.ReconnectMax
			}
			continue
		}
		// Clean disconnect: reset backoff for the next attempt.
		backoff = c.cfg.ReconnectBackoff
	}
}

func (c *Client) isClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

func (c *Client) dialAndServe(ctx context.Context, send SendFunc) error {
	d := websocket.Dialer{HandshakeTimeout: c.cfg.HandshakeTimeout}
	conn, resp, err := d.DialContext(ctx, c.cfg.URL, c.cfg.Headers)
	if err != nil {
		if resp != nil {
			return fmt.Errorf("dial %s: %w (status %d)", c.cfg.URL, err, resp.StatusCode)
		}
		return fmt.Errorf("dial %s: %w", c.cfg.URL, err)
	}
	defer conn.Close()

	conn.SetReadDeadline(time.Now().Add(c.cfg.PongTimeout))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(c.cfg.PongTimeout))
	})

	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()

	log.Printf("[ws] connected to %s", c.cfg.URL)

	if c.cfg.OnConnect != nil {
		go func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("[ws] OnConnect panic: %v", r)
				}
			}()
			c.cfg.OnConnect()
		}()
	}

	// Ping loop.
	pingCtx, cancelPing := context.WithCancel(ctx)
	defer cancelPing()
	go c.pingLoop(pingCtx, conn)

	// Read loop.
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		switch mt {
		case websocket.TextMessage:
			send(Frame{Data: data})
		case websocket.BinaryMessage:
			send(Frame{Binary: true, Data: data})
		case websocket.CloseMessage:
			return errors.New("server closed connection")
		case websocket.PingMessage:
			// gorilla auto-replies with pong via the handler.
		case websocket.PongMessage:
			// deadline already extended by SetPongHandler.
		}
	}
}

func (c *Client) pingLoop(ctx context.Context, conn *websocket.Conn) {
	t := time.NewTicker(c.cfg.PingInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := conn.WriteMessage(websocket.PingMessage, nil)
			c.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// SendText writes a JSON-encoded text frame. The value is encoded
// inline so callers don't have to import encoding/json.
func (c *Client) SendText(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return c.send(websocket.TextMessage, data)
}

// SendBinary writes a raw binary frame (typically one OPUS packet).
func (c *Client) SendBinary(data []byte) error {
	return c.send(websocket.BinaryMessage, data)
}

func (c *Client) send(opcode int, data []byte) error {
	c.mu.Lock()
	conn := c.conn
	c.mu.Unlock()
	if conn == nil {
		return errors.New("ws: not connected")
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(opcode, data)
}

// Close shuts the client down. The Connect loop will exit.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}
