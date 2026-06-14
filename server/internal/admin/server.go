package admin

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cloudwego/eino/schema"
	"github.com/redis/go-redis/v9"
)

const (
	sessionCookie = "xiaoli_admin_session"
	stateCookie   = "xiaoli_admin_state"
)

var hopByHopHeaders = map[string]struct{}{
	"connection":          {},
	"keep-alive":          {},
	"proxy-authenticate":  {},
	"proxy-authorization": {},
	"te":                  {},
	"trailer":             {},
	"transfer-encoding":   {},
	"upgrade":             {},
	"content-length":      {},
}

type AdminServer struct {
	cfg          Config
	signer       *signer
	bridge       *BridgeClient
	httpClient   *http.Client
	stream       *streamHub
	audioStore   *audioStore
	deviceHub    *DeviceHub
	conversation *ConversationPipeline
	memory       memoryReader
	agent        *EinoAgent
	imagesMu     sync.Mutex
	images       map[string]imageRecord
	imagesByDev  map[string][]string
	larkMu       sync.Mutex
	larkEvents   map[string]time.Time
	oidcMu       sync.Mutex
	oidc         *oidcConfig
	oidcFetcher  func() (oidcConfig, error)
}

type imageRecord struct {
	ID          string
	DeviceID    string
	ContentType string
	Body        []byte
	CreatedAt   time.Time
}

type oidcConfig struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
	EndSessionEndpoint    string `json:"end_session_endpoint"`
}

func NewServer(cfg Config) *AdminServer {
	if cfg.SessionMaxAge == 0 {
		cfg.SessionMaxAge = 7 * 24 * time.Hour
	}
	client := &http.Client{Timeout: 125 * time.Second}
	stream := newStreamHub()
	audioStore := newAudioStore(cfg.now)
	asr := newOpenAITranscriber(cfg)
	agent := newEinoAgent(cfg)
	var memory memoryReader
	if agent != nil && agent.memory != nil {
		memory = agent.memory
	} else {
		memory = newRedisMemory(cfg)
	}
	vision := newGoVisionClient(cfg)
	tts := newHTTPSpeechSynthesizer(cfg, nil)
	deviceHub := NewDeviceHub(cfg, stream, audioStore, asr, agent, vision, tts)
	if agent != nil {
		agent.SetHub(deviceHub)
	}
	conversation := newConversationPipeline(agent, deviceHub)
	deviceHub.conversation = conversation
	s := &AdminServer{
		cfg:          cfg,
		signer:       newSigner(cfg.SessionSecret, cfg.now),
		httpClient:   client,
		bridge:       NewBridgeClient(cfg.BridgeBaseURL, client),
		stream:       stream,
		audioStore:   audioStore,
		agent:        agent,
		deviceHub:    deviceHub,
		conversation: conversation,
		memory:       memory,
		images:       map[string]imageRecord{},
		imagesByDev:  map[string][]string{},
		larkEvents:   map[string]time.Time{},
	}
	s.oidcFetcher = s.fetchOIDCConfig
	return s
}

func (s *AdminServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/admin") {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
	}
	switch {
	case r.URL.Path == "/health":
		s.handleHealth(w, r)
	case r.URL.Path == "/xiaozhi/ota/" || r.URL.Path == "/xiaozhi/ota":
		s.handleXiaozhiOTA(w, r)
	case r.URL.Path == "/xiaozhi/v1/" || r.URL.Path == "/xiaozhi/v1":
		s.handleXiaozhiWebSocket(w, r)
	case r.URL.Path == "/lark/events":
		s.handleLarkEvents(w, r)
	case strings.HasPrefix(r.URL.Path, "/xiaoli/audio/"):
		s.handleDeviceAudio(w, r)
	case strings.HasPrefix(r.URL.Path, "/mcp/vision/"):
		s.handleVisionProxy(w, r)
	case r.URL.Path == "/admin/internal/stream/frame":
		s.handleInternalStreamFrame(w, r)
	case r.URL.Path == "/admin/internal/images/latest":
		s.handleInternalLatestImage(w, r)
	case r.URL.Path == "/admin" || r.URL.Path == "/admin/":
		s.handleIndex(w, r)
	case r.URL.Path == "/admin/memory":
		s.handleMemoryPage(w, r)
	case r.URL.Path == "/admin/login":
		s.handleLogin(w, r)
	case r.URL.Path == "/admin/callback":
		s.handleCallback(w, r)
	case r.URL.Path == "/admin/logout":
		s.handleLogout(w, r)
	case r.URL.Path == "/admin/api/me":
		s.withUser(w, r, s.handleMe)
	case r.URL.Path == "/admin/api/devices":
		s.withUser(w, r, s.handleDevices)
	case r.URL.Path == "/admin/api/tools":
		s.withUser(w, r, s.handleTools)
	case r.URL.Path == "/admin/api/call":
		s.withUser(w, r, s.handleCall)
	case r.URL.Path == "/admin/api/schedules":
		s.withUser(w, r, s.handleSchedules)
	case r.URL.Path == "/admin/api/memory":
		s.withUser(w, r, s.handleMemoryList)
	case r.URL.Path == "/admin/api/memory/detail":
		s.withUser(w, r, s.handleMemoryDetail)
	case r.URL.Path == "/admin/api/speak":
		s.withUser(w, r, s.handleSpeak)
	case r.URL.Path == "/admin/api/speak/stop":
		s.withUser(w, r, s.handleSpeakStop)
	case r.URL.Path == "/admin/api/snapshot":
		s.withUser(w, r, s.handleSnapshot)
	case r.URL.Path == "/admin/api/stream/start":
		s.withUser(w, r, s.handleStreamStart)
	case r.URL.Path == "/admin/api/stream/stop":
		s.withUser(w, r, s.handleStreamStop)
	case strings.HasPrefix(r.URL.Path, "/admin/api/images/"):
		s.withUser(w, r, s.handleImage)
	case r.URL.Path == "/admin/ws/stream":
		s.withUser(w, r, s.handleStreamWS)
	default:
		http.NotFound(w, r)
	}
}

func (s *AdminServer) handleIndex(w http.ResponseWriter, r *http.Request) {
	user := s.getUser(r)
	if user == nil {
		s.loginRedirect(w, r, safeReturnTo(r.URL.RequestURI()))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, dashboardHTML(user))
}

func (s *AdminServer) handleMemoryPage(w http.ResponseWriter, r *http.Request) {
	user := s.getUser(r)
	if user == nil {
		s.loginRedirect(w, r, safeReturnTo(r.URL.RequestURI()))
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, memoryHTML(user))
}

func (s *AdminServer) handleLogin(w http.ResponseWriter, r *http.Request) {
	returnTo := safeReturnTo(r.URL.Query().Get("return_to"))
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	s.loginRedirect(w, r, returnTo)
}

func (s *AdminServer) loginRedirect(w http.ResponseWriter, r *http.Request, returnTo string) {
	returnTo = safeReturnTo(returnTo)
	oidc, err := s.getOIDCConfig()
	if err != nil {
		http.Error(w, "load oidc config failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	state := randomToken(24)
	nonce := randomToken(24)
	codeVerifier := randomToken(48)
	challengeBytes := sha256.Sum256([]byte(codeVerifier))
	codeChallenge := base64.RawURLEncoding.EncodeToString(challengeBytes[:])
	now := s.cfg.now().Unix()
	stateValue, err := s.signer.sign(map[string]any{
		"state":         state,
		"nonce":         nonce,
		"code_verifier": codeVerifier,
		"return_to":     returnTo,
		"iat":           now,
		"exp":           now + 600,
	})
	if err != nil {
		http.Error(w, "cannot create login state", http.StatusInternalServerError)
		return
	}
	query := url.Values{
		"client_id":             {s.cfg.LogtoAppID},
		"redirect_uri":          {s.cfg.PublicBaseURL + "/admin/callback"},
		"response_type":         {"code"},
		"scope":                 {"openid profile email"},
		"state":                 {state},
		"nonce":                 {nonce},
		"code_challenge":        {codeChallenge},
		"code_challenge_method": {"S256"},
	}
	http.SetCookie(w, signedCookie(stateCookie, stateValue, 10*time.Minute))
	http.Redirect(w, r, oidc.AuthorizationEndpoint+"?"+query.Encode(), http.StatusFound)
}

func (s *AdminServer) handleCallback(w http.ResponseWriter, r *http.Request) {
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code or state", http.StatusBadRequest)
		return
	}
	stateCookieValue, err := r.Cookie(stateCookie)
	if err != nil {
		http.Error(w, "missing login state", http.StatusBadRequest)
		return
	}
	expected, err := s.signer.verify(stateCookieValue.Value, 10*time.Minute)
	if err != nil || !hmac.Equal([]byte(stringValue(expected["state"])), []byte(state)) {
		http.Error(w, "invalid login state", http.StatusBadRequest)
		return
	}
	user, err := s.exchangeLogtoUser(r.Context(), code, stringValue(expected["code_verifier"]))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.userAllowed(user) {
		http.Error(w, "user is not allowed", http.StatusForbidden)
		return
	}
	now := s.cfg.now().Unix()
	session, err := s.signer.sign(map[string]any{
		"user": user,
		"iat":  now,
		"exp":  now + int64(s.cfg.SessionMaxAge.Seconds()),
	})
	if err != nil {
		http.Error(w, "cannot create session", http.StatusInternalServerError)
		return
	}
	http.SetCookie(w, signedCookie(sessionCookie, session, s.cfg.SessionMaxAge))
	clearCookie(w, stateCookie)
	http.Redirect(w, r, safeReturnTo(stringValue(expected["return_to"])), http.StatusFound)
}

func (s *AdminServer) exchangeLogtoUser(ctx context.Context, code string, verifier string) (map[string]any, error) {
	oidc, err := s.getOIDCConfig()
	if err != nil {
		return nil, err
	}
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {s.cfg.PublicBaseURL + "/admin/callback"},
		"code_verifier": {verifier},
	}
	tokenPayload, err := s.postToken(ctx, oidc.TokenEndpoint, form, true)
	if err != nil {
		tokenPayload, err = s.postToken(ctx, oidc.TokenEndpoint, form, false)
		if err != nil {
			return nil, err
		}
	}
	accessToken := stringValue(tokenPayload["access_token"])
	if accessToken == "" {
		return nil, errors.New("missing access token")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, oidc.UserinfoEndpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("load userinfo failed: %s", string(body))
	}
	var userinfo map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&userinfo); err != nil {
		return nil, err
	}
	return map[string]any{
		"sub":      userinfo["sub"],
		"email":    userinfo["email"],
		"name":     userinfo["name"],
		"username": userinfo["username"],
	}, nil
}

