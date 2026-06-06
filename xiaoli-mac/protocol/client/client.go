// Package client implements the device side of the xiaoli protocol.
//
// It wraps a WebSocket transport, sends the initial "hello" handshake,
// and exposes SendListen / SendAudio / SendAbort for the audio and
// control pipeline. Inbound frames are routed to a SendFunc supplied
// at Connect time, which is expected to forward them onto the main
// event loop (see app.Submit).
package client

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"xiaoli/mac/protocol"
	"xiaoli/mac/protocol/transport"
)

// xiaozhiPath is the WebSocket mount point the xiaoli-admin server
// exposes (see server/internal/admin/server.go: handleXiaozhiWebSocket).
const xiaozhiPath = "/xiaozhi/v1/"

// normalizeServerURL accepts a base ws(s)://host endpoint and appends
// the xiaozhi WebSocket path. URLs that already carry a path (e.g.
// tests pointing at /ws, or power users who want a custom route) are
// returned unchanged.
func normalizeServerURL(raw string) string {
	raw = strings.TrimRight(raw, "/")
	for _, scheme := range []string{"wss://", "ws://"} {
		if !strings.HasPrefix(raw, scheme) {
			continue
		}
		host := strings.TrimPrefix(raw, scheme)
		if strings.Contains(host, "/") {
			return raw
		}
		return scheme + host + xiaozhiPath
	}
	return raw
}

// Client owns the device WebSocket and a small state machine tracking
// whether the server has acknowledged the "hello" handshake.
type Client struct {
	ws *transport.Client

	deviceID string
	authKey  string

	helloAcked chan struct{}
	onHello    func(protocol.Envelope)
}

// Config builds the underlying transport.
type Config struct {
	URL      string
	DeviceID string
	AuthKey  string

	// OnHelloAck, if set, is called from a transport goroutine
	// after the server replies to the device hello. It carries the
	// raw JSON envelope, so the app can inspect audio_params,
	// session_id, etc. Errors are not fatal.
	OnHelloAck func(env protocol.Envelope)
}

// New returns a Client. Call Connect to open the socket.
func New(cfg Config) *Client {
	headers := http.Header{}
	headers.Set("Device-Id", cfg.DeviceID)
	if cfg.AuthKey != "" {
		headers.Set("Authorization", cfg.AuthKey)
	}
	c := &Client{
		deviceID:   cfg.DeviceID,
		authKey:    cfg.AuthKey,
		helloAcked: make(chan struct{}),
		onHello:    cfg.OnHelloAck,
	}
	ws := transport.New(transport.Config{
		URL:     normalizeServerURL(cfg.URL),
		Headers: headers,
		OnConnect: func() {
			if err := c.SendHello(); err != nil {
				log.Printf("[client] SendHello: %v", err)
			}
		},
	})
	c.ws = ws
	return c
}

// Connect blocks until ctx is cancelled. The first frame sent is
// {"type":"hello"}; the server's hello response is consumed silently.
// All other inbound frames are forwarded to onFrame.
//
// The helloAcked channel is closed as soon as the server confirms.
func (c *Client) Connect(ctx context.Context, onFrame transport.SendFunc) error {
	// We layer on top of transport.Client.Connect. To know when the
	// server hello arrives, we wrap the SendFunc.
	wrapped := func(f transport.Frame) {
		if !f.Binary {
			// Look for a hello from the server; close the channel
			// exactly once. We don't otherwise intercept frames.
			if c.consumeServerHello(f.Data) {
				return
			}
		}
		onFrame(f)
	}
	return c.ws.Connect(ctx, wrapped)
}

func (c *Client) consumeServerHello(payload []byte) bool {
	var env protocol.Envelope
	if err := jsonUnmarshal(payload, &env); err != nil {
		return false
	}
	if env.Type != "hello" {
		return false
	}
	select {
	case <-c.helloAcked:
		// already closed
	default:
		close(c.helloAcked)
		log.Printf("[client] server hello ok: session=%s audio=%v",
			env.SessionID, env.AudioParams)
	}
	if c.onHello != nil {
		c.onHello(env)
	}
	return true
}

// HelloAcked returns a channel that is closed once the server replies
// to our initial hello. Callers can use it to gate the audio pipeline.
func (c *Client) HelloAcked() <-chan struct{} { return c.helloAcked }

// SendHello sends {"type":"hello"} on the text channel. The server
// responds with its own hello describing the audio parameters.
func (c *Client) SendHello() error {
	return c.ws.SendText(map[string]any{"type": "hello"})
}

// SendListenStart asks the server to begin voice recognition.
// mode: "auto" (server-side VAD), "manual" (client-side), "realtime"
// (device sends interim "detect" frames).
func (c *Client) SendListenStart(mode string) error {
	if mode == "" {
		mode = "auto"
	}
	return c.ws.SendText(map[string]any{
		"type":  "listen",
		"state": "start",
		"mode":  mode,
	})
}

// SendListenStop ends the current voice turn.
func (c *Client) SendListenStop() error {
	return c.ws.SendText(map[string]any{
		"type":  "listen",
		"state": "stop",
	})
}

// SendListenDetect pushes an interim text result in realtime mode.
func (c *Client) SendListenDetect(text string) error {
	return c.ws.SendText(map[string]any{
		"type":  "listen",
		"state": "detect",
		"text":  text,
	})
}

// SendAbort tells the server to drop the current TTS/LLM turn.
func (c *Client) SendAbort() error {
	return c.ws.SendText(map[string]any{"type": "abort"})
}

// SendMCP forwards a JSON-RPC payload to the server.
func (c *Client) SendMCP(payload any) error {
	return c.ws.SendText(map[string]any{
		"type":    "mcp",
		"payload": payload,
	})
}

// SendAudio pushes a single OPUS packet.
func (c *Client) SendAudio(opus []byte) error {
	if len(opus) == 0 {
		return fmt.Errorf("ws: empty audio packet")
	}
	return c.ws.SendBinary(opus)
}

// Close terminates the connection.
func (c *Client) Close() { c.ws.Close() }
