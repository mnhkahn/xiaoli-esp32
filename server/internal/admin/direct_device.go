package admin

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type DeviceController interface {
	Devices(ctx context.Context) ([]Device, error)
	Tools(ctx context.Context, deviceID string) (ToolListResponse, error)
	Call(ctx context.Context, request BridgeCallRequest) (BridgeCallResult, error)
	Speak(ctx context.Context, deviceID string, text string) (map[string]any, error)
	StopSpeak(ctx context.Context, deviceID string) (map[string]any, error)
}

type DeviceHub struct {
	cfg    Config
	stream *streamHub
	audio  *audioStore
	asr    SpeechRecognizer
	agent  *EinoAgent
	vision VisionAnalyzer
	tts    SpeechSynthesizer

	mu       sync.Mutex
	sessions map[string]*deviceSession
}

type deviceSession struct {
	hub          *DeviceHub
	deviceID     string
	sessionID    string
	clientIP     string
	connectedAt  time.Time
	lastActivity time.Time
	conn         net.Conn

	writeMu  sync.Mutex
	mu       sync.Mutex
	closed   bool
	mcpReady bool
	tools    []map[string]any
	nextID   int
	pending  map[int]chan mcpCallResult

	voiceMu           sync.Mutex
	listening         bool
	listenMode        string
	audioFrames       [][]byte
	voiceRunning      bool
	lastVoiceAt       time.Time
	lastVoiceActivity time.Time
	hasVoice          bool
	audioRecvCnt      int
	audioVoiceCnt     int
	vad               *SileroVAD
}

type mcpCallResult struct {
	Result any
	Raw    string
	Error  string
}

func NewDeviceHub(cfg Config, stream *streamHub, audio *audioStore, asr SpeechRecognizer, agent *EinoAgent, vision VisionAnalyzer, tts SpeechSynthesizer) *DeviceHub {
	return &DeviceHub{
		cfg:      cfg,
		stream:   stream,
		audio:    audio,
		asr:      asr,
		agent:    agent,
		vision:   vision,
		tts:      tts,
		sessions: map[string]*deviceSession{},
	}
}

func (h *DeviceHub) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	deviceID := strings.TrimSpace(r.Header.Get("Device-Id"))
	if deviceID == "" {
		deviceID = strings.TrimSpace(r.Header.Get("Client-Id"))
	}
	if deviceID == "" {
		http.Error(w, "missing Device-Id", http.StatusBadRequest)
		return
	}
	if !h.deviceAllowed(deviceID) {
		http.Error(w, "device is not allowed", http.StatusForbidden)
		return
	}
	if !h.deviceAuthorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	peer, err := acceptWebSocket(w, r)
	if err != nil {
		return
	}
	session := h.register(deviceID, peer.conn, clientIP(r))
	defer h.unregister(session)
	defer peer.conn.Close()

	for {
		opcode, payload, err := readWebSocketFrame(peer.reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				// The device reconnects on network errors; no response is possible here.
			}
			_ = writeWebSocketFrame(peer.conn, wsOpcodeClose, nil)
			return
		}
		session.touch()
		switch opcode {
		case wsOpcodeText:
			log.Printf("ws text from %s: %s", session.deviceID, string(payload))
			h.handleText(session, payload)
		case wsOpcodeBinary:
			h.handleAudio(session, payload)
		case wsOpcodePing:
			_ = session.writeFrame(wsOpcodePong, payload)
		case wsOpcodeClose:
			_ = session.writeFrame(wsOpcodeClose, nil)
			return
		}
	}
}

func (h *DeviceHub) Devices(ctx context.Context) ([]Device, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	devices := make([]Device, 0, len(h.sessions))
	for _, session := range h.sessions {
		session.mu.Lock()
		devices = append(devices, Device{
			DeviceID:     session.deviceID,
			SessionID:    session.sessionID,
			ClientIP:     session.clientIP,
			MCPReady:     session.mcpReady,
			ToolCount:    len(session.tools),
			ConnectedAt:  float64(session.connectedAt.UnixNano()) / 1e9,
			LastActivity: float64(session.lastActivity.UnixNano()) / 1e9,
		})
		session.mu.Unlock()
	}
	return devices, nil
}