func (s *AdminServer) postToken(ctx context.Context, endpoint string, form url.Values, basic bool) (map[string]any, error) {
	body := url.Values{}
	for key, values := range form {
		for _, value := range values {
			body.Add(key, value)
		}
	}
	if !basic {
		body.Set("client_id", s.cfg.LogtoAppID)
		body.Set("client_secret", s.cfg.LogtoAppSecret)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(body.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if basic {
		req.SetBasicAuth(s.cfg.LogtoAppID, s.cfg.LogtoAppSecret)
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, fmt.Errorf("token exchange failed: %s", string(body))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (s *AdminServer) handleLogout(w http.ResponseWriter, r *http.Request) {
	location := s.cfg.PublicBaseURL + "/admin"
	if oidc, err := s.getOIDCConfig(); err == nil && oidc.EndSessionEndpoint != "" {
		query := url.Values{
			"client_id":                {s.cfg.LogtoAppID},
			"post_logout_redirect_uri": {s.cfg.PublicBaseURL + "/admin"},
		}
		location = oidc.EndSessionEndpoint + "?" + query.Encode()
	}
	clearCookie(w, sessionCookie)
	clearCookie(w, stateCookie)
	http.Redirect(w, r, location, http.StatusFound)
}

func (s *AdminServer) handleMe(w http.ResponseWriter, r *http.Request, user map[string]any) {
	writeJSON(w, http.StatusOK, map[string]any{"user": user})
}

func (s *AdminServer) handleDevices(w http.ResponseWriter, r *http.Request, user map[string]any) {
	devices, err := s.deviceController().Devices(r.Context())
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": devices})
}

func (s *AdminServer) handleTools(w http.ResponseWriter, r *http.Request, user map[string]any) {
	result, err := s.deviceController().Tools(r.Context(), r.URL.Query().Get("device_id"))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	result.Tools = normalizeAdminTools(result.Tools)
	writeJSON(w, http.StatusOK, result)
}

func normalizeAdminTools(tools []map[string]any) []map[string]any {
	normalized := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		name := strings.TrimSpace(stringValue(tool["name"]))
		if name == "" {
			continue
		}
		parameters := tool["inputSchema"]
		if parameters == nil {
			parameters = map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			}
		}
		normalized = append(normalized, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        name,
				"description": stringValue(tool["description"]),
				"parameters":  parameters,
			},
		})
	}
	return normalized
}

func (s *AdminServer) handleCall(w http.ResponseWriter, r *http.Request, user map[string]any) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request BridgeCallRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if request.DeviceID == "" || request.Tool == "" {
		http.Error(w, "device_id and tool are required", http.StatusBadRequest)
		return
	}
	request.Timeout = normalizeMCPTimeout(request.Tool, request.Timeout)
	started := s.cfg.now()
	result, err := s.deviceController().Call(r.Context(), request)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	preview := s.buildResultPreviewForCall(request.DeviceID, request.Tool, result.Result, started.Add(-2*time.Second))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         result.OK,
		"result":     result.Result,
		"raw":        result.Raw,
		"error":      result.Error,
		"elapsed_ms": result.ElapsedMS,
		"preview":    preview,
	})
}

func (s *AdminServer) handleSchedules(w http.ResponseWriter, r *http.Request, user map[string]any) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"schedules": s.schedules()})
}

func (s *AdminServer) schedules() []map[string]any {
	interval := s.cfg.StudyMonitorInterval
	if interval == 0 {
		interval = 10 * time.Minute
	}
	return []map[string]any{
		{
			"id":               "study_monitor",
			"name":             "学习状态监控",
			"description":      "在设定时间窗内定时调用摄像头检查学习状态，并按需发送语音提醒和飞书通知。",
			"enabled":          s.cfg.StudyMonitorEnabled,
			"timezone":         s.cfg.StudyMonitorTimezone,
			"window":           fmt.Sprintf("%02d:00-%02d:00", s.cfg.StudyMonitorStartHour, s.cfg.StudyMonitorEndHour),
			"interval_seconds": int(interval.Seconds()),
			"camera_tool":      s.cfg.StudyMonitorCameraTool,
			"reminder_text":    s.cfg.StudyMonitorReminder,
			"device_ids":       s.cfg.StudyMonitorDeviceIDs,
		},
		{
			"id":          "morning_greeting",
			"name":        "早安问候",
			"description": "每天早上固定时间向在线设备播放问候语；没有在线设备时跳过，不补播。",
			"enabled":     s.cfg.MorningGreetingEnabled,
			"timezone":    s.cfg.MorningGreetingTimezone,
			"time":        fmt.Sprintf("%02d:%02d", clampInt(s.cfg.MorningGreetingHour, 0, 23, 8), clampInt(s.cfg.MorningGreetingMinute, 0, 59, 0)),
			"text":        firstText(strings.TrimSpace(s.cfg.MorningGreetingText), "早上好。"),
			"device_ids":  s.cfg.MorningGreetingDeviceIDs,
		},
	}
}

