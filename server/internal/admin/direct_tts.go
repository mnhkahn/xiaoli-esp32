package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type SpeechSynthesizer interface {
	Synthesize(ctx context.Context, text string) (contentType string, body []byte, err error)
}

type httpSpeechSynthesizer struct {
	url            string
	apiKey         string
	model          string
	voice          string
	responseFormat string
	client         *http.Client
}

func newHTTPSpeechSynthesizer(cfg Config, client *http.Client) SpeechSynthesizer {
	if cfg.GoTTSAPIKey == "" {
		return nil
	}
	if client == nil {
		client = &http.Client{Timeout: cfg.GoTTSTimeout}
	}
	return &httpSpeechSynthesizer{
		url:            cfg.GoTTSURL,
		apiKey:         cfg.GoTTSAPIKey,
		model:          cfg.GoTTSModel,
		voice:          cfg.GoTTSVoice,
		responseFormat: strings.ToLower(strings.TrimSpace(cfg.GoTTSResponseFormat)),
		client:         client,
	}
}

func (s *httpSpeechSynthesizer) Synthesize(ctx context.Context, text string) (string, []byte, error) {
	if s == nil || s.apiKey == "" {
		return "", nil, errors.New("TTS is not configured")
	}
	if s.responseFormat != "opus" && s.responseFormat != "ogg" {
		return "", nil, fmt.Errorf("Go TTS must return Ogg Opus audio; set XIAOLI_GO_TTS_RESPONSE_FORMAT=opus, got %q", s.responseFormat)
	}
	payload := map[string]any{
		"model":           s.model,
		"voice":           s.voice,
		"input":           text,
		"response_format": "opus",
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.url, bytes.NewReader(body))
	if err != nil {
		return "", nil, err
	}
	req.Header.Set("Authorization", "Bearer "+s.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		return "", nil, fmt.Errorf("TTS request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(errorBody)))
	}
	audio, err := io.ReadAll(io.LimitReader(resp.Body, 2*1024*1024))
	if err != nil {
		return "", nil, err
	}
	if len(audio) == 0 {
		return "", nil, errors.New("TTS returned empty audio")
	}
	contentType := resp.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "audio/ogg"
	}
	return normalizeOggContentType(contentType), audio, nil
}

type audioRecord struct {
	ID          string
	Token       string
	ContentType string
	Body        []byte
	CreatedAt   time.Time
	ExpiresAt   time.Time
}

type audioStore struct {
	mu      sync.Mutex
	records map[string]audioRecord
	now     func() time.Time
	maxAge  time.Duration
}

func newAudioStore(now func() time.Time) *audioStore {
	if now == nil {
		now = time.Now
	}
	return &audioStore{
		records: map[string]audioRecord{},
		now:     now,
		maxAge:  10 * time.Minute,
	}
}

func (s *audioStore) put(contentType string, body []byte) audioRecord {
	now := s.now()
	record := audioRecord{
		ID:          randomToken(18),
		Token:       randomToken(18),
		ContentType: normalizeOggContentType(contentType),
		Body:        append([]byte(nil), body...),
		CreatedAt:   now,
		ExpiresAt:   now.Add(s.maxAge),
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, item := range s.records {
		if item.ExpiresAt.Before(now) {
			delete(s.records, id)
		}
	}
	s.records[record.ID] = record
	return record
}

func (s *audioStore) get(id string, token string) (audioRecord, bool) {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[id]
	if !ok || record.Token == "" || record.Token != token || record.ExpiresAt.Before(now) {
		return audioRecord{}, false
	}
	return record, true
}

func normalizeOggContentType(value string) string {
	value = strings.ToLower(strings.TrimSpace(strings.Split(value, ";")[0]))
	switch value {
	case "audio/ogg", "audio/opus", "application/ogg":
		return "audio/ogg"
	default:
		return "audio/ogg"
	}
}