func (h *DeviceHub) Tools(ctx context.Context, deviceID string) (ToolListResponse, error) {
	session := h.session(deviceID)
	if session == nil {
		return ToolListResponse{}, fmt.Errorf("device is not online")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	tools := append([]map[string]any(nil), session.tools...)
	return ToolListResponse{Tools: tools, Ready: session.mcpReady}, nil
}

func (h *DeviceHub) Call(ctx context.Context, request BridgeCallRequest) (BridgeCallResult, error) {
	if request.Arguments == nil {
		request.Arguments = map[string]any{}
	}
	session := h.session(request.DeviceID)
	if session == nil {
		return BridgeCallResult{}, fmt.Errorf("device is not online")
	}
	timeout := time.Duration(request.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	started := time.Now()
	result, err := session.callMCP(callCtx, "tools/call", map[string]any{
		"name":      request.Tool,
		"arguments": request.Arguments,
	})
	elapsed := int(time.Since(started) / time.Millisecond)
	if err != nil {
		return BridgeCallResult{}, err
	}
	return BridgeCallResult{
		OK:        result.Error == "",
		Result:    result.Result,
		Raw:       result.Raw,
		Error:     result.Error,
		ElapsedMS: elapsed,
	}, nil
}

func (h *DeviceHub) Speak(ctx context.Context, deviceID string, text string) (map[string]any, error) {
	session := h.session(deviceID)
	if session == nil {
		return nil, fmt.Errorf("device is not online")
	}
	if h.tts == nil {
		return nil, fmt.Errorf("Go TTS is not configured")
	}
	if err := h.playAssistantText(ctx, session, text); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok":        true,
		"status":    "played",
		"device_id": deviceID,
	}, nil
}

func (h *DeviceHub) StopSpeak(ctx context.Context, deviceID string) (map[string]any, error) {
	session := h.session(deviceID)
	if session == nil {
		return nil, fmt.Errorf("device is not online")
	}
	if err := session.writeJSON(map[string]any{"type": "tts", "state": "stop"}); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "status": "stop_signal_sent", "device_id": deviceID}, nil
}

func (h *DeviceHub) register(deviceID string, conn net.Conn, clientIP string) *deviceSession {
	now := time.Now()
	session := &deviceSession{
		hub:               h,
		deviceID:          deviceID,
		sessionID:         randomToken(18),
		clientIP:          clientIP,
		connectedAt:       now,
		lastActivity:      now,
		conn:              conn,
		pending:           map[int]chan mcpCallResult{},
		lastVoiceActivity: now,
	}
	h.mu.Lock()
	if old := h.sessions[deviceID]; old != nil {
		old.close()
	}
	h.sessions[deviceID] = session
	h.mu.Unlock()
	log.Printf("device connected: %s from %s", deviceID, clientIP)
	go session.idleTimeoutWatcher()
	return session
}

func (h *DeviceHub) unregister(session *deviceSession) {
	h.mu.Lock()
	if h.sessions[session.deviceID] == session {
		delete(h.sessions, session.deviceID)
	}
	h.mu.Unlock()
	log.Printf("device disconnected: %s", session.deviceID)
	session.close()
}

func (h *DeviceHub) session(deviceID string) *deviceSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	if deviceID == "" && len(h.sessions) == 1 {
		for _, session := range h.sessions {
			return session
		}
	}
	return h.sessions[deviceID]
}

func (h *DeviceHub) handleText(session *deviceSession, body []byte) {
	var message map[string]json.RawMessage
	if err := json.Unmarshal(body, &message); err != nil {
		return
	}
	var typ string
	_ = json.Unmarshal(message["type"], &typ)
	switch typ {
	case "listen":
		h.handleListenMessage(session, message)
	case "abort":
		session.stopVoiceRecording()
	case "hello":
		_ = session.writeJSON(map[string]any{
			"type":       "hello",
			"transport":  "websocket",
			"version":    1,
			"session_id": session.sessionID,
			"audio_params": map[string]any{
				"format":         "opus",
				"sample_rate":    16000,
				"channels":       1,
				"frame_duration": 60,
			},
		})
		go h.bootstrapMCP(session)
	case "mcp":
		h.handleMCPMessage(session, message["payload"])
	}
}

