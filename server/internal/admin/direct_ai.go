package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
)

type SpeechRecognizer interface {
	Transcribe(ctx context.Context, oggOpus []byte) (string, error)
}

type ChatCompleter interface {
	Complete(ctx context.Context, messages []chatMessage) (string, error)
}

type VisionAnalyzer interface {
	Analyze(ctx context.Context, question string, contentType string, image []byte) (string, error)
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type openAITranscriber struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

type openAIChatClient struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

func newOpenAITranscriber(cfg Config) SpeechRecognizer {
	if cfg.GoASRAPIKey == "" {
		return nil
	}
	return &openAITranscriber{
		url:    cfg.GoASRURL,
		apiKey: cfg.GoASRAPIKey,
		model:  cfg.GoASRModel,
		client: &http.Client{Timeout: cfg.GoASRTimeout},
	}
}

func newGoLLMClient(cfg Config) ChatCompleter {
	if cfg.GoLLMAPIKey == "" {
		return nil
	}
	return &openAIChatClient{
		url:    cfg.GoLLMURL,
		apiKey: cfg.GoLLMAPIKey,
		model:  cfg.GoLLMModel,
		client: &http.Client{Timeout: cfg.GoLLMTimeout},
	}
}

func newGoVisionClient(cfg Config) VisionAnalyzer {
	if cfg.GoVLLMAPIKey == "" {
		return nil
	}
	return &openAIChatClient{
		url:    cfg.GoVLLMURL,
		apiKey: cfg.GoVLLMAPIKey,
		model:  cfg.GoVLLMModel,
		client: &http.Client{Timeout: cfg.GoVLLMTimeout},
	}
}

func (c *openAITranscriber) Transcribe(ctx context.Context, oggOpus []byte) (string, error) {
	if c == nil || c.apiKey == "" {
		return "", errors.New("ASR is not configured")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", c.model); err != nil {
		return "", err
	}
	_ = writer.WriteField("response_format", "json")
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="speech.ogg"`)
	header.Set("Content-Type", "audio/ogg")
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(oggOpus); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		return "", fmt.Errorf("ASR request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(errorBody)))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	text := strings.TrimSpace(stringValue(payload["text"]))
	if text == "" {
		return "", errors.New("ASR returned empty text")
	}
	return text, nil
}

func (c *openAIChatClient) Complete(ctx context.Context, messages []chatMessage) (string, error) {
	if c == nil || c.apiKey == "" {
		return "", errors.New("LLM is not configured")
	}
	payload := map[string]any{
		"model":       c.model,
		"messages":    messages,
		"temperature": 0.2,
		"max_tokens":  180,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 800))
		return "", fmt.Errorf("LLM request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(errorBody)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("LLM returned no choices")
	}
	answer := strings.TrimSpace(contentText(parsed.Choices[0].Message.Content))
	if answer == "" {
		return "", errors.New("LLM returned empty answer")
	}
	return answer, nil
}

func (c *openAIChatClient) Analyze(ctx context.Context, question string, contentType string, image []byte) (string, error) {
	if c == nil || c.apiKey == "" {
		return "", errors.New("vision model is not configured")
	}
	if question == "" {
		question = "请描述这张图片里的内容。"
	}
	dataURL := "data:" + normalizeImageContentType(contentType, "image/jpeg") + ";base64," + base64.StdEncoding.EncodeToString(image)
	return c.Complete(ctx, []chatMessage{
		{Role: "system", Content: "你是一个视觉助手。回答要简短、直接，适合通过语音播放。"},
		{Role: "user", Content: []map[string]any{
			{"type": "text", "text": question},
			{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
		}},
	})
}

func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if text := stringValue(m["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return stringValue(value)
	}
}
