package admin

import (
	"fmt"
	"net/http"
	"path"
	"strings"
)

func (s *AdminServer) deviceController() DeviceController {
	if s.cfg.DirectDeviceServer && s.deviceHub != nil {
		return s.deviceHub
	}
	return s.bridge
}

func (s *AdminServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *AdminServer) handleXiaozhiOTA(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	deviceID := strings.TrimSpace(r.Header.Get("Device-Id"))
	if deviceID != "" && s.deviceHub != nil && !s.deviceHub.deviceAllowed(deviceID) {
		http.Error(w, "device is not allowed", http.StatusForbidden)
		return
	}
	wsURL := strings.TrimRight(s.cfg.PublicBaseURL, "/") + "/xiaozhi/v1/"
	wsURL = strings.Replace(wsURL, "https://", "wss://", 1)
	wsURL = strings.Replace(wsURL, "http://", "ws://", 1)
	writeJSON(w, http.StatusOK, map[string]any{
		"server_time": map[string]any{
			"timestamp":       s.cfg.now().UnixMilli(),
			"timezone_offset": 480,
		},
		"websocket": map[string]any{
			"url":     wsURL,
			"token":   s.cfg.DeviceAuthKey,
			"version": 1,
		},
		"firmware": map[string]any{},
	})
}

func (s *AdminServer) handleXiaozhiWebSocket(w http.ResponseWriter, r *http.Request) {
	if s.deviceHub == nil {
		http.Error(w, "direct device server is not configured", http.StatusServiceUnavailable)
		return
	}
	s.deviceHub.HandleWebSocket(w, r)
}

func (s *AdminServer) handleDeviceAudio(w http.ResponseWriter, r *http.Request) {
	if s.audioStore == nil {
		http.NotFound(w, r)
		return
	}
	id := path.Base(r.URL.Path)
	record, ok := s.audioStore.get(id, r.URL.Query().Get("token"))
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", record.ContentType)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(record.Body)))
	_, _ = w.Write(record.Body)
}