func (h *DeviceHub) handleMCPMessage(session *deviceSession, raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return
	}
	if id, ok := jsonNumberID(payload["id"]); ok {
		if _, hasResult := payload["result"]; hasResult {
			session.completeMCP(id, mcpCallResult{Result: decodeAny(payload["result"]), Raw: compactJSON(payload["result"])})
			return
		}
		if _, hasError := payload["error"]; hasError {
			session.completeMCP(id, mcpCallResult{Error: mcpErrorMessage(payload["error"]), Raw: compactJSON(payload["error"])})
			return
		}
	}
	var method string
	_ = json.Unmarshal(payload["method"], &method)
	if method == "xiaoli/vision_frame" {
		h.handleVisionFrameNotification(session, payload["params"])
	}
}

func (h *DeviceHub) handleVisionFrameNotification(session *deviceSession, raw json.RawMessage) {
	var params struct {
		StreamID    string `json:"stream_id"`
		Seq         any    `json:"seq"`
		TimestampMS any    `json:"timestamp_ms"`
		MimeType    string `json:"mime_type"`
		Data        string `json:"data"`
	}
	if err := json.Unmarshal(raw, &params); err != nil || params.Data == "" {
		return
	}
	contentType := normalizeImageContentType(params.MimeType, "image/jpeg")
	h.stream.publish(StreamEvent{
		Type:        "frame",
		DeviceID:    session.deviceID,
		ContentType: contentType,
		Image:       "data:" + contentType + ";base64," + params.Data,
		Size:        base64DecodedSize(params.Data),
		TS:          float64(time.Now().UnixNano()) / 1e9,
		StreamID:    params.StreamID,
		Seq:         fmt.Sprint(params.Seq),
		TimestampMS: fmt.Sprint(params.TimestampMS),
	})
}

func (h *DeviceHub) handleListenMessage(session *deviceSession, message map[string]json.RawMessage) {
	var state string
	_ = json.Unmarshal(message["state"], &state)
	log.Printf("listen %s from %s", state, session.deviceID)
	switch state {
	case "start":
		var mode string
		_ = json.Unmarshal(message["mode"], &mode)
		session.startVoiceRecording(mode)
	case "stop":
		frames := session.stopVoiceRecording()
		log.Printf("listen stop from %s: %d frames", session.deviceID, len(frames))
		if len(frames) > 0 {
			go h.processVoiceTurn(session, frames)
		}
	case "detect":
		var text string
		_ = json.Unmarshal(message["text"], &text)
		if strings.TrimSpace(text) != "" {
			session.voiceMu.Lock()
			session.lastVoiceActivity = time.Now()
			session.voiceMu.Unlock()
			_ = session.writeJSON(map[string]any{"type": "stt", "text": text})
		}
	}
}

func (h *DeviceHub) handleAudio(session *deviceSession, payload []byte) {
	recv, voice, total := session.appendVoiceFrame(payload)
	if recv > 0 && recv%100 == 0 {
		log.Printf("audio recv from %s: recv=%d voice=%d buffered=%d lastSize=%d", session.deviceID, recv, voice, total, len(payload))
	}
}

