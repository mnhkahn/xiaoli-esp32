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

type fakeSpeechSynthesizer struct {
	contentType string
	body        []byte
}

func (f fakeSpeechSynthesizer) Synthesize(ctx context.Context, text string) (string, []byte, error) {
	return f.contentType, f.body, nil
}

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

func TestDirectSpeakSynthesizesOggAndCallsPlayURLTool(t *testing.T) {
	cfg := testConfig()
	cfg.PublicBaseURL = "https://example.test"
	stream := newStreamHub()
	audio := newAudioStore(cfg.now)
	hub := NewDeviceHub(cfg, stream, audio, nil, nil, nil, fakeSpeechSynthesizer{
		contentType: "audio/ogg",
		body:        []byte("ogg-opus-bytes"),
	})
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()
	session := hub.register("device-1", serverConn, "127.0.0.1")
	defer hub.unregister(session)

	urlCh := make(chan string, 1)
	ttsStates := make(chan string, 3)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			opcode, payload, err := readServerFrame(clientConn)
			if err != nil {
				return
			}
			if opcode != wsOpcodeText {
				continue
			}
			var envelope map[string]any
			if err := json.Unmarshal(payload, &envelope); err != nil {
				return
			}
			if envelope["type"] == "tts" {
				ttsStates <- stringValue(envelope["state"])
				if envelope["state"] == "stop" {
					return
				}
				continue
			}
			if envelope["type"] != "mcp" {
				continue
			}
			mcp := envelope["payload"].(map[string]any)
			id := int(mcp["id"].(float64))
			params := mcp["params"].(map[string]any)
			if params["name"] != "self.audio_speaker.play_ogg_url" {
				t.Errorf("tool name = %#v", params["name"])
			}
			args := params["arguments"].(map[string]any)
			url := stringValue(args["url"])
			urlCh <- url
			session.completeMCP(id, mcpCallResult{Result: true, Raw: "true"})
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result, err := hub.Speak(ctx, "device-1", "你好")
	if err != nil {
		t.Fatalf("Speak returned error: %v", err)
	}
	if result["ok"] != true {
		t.Fatalf("Speak result = %#v", result)
	}
	url := <-urlCh
	if !strings.HasPrefix(url, "https://example.test/xiaoli/audio/") || !strings.Contains(url, "token=") {
		t.Fatalf("audio url = %q", url)
	}
	if result["bytes"] != 14 {
		t.Fatalf("bytes = %#v, want 14", result["bytes"])
	}
	wantStates := []string{"start", "sentence_start", "stop"}
	for _, want := range wantStates {
		select {
		case got := <-ttsStates:
			if got != want {
				t.Fatalf("tts state = %q, want %q", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for tts state %q", want)
		}
	}
	clientConn.Close()
	<-done
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