type memoryListItem struct {
	Key        string `json:"key"`
	DeviceID   string `json:"device_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
	Bytes      int    `json:"bytes"`
	Online     bool   `json:"online"`
}

type memoryMessageSummary struct {
	Index                int                `json:"index"`
	Role                 string             `json:"role"`
	Content              string             `json:"content"`
	Name                 string             `json:"name,omitempty"`
	ToolCalls            []schema.ToolCall  `json:"tool_calls,omitempty"`
	ToolCallID           string             `json:"tool_call_id,omitempty"`
	ToolName             string             `json:"tool_name,omitempty"`
	FinishReason         string             `json:"finish_reason,omitempty"`
	Usage                *schema.TokenUsage `json:"usage,omitempty"`
	ReasoningContent     string             `json:"reasoning_content,omitempty"`
	MultiContentParts    int                `json:"multi_content_parts,omitempty"`
	UserInputParts       int                `json:"user_input_parts,omitempty"`
	AssistantOutputParts int                `json:"assistant_output_parts,omitempty"`
	RawJSON              string             `json:"raw_json"`
}

func (s *AdminServer) handleMemoryList(w http.ResponseWriter, r *http.Request, user map[string]any) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	prefix := s.memoryPrefix()
	devices, deviceErr := s.deviceController().Devices(r.Context())
	online := map[string]bool{}
	for _, device := range devices {
		online[device.DeviceID] = true
	}
	if s.memory == nil || !s.memory.Enabled() {
		payload := map[string]any{
			"enabled":  false,
			"prefix":   prefix,
			"devices":  devices,
			"memories": []memoryListItem{},
		}
		if deviceErr != nil {
			payload["device_error"] = deviceErr.Error()
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}
	keys, err := s.memory.List(r.Context(), 200)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"enabled": true, "prefix": prefix, "error": err.Error()})
		return
	}
	memories := make([]memoryListItem, 0, len(keys))
	for _, key := range keys {
		memories = append(memories, memoryListItem{
			Key:        key.Key,
			DeviceID:   key.DeviceID,
			TTLSeconds: key.TTLSeconds,
			Bytes:      key.Bytes,
			Online:     online[key.DeviceID],
		})
	}
	sort.Slice(memories, func(i, j int) bool {
		if memories[i].DeviceID == memories[j].DeviceID {
			return memories[i].Key < memories[j].Key
		}
		return memories[i].DeviceID < memories[j].DeviceID
	})
	payload := map[string]any{
		"enabled":  true,
		"prefix":   prefix,
		"devices":  devices,
		"memories": memories,
	}
	if deviceErr != nil {
		payload["device_error"] = deviceErr.Error()
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *AdminServer) handleMemoryDetail(w http.ResponseWriter, r *http.Request, user map[string]any) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	deviceID := strings.TrimSpace(r.URL.Query().Get("device_id"))
	if deviceID == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	order := normalizeMemoryOrder(r.URL.Query().Get("order"))
	prefix := s.memoryPrefix()
	if s.memory == nil || !s.memory.Enabled() {
		writeJSON(w, http.StatusOK, map[string]any{
			"enabled":   false,
			"prefix":    prefix,
			"device_id": deviceID,
			"order":     order,
			"messages":  []memoryMessageSummary{},
		})
		return
	}
	value, err := s.memory.LoadRaw(r.Context(), deviceID)
	if err != nil {
		if errors.Is(err, redis.Nil) || strings.TrimSpace(err.Error()) == "redis: nil" {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{"enabled": true, "prefix": prefix, "device_id": deviceID, "error": err.Error()})
		return
	}
	messages, parseErr := summarizeMemoryMessages(value.Raw)
	if order == "newest" {
		reverseMemoryMessages(messages)
	}
	payload := map[string]any{
		"enabled":       true,
		"prefix":        prefix,
		"key":           value.Key,
		"device_id":     value.DeviceID,
		"ttl_seconds":   value.TTLSeconds,
		"bytes":         value.Bytes,
		"order":         order,
		"message_count": len(messages),
		"messages":      messages,
		"raw_json":      string(value.Raw),
	}
	if parseErr != nil {
		payload["parse_error"] = parseErr.Error()
	}
	writeJSON(w, http.StatusOK, payload)
}

func (s *AdminServer) memoryPrefix() string {
	if s.memory != nil && s.memory.Prefix() != "" {
		return s.memory.Prefix()
	}
	return s.cfg.RedisKeyPrefix
}

func normalizeMemoryOrder(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), "oldest") {
		return "oldest"
	}
	return "newest"
}

func summarizeMemoryMessages(raw []byte) ([]memoryMessageSummary, error) {
	if len(raw) == 0 {
		return []memoryMessageSummary{}, nil
	}
	var messages []*schema.Message
	if err := json.Unmarshal(raw, &messages); err != nil {
		return []memoryMessageSummary{}, err
	}
	summaries := make([]memoryMessageSummary, 0, len(messages))
	for i, message := range messages {
		if message == nil {
			continue
		}
		item := memoryMessageSummary{
			Index:                i,
			Role:                 string(message.Role),
			Content:              message.Content,
			Name:                 message.Name,
			ToolCalls:            message.ToolCalls,
			ToolCallID:           message.ToolCallID,
			ToolName:             message.ToolName,
			ReasoningContent:     message.ReasoningContent,
			MultiContentParts:    len(message.MultiContent),
			UserInputParts:       len(message.UserInputMultiContent),
			AssistantOutputParts: len(message.AssistantGenMultiContent),
		}
		if message.ResponseMeta != nil {
			item.FinishReason = message.ResponseMeta.FinishReason
			item.Usage = message.ResponseMeta.Usage
		}
		if encoded, err := json.Marshal(message); err == nil {
			item.RawJSON = string(encoded)
		}
		summaries = append(summaries, item)
	}
	return summaries, nil
}

func reverseMemoryMessages(messages []memoryMessageSummary) {
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
}

func (s *AdminServer) handleSpeak(w http.ResponseWriter, r *http.Request, user map[string]any) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		DeviceID string `json:"device_id"`
		Text     string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.DeviceID) == "" || strings.TrimSpace(request.Text) == "" {
		http.Error(w, "device_id and text are required", http.StatusBadRequest)
		return
	}
	result, err := s.deviceController().Speak(r.Context(), request.DeviceID, request.Text)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *AdminServer) handleSpeakStop(w http.ResponseWriter, r *http.Request, user map[string]any) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		DeviceID string `json:"device_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.DeviceID) == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	result, err := s.deviceController().StopSpeak(r.Context(), request.DeviceID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *AdminServer) handleSnapshot(w http.ResponseWriter, r *http.Request, user map[string]any) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var request struct {
		DeviceID   string `json:"device_id"`
		Resolution string `json:"resolution"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(request.DeviceID) == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	resolution := normalizeSnapshotResolution(request.Resolution)
	started := s.cfg.now()
	result, err := s.deviceController().Call(r.Context(), BridgeCallRequest{
		DeviceID: request.DeviceID,
		Tool:     "self.camera.snapshot",
		Arguments: map[string]any{
			"resolution": resolution,
		},
		Timeout: 60,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	preview := s.buildResultPreviewForCall(request.DeviceID, "self.camera.snapshot", result.Result, started.Add(-2*time.Second))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         result.OK,
		"result":     result.Result,
		"raw":        result.Raw,
		"error":      result.Error,
		"elapsed_ms": result.ElapsedMS,
		"preview":    preview,
	})
}

func (s *AdminServer) handleStreamStart(w http.ResponseWriter, r *http.Request, user map[string]any) {
	s.handleCameraStreamTool(w, r, "self.camera.start_stream")
}

func (s *AdminServer) handleStreamStop(w http.ResponseWriter, r *http.Request, user map[string]any) {
	s.handleCameraStreamTool(w, r, "self.camera.stop_stream")
}

func (s *AdminServer) handleCameraStreamTool(w http.ResponseWriter, r *http.Request, tool string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	deviceID := stringValue(body["device_id"])
	if deviceID == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	args := map[string]any{}
	if tool == "self.camera.start_stream" {
		args["fps"] = clampInt(body["fps"], 1, 3, 1)
		args["duration_sec"] = clampInt(body["duration_sec"], 1, 60, 30)
		args["resolution"] = normalizeStreamResolution(stringValue(body["resolution"]))
		args["transport"] = normalizeStreamTransport(firstNonEmptyString(stringValue(body["transport"]), "lan"))
	}
	result, err := s.deviceController().Call(r.Context(), BridgeCallRequest{
		DeviceID:  deviceID,
		Tool:      tool,
		Arguments: args,
		Timeout:   10,
	})
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (s *AdminServer) handleImage(w http.ResponseWriter, r *http.Request, user map[string]any) {
	id := path.Base(r.URL.Path)
	s.imagesMu.Lock()
	record, ok := s.images[id]
	s.imagesMu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", record.ContentType)
	_, _ = w.Write(record.Body)
}

func (s *AdminServer) withUser(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request, map[string]any)) {
	user := s.getUser(r)
	if user == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	next(w, r, user)
}

func (s *AdminServer) getUser(r *http.Request) map[string]any {
	cookie, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	payload, err := s.signer.verify(cookie.Value, 0)
	if err != nil {
		return nil
	}
	user, ok := payload["user"].(map[string]any)
	if !ok || !s.userAllowed(user) {
		return nil
	}
	return user
}

func (s *AdminServer) userAllowed(user map[string]any) bool {
	if len(s.cfg.AllowedUsers) == 0 {
		return true
	}
	allowed := map[string]struct{}{}
	for _, item := range s.cfg.AllowedUsers {
		allowed[item] = struct{}{}
	}
	if _, ok := allowed["*"]; ok {
		return true
	}
	for _, key := range []string{"sub", "email", "username", "name"} {
		if _, ok := allowed[stringValue(user[key])]; ok {
			return true
		}
	}
	return false
}

func (s *AdminServer) getOIDCConfig() (oidcConfig, error) {
	s.oidcMu.Lock()
	defer s.oidcMu.Unlock()
	if s.oidc != nil {
		return *s.oidc, nil
	}
	cfg, err := s.oidcFetcher()
	if err != nil {
		return oidcConfig{}, err
	}
	s.oidc = &cfg
	return cfg, nil
}

func (s *AdminServer) fetchOIDCConfig() (oidcConfig, error) {
	if s.cfg.LogtoEndpoint == "/" || s.cfg.LogtoEndpoint == "" {
		return oidcConfig{}, errors.New("LOGTO_ENDPOINT is required")
	}
	discoveryURL := strings.TrimRight(s.cfg.LogtoEndpoint, "/") + "/oidc/.well-known/openid-configuration"
	req, err := http.NewRequest(http.MethodGet, discoveryURL, nil)
	if err != nil {
		return oidcConfig{}, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return oidcConfig{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return oidcConfig{}, fmt.Errorf("discovery failed: %s", string(body))
	}
	var cfg oidcConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return oidcConfig{}, err
	}
	return cfg, nil
}

func safeReturnTo(returnTo string) string {
	if !strings.HasPrefix(returnTo, "/admin") {
		return "/admin"
	}
	return returnTo
}

func signedCookie(name, value string, maxAge time.Duration) *http.Cookie {
	return &http.Cookie{
		Name:     name,
		Value:    value,
		Path:     "/admin",
		MaxAge:   int(maxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	}
}

func clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{
		Name:     name,
		Value:    "",
		Path:     "/admin",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteLaxMode,
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	encoder := json.NewEncoder(w)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(payload)
}

func randomToken(bytesLen int) string {
	raw := make([]byte, bytesLen)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func clampInt(value any, min int, max int, fallback int) int {
	parsed, ok := int64Value(value)
	if !ok {
		return fallback
	}
	if int(parsed) < min {
		return min
	}
	if int(parsed) > max {
		return max
	}
	return int(parsed)
}

func normalizeMCPTimeout(toolName string, requested int) int {
	if requested <= 0 {
		requested = 30
	}
	if longRunningMCPTool(toolName) && requested < 120 {
		requested = 120
	}
	if requested > 120 {
		return 120
	}
	return requested
}

func longRunningMCPTool(toolName string) bool {
	normalized := strings.ToLower(toolName)
	for _, marker := range []string{"camera", "photo", "vision", "image", "snapshot", "拍照", "摄像"} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func normalizeSnapshotResolution(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "qvga", "vga", "svga", "xga", "uxga", "legacy_vga":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "vga"
	}
}

func normalizeStreamResolution(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "qqvga", "qvga", "vga", "svga":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "qqvga"
	}
}

func normalizeStreamTransport(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "lan", "remote", "auto":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return "lan"
	}
}

func dashboardHTML(user map[string]any) string {
	userLabel := html.EscapeString(firstNonEmpty(user, "email", "name", "sub"))
	return `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>小李设备后台</title>
  <style>
    * { box-sizing: border-box; }
    body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f6f7f9; color: #17202a; overflow-x: hidden; }
    header { display: flex; align-items: center; justify-content: space-between; padding: 14px 20px; border-bottom: 1px solid #d9dee7; background: #fff; }
    h1 { margin: 0; font-size: 18px; font-weight: 650; }
    h2 { margin: 0 0 12px; font-size: 15px; }
    main { max-width: 1360px; margin: 0 auto; padding: 18px; display: grid; gap: 16px; }
    section { background: #fff; border: 1px solid #d9dee7; border-radius: 8px; padding: 14px; }
    .tool-grid { display: grid; gap: 16px; grid-template-columns: minmax(280px, 380px) minmax(0, 1fr); align-items: start; }
    button, select, input, textarea { font: inherit; }
    button { border: 1px solid #d9dee7; background: #fff; border-radius: 6px; padding: 8px 10px; cursor: pointer; min-height: 36px; }
    button.primary { background: #0f766e; border-color: #0f766e; color: #fff; }
    button:disabled { opacity: .56; cursor: wait; }
    button[aria-busy="true"] { position: relative; }
    button[aria-busy="true"]::after { content: ""; display: inline-block; width: 10px; height: 10px; margin-left: 8px; border: 2px solid currentColor; border-right-color: transparent; border-radius: 999px; animation: spin .7s linear infinite; vertical-align: -1px; }
    select, input, textarea { width: 100%; border: 1px solid #d9dee7; border-radius: 6px; padding: 8px; background: #fff; }
    textarea { min-height: 180px; resize: vertical; font-family: ui-monospace, SFMono-Regular, Menlo, monospace; font-size: 13px; }
    pre { margin: 0; white-space: pre-wrap; word-break: break-word; background: #101828; color: #e6edf3; border-radius: 6px; padding: 12px; min-height: 320px; max-height: 70vh; overflow: auto; }
    .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
    .stack { display: grid; gap: 10px; }
    .device-row { display: grid; gap: 8px; grid-template-columns: auto minmax(260px, 1fr) auto; align-items: center; }
    .muted { color: #667085; font-size: 13px; }
    .tabs { display: flex; gap: 6px; border-bottom: 1px solid #d9dee7; margin-bottom: 14px; overflow-x: auto; }
    .tab-button { border-bottom-left-radius: 0; border-bottom-right-radius: 0; margin-bottom: -1px; white-space: nowrap; }
    .tab-button.active { border-color: #0f766e; border-bottom-color: #fff; color: #0f766e; font-weight: 650; }
    .tab-panel { display: grid; gap: 12px; }
    .tab-panel[hidden] { display: none; }
    .preview { display: grid; gap: 10px; }
    img.preview-image { max-width: min(100%, 640px); border: 1px solid #d9dee7; border-radius: 6px; background: #f2f4f7; }
    .video-controls { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
    .video-controls select { width: auto; min-width: 170px; }
    .stream-viewer { position: relative; display: grid; place-items: center; min-height: 440px; border: 1px solid #d9dee7; border-radius: 8px; background: #0b1220; overflow: hidden; }
    .stream-viewer img { display: none; width: 100%; max-height: 72vh; object-fit: contain; }
    .stream-viewer.has-frame img { display: block; }
    .stream-viewer.has-frame .stream-placeholder { display: none; }
    .stream-placeholder { color: #cbd5e1; font-size: 14px; }
    .schedule-list { display: grid; gap: 10px; }
    .schedule-item { border: 1px solid #d9dee7; border-radius: 8px; padding: 12px; display: grid; gap: 8px; }
    .schedule-title { display: flex; gap: 8px; align-items: center; justify-content: space-between; }
    .pill { border-radius: 999px; padding: 2px 8px; font-size: 12px; background: #eef2f6; color: #667085; }
    .pill.ok { background: #dcfae6; color: #067647; }
    @keyframes spin { to { transform: rotate(360deg); } }
    @media (max-width: 820px) { .tool-grid, .device-row { grid-template-columns: 1fr; } .video-controls select { min-width: auto; width: 100%; } }
    @media (max-width: 640px) { main { padding: 12px; } section { padding: 10px; } .stream-viewer { min-height: 240px; } pre { min-height: 200px; } .tab-button { font-size: 13px; padding: 6px 8px; } }
  </style>
</head>
<body>
  <header>
    <h1>小李设备后台</h1>
    <div class="row"><a href="/admin/memory">记忆查看</a><span class="muted">` + userLabel + `</span><a href="/admin/logout">退出</a></div>
  </header>
  <main>
    <section id="deviceBar">
      <div class="device-row">
        <label class="muted" for="device">设备列表</label>
        <select id="device"></select>
        <button id="refresh">刷新</button>
      </div>
    </section>
    <section>
      <div class="tabs">
        <button class="tab-button active" data-tab-target="toolsTab" type="button">MCP 工具</button>
        <button class="tab-button" data-tab-target="videoTab" type="button">视频播放</button>
        <button class="tab-button" data-tab-target="audioTab" type="button">语音文本发送</button>
        <button class="tab-button" data-tab-target="scheduleTab" type="button">定时任务</button>
      </div>
      <div id="toolsTab" class="tab-panel">
        <div class="tool-grid">
          <div class="stack">
            <h2>MCP 工具</h2>
            <select id="tool"></select>
            <textarea id="args">{}</textarea>
            <button id="call" class="primary">调用</button>
          </div>
          <div class="stack">
            <h2>结果</h2>
            <div id="preview" class="preview"></div>
            <pre id="output">加载中...</pre>
          </div>
        </div>
      </div>
      <div id="videoTab" class="tab-panel" hidden>
        <div class="video-controls">
          <select id="snapshotResolution" aria-label="拍照清晰度">
            <option value="qvga">快速 320x240</option>
            <option value="vga" selected>标准 640x480</option>
            <option value="svga">清晰 800x600</option>
            <option value="xga">细节 1024x768</option>
            <option value="uxga">最高 1600x1200</option>
            <option value="legacy_vga">旧版 640x480</option>
          </select>
          <button id="snapshot">拍照</button>
          <select id="streamResolution" aria-label="视频清晰度">
            <option value="qqvga" selected>极速 160x120</option>
            <option value="qvga">低清 320x240</option>
            <option value="vga">标准 640x480</option>
            <option value="svga">清晰 800x600</option>
          </select>
          <button id="streamStart" class="primary">开始视频流</button>
          <button id="streamStop">停止视频流</button>
          <span id="streamStatus" class="muted">等待开始</span>
        </div>
        <div id="streamViewer" class="stream-viewer">
          <img id="streamImage" alt="视频画面">
          <div id="streamPlaceholder" class="stream-placeholder">点击开始视频流后，画面会显示在这里</div>
        </div>
      </div>
      <div id="audioTab" class="tab-panel" hidden>
        <label class="muted" for="speakText">要发送给设备播放的文字</label>
        <textarea id="speakText" placeholder="输入要播放的文字"></textarea>
        <div class="row">
          <button id="speak" class="primary">发送语音文本</button>
          <button id="speakStop">停止语音</button>
        </div>
        <pre id="speakOutput">等待发送...</pre>
      </div>
      <div id="scheduleTab" class="tab-panel" hidden>
        <div class="row">
          <h2>定时任务</h2>
          <button id="refreshSchedules">刷新</button>
        </div>
        <div id="schedules" class="schedule-list"></div>
      </div>
    </section>
  </main>
  <script>
    const $ = (id) => document.getElementById(id);
    let devices = [];
    let tools = [];
    let streamSocket = null;
    let directStreamURL = "";

    async function api(url, options = {}) {
      const res = await fetch(url, { credentials: "same-origin", ...options });
      if (!res.ok) throw new Error(await res.text());
      return await res.json();
    }

    async function withBusy(button, busyText, action) {
      if (!button || button.disabled) return;
      const originalText = button.textContent;
      button.disabled = true;
      button.setAttribute("aria-busy", "true");
      button.textContent = busyText;
      try {
        return await action();
      } finally {
        button.disabled = false;
        button.removeAttribute("aria-busy");
        button.textContent = originalText;
      }
    }

    function selectedDevice() { return $("device").value || ""; }
    function show(value) { $("output").textContent = typeof value === "string" ? value : JSON.stringify(value, null, 2); }
    function showSpeak(value) { $("speakOutput").textContent = typeof value === "string" ? value : JSON.stringify(value, null, 2); }
    function setStreamStatus(text) { $("streamStatus").textContent = text; }

    function renderPreview(preview) {
      $("preview").innerHTML = "";
      if (!preview) return;
      for (const src of preview.images || []) {
        const img = document.createElement("img");
        img.className = "preview-image";
        img.src = src;
        $("preview").appendChild(img);
      }
      if (preview.text) {
        const p = document.createElement("p");
        p.textContent = preview.text;
        $("preview").appendChild(p);
      }
    }

    function renderStreamImage(src) {
      if (!src) return;
      $("streamImage").src = src;
      $("streamViewer").classList.add("has-frame");
    }

    function renderDirectStream(url) {
      if (!url) return;
      directStreamURL = url;
      const separator = url.includes("?") ? "&" : "?";
      $("streamImage").onerror = async () => {
        if (!directStreamURL) return;
        directStreamURL = "";
        setStreamStatus("退回后台中转流");
        try {
          await startRelayedStream(selectedDevice());
        } catch (err) {
          setStreamStatus(String(err));
        }
      };
      $("streamImage").src = url + separator + "ts=" + Date.now();
      $("streamViewer").classList.add("has-frame");
      setStreamStatus("局域网直连播放中");
    }

    async function loadDevices() {
      const data = await api("/admin/api/devices");
      devices = data.devices || [];
      if (!devices.length) {
        $("device").innerHTML = "<option value=\"\">当前没有在线设备</option>";
        $("tool").innerHTML = "";
        show("当前没有在线设备");
        return;
      }
      const current = selectedDevice();
      $("device").innerHTML = devices.map(d => "<option value=\"" + d.device_id + "\">" + d.device_id + " " + (d.mcp_ready ? "ready" : "not ready") + "</option>").join("");
      if (current && devices.some(d => d.device_id === current)) $("device").value = current;
      await loadTools();
      show(data);
    }
    async function loadTools() {
      const id = selectedDevice();
      if (!id) return;
      const data = await api("/admin/api/tools?device_id=" + encodeURIComponent(id));
      tools = data.tools || [];
      $("tool").innerHTML = tools.map(t => {
        const name = ((t.function || {}).name || "");
        return "<option value=\"" + name + "\">" + name + "</option>";
      }).join("");
      updateArgsFromTool();
    }

    function defaultValueForSchema(key, schema) {
      schema = schema || {};
      key = String(key || "").toLowerCase();
      if (key === "question") return "请描述这张图片里的内容。";
      if (["query", "prompt", "text", "message", "instruction"].includes(key)) return "请帮我执行这个工具。";
      if (schema.default !== undefined) return schema.default;
      if (schema.enum && schema.enum.length) return schema.enum[0];
      if (schema.minimum !== undefined) return schema.minimum;
      if (schema.type === "integer" || schema.type === "number") return 0;
      if (schema.type === "boolean") return false;
      if (schema.type === "array") return [];
      if (schema.type === "object" && schema.properties) {
        const nested = {};
        for (const [childKey, childSchema] of Object.entries(schema.properties)) {
          nested[childKey] = defaultValueForSchema(childKey, childSchema);
        }
        return nested;
      }
      if (schema.type === "object") return {};
      return "";
    }

    function updateArgsFromTool() {
      const toolName = $("tool").value;
      const tool = tools.find(t => ((t.function || {}).name || "") === toolName);
      const params = ((tool || {}).function || {}).parameters || {};
      const props = params.properties || {};
      const required = new Set(params.required || []);
      const args = {};
      for (const [key, schema] of Object.entries(props)) {
        if (required.has(key) || schema.default !== undefined || Object.keys(props).length <= 8) {
          args[key] = defaultValueForSchema(key, schema);
        }
      }
      $("args").value = JSON.stringify(args, null, 2);
    }

    async function loadSchedules() {
      const data = await api("/admin/api/schedules");
      const schedules = data.schedules || [];
      $("schedules").innerHTML = schedules.map(item => {
        const enabled = item.enabled ? "启用" : "停用";
        const cls = item.enabled ? "pill ok" : "pill";
        return "<div class=\"schedule-item\">" +
          "<div class=\"schedule-title\"><strong>" + (item.name || item.id || "") + "</strong><span class=\"" + cls + "\">" + enabled + "</span></div>" +
          "<div class=\"muted\">" + (item.description || "") + "</div>" +
          "<div class=\"muted\">时间窗：" + (item.window || "-") + " / 时区：" + (item.timezone || "-") + " / 间隔：" + (item.interval_seconds || "-") + " 秒</div>" +
          "<div class=\"muted\">工具：" + (item.camera_tool || "-") + "</div>" +
        "</div>";
      }).join("") || "<p class=\"muted\">暂无定时任务</p>";
    }

    async function callTool(name, args = {}) {
      const data = await api("/admin/api/call", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ device_id: selectedDevice(), tool: name, arguments: args, timeout: timeoutForTool(name) }),
      });
      renderPreview(data.preview);
      show(data);
    }

    function timeoutForTool(name) {
      name = String(name || "").toLowerCase();
      return /camera|photo|vision|image|snapshot|拍照|摄像/.test(name) ? 120 : 30;
    }

    $("refresh").onclick = () => withBusy($("refresh"), "刷新中...", loadDevices);
    $("device").onchange = loadTools;
    $("tool").onchange = updateArgsFromTool;
    document.querySelectorAll("[data-tab-target]").forEach((button) => {
      button.addEventListener("click", () => {
        document.querySelectorAll("[data-tab-target]").forEach((item) => item.classList.remove("active"));
        document.querySelectorAll(".tab-panel").forEach((panel) => panel.hidden = true);
        button.classList.add("active");
        const panel = document.getElementById(button.dataset.tabTarget);
        if (panel) panel.hidden = false;
        if (button.dataset.tabTarget === "scheduleTab") loadSchedules().catch(err => $("schedules").textContent = String(err));
      });
    });
    $("call").onclick = () => withBusy($("call"), "调用中...", async () => {
      await callTool($("tool").value, JSON.parse($("args").value || "{}"));
    }).catch(err => show(String(err)));
    $("speak").onclick = async () => {
      await withBusy($("speak"), "发送中...", async () => {
        const data = await api("/admin/api/speak", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ device_id: selectedDevice(), text: $("speakText").value }),
        });
        showSpeak(data);
      }).catch(err => showSpeak(String(err)));
    };
    $("speakStop").onclick = async () => {
      await withBusy($("speakStop"), "停止中...", async () => {
        const data = await api("/admin/api/speak/stop", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ device_id: selectedDevice() }),
        });
        showSpeak(data);
      }).catch(err => showSpeak(String(err)));
    };
    async function startRelayedStream(id) {
      if (!id) throw new Error("没有选择在线设备");
      if (streamSocket) streamSocket.close();
      directStreamURL = "";
      const scheme = location.protocol === "https:" ? "wss:" : "ws:";
      streamSocket = new WebSocket(scheme + "//" + location.host + "/admin/ws/stream?device_id=" + encodeURIComponent(id));
      streamSocket.onopen = () => setStreamStatus("等待画面...");
      streamSocket.onmessage = (event) => {
        const data = JSON.parse(event.data);
        if (data.image) {
          $("streamImage").onerror = null;
          renderStreamImage(data.image);
          setStreamStatus("后台中转播放中");
        }
      };
      streamSocket.onerror = () => setStreamStatus("视频连接异常");
      streamSocket.onclose = () => {
        if (!directStreamURL) setStreamStatus("视频连接已关闭");
      };
      await api("/admin/api/stream/start", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ device_id: id, fps: 1, duration_sec: 30, resolution: $("streamResolution").value, transport: "remote" }),
      });
    }
    async function captureSnapshot() {
      const id = selectedDevice();
      if (!id) throw new Error("没有选择在线设备");
      setStreamStatus("正在拍照...");
      const data = await api("/admin/api/snapshot", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ device_id: id, resolution: $("snapshotResolution").value }),
      });
      renderPreview(data.preview);
      renderStreamImage((data.preview.images || [])[0]);
      show(data);
      setStreamStatus("拍照完成");
    }
    $("snapshot").onclick = () => withBusy($("snapshot"), "拍照中...", captureSnapshot).catch(err => {
      setStreamStatus(String(err));
      show(String(err));
    });
    $("streamStart").onclick = async () => {
      await withBusy($("streamStart"), "启动中...", async () => {
        const id = selectedDevice();
        if (!id) throw new Error("没有选择在线设备");
        if (streamSocket) streamSocket.close();
        directStreamURL = "";
        $("streamImage").onerror = null;
        $("streamViewer").classList.remove("has-frame");
        $("streamImage").src = "";
        setStreamStatus("连接局域网直连流...");
        const response = await api("/admin/api/stream/start", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ device_id: id, fps: 1, duration_sec: 30, resolution: $("streamResolution").value, transport: "lan" }),
        });
        const result = response.result || {};
        show(response);
        if (result.mjpeg_url) {
          renderDirectStream(result.mjpeg_url);
          return;
        }
        setStreamStatus("退回后台中转流");
        await startRelayedStream(id);
      }).catch(err => setStreamStatus(String(err)));
    };
    $("streamStop").onclick = async () => {
      await withBusy($("streamStop"), "停止中...", async () => {
        const id = selectedDevice();
        if (streamSocket) streamSocket.close();
        directStreamURL = "";
        $("streamImage").onerror = null;
        await api("/admin/api/stream/stop", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ device_id: id }),
        });
        setStreamStatus("已停止");
      }).catch(err => setStreamStatus(String(err)));
    };
    $("refreshSchedules").onclick = () => withBusy($("refreshSchedules"), "刷新中...", loadSchedules).catch(err => $("schedules").textContent = String(err));
    loadDevices().catch(err => show(String(err)));
  </script>
