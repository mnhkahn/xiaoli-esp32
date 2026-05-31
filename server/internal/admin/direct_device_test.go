package admin

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeVisionAnalyzer struct {
	answer string
}

func (f fakeVisionAnalyzer) Analyze(ctx context.Context, question string, contentType string, image []byte) (string, error) {
	return f.answer, nil
}

func TestDirectOTAResponsePointsDeviceAtGoWebSocket(t *testing.T) {
	cfg := testConfig()
	cfg.PublicBaseURL = "https://example.test"
	cfg.DeviceAuthKey = "device-token"
	srv := NewServer(cfg)
	req := httptest.NewRequest(http.MethodPost, "/xiaozhi/ota/", strings.NewReader(`{}`))
	req.Header.Set("Device-Id", "device-1")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	websocket := payload["websocket"].(map[string]any)
	if websocket["url"] != "wss://example.test/xiaozhi/v1/" {
		t.Fatalf("websocket url = %#v", websocket["url"])
	}
	if websocket["token"] != "device-token" {
		t.Fatalf("websocket token = %#v", websocket["token"])
	}
}

func TestDirectVisionExplainUsesGoVisionModel(t *testing.T) {
	cfg := testConfig()
	cfg.DirectDeviceServer = true
	cfg.DeviceAuthEnabled = false
	srv := NewServer(cfg)
	srv.deviceHub.vision = fakeVisionAnalyzer{answer: "画面里有一张桌子。"}
	req := httptest.NewRequest(http.MethodPost, "/mcp/vision/explain", strings.NewReader("jpeg-bytes"))
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Device-Id", "device-1")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["response"] != "画面里有一张桌子。" {
		t.Fatalf("response = %#v", payload["response"])
	}
	urls := srv.recentDeviceImageURLs("device-1", time.Unix(0, 0))
	if len(urls) != 1 {
		t.Fatalf("recent urls = %#v", urls)
	}
}

func readServerFrame(conn net.Conn) (byte, []byte, error) {
	header := make([]byte, 2)
	if _, err := io.ReadFull(conn, header); err != nil {
		return 0, nil, err
	}
	opcode := header[0] & 0x0f
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		extended := make([]byte, 2)
		if _, err := io.ReadFull(conn, extended); err != nil {
			return 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended))
	case 127:
		extended := make([]byte, 8)
		if _, err := io.ReadFull(conn, extended); err != nil {
			return 0, nil, err
		}
		length = binary.BigEndian.Uint64(extended)
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(conn, payload); err != nil {
		return 0, nil, err
	}
	return opcode, payload, nil
}
