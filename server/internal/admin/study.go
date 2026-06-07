package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strings"
	"time"
)

const studyMonitorPrompt = `请检查这张照片中孩子的学习状态，重点判断：
1. 坐姿是否端正，是否趴桌、歪斜、低头过近或离座；
2. 是否正在认真学习，是否明显走神、玩东西或看无关内容；
3. 如果需要提醒，请只针对坐姿或学习状态给出简短提醒。

请尽量返回 JSON：
{"need_reminder": true/false, "posture": "...", "focus": "...", "summary": "...", "reminder_text": "..."}
`

var studyProblemKeywords = []string{
	"坐姿有问题", "趴", "趴桌", "歪", "歪斜", "低头", "过近", "离座", "走神", "分心", "玩东西", "玩手机", "不认真", "需要提醒",
}

var studyNegationKeywords = []string{"没有明显问题", "未发现问题", "坐姿端正", "认真学习", "无需提醒", "不需要提醒"}

type studyDecision struct {
	NeedReminder bool
	AnalysisText string
	ReminderText string
}

func (s *AdminServer) StartBackground(ctx context.Context) {
	if s.cfg.StudyMonitorEnabled {
		go s.runStudyMonitorScheduler(ctx)
	}
	if s.cfg.MorningGreetingEnabled {
		go s.runMorningGreetingScheduler(ctx)
	}
}