</body>
	</html>`
}

func memoryHTML(user map[string]any) string {
	userLabel := html.EscapeString(firstNonEmpty(user, "email", "name", "sub"))
	return `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>小李记忆查看</title>
  <style>
    * { box-sizing: border-box; }
    body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; background: #f6f7f9; color: #17202a; overflow-x: hidden; }
    header { display: flex; align-items: center; justify-content: space-between; gap: 12px; padding: 14px 20px; border-bottom: 1px solid #d9dee7; background: #fff; }
    h1 { margin: 0; font-size: 18px; font-weight: 650; }
    h2 { margin: 0; font-size: 15px; }
    a { color: #0f766e; text-decoration: none; }
    main { max-width: 1480px; margin: 0 auto; padding: 18px; display: grid; gap: 14px; }
    section { background: #fff; border: 1px solid #d9dee7; border-radius: 8px; padding: 14px; }
    button, select, input { font: inherit; }
    button { border: 1px solid #d9dee7; background: #fff; border-radius: 6px; padding: 8px 10px; cursor: pointer; min-height: 36px; }
    button.primary { background: #0f766e; border-color: #0f766e; color: #fff; }
    select, input { width: 100%; border: 1px solid #d9dee7; border-radius: 6px; padding: 8px; background: #fff; min-height: 36px; }
    pre { margin: 0; white-space: pre-wrap; word-break: break-word; background: #101828; color: #e6edf3; border-radius: 6px; padding: 12px; min-height: 240px; max-height: 62vh; overflow: auto; }
    .row { display: flex; gap: 8px; align-items: center; flex-wrap: wrap; }
    .muted { color: #667085; font-size: 13px; }
    .toolbar { display: grid; grid-template-columns: minmax(220px, 1fr) 160px auto; gap: 8px; align-items: center; }
    .memory-picker { margin-top: 12px; display: grid; gap: 10px; }
    .memory-picker .list { grid-template-columns: repeat(auto-fit, minmax(260px, 1fr)); max-height: 220px; }
    .memory-grid { display: grid; grid-template-columns: minmax(0, 1fr) minmax(0, 1fr); gap: 14px; align-items: start; }
    .panel { display: grid; gap: 10px; min-width: 0; }
    .list { display: grid; gap: 8px; max-height: 72vh; overflow: auto; padding-right: 4px; }
    .item { text-align: left; display: grid; gap: 4px; border-radius: 6px; }
    .item.active { border-color: #0f766e; box-shadow: 0 0 0 1px #0f766e inset; }
    .message { width: 100%; border: 1px solid #d9dee7; border-left-width: 4px; border-radius: 6px; padding: 10px 12px; display: block; text-align: left; overflow: hidden; color: #17202a; background: #fff; -webkit-appearance: none; appearance: none; }
    .message.user { border-left-color: #2563eb; background: #eff6ff; }
    .message.assistant { border-left-color: #0f766e; background: #ecfdf5; }
    .message.tool { border-left-color: #9333ea; background: #faf5ff; }
    .message.active { outline: 2px solid #17202a; }
    .message-head { display: grid; grid-template-columns: auto minmax(0, 1fr) auto; align-items: center; width: 100%; gap: 8px; font-size: 12px; color: #667085; }
    .message-label, .message-finish { white-space: nowrap; }
    .message-preview { min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; color: #17202a; font-size: 13px; }
    .stats { display: flex; gap: 8px; flex-wrap: wrap; }
    .pill { border-radius: 999px; padding: 2px 8px; font-size: 12px; background: #eef2f6; color: #667085; }
    .pill.ok { background: #dcfae6; color: #067647; }
    @media (max-width: 1100px) { .memory-grid { grid-template-columns: 1fr; } .toolbar { grid-template-columns: 1fr; } }
  </style>
</head>
<body>
  <header>
    <div class="row"><h1>小李记忆查看</h1><a href="/admin">返回设备后台</a></div>
    <div class="row"><span class="muted">` + userLabel + `</span><a href="/admin/logout">退出</a></div>
  </header>
  <main>
    <section>
      <div class="toolbar">
        <input id="memoryFilter" placeholder="过滤 device_id 或 Redis key">
        <select id="memorySort" aria-label="消息排序">
          <option value="newest" selected>近到远</option>
          <option value="oldest">远到近</option>
        </select>
        <button id="refreshMemory" class="primary">刷新</button>
      </div>
      <div id="memoryStatus" class="muted" style="margin-top:10px;">加载中...</div>
      <div class="memory-picker">
        <div class="row">
          <h2>设备 / Redis Key</h2>
          <span class="muted">选择一个设备后查看下面的历史记录。</span>
        </div>
        <div id="memoryList" class="list"></div>
      </div>
    </section>
    <section class="memory-grid">
      <div class="panel">
        <h2>消息时间线</h2>
        <div id="messageList" class="list"></div>
      </div>
      <div class="panel">
        <h2>消息详情</h2>
        <div id="memoryMeta" class="stats"></div>
        <pre id="messageDetail">选择一条消息查看详情。</pre>
      </div>
    </section>
  </main>
  <script>
    const $ = (id) => document.getElementById(id);
    let memoryState = { memories: [], selectedDevice: "", detail: null, selectedIndex: null };

    async function api(url) {
      const res = await fetch(url, { credentials: "same-origin" });
      if (!res.ok) throw new Error(await res.text());
      return await res.json();
    }

    function setStatus(text) { $("memoryStatus").textContent = text; }
    function clearNode(node) { while (node.firstChild) node.removeChild(node.firstChild); }
    function text(value) { return value === undefined || value === null ? "" : String(value); }

    function filteredMemories() {
      const query = $("memoryFilter").value.trim().toLowerCase();
      if (!query) return memoryState.memories;
      return memoryState.memories.filter(item =>
        text(item.device_id).toLowerCase().includes(query) ||
        text(item.key).toLowerCase().includes(query)
      );
    }

    function renderMemoryList() {
      const list = $("memoryList");
      clearNode(list);
      const memories = filteredMemories();
      if (!memories.length) {
        const empty = document.createElement("p");
        empty.className = "muted";
        empty.textContent = "没有匹配的 Redis 记忆 key。";
        list.appendChild(empty);
        return;
      }
      for (const item of memories) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "item" + (item.device_id === memoryState.selectedDevice ? " active" : "");
        button.onclick = () => loadDetail(item.device_id);

        const title = document.createElement("strong");
        title.textContent = item.device_id || "(empty device)";
        const key = document.createElement("span");
        key.className = "muted";
        key.textContent = item.key || "";
        const meta = document.createElement("span");
        meta.className = "muted";
        meta.textContent = "TTL " + item.ttl_seconds + "s / " + item.bytes + " bytes" + (item.online ? " / online" : "");
        button.appendChild(title);
        button.appendChild(key);
        button.appendChild(meta);
        list.appendChild(button);
      }
    }

    function renderDetail() {
      const list = $("messageList");
      const meta = $("memoryMeta");
      clearNode(list);
      clearNode(meta);
      $("messageDetail").textContent = "选择一条消息查看详情。";
      memoryState.selectedIndex = null;
      const detail = memoryState.detail;
      if (!detail) return;

      for (const item of [
        "key: " + (detail.key || ""),
        "messages: " + (detail.message_count || 0),
        "ttl: " + (detail.ttl_seconds || 0) + "s",
        "order: " + (detail.order || "")
      ]) {
        const pill = document.createElement("span");
        pill.className = "pill";
        pill.textContent = item;
        meta.appendChild(pill);
      }

      if (detail.parse_error) {
        const warning = document.createElement("p");
        warning.className = "muted";
        warning.textContent = "JSON 解析失败：" + detail.parse_error;
        list.appendChild(warning);
        $("messageDetail").textContent = detail.raw_json || "";
        return;
      }

      for (const msg of detail.messages || []) {
        const button = document.createElement("button");
        button.type = "button";
        button.className = "message " + (msg.role || "");
        button.onclick = () => selectMessage(msg);

        const head = document.createElement("span");
        head.className = "message-head";
        const left = document.createElement("span");
        left.className = "message-label";
        left.textContent = "#" + msg.index + " " + (msg.role || "");
        const preview = document.createElement("span");
        preview.className = "message-preview";
        preview.textContent = msg.content || msg.reasoning_content || "(no text content)";
        const right = document.createElement("span");
        right.className = "message-finish";
        right.textContent = msg.finish_reason ? "finish: " + msg.finish_reason : "";
        head.appendChild(left);
        head.appendChild(preview);
        head.appendChild(right);
        button.appendChild(head);
        list.appendChild(button);
      }
      if (!(detail.messages || []).length) {
        const empty = document.createElement("p");
        empty.className = "muted";
        empty.textContent = "这个 key 里没有消息。";
        list.appendChild(empty);
      }
    }

    function selectMessage(msg) {
      memoryState.selectedIndex = msg.index;
      document.querySelectorAll(".message").forEach(node => node.classList.remove("active"));
      for (const node of document.querySelectorAll(".message")) {
        if (node.textContent.startsWith("#" + msg.index + " ")) node.classList.add("active");
      }
      $("messageDetail").textContent = JSON.stringify(msg, null, 2);
    }

    async function loadList() {
      setStatus("加载中...");
      const data = await api("/admin/api/memory");
      if (!data.enabled) {
        memoryState.memories = [];
        renderMemoryList();
        renderDetail();
        setStatus("Redis memory 未配置。当前 key 前缀：" + (data.prefix || ""));
        return;
      }
      memoryState.memories = data.memories || [];
      renderMemoryList();
      const suffix = data.device_error ? "；在线设备读取失败：" + data.device_error : "";
      setStatus("Redis key 前缀：" + (data.prefix || "") + "；记忆 key 数：" + memoryState.memories.length + suffix);
      if (memoryState.memories.length) {
        const keep = memoryState.memories.find(item => item.device_id === memoryState.selectedDevice);
        await loadDetail((keep || memoryState.memories[0]).device_id);
      } else {
        memoryState.detail = null;
        renderDetail();
      }
    }

    async function loadDetail(deviceID) {
      if (!deviceID) return;
      memoryState.selectedDevice = deviceID;
      renderMemoryList();
      const order = $("memorySort").value || "newest";
      const data = await api("/admin/api/memory/detail?device_id=" + encodeURIComponent(deviceID) + "&order=" + encodeURIComponent(order));
      memoryState.detail = data;
      renderDetail();
    }

    $("refreshMemory").onclick = () => loadList().catch(err => setStatus(String(err)));
    $("memoryFilter").oninput = renderMemoryList;
    $("memorySort").onchange = () => loadDetail(memoryState.selectedDevice).catch(err => setStatus(String(err)));
    loadList().catch(err => setStatus(String(err)));
  </script>
</body>
</html>`
}

func firstNonEmpty(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringValue(m[key]); value != "" && value != "<nil>" {
			return value
		}
	}
	return "admin"
}

func (s *AdminServer) handleVisionProxy(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[vision] read body failed: %v", err)
		http.Error(w, "read body failed", http.StatusBadRequest)
		return
	}
	contentType := r.Header.Get("Content-Type")
	deviceID := r.Header.Get("device-id")
	log.Printf("[vision] %s from device=%s content-type=%s body=%d bytes", r.URL.Path, deviceID, contentType, len(body))

	if r.URL.Path == "/mcp/vision/snapshot" {
		s.handleVisionSnapshot(w, r, body, contentType, deviceID)
		return
	}
	if r.URL.Path == "/mcp/vision/explain" && s.cfg.DirectDeviceServer {
		s.handleVisionExplain(w, r, body, contentType, deviceID)
		return
	}
	if r.URL.Path == "/mcp/vision/stream/frame" {
		s.handleStreamFrame(w, r, body, contentType, deviceID)
		return
	}
	if image, ok := s.extractVisionImage(contentType, body); ok && deviceID != "" {
		s.storeVisionImage(deviceID, image.ContentType, image.Body)
	}
	targetURL := s.cfg.VisionProxyBaseURL + r.URL.RequestURI()
	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, bytes.NewReader(body))
	if err != nil {
		log.Printf("[vision] create upstream request failed: %v", err)
		http.Error(w, "create upstream request failed", http.StatusInternalServerError)
		return
	}
	copyProxyHeaders(req.Header, r.Header)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		log.Printf("[vision] upstream failed for %s: %v", targetURL, err)
		http.Error(w, "vision upstream failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	log.Printf("[vision] upstream response for %s: status=%d", r.URL.Path, resp.StatusCode)
	for key, values := range resp.Header {
		if _, skip := hopByHopHeaders[strings.ToLower(key)]; skip {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

func (s *AdminServer) handleVisionSnapshot(w http.ResponseWriter, r *http.Request, body []byte, contentType string, deviceID string) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if deviceID == "" {
		log.Printf("[vision] snapshot from empty device-id")
		http.Error(w, "missing device-id", http.StatusBadRequest)
		return
	}
	image, ok := s.extractVisionImage(contentType, body)
	if !ok {
		log.Printf("[vision] snapshot from %s: no image data", deviceID)
		http.Error(w, "missing image snapshot", http.StatusBadRequest)
		return
	}
	if len(image.Body) > 2*1024*1024 {
		log.Printf("[vision] snapshot from %s: image too large: %d bytes", deviceID, len(image.Body))
		http.Error(w, "image snapshot too large", http.StatusRequestEntityTooLarge)
		return
	}
	fields := multipartFields(contentType, body)
	resolution := normalizeSnapshotResolution(firstNonEmptyString(fields["resolution"], r.Header.Get("X-Xiaoli-Resolution")))
	width := intHeaderOrField(fields["width"], r.Header.Get("X-Xiaoli-Width"))
	height := intHeaderOrField(fields["height"], r.Header.Get("X-Xiaoli-Height"))
	imageID := s.storeVisionImage(deviceID, image.ContentType, image.Body)
	imageURL := "/admin/api/images/" + imageID
	event := s.publishFrame(deviceID, image.ContentType, base64.StdEncoding.EncodeToString(image.Body), map[string]string{
		"stream_id":    "snapshot-" + imageID,
		"seq":          "0",
		"timestamp_ms": fmt.Sprintf("%d", s.cfg.now().UnixMilli()),
	})
	log.Printf("[vision] snapshot from %s: stored=%s bytes=%d resolution=%s", deviceID, imageID, len(image.Body), resolution)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"image_url":    imageURL,
		"content_type": image.ContentType,
		"bytes":        len(image.Body),
		"resolution":   resolution,
		"width":        width,
		"height":       height,
		"stream_id":    event.StreamID,
	})
}

func (s *AdminServer) handleStreamFrame(w http.ResponseWriter, r *http.Request, body []byte, contentType string, deviceID string) {
	if deviceID == "" {
		http.Error(w, "missing device-id", http.StatusBadRequest)
		return
	}
	image, ok := s.extractVisionImage(contentType, body)
	if !ok {
		http.Error(w, "missing image frame", http.StatusBadRequest)
		return
	}
	if len(image.Body) > 1024*1024 {
		http.Error(w, "image frame too large", http.StatusRequestEntityTooLarge)
		return
	}
	fields := multipartFields(contentType, body)
	event := s.publishFrame(deviceID, image.ContentType, base64.StdEncoding.EncodeToString(image.Body), map[string]string{
		"stream_id":    fields["stream_id"],
		"seq":          fields["seq"],
		"timestamp_ms": fields["timestamp_ms"],
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": event.Seq, "stream_id": event.StreamID})
}

func (s *AdminServer) handleInternalStreamFrame(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.InternalStreamToken == "" || !hmac.Equal([]byte(r.Header.Get("X-Xiaoli-Internal-Token")), []byte(s.cfg.InternalStreamToken)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var request struct {
		DeviceID    string `json:"device_id"`
		ContentType string `json:"content_type"`
		Data        string `json:"data"`
		StreamID    string `json:"stream_id"`
		Seq         string `json:"seq"`
		TimestampMS string `json:"timestamp_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if request.DeviceID == "" || request.Data == "" {
		http.Error(w, "device_id and data are required", http.StatusBadRequest)
		return
	}
	event := s.publishFrame(request.DeviceID, normalizeImageContentType(request.ContentType, ""), request.Data, map[string]string{
		"stream_id":    request.StreamID,
		"seq":          request.Seq,
		"timestamp_ms": request.TimestampMS,
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": event.Seq, "stream_id": event.StreamID})
}

func (s *AdminServer) handleInternalLatestImage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.cfg.InternalStreamToken == "" || !hmac.Equal([]byte(r.Header.Get("X-Xiaoli-Internal-Token")), []byte(s.cfg.InternalStreamToken)) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	deviceID := strings.TrimSpace(r.URL.Query().Get("device_id"))
	if deviceID == "" {
		http.Error(w, "device_id is required", http.StatusBadRequest)
		return
	}
	record := s.recentDeviceImageRecord(deviceID, time.Unix(0, 0))
	if record == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", record.ContentType)
	_, _ = w.Write(record.Body)
}

type extractedImage struct {
	ContentType string
	Body        []byte
}

func (s *AdminServer) extractVisionImage(contentType string, body []byte) (extractedImage, bool) {
	mediaType, params, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(mediaType)
	if strings.HasPrefix(mediaType, "image/") {
		return extractedImage{ContentType: mediaType, Body: body}, true
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		return extractedImage{}, false
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		payload, _ := io.ReadAll(part)
		partType := part.Header.Get("Content-Type")
		name := strings.ToLower(part.FormName())
		filename := strings.ToLower(part.FileName())
		imageField := name == "image" || name == "photo" || name == "picture" || name == "file"
		imageFile := strings.HasSuffix(filename, ".jpg") || strings.HasSuffix(filename, ".jpeg") || strings.HasSuffix(filename, ".png") || strings.HasSuffix(filename, ".webp") || strings.HasSuffix(filename, ".gif")
		if len(payload) > 0 && (strings.HasPrefix(strings.ToLower(partType), "image/") || imageField || imageFile) {
			return extractedImage{ContentType: normalizeImageContentType(partType, filename), Body: payload}, true
		}
	}
	return extractedImage{}, false
}

func multipartFields(contentType string, body []byte) map[string]string {
	mediaType, params, _ := mime.ParseMediaType(contentType)
	if !strings.HasPrefix(strings.ToLower(mediaType), "multipart/") {
		return nil
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	fields := map[string]string{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			break
		}
		if part.FormName() == "" || part.FileName() != "" {
			continue
		}
		payload, _ := io.ReadAll(part)
		fields[part.FormName()] = strings.TrimSpace(string(payload))
	}
	return fields
}

func normalizeImageContentType(contentType string, filename string) string {
	contentType = strings.ToLower(strings.TrimSpace(strings.Split(contentType, ";")[0]))
	if strings.HasPrefix(contentType, "image/") {
		return contentType
	}
	filename = strings.ToLower(filename)
	switch {
	case strings.HasSuffix(filename, ".png"):
		return "image/png"
	case strings.HasSuffix(filename, ".webp"):
		return "image/webp"
	case strings.HasSuffix(filename, ".gif"):
		return "image/gif"
	default:
		return "image/jpeg"
	}
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func intHeaderOrField(values ...string) int {
	for _, value := range values {
		parsed, ok := int64Value(strings.TrimSpace(value))
		if ok && parsed > 0 {
			return int(parsed)
		}
	}
	return 0
}

func (s *AdminServer) storeVisionImage(deviceID string, contentType string, body []byte) string {
	s.imagesMu.Lock()
	defer s.imagesMu.Unlock()
	now := s.cfg.now()
	cutoff := now.Add(-10 * time.Minute)
	for id, record := range s.images {
		if record.CreatedAt.Before(cutoff) {
			delete(s.images, id)
		}
	}
	id := randomToken(16)
	record := imageRecord{
		ID:          id,
		DeviceID:    deviceID,
		ContentType: normalizeImageContentType(contentType, ""),
		Body:        append([]byte(nil), body...),
		CreatedAt:   now,
	}
	s.images[id] = record
	ids := append(s.imagesByDev[deviceID], id)
	for len(ids) > 8 {
		delete(s.images, ids[0])
		ids = ids[1:]
	}
	s.imagesByDev[deviceID] = ids
	return id
}

func (s *AdminServer) recentDeviceImageURLs(deviceID string, since time.Time) []string {
	s.imagesMu.Lock()
	defer s.imagesMu.Unlock()
	var urls []string
	ids := s.imagesByDev[deviceID]
	for i := len(ids) - 1; i >= 0; i-- {
		record, ok := s.images[ids[i]]
		if !ok {
			continue
		}
		if record.CreatedAt.Before(since) {
			break
		}
		urls = append(urls, "/admin/api/images/"+record.ID)
		if len(urls) >= 3 {
			break
		}
	}
	return urls
}

func (s *AdminServer) publishFrame(deviceID string, contentType string, encodedBody string, metadata map[string]string) StreamEvent {
	contentType = normalizeImageContentType(contentType, "")
	return s.stream.publish(StreamEvent{
		Type:        "frame",
		DeviceID:    deviceID,
		ContentType: contentType,
		Image:       "data:" + contentType + ";base64," + encodedBody,
		Size:        len(encodedBody) * 3 / 4,
		TS:          float64(s.cfg.now().UnixNano()) / 1e9,
		StreamID:    metadata["stream_id"],
		Seq:         metadata["seq"],
		TimestampMS: metadata["timestamp_ms"],
	})
}

func (s *AdminServer) buildResultPreviewForCall(deviceID string, toolName string, value any, since time.Time) map[string]any {
	var extra []string
	if strings.Contains(toolName, "camera") || strings.Contains(toolName, "photo") || strings.Contains(toolName, "拍照") {
		extra = s.recentDeviceImageURLs(deviceID, since)
	}
	preview := buildResultPreview(value, extra)
	if len(preview.Images) > 1 {
		preview.Images = preview.Images[:1]
	}
	return map[string]any{"images": preview.Images, "text": strings.Join(preview.Texts, "\n\n")}
}

type resultPreview struct {
	Images []string
	Texts  []string
}

func buildResultPreview(value any, extraImages []string) resultPreview {
	preview := resultPreview{}
	seenImages := map[string]struct{}{}
	seenTexts := map[string]struct{}{}
	addImage := func(src string) {
		if src == "" || len(preview.Images) >= 8 {
			return
		}
		if _, ok := seenImages[src]; ok {
			return
		}
		seenImages[src] = struct{}{}
		preview.Images = append(preview.Images, src)
	}
	addText := func(text string) {
		text = strings.TrimSpace(text)
		if text == "" || len(preview.Texts) >= 8 {
			return
		}
		if _, ok := seenTexts[text]; ok {
			return
		}
		seenTexts[text] = struct{}{}
		preview.Texts = append(preview.Texts, text)
	}
	for _, src := range extraImages {
		addImage(src)
	}
	var walk func(any, string)
	walk = func(node any, key string) {
		switch typed := node.(type) {
		case map[string]any:
			keys := make([]string, 0, len(typed))
			for childKey := range typed {
				keys = append(keys, childKey)
			}
			sort.Strings(keys)
			for _, childKey := range keys {
				walk(typed[childKey], childKey)
			}
		case []any:
			for _, item := range typed {
				walk(item, key)
			}
		case string:
			normalized := normalizeKey(key)
			if src := imageSrc(normalized, typed); src != "" {
				addImage(src)
			} else if isTextKey(normalized) {
				addText(typed)
			}
		}
	}
	walk(value, "")
	return preview
}

func imageSrc(normalizedKey string, raw string) string {
	raw = strings.TrimSpace(raw)
	imageLikeKey := strings.Contains(normalizedKey, "image") || strings.Contains(normalizedKey, "photo") || strings.Contains(normalizedKey, "picture") || strings.Contains(normalizedKey, "thumbnail") || normalizedKey == "url" || normalizedKey == "base64"
	if strings.HasPrefix(raw, "data:image/") {
		return raw
	}
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		clean := strings.ToLower(strings.SplitN(raw, "?", 2)[0])
		if imageLikeKey || strings.HasSuffix(clean, ".jpg") || strings.HasSuffix(clean, ".jpeg") || strings.HasSuffix(clean, ".png") || strings.HasSuffix(clean, ".webp") || strings.HasSuffix(clean, ".gif") {
			return raw
		}
	}
	if imageLikeKey && looksBase64(raw) {
		return "data:image/jpeg;base64," + strings.Join(strings.Fields(raw), "")
	}
	return ""
}

func isTextKey(key string) bool {
	for _, item := range []string{"description", "explain", "analysis", "text", "message", "answer", "caption", "summary", "response"} {
		if strings.Contains(key, item) {
			return true
		}
	}
	return false
}

func normalizeKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func looksBase64(raw string) bool {
	raw = strings.Join(strings.Fields(raw), "")
	if len(raw) < 16 {
		return false
	}
	for _, r := range raw {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '+' || r == '/' || r == '=') {
			return false
		}
	}
	return true
}

func copyProxyHeaders(dst http.Header, src http.Header) {
	for key, values := range src {
		if _, skip := hopByHopHeaders[strings.ToLower(key)]; skip {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
}
