package admin

import (
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"
)

const websocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

func (s *AdminServer) handleStreamWS(w http.ResponseWriter, r *http.Request, user map[string]any) {
	deviceID := r.URL.Query().Get("device_id")
	if deviceID == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || !strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "websocket upgrade required", http.StatusBadRequest)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "websocket unsupported", http.StatusInternalServerError)
		return
	}
	conn, bufrw, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	acceptBytes := sha1.Sum([]byte(key + websocketGUID))
	accept := base64.StdEncoding.EncodeToString(acceptBytes[:])
	_, _ = bufrw.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_, _ = bufrw.WriteString("Upgrade: websocket\r\n")
	_, _ = bufrw.WriteString("Connection: Upgrade\r\n")
	_, _ = bufrw.WriteString("Sec-WebSocket-Accept: " + accept + "\r\n\r\n")
	if err := bufrw.Flush(); err != nil {
		return
	}

	ch, unsubscribe := s.stream.subscribe(deviceID)
	defer unsubscribe()
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case event, ok := <-ch:
			if !ok || writeWebSocketJSON(conn, event) != nil {
				return
			}
		case <-ticker.C:
			if writeWebSocketJSON(conn, map[string]any{"type": "heartbeat", "ts": s.cfg.now().Unix()}) != nil {
				return
			}
		case <-r.Context().Done():
			return
		}
	}
}

func writeWebSocketJSON(conn net.Conn, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	header := []byte{0x81}
	switch {
	case len(body) < 126:
		header = append(header, byte(len(body)))
	case len(body) <= 0xffff:
		header = append(header, 126, byte(len(body)>>8), byte(len(body)))
	default:
		header = append(header, 127)
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(body)))
		header = append(header, length[:]...)
	}
	if _, err := conn.Write(header); err != nil {
		return err
	}
	_, err = conn.Write(body)
	return err
}
