package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func (s *AdminServer) handleVisionExplain(w http.ResponseWriter, r *http.Request, body []byte, contentType string, deviceID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if deviceID == "" {
		http.Error(w, "missing device-id", http.StatusBadRequest)
		return
	}
	if s.deviceHub != nil && !s.deviceHub.deviceAllowed(deviceID) {
		http.Error(w, "device is not allowed", http.StatusForbidden)
		return
	}
	if s.cfg.DeviceAuthEnabled && s.deviceHub != nil && !s.deviceHub.deviceAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if s.deviceHub == nil || s.deviceHub.vision == nil {
		http.Error(w, "vision model is not configured", http.StatusServiceUnavailable)
		return
	}
	image, ok := s.extractVisionImage(contentType, body)
	if !ok {
		http.Error(w, "missing image", http.StatusBadRequest)
		return
	}
	if len(image.Body) > 2*1024*1024 {
		http.Error(w, "image too large", http.StatusRequestEntityTooLarge)
		return
	}
	fields := multipartFields(contentType, body)
	question := strings.TrimSpace(fields["question"])
	if question == "" {
		question = "请描述这张图片里的内容。"
	}
	s.storeVisionImage(deviceID, image.ContentType, image.Body)
	ctx, cancel := context.WithTimeout(r.Context(), s.cfg.GoVLLMTimeout+5*time.Second)
	defer cancel()
	answer, err := s.deviceHub.vision.Analyze(ctx, question, image.ContentType, image.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("vision model failed: %s", err), http.StatusBadGateway)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"response": answer,
		"text":     answer,
	})
}