func (h *DeviceHub) processVoiceTurn(session *deviceSession, frames [][]byte) {
	if !session.tryStartVoiceProcessing() {
		return
	}
	defer session.finishVoiceProcessing()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	if h.asr == nil {
		_ = h.playAssistantText(ctx, session, "我现在还没有配置语音识别。")
		return
	}
	ogg, err := buildOggOpus(frames, 16000, 1, 60)
	if err != nil {
		log.Printf("voice turn ogg build failed for %s: %v", session.deviceID, err)
		_ = h.playAssistantText(ctx, session, "这次没有听清楚。")
		return
	}
	log.Printf("voice turn ogg built for %s: bytes=%d frames=%d", session.deviceID, len(ogg), len(frames))
	text, err := h.asr.Transcribe(ctx, ogg)
	if err != nil || strings.TrimSpace(text) == "" {
		log.Printf("voice turn ASR failed for %s: err=%v text=%q", session.deviceID, err, text)
		_ = h.playAssistantText(ctx, session, "这次没有听清楚。")
		return
	}
	log.Printf("voice turn ASR ok for %s: text=%q", session.deviceID, text)
	_ = session.writeJSON(map[string]any{"type": "stt", "text": text})

	answer := h.answerUserText(ctx, session, text)
	log.Printf("voice turn LLM answer for %s: %q", session.deviceID, answer)
	if strings.TrimSpace(answer) == "" {
		answer = "我现在还没想好怎么回答。"
	}
	_ = h.playAssistantText(ctx, session, answer)
}

func (h *DeviceHub) answerUserText(ctx context.Context, session *deviceSession, userText string) string {
	// needsVision 快速路径：视觉关键词直接调用 camera tool，不走 Agent ReAct 循环
	if needsVision(userText) {
		result, err := h.Call(ctx, BridgeCallRequest{
			DeviceID: session.deviceID,
			Tool:     "self.camera.take_photo",
			Arguments: map[string]any{
				"question": userText,
			},
			Timeout: 120,
		})
		if err == nil && result.Error == "" {
			if text := strings.TrimSpace(extractMCPText(result.Result)); text != "" {
				return text
			}
		}
		if err != nil {
			return "我现在看不了摄像头，原因是" + err.Error()
		}
	}
	if h.agent == nil {
		return "我现在还没有配置语言模型。"
	}
	answer, err := h.agent.Chat(ctx, session.deviceID, userText)
	if err != nil {
		log.Printf("agent chat failed for %s: %v", session.deviceID, err)
		return "我现在回答不了，语言模型调用失败。"
	}
	return answer
}

func (h *DeviceHub) playAssistantText(ctx context.Context, session *deviceSession, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	_ = session.writeJSON(map[string]any{"type": "llm", "emotion": "neutral"})
	_ = session.writeJSON(map[string]any{"type": "tts", "state": "start", "session_id": session.sessionID})
	_ = session.writeJSON(map[string]any{"type": "tts", "state": "sentence_start", "text": text, "session_id": session.sessionID})
	defer func() {
		_ = session.writeJSON(map[string]any{"type": "tts", "state": "stop", "session_id": session.sessionID})
	}()
	if h.tts == nil {
		return fmt.Errorf("TTS is not configured")
	}
	contentType, body, err := h.tts.Synthesize(ctx, text)
	if err != nil {
		log.Printf("tts synth failed for %s: text=%q err=%v", session.deviceID, text, err)
		return err
	}
	log.Printf("tts synth ok for %s: text=%q contentType=%s bytes=%d", session.deviceID, text, contentType, len(body))

	packets, frameDuration := extractOpusPackets(body)
	if len(packets) == 0 {
		log.Printf("tts no opus packets extracted for %s", session.deviceID)
		return errors.New("no opus packets")
	}
	if frameDuration <= 0 || frameDuration > 100*time.Millisecond {
		frameDuration = 20 * time.Millisecond
	}
	log.Printf("tts stream start for %s: packets=%d frameDur=%s", session.deviceID, len(packets), frameDuration)

	ticker := time.NewTicker(frameDuration)
	defer ticker.Stop()
	for i, pkt := range packets {
		if err := session.writeFrame(wsOpcodeBinary, pkt); err != nil {
			log.Printf("tts stream send failed for %s at packet %d/%d: %v", session.deviceID, i+1, len(packets), err)
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	log.Printf("tts stream done for %s: sent %d packets", session.deviceID, len(packets))
	return nil
}

func (h *DeviceHub) bootstrapMCP(session *deviceSession) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	_, _ = session.callMCP(ctx, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities": map[string]any{
			"vision": map[string]any{
				"url":   strings.TrimRight(h.cfg.PublicBaseURL, "/") + "/mcp/vision/explain",
				"token": h.cfg.DeviceAuthKey,
			},
		},
		"clientInfo": map[string]any{"name": "xiaoli-go-admin", "version": "0.1.0"},
	})
	var allTools []map[string]any
	cursor := ""
	for i := 0; i < 8; i++ {
		params := map[string]any{"withUserTools": true}
		if cursor != "" {
			params["cursor"] = cursor
		}
		result, err := session.callMCP(ctx, "tools/list", params)
		if err != nil || result.Error != "" {
			break
		}
		payload, ok := result.Result.(map[string]any)
		if !ok {
			break
		}
		for _, item := range anySlice(payload["tools"]) {
			if tool, ok := item.(map[string]any); ok {
				allTools = append(allTools, tool)
			}
		}
		cursor = stringValue(payload["nextCursor"])
		if cursor == "" {
			break
		}
	}
	session.mu.Lock()
	session.tools = allTools
	session.mcpReady = len(allTools) > 0
	session.mu.Unlock()
	log.Printf("device MCP ready: %s tools=%d", session.deviceID, len(allTools))
}