func (s *AdminServer) runStudyMonitorScheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var lastSlot int64
	for {
		now := s.studyMonitorNow()
		if slot := s.studyMonitorSlot(now); slot != nil && *slot != lastSlot {
			lastSlot = *slot
			_ = s.runStudyMonitorOnce(ctx, now)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *AdminServer) studyMonitorNow() time.Time {
	location, err := time.LoadLocation(s.cfg.StudyMonitorTimezone)
	if err != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	return s.cfg.now().In(location)
}

func (s *AdminServer) studyMonitorSlot(checkedAt time.Time) *int64 {
	location, err := time.LoadLocation(s.cfg.StudyMonitorTimezone)
	if err == nil {
		checkedAt = checkedAt.In(location)
	}
	if !s.inStudyMonitorWindow(checkedAt) {
		return nil
	}
	interval := s.cfg.StudyMonitorInterval
	if interval < time.Minute {
		interval = time.Minute
	}
	slot := checkedAt.Unix() - checkedAt.Unix()%int64(interval.Seconds())
	return &slot
}

func (s *AdminServer) inStudyMonitorWindow(checkedAt time.Time) bool {
	start := s.cfg.StudyMonitorStartHour
	end := s.cfg.StudyMonitorEndHour
	hour := checkedAt.Hour()
	if start == end {
		return true
	}
	if start < end {
		return start <= hour && hour < end
	}
	return hour >= start || hour < end
}

func (s *AdminServer) runMorningGreetingScheduler(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	var lastSlot int64
	for {
		now := s.morningGreetingNow()
		if slot := s.morningGreetingSlot(now); slot != nil && *slot != lastSlot {
			lastSlot = *slot
			if err := s.runMorningGreetingOnce(ctx, now); err != nil {
				log.Printf("morning greeting failed: %v", err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *AdminServer) morningGreetingNow() time.Time {
	location, err := time.LoadLocation(s.cfg.MorningGreetingTimezone)
	if err != nil {
		location = time.FixedZone("CST", 8*3600)
	}
	return s.cfg.now().In(location)
}

func (s *AdminServer) morningGreetingSlot(checkedAt time.Time) *int64 {
	location, err := time.LoadLocation(s.cfg.MorningGreetingTimezone)
	if err == nil {
		checkedAt = checkedAt.In(location)
	}
	hour := clampInt(s.cfg.MorningGreetingHour, 0, 23, 8)
	minute := clampInt(s.cfg.MorningGreetingMinute, 0, 59, 0)
	if checkedAt.Hour() != hour || checkedAt.Minute() != minute {
		return nil
	}
	dayStart := time.Date(checkedAt.Year(), checkedAt.Month(), checkedAt.Day(), 0, 0, 0, 0, checkedAt.Location())
	slot := dayStart.Unix()
	return &slot
}

func (s *AdminServer) runMorningGreetingOnce(ctx context.Context, checkedAt time.Time) error {
	controller := s.deviceController()
	devices, err := controller.Devices(ctx)
	if err != nil {
		return err
	}
	deviceID := pickOnlineDevice(devices, s.cfg.MorningGreetingDeviceIDs)
	if deviceID == "" {
		log.Printf("morning greeting skipped at %s: no eligible device (allowlist=%v, online=%d)", checkedAt.Format(time.RFC3339), s.cfg.MorningGreetingDeviceIDs, len(devices))
		return nil
	}

	text := s.dailyEncouragement(ctx)
	if text == "" {
		text = strings.TrimSpace(s.cfg.MorningGreetingText)
	}
	if text == "" {
		text = "早上好。"
	}

	_, err = controller.Speak(ctx, deviceID, text)
	if err != nil {
		return err
	}
	log.Printf("morning greeting played for %s at %s: %q", deviceID, checkedAt.Format(time.RFC3339), text)
	return nil
}

func (s *AdminServer) dailyEncouragement(ctx context.Context) string {
	if s.agent == nil {
		return ""
	}

	// Fetch the skill prompt from the MCP server
	promptText := s.fetchMCPPrompt(ctx, "daily-encouragement")
	if promptText == "" {
		return ""
	}

	// Let the LLM generate the greeting — it has all external MCP tools available
	// and can decide whether to call curl for weather, holiday info, etc.
	userMsg := fmt.Sprintf(
		"今天的日期是 %s。请根据上面的规则生成今日鼓励，只返回一句话。",
		s.cfg.now().Format("2006年1月2日 周一"),
	)
	greeting, err := s.agent.Generate(ctx, promptText, userMsg)
	if err != nil {
		log.Printf("daily encouragement: generate error: %v", err)
		return ""
	}
	return greeting
}

func (s *AdminServer) fetchMCPPrompt(ctx context.Context, promptName string) string {
	urls := s.cfg.ExternalMCPURLs
	if len(urls) == 0 {
		return ""
	}

	initBody := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"xiaoli-server","version":"1.0"}}}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, urls[0], strings.NewReader(initBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return ""
	}
	sessionID := resp.Header.Get("Mcp-Session-Id")
	resp.Body.Close()
	if sessionID == "" {
		return ""
	}

	getBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":2,"method":"prompts/get","params":{"name":"%s"}}`, promptName)
	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, urls[0], strings.NewReader(getBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Accept", "application/json, text/event-stream")
	req2.Header.Set("Mcp-Session-Id", sessionID)
	resp2, err := s.httpClient.Do(req2)
	if err != nil {
		return ""
	}
	defer resp2.Body.Close()
	raw, _ := io.ReadAll(resp2.Body)
	bodyStr := string(raw)
	if idx := strings.Index(bodyStr, "data: "); idx >= 0 {
		bodyStr = bodyStr[idx+6:]
	}

	var result struct {
		Result *struct {
			Messages []struct {
				Content struct {
					Text string `json:"text"`
				} `json:"content"`
			} `json:"messages"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(bodyStr), &result); err != nil {
		return ""
	}
	if result.Result == nil || len(result.Result.Messages) == 0 {
		return ""
	}
	return result.Result.Messages[0].Content.Text
}

// pickOnlineDevice returns the first online device whose ID is in
// allowlist. If allowlist is empty, the first online device is
// returned (backward-compatible behaviour). Returns "" if no device
// is eligible, which signals the caller to skip the run.
func pickOnlineDevice(devices []Device, allowlist []string) string {
	if len(allowlist) == 0 {
		if len(devices) == 0 {
			return ""
		}
		return devices[0].DeviceID
	}
	for _, d := range devices {
		for _, allowed := range allowlist {
			if d.DeviceID == allowed {
				return d.DeviceID
			}
		}
	}
	return ""
}

func (s *AdminServer) runStudyMonitorOnce(ctx context.Context, checkedAt time.Time) error {
	controller := s.deviceController()
	devices, err := controller.Devices(ctx)
	if err != nil {
		return err
	}
	deviceID := pickOnlineDevice(devices, s.cfg.StudyMonitorDeviceIDs)
	if deviceID == "" {
		log.Printf("study monitor skipped at %s: no eligible device (allowlist=%v, online=%d)", checkedAt.Format(time.RFC3339), s.cfg.StudyMonitorDeviceIDs, len(devices))
		return nil
	}
	started := s.cfg.now()
	result, err := controller.Call(ctx, BridgeCallRequest{
		DeviceID:  deviceID,
		Tool:      s.cfg.StudyMonitorCameraTool,
		Arguments: map[string]any{"question": studyMonitorPrompt},
		Timeout:   int(s.cfg.StudyMonitorToolTimeout.Seconds()),
	})
	if err != nil {
		return err
	}
	decision := s.parseStudyDecision(result.Result)
	reminderResult := ""
	if decision.NeedReminder {
		if response, err := controller.Speak(ctx, deviceID, decision.ReminderText); err == nil {
			encoded, _ := json.Marshal(response)
			reminderResult = string(encoded)
		} else {
			reminderResult = err.Error()
		}
	}
	imageKey := ""
	if record := s.recentDeviceImageRecord(deviceID, started.Add(-2*time.Second)); record != nil {
		if key, err := s.uploadLarkImage(ctx, record.Body, record.ContentType); err == nil {
			imageKey = key
		}
	}
	return s.sendLarkStudyMessage(ctx, studyLarkPayloadInput{
		DeviceID:       deviceID,
		AnalysisText:   decision.AnalysisText,
		NeedReminder:   decision.NeedReminder,
		ReminderText:   decision.ReminderText,
		ImageKey:       imageKey,
		CheckedAt:      checkedAt,
		ReminderResult: reminderResult,
		ElapsedMS:      result.ElapsedMS,
	})
}

func (s *AdminServer) parseStudyDecision(value any) studyDecision {
	parsed := s.extractStudyDecisionPayload(value)
	if payload, ok := parsed.(map[string]any); ok {
		var textParts []string
		for _, key := range []string{"summary", "posture", "focus", "response", "analysis", "message", "text"} {
			if text := strings.TrimSpace(stringValue(payload[key])); text != "" && text != "<nil>" {
				textParts = append(textParts, text)
			}
		}
		analysisText := strings.Join(textParts, "\n")
		if analysisText == "" {
			encoded, _ := json.Marshal(payload)
			analysisText = string(encoded)
		}
		needReminder, ok := payload["need_reminder"].(bool)
		if !ok {
			needReminder, ok = payload["needReminder"].(bool)
		}
		if !ok {
			needReminder, ok = payload["remind"].(bool)
		}
		if !ok {
			needReminder = studyTextNeedsReminder(analysisText)
		}
		reminderText := strings.TrimSpace(stringValue(payload["reminder_text"]))
		if reminderText == "" || reminderText == "<nil>" {
			reminderText = strings.TrimSpace(stringValue(payload["reminder"]))
		}
		if reminderText == "" || reminderText == "<nil>" {
			reminderText = s.cfg.StudyMonitorReminder
		}
		return studyDecision{NeedReminder: needReminder, AnalysisText: analysisText, ReminderText: reminderText}
	}
	analysisText := strings.TrimSpace(stringValue(parsed))
	return studyDecision{
		NeedReminder: studyTextNeedsReminder(analysisText),
		AnalysisText: analysisText,
		ReminderText: s.cfg.StudyMonitorReminder,
	}
}

func (s *AdminServer) extractStudyDecisionPayload(value any) any {
	parsed := tryJSONValue(value)
	if payload, ok := parsed.(map[string]any); ok {
		for _, key := range []string{"need_reminder", "needReminder", "remind"} {
			if _, ok := payload[key]; ok {
				return payload
			}
		}
		for _, key := range []string{"response", "result", "text", "message", "answer"} {
			if child, ok := payload[key]; ok && child != nil {
				extracted := s.extractStudyDecisionPayload(child)
				if _, ok := extracted.(map[string]any); ok {
					return extracted
				}
			}
		}
		if content, ok := payload["content"].([]any); ok {
			for _, item := range content {
				extracted := s.extractStudyDecisionPayload(item)
				if _, ok := extracted.(map[string]any); ok {
					return extracted
				}
			}
		}
		return payload
	}
	if items, ok := parsed.([]any); ok {
		for _, item := range items {
			extracted := s.extractStudyDecisionPayload(item)
			if _, ok := extracted.(map[string]any); ok {
				return extracted
			}
		}
	}
	return parsed
}

func tryJSONValue(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	var parsed any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		return value
	}
	return parsed
}

func studyTextNeedsReminder(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	for _, item := range studyNegationKeywords {
		if strings.Contains(text, item) {
			return false
		}
	}
	for _, item := range studyProblemKeywords {
		if strings.Contains(text, item) {
			return true
		}
	}
	return false
}

type studyLarkPayloadInput struct {
	DeviceID       string
	AnalysisText   string
	NeedReminder   bool
	ReminderText   string
	ImageKey       string
	CheckedAt      time.Time
	ReminderResult string
	ElapsedMS      int
}

func (s *AdminServer) buildLarkPostPayload(input studyLarkPayloadInput) map[string]any {
	status := "状态正常"
	if input.NeedReminder {
		status = "需要提醒"
	}
	lines := [][]map[string]string{
		{{"tag": "text", "text": "设备：" + input.DeviceID}},
		{{"tag": "text", "text": "结论：" + status}},
		{{"tag": "text", "text": "解读：" + firstText(input.AnalysisText, "无")}},
	}
	if input.NeedReminder {
		lines = append(lines, []map[string]string{{"tag": "text", "text": "已提醒：" + input.ReminderText}})
	}
	if input.ReminderResult != "" {
		lines = append(lines, []map[string]string{{"tag": "text", "text": "喇叭调用：" + truncate(input.ReminderResult, 120)}})
	}
	if input.ElapsedMS > 0 {
		lines = append(lines, []map[string]string{{"tag": "text", "text": fmt.Sprintf("耗时：%dms", input.ElapsedMS)}})
	}
	if input.ImageKey != "" {
		lines = append(lines, []map[string]string{{"tag": "img", "image_key": input.ImageKey}})
	} else {
		lines = append(lines, []map[string]string{{"tag": "text", "text": "图片：未上传成功"}})
	}
	return map[string]any{
		"msg_type": "post",
		"content": map[string]any{
			"post": map[string]any{
				"zh_cn": map[string]any{
					"title":   "学习状态巡检 " + input.CheckedAt.Format("2006-01-02 15:04"),
					"content": lines,
				},
			},
		},
	}
}

func (s *AdminServer) sendLarkStudyMessage(ctx context.Context, input studyLarkPayloadInput) error {
	if s.cfg.LarkWebhookURL == "" {
		return nil
	}
	payload := s.buildLarkPostPayload(input)
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.LarkWebhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		text, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return fmt.Errorf("lark webhook failed: %d %s", resp.StatusCode, string(text))
	}
	return nil
}

func (s *AdminServer) uploadLarkImage(ctx context.Context, body []byte, contentType string) (string, error) {
	if s.cfg.LarkAppID == "" || s.cfg.LarkAppSecret == "" {
		return "", nil
	}
	token, err := s.getLarkTenantAccessToken(ctx)
	if err != nil {
		return "", err
	}
	var form bytes.Buffer
	writer := multipart.NewWriter(&form)
	_ = writer.WriteField("image_type", "message")
	part, err := writer.CreateFormFile("image", "study-monitor.jpg")
	if err != nil {
		return "", err
	}
	_, _ = part.Write(body)
	_ = writer.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://open.larksuite.com/open-apis/im/v1/images", &form)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	req.Header.Set("X-Image-Content-Type", contentType)
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if code, _ := int64Value(payload["code"]); code != 0 {
		return "", fmt.Errorf("lark image upload failed: %v", payload)
	}
	data, _ := payload["data"].(map[string]any)
	return stringValue(data["image_key"]), nil
}

func (s *AdminServer) getLarkTenantAccessToken(ctx context.Context) (string, error) {
	requestBody, _ := json.Marshal(map[string]string{"app_id": s.cfg.LarkAppID, "app_secret": s.cfg.LarkAppSecret})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://open.larksuite.com/open-apis/auth/v3/tenant_access_token/internal", bytes.NewReader(requestBody))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if code, _ := int64Value(payload["code"]); code != 0 {
		return "", fmt.Errorf("lark tenant_access_token failed: %v", payload)
	}
	return stringValue(payload["tenant_access_token"]), nil
}

func (s *AdminServer) recentDeviceImageRecord(deviceID string, since time.Time) *imageRecord {
	s.imagesMu.Lock()
	defer s.imagesMu.Unlock()
	ids := s.imagesByDev[deviceID]
	for i := len(ids) - 1; i >= 0; i-- {
		record, ok := s.images[ids[i]]
		if !ok {
			continue
		}
		if record.CreatedAt.Before(since) {
			break
		}
		copyRecord := record
		return &copyRecord
	}
	return nil
}

func firstText(value string, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func truncate(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max]
}
