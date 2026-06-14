package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	larkEventTypeURLVerification = "url_verification"
	larkEventTypeMessageReceive  = "im.message.receive_v1"
)

type larkCallback struct {
	Schema string `json:"schema"`
	Header struct {
		EventID   string `json:"event_id"`
		EventType string `json:"event_type"`
		AppID     string `json:"app_id"`
		TenantKey string `json:"tenant_key"`
	} `json:"header"`
	Event json.RawMessage `json:"event"`

	Type      string `json:"type"`
	Challenge string `json:"challenge"`
}

type larkChallengeEvent struct {
	Challenge string `json:"challenge"`
}

type larkMessageEvent struct {
	Sender struct {
		SenderType string `json:"sender_type"`
		SenderID   struct {
			OpenID  string `json:"open_id"`
			UserID  string `json:"user_id"`
			UnionID string `json:"union_id"`
		} `json:"sender_id"`
	} `json:"sender"`
	Message struct {
		MessageID   string `json:"message_id"`
		ChatID      string `json:"chat_id"`
		MessageType string `json:"message_type"`
		Content     string `json:"content"`
	} `json:"message"`
}

type larkTextContent struct {
	Text string `json:"text"`
}

func (s *AdminServer) handleLarkEvents(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.LarkEnabled() {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2*1024*1024))
	if err != nil {
		http.Error(w, "read event failed", http.StatusBadRequest)
		return
	}
	var callback larkCallback
	if err := json.Unmarshal(raw, &callback); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if callback.Header.AppID != "" && callback.Header.AppID != s.cfg.LarkAppID {
		http.Error(w, "app id mismatch", http.StatusForbidden)
		return
	}
	switch callback.eventType() {
	case larkEventTypeURLVerification:
		challenge := callback.Challenge
		if challenge == "" && len(callback.Event) > 0 {
			var event larkChallengeEvent
			_ = json.Unmarshal(callback.Event, &event)
			challenge = event.Challenge
		}
		writeJSON(w, http.StatusOK, map[string]string{"challenge": challenge})
	case larkEventTypeMessageReceive:
		if callback.Header.EventID != "" && s.larkEventSeen(callback.Header.EventID) {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "duplicate": true})
			return
		}
		if err := s.handleLarkTextMessage(r.Context(), callback); err != nil {
			log.Printf("[lark] message handling failed: %v", err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": false})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	default:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "ignored": true})
	}
}

func (c larkCallback) eventType() string {
	if c.Header.EventType != "" {
		return c.Header.EventType
	}
	return c.Type
}

func (s *AdminServer) handleLarkTextMessage(ctx context.Context, callback larkCallback) error {
	var event larkMessageEvent
	if err := json.Unmarshal(callback.Event, &event); err != nil {
		return fmt.Errorf("decode message event: %w", err)
	}
	if event.Sender.SenderType == "bot" {
		return nil
	}
	if event.Message.MessageType != "text" {
		return nil
	}
	text := extractLarkText(event.Message.Content)
	if text == "" {
		return nil
	}
	senderID := event.senderID()
	if event.Message.ChatID == "" || senderID == "" || event.Message.MessageID == "" {
		return fmt.Errorf("message event missing chat, sender, or message id")
	}
	if s.conversation == nil {
		return fmt.Errorf("conversation pipeline is not configured")
	}
	reply, err := s.conversation.Run(ctx, LarkTextFactory{}.Build(event.Message.ChatID, senderID, text))
	if err != nil {
		return err
	}
	return s.newLarkClient().ReplyText(ctx, event.Message.MessageID, reply.Text)
}

func (e larkMessageEvent) senderID() string {
	if e.Sender.SenderID.OpenID != "" {
		return e.Sender.SenderID.OpenID
	}
	if e.Sender.SenderID.UserID != "" {
		return e.Sender.SenderID.UserID
	}
	return e.Sender.SenderID.UnionID
}

func extractLarkText(content string) string {
	var payload larkTextContent
	if err := json.Unmarshal([]byte(content), &payload); err != nil {
		return ""
	}
	return strings.TrimSpace(payload.Text)
}

func (s *AdminServer) larkEventSeen(eventID string) bool {
	if eventID == "" {
		return false
	}
	now := s.cfg.now()
	s.larkMu.Lock()
	defer s.larkMu.Unlock()
	for id, seenAt := range s.larkEvents {
		if now.Sub(seenAt) > time.Hour {
			delete(s.larkEvents, id)
		}
	}
	if _, ok := s.larkEvents[eventID]; ok {
		return true
	}
	s.larkEvents[eventID] = now
	return false
}

type LarkClient struct {
	appID      string
	appToken   string
	httpClient *http.Client
}

func (s *AdminServer) newLarkClient() *LarkClient {
	return &LarkClient{
		appID:      s.cfg.LarkAppID,
		appToken:   s.cfg.LarkAppToken,
		httpClient: s.httpClient,
	}
}

func (c *LarkClient) ReplyText(ctx context.Context, messageID string, text string) error {
	token, err := c.tenantAccessToken(ctx)
	if err != nil {
		return err
	}
	content, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]string{
		"msg_type": "text",
		"content":  string(content),
	})
	if err != nil {
		return err
	}
	endpoint := "https://open.larksuite.com/open-apis/im/v1/messages/" + url.PathEscape(messageID) + "/reply"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("lark reply failed: %d %s", resp.StatusCode, string(raw))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if code, _ := int64Value(payload["code"]); code != 0 {
		return fmt.Errorf("lark reply failed: %v", payload)
	}
	return nil
}

func (c *LarkClient) tenantAccessToken(ctx context.Context) (string, error) {
	requestBody, err := json.Marshal(map[string]string{"app_id": c.appID, "app_secret": c.appToken})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://open.larksuite.com/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(requestBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return "", fmt.Errorf("lark tenant_access_token failed: %d %s", resp.StatusCode, string(raw))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if code, _ := int64Value(payload["code"]); code != 0 {
		return "", fmt.Errorf("lark tenant_access_token failed: %v", payload)
	}
	token := stringValue(payload["tenant_access_token"])
	if token == "" {
		return "", fmt.Errorf("lark tenant_access_token missing")
	}
	return token, nil
}