func (s *deviceSession) callMCP(ctx context.Context, method string, params map[string]any) (mcpCallResult, error) {
	id, ch := s.prepareMCPCall()
	defer s.removeMCPCall(id)
	envelope := map[string]any{
		"session_id": s.sessionID,
		"type":       "mcp",
		"payload": map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"method":  method,
			"params":  params,
		},
	}
	if err := s.writeJSON(envelope); err != nil {
		log.Printf("mcp call write failed for %s id=%d method=%s: %v", s.deviceID, id, method, err)
		return mcpCallResult{}, err
	}
	log.Printf("mcp call sent to %s id=%d method=%s", s.deviceID, id, method)
	s.voiceMu.Lock()
	s.lastVoiceActivity = time.Now()
	s.voiceMu.Unlock()
	select {
	case result, ok := <-ch:
		if !ok {
			log.Printf("mcp call channel closed for %s id=%d method=%s", s.deviceID, id, method)
			return mcpCallResult{}, errors.New("device connection closed")
		}
		log.Printf("mcp call result for %s id=%d method=%s error=%q", s.deviceID, id, method, result.Error)
		return result, nil
	case <-ctx.Done():
		log.Printf("mcp call timeout for %s id=%d method=%s err=%v", s.deviceID, id, method, ctx.Err())
		return mcpCallResult{}, ctx.Err()
	}
}

func (s *deviceSession) prepareMCPCall() (int, chan mcpCallResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	ch := make(chan mcpCallResult, 1)
	s.pending[id] = ch
	return id, ch
}

func (s *deviceSession) removeMCPCall(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.pending, id)
}

func (s *deviceSession) completeMCP(id int, result mcpCallResult) {
	s.mu.Lock()
	ch := s.pending[id]
	s.mu.Unlock()
	if ch == nil {
		return
	}
	select {
	case ch <- result:
	default:
	}
}

func (s *deviceSession) writeJSON(value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return s.writeFrame(wsOpcodeText, body)
}

func (s *deviceSession) writeFrame(opcode byte, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	s.mu.Lock()
	closed := s.closed
	conn := s.conn
	s.mu.Unlock()
	if closed {
		return errors.New("device connection closed")
	}
	return writeWebSocketFrame(conn, opcode, payload)
}

func (s *deviceSession) touch() {
	s.mu.Lock()
	s.lastActivity = time.Now()
	s.mu.Unlock()
}

func (s *deviceSession) close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	for id, ch := range s.pending {
		close(ch)
		delete(s.pending, id)
	}
	conn := s.conn
	s.mu.Unlock()
	if conn != nil {
		_ = conn.SetWriteDeadline(time.Now().Add(time.Second))
		_ = writeWebSocketFrame(conn, wsOpcodeClose, nil)
		_ = conn.Close()
	}
	s.voiceMu.Lock()
	if s.vad != nil {
		s.vad.Close()
		s.vad = nil
	}
	s.voiceMu.Unlock()
}

