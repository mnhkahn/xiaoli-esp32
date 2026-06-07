package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTTSSynthesizeSavesSampleForLocalPlayback(t *testing.T) {
	cfg := testConfig()
	cfg.GoTTSURL = env("XIAOLI_GO_TTS_URL", "https://api.siliconflow.cn/v1/audio/speech")
	cfg.GoTTSAPIKey = env("XIAOLI_GO_TTS_API_KEY", env("SILICONFLOW_API_KEY", ""))
	cfg.GoTTSModel = env("XIAOLI_GO_TTS_MODEL", env("SILICONFLOW_TTS_MODEL", cfg.GoTTSModel))
	cfg.GoTTSVoice = env("XIAOLI_GO_TTS_VOICE", env("SILICONFLOW_TTS_VOICE", cfg.GoTTSVoice))
	cfg.GoTTSResponseFormat = env("XIAOLI_GO_TTS_RESPONSE_FORMAT", "opus")
	cfg.GoTTSTimeout = 30 * time.Second

	if cfg.GoTTSAPIKey == "" {
		t.Skip("skipping real TTS test: set XIAOLI_GO_TTS_API_KEY or SILICONFLOW_API_KEY to run")
	}

	synth := newHTTPSpeechSynthesizer(cfg, &http.Client{Timeout: cfg.GoTTSTimeout})
	if synth == nil {
		t.Fatal("synthesizer was not constructed despite API key being set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), cfg.GoTTSTimeout)
	defer cancel()

	contentType, body, err := synth.Synthesize(ctx, "你好宝宝")
	if err != nil {
		t.Fatalf("Synthesize returned error: %v", err)
	}
	if len(body) == 0 {
		t.Fatal("Synthesize returned empty body")
	}
	if !strings.HasPrefix(contentType, "audio/") {
		t.Fatalf("contentType = %q, want audio/*", contentType)
	}
	if !bytes.HasPrefix(body, []byte("OggS")) {
		t.Fatalf("body is not an Ogg container (first 4 bytes = %q)", body[:4])
	}

	outputPath := filepath.Join(t.TempDir(), "tts_sample.ogg")
	if err := os.WriteFile(outputPath, body, 0o644); err != nil {
		t.Fatalf("write sample: %v", err)
	}

	t.Logf("Saved %d bytes of %s to %s", len(body), contentType, outputPath)

	persistedPath, err := filepath.Abs(filepath.Join("testdata", "tts_sample.ogg"))
	if err == nil {
		if err := os.MkdirAll(filepath.Dir(persistedPath), 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(persistedPath, body, 0o644); err != nil {
			t.Fatalf("write persistent sample: %v", err)
		}
		t.Logf("Also saved persistent copy to %s", persistedPath)
	}
}

func TestTTSSynthesizeRejectsNonOpusResponseFormat(t *testing.T) {
	cfg := testConfig()
	cfg.GoTTSAPIKey = "test-key"
	cfg.GoTTSResponseFormat = "mp3"

	synth, ok := newHTTPSpeechSynthesizer(cfg, nil).(*httpSpeechSynthesizer)
	if !ok {
		t.Fatal("expected *httpSpeechSynthesizer")
	}

	_, _, err := synth.Synthesize(context.Background(), "你好宝宝")
	if err == nil {
		t.Fatal("expected error for mp3 response format")
	}
	if !strings.Contains(err.Error(), "Ogg Opus") {
		t.Fatalf("error %q should mention Ogg Opus", err)
	}
}

func TestTTSSynthesizePostsExpectedRequestAndDecodesBody(t *testing.T) {
	var capturedAuth string
	var capturedPayload map[string]any
	oggBody := []byte("OggS" + "fake-opus-payload")

	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		capturedAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&capturedPayload); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": {"audio/ogg"}},
			Body:       io.NopCloser(bytes.NewReader(oggBody)),
		}, nil
	})}

	cfg := testConfig()
	cfg.GoTTSAPIKey = "test-key"
	cfg.GoTTSModel = "test-model"
	cfg.GoTTSVoice = "test-voice"
	cfg.GoTTSResponseFormat = "opus"

	synth := newHTTPSpeechSynthesizer(cfg, httpClient).(*httpSpeechSynthesizer)

	contentType, body, err := synth.Synthesize(context.Background(), "你好宝宝")
	if err != nil {
		t.Fatalf("Synthesize returned error: %v", err)
	}
	if capturedAuth != "Bearer test-key" {
		t.Fatalf("Authorization = %q, want Bearer test-key", capturedAuth)
	}
	if capturedPayload["model"] != "test-model" {
		t.Fatalf("model = %v, want test-model", capturedPayload["model"])
	}
	if capturedPayload["voice"] != "test-voice" {
		t.Fatalf("voice = %v, want test-voice", capturedPayload["voice"])
	}
	if capturedPayload["input"] != "你好宝宝" {
		t.Fatalf("input = %v, want 你好宝宝", capturedPayload["input"])
	}
	if capturedPayload["response_format"] != "opus" {
		t.Fatalf("response_format = %v, want opus", capturedPayload["response_format"])
	}
	if contentType != "audio/ogg" {
		t.Fatalf("contentType = %q, want audio/ogg", contentType)
	}
	if !bytes.Equal(body, oggBody) {
		t.Fatalf("body = %q, want %q", body, oggBody)
	}
}

func TestTTSSynthesizeSurfacesUpstreamErrorBody(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": {"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"error":"invalid api key"}`)),
		}, nil
	})}

	cfg := testConfig()
	cfg.GoTTSAPIKey = "test-key"
	cfg.GoTTSResponseFormat = "opus"

	synth := newHTTPSpeechSynthesizer(cfg, httpClient).(*httpSpeechSynthesizer)

	_, _, err := synth.Synthesize(context.Background(), "你好宝宝")
	if err == nil {
		t.Fatal("expected error for 401 response")
	}
	if !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), "invalid api key") {
		t.Fatalf("error %q should contain status and upstream body", err)
	}
}