func (s *deviceSession) startVoiceRecording(mode string) {
	s.voiceMu.Lock()
	defer s.voiceMu.Unlock()
	s.listening = true
	s.listenMode = mode
	s.audioFrames = nil
	s.lastVoiceAt = time.Time{}
	s.lastVoiceActivity = time.Now()
	s.hasVoice = false
	s.audioRecvCnt = 0
	s.audioVoiceCnt = 0
	if s.vad == nil {
		v, err := NewSileroVAD()
		if err != nil {
			log.Printf("silero vad init failed for %s: %v", s.deviceID, err)
		} else {
			s.vad = v
		}
	}
	if s.vad != nil {
		log.Printf("listen start %s mode=%s vad=silero", s.deviceID, mode)
	} else {
		log.Printf("listen start %s mode=%s vad=FALLBACK (silero unavailable)", s.deviceID, mode)
	}
	if mode == "auto" {
		go s.autoStopWatcher()
	}
}

func (s *deviceSession) appendVoiceFrame(payload []byte) (recv, voice, total int) {
	if len(payload) == 0 {
		return 0, 0, 0
	}
	s.voiceMu.Lock()
	defer s.voiceMu.Unlock()
	if !s.listening {
		return 0, 0, 0
	}
	if len(s.audioFrames) >= 400 {
		return s.audioRecvCnt, s.audioVoiceCnt, len(s.audioFrames)
	}
	s.audioRecvCnt++
	if s.vad != nil {
		isVoice, ran, prob := s.vad.Detect(payload)
		if isVoice {
			s.lastVoiceAt = time.Now()
			s.lastVoiceActivity = s.lastVoiceAt
			s.hasVoice = true
			s.audioVoiceCnt++
		}
		if ran && s.audioRecvCnt%25 == 0 {
			log.Printf("vad sample %s: prob=%.2f isVoice=%v voiceCnt=%d/%d", s.deviceID, prob, isVoice, s.audioVoiceCnt, s.audioRecvCnt)
		}
	} else {
		// Fallback: if VAD missing, accept all frames so we don't block.
		s.lastVoiceAt = time.Now()
		s.lastVoiceActivity = s.lastVoiceAt
		s.hasVoice = true
		s.audioVoiceCnt++
	}
	frame := append([]byte(nil), payload...)
	s.audioFrames = append(s.audioFrames, frame)
	return s.audioRecvCnt, s.audioVoiceCnt, len(s.audioFrames)
}

func (s *deviceSession) idleTimeoutWatcher() {
	const idleTimeout = 180 * time.Second
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		s.mu.Lock()
		closed := s.closed
		s.mu.Unlock()
		if closed {
			return
		}
		s.voiceMu.Lock()
		lastVoice := s.lastVoiceActivity
		s.voiceMu.Unlock()
		idle := time.Since(lastVoice)
		if idle > idleTimeout {
			log.Printf("idle timeout for %s: %.0fs without voice, closing connection", s.deviceID, idle.Seconds())
			s.close()
			return
		}
	}
}

func (s *deviceSession) autoStopWatcher() {
	const silenceTimeout = 500 * time.Millisecond
	const maxDuration = 30 * time.Second
	started := time.Now()
	for {
		s.voiceMu.Lock()
		listening := s.listening && s.listenMode == "auto"
		hasVoice := s.hasVoice
		lastVoice := s.lastVoiceAt
		s.voiceMu.Unlock()
		if !listening {
			return
		}
		if hasVoice && time.Since(lastVoice) > silenceTimeout {
			frames := s.stopVoiceRecording()
			log.Printf("auto-stop from %s: %d frames (silence %.1fs)", s.deviceID, len(frames), time.Since(lastVoice).Seconds())
			if len(frames) > 0 {
				go s.hub.processVoiceTurn(s, frames)
			}
			return
		}
		if time.Since(started) > maxDuration {
			frames := s.stopVoiceRecording()
			if hasVoice {
				log.Printf("auto-stop from %s: %d frames (max duration, has voice)", s.deviceID, len(frames))
				if len(frames) > 0 {
					go s.hub.processVoiceTurn(s, frames)
				}
			} else {
				log.Printf("auto-stop from %s: %d frames discarded (max duration, no voice)", s.deviceID, len(frames))
			}
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (s *deviceSession) stopVoiceRecording() [][]byte {
	s.voiceMu.Lock()
	defer s.voiceMu.Unlock()
	if !s.listening && len(s.audioFrames) == 0 {
		return nil
	}
	s.listening = false
	frames := make([][]byte, len(s.audioFrames))
	for i := range s.audioFrames {
		frames[i] = append([]byte(nil), s.audioFrames[i]...)
	}
	s.audioFrames = nil
	return frames
}

func (s *deviceSession) tryStartVoiceProcessing() bool {
	s.voiceMu.Lock()
	defer s.voiceMu.Unlock()
	if s.voiceRunning {
		return false
	}
	s.voiceRunning = true
	return true
}

func (s *deviceSession) finishVoiceProcessing() {
	s.voiceMu.Lock()
	s.voiceRunning = false
	s.voiceMu.Unlock()
}

func needsVision(text string) bool {
	text = strings.ToLower(text)
	for _, marker := range []string{"看", "看看", "照片", "图片", "图像", "摄像头", "画面", "拍", "坐姿", "学习状态", "我现在"} {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

func extractMCPText(value any) string {
	switch v := value.(type) {
	case string:
		var parsed any
		if json.Unmarshal([]byte(v), &parsed) == nil {
			if text := extractMCPText(parsed); text != "" {
				return text
			}
		}
		return v
	case map[string]any:
		if content, ok := v["content"].([]any); ok {
			var parts []string
			for _, item := range content {
				if m, ok := item.(map[string]any); ok {
					if text := strings.TrimSpace(stringValue(m["text"])); text != "" {
						parts = append(parts, extractMCPText(text))
					}
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "\n")
			}
		}
		for _, key := range []string{"response", "answer", "text", "message", "summary", "analysis", "result"} {
			if text := strings.TrimSpace(stringValue(v[key])); text != "" && text != "<nil>" {
				return text
			}
		}
	case []any:
		var parts []string
		for _, item := range v {
			if text := strings.TrimSpace(extractMCPText(item)); text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func (h *DeviceHub) deviceAllowed(deviceID string) bool {
	if len(h.cfg.AllowedDeviceIDs) == 0 {
		return true
	}
	for _, allowed := range h.cfg.AllowedDeviceIDs {
		if allowed == deviceID {
			return true
		}
	}
	return false
}

func (h *DeviceHub) deviceAuthorized(r *http.Request) bool {
	if !h.cfg.DeviceAuthEnabled {
		return true
	}
	if h.cfg.DeviceAuthKey == "" {
		return false
	}
	auth := strings.TrimSpace(r.Header.Get("Authorization"))
	return auth == h.cfg.DeviceAuthKey || auth == "Bearer "+h.cfg.DeviceAuthKey
}

func clientIP(r *http.Request) string {
	if value := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); value != "" {
		return strings.TrimSpace(strings.Split(value, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil {
		return host
	}
	return r.RemoteAddr
}

func jsonNumberID(raw json.RawMessage) (int, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var id int
	if err := json.Unmarshal(raw, &id); err == nil {
		return id, true
	}
	return 0, false
}

func decodeAny(raw json.RawMessage) any {
	var value any
	_ = json.Unmarshal(raw, &value)
	return value
}

func compactJSON(raw json.RawMessage) string {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return string(raw)
	}
	return out.String()
}

func mcpErrorMessage(raw json.RawMessage) string {
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err == nil {
		if message := stringValue(payload["message"]); message != "" {
			return message
		}
	}
	return compactJSON(raw)
}

func anySlice(value any) []any {
	if items, ok := value.([]any); ok {
		return items
	}
	return nil
}

func base64DecodedSize(value string) int {
	if value == "" {
		return 0
	}
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
		return len(decoded)
	}
	return len(value) * 3 / 4
}

