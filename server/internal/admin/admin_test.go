package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig() Config {
	return Config{
		Host:                "127.0.0.1",
		Port:                8004,
		PublicBaseURL:       "https://xiaoli-server.fly.dev",
		SessionSecret:       "0123456789abcdef0123456789abcdef",
		LogtoEndpoint:       "https://fpilyb.logto.app/",
		LogtoAppID:          "app-id",
		LogtoAppSecret:      "app-secret",
		BridgeBaseURL:       "http://127.0.0.1:8005",
		VisionProxyBaseURL:  "http://127.0.0.1:8003",
		InternalStreamToken: "0123456789abcdef0123456789abcdef",
		Now: func() time.Time {
			return time.Unix(1_700_000_000, 0)
		},
	}
}

func TestLangSmithReferencesRemovedFromServerRuntime(t *testing.T) {
	for _, path := range []string{
		"config.go",
		"direct_ai.go",
		"../../.env.example",
		"../../fly.toml",
		"../../README.md",
	} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if strings.Contains(strings.ToLower(string(data)), "langsmith") {
			t.Fatalf("%s should not contain LangSmith references", path)
		}
	}
	if _, err := os.Stat("../../pkg/langsmith"); !os.IsNotExist(err) {
		t.Fatalf("server/pkg/langsmith should be removed, stat err=%v", err)
	}
}

func TestDockerfileCopySourcesExistInBuildContext(t *testing.T) {
	dockerfile := "../../Dockerfile"
	data, err := os.ReadFile(dockerfile)
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	for lineNumber, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.ToUpper(fields[0]) != "COPY" {
			continue
		}
		if strings.HasPrefix(fields[1], "--from=") {
			continue
		}
		for _, source := range fields[1 : len(fields)-1] {
			source = strings.Trim(source, `"'`)
			path := filepath.Clean(filepath.Join("../..", source))
			info, err := os.Stat(path)
			if err != nil {
				t.Fatalf("Dockerfile line %d copies missing source %q: %v", lineNumber+1, source, err)
			}
			if info.IsDir() && !directoryHasFile(t, path) {
				t.Fatalf("Dockerfile line %d copies empty directory %q", lineNumber+1, source)
			}
		}
	}
}

func directoryHasFile(t *testing.T, root string) bool {
	t.Helper()
	hasFile := false
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			hasFile = true
			return filepath.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return hasFile
}

func TestSignedSessionRoundTripRejectsTampering(t *testing.T) {
	signer := newSigner("0123456789abcdef0123456789abcdef", func() time.Time {
		return time.Unix(1_700_000_000, 0)
	})
	token, err := signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  1_700_000_000,
		"exp":  1_700_000_600,
	})
	if err != nil {
		t.Fatalf("sign returned error: %v", err)
	}

	payload, err := signer.verify(token, 0)
	if err != nil {
		t.Fatalf("verify returned error: %v", err)
	}
	user := payload["user"].(map[string]any)
	if user["sub"] != "logto-user" {
		t.Fatalf("unexpected user sub: %#v", user["sub"])
	}

	if _, err := signer.verify(token+"x", 0); err == nil {
		t.Fatal("tampered token verified successfully")
	}
}

func TestLogtoLoginRedirectUsesDiscoveredAuthorizationEndpoint(t *testing.T) {
	cfg := testConfig()
	cfg.LogtoEndpoint = "https://fpilyb.logto.app/"
	cfg.LogtoAppID = "app-id"
	cfg.LogtoAppSecret = "app-secret"
	srv := NewServer(cfg)
	srv.oidcFetcher = func() (oidcConfig, error) {
		return oidcConfig{
			AuthorizationEndpoint: "https://fpilyb.logto.app/authorize",
			TokenEndpoint:         "https://fpilyb.logto.app/token",
			UserinfoEndpoint:      "https://fpilyb.logto.app/userinfo",
			EndSessionEndpoint:    "https://fpilyb.logto.app/logout",
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/login?return_to=/admin", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusFound {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusFound)
	}
	location := rr.Header().Get("Location")
	if !strings.HasPrefix(location, "https://fpilyb.logto.app/authorize?") {
		t.Fatalf("Location = %q, want discovered authorize endpoint", location)
	}
	if !strings.Contains(location, "redirect_uri=https%3A%2F%2Fxiaoli-server.fly.dev%2Fadmin%2Fcallback") {
		t.Fatalf("Location does not include public callback: %q", location)
	}
	if findCookie(rr.Result().Cookies(), stateCookie) == nil {
		t.Fatal("state cookie was not set")
	}
}

func TestAdminLoginRedirectIsNotCached(t *testing.T) {
	srv := NewServer(testConfig())
	srv.oidcFetcher = func() (oidcConfig, error) {
		return oidcConfig{
			AuthorizationEndpoint: "https://fpilyb.logto.app/authorize",
			TokenEndpoint:         "https://fpilyb.logto.app/token",
			UserinfoEndpoint:      "https://fpilyb.logto.app/userinfo",
		}, nil
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/login?return_to=/admin", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if got := rr.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestLoginPostIsRejectedBecauseOnlyLogtoIsSupported(t *testing.T) {
	srv := NewServer(testConfig())
	req := httptest.NewRequest(http.MethodPost, "/admin/login?return_to=/admin", strings.NewReader("ignored=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusMethodNotAllowed)
	}
}

func TestDashboardRestoresThreeTabControlLayout(t *testing.T) {
	html := dashboardHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`id="deviceBar"`,
		`id="device"`,
		`id="refresh"`,
		`data-tab-target="toolsTab"`,
		`>MCP 工具</button>`,
		`data-tab-target="videoTab"`,
		`>视频播放</button>`,
		`data-tab-target="audioTab"`,
		`>语音文本发送</button>`,
		`data-tab-target="scheduleTab"`,
		`>定时任务</button>`,
		`id="tool"`,
		`id="args"`,
		`id="streamViewer"`,
		`id="streamImage"`,
		`id="snapshot"`,
		`id="streamResolution"`,
		`id="speakText"`,
		`id="speakStop"`,
		`id="streamStart"`,
		`id="streamStop"`,
		`id="schedules"`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing %s", fragment)
		}
	}

	deviceBarStart := strings.Index(html, `id="deviceBar"`)
	tabsStart := strings.Index(html, `class="tabs"`)
	if deviceBarStart < 0 || tabsStart < 0 || deviceBarStart > tabsStart {
		t.Fatal("device bar should appear before tabs")
	}
	deviceBar := html[deviceBarStart:tabsStart]
	if strings.Contains(deviceBar, `id="status"`) || strings.Contains(deviceBar, `id="photo"`) {
		t.Fatal("top device bar should not contain quick action buttons")
	}

	toolsButton := strings.Index(html, `data-tab-target="toolsTab"`)
	videoButton := strings.Index(html, `data-tab-target="videoTab"`)
	audioButton := strings.Index(html, `data-tab-target="audioTab"`)
	scheduleButton := strings.Index(html, `data-tab-target="scheduleTab"`)
	if !(toolsButton >= 0 && videoButton > toolsButton && audioButton > videoButton && scheduleButton > audioButton) {
		t.Fatal("tab order should be MCP tools, video playback, audio text, schedules")
	}

	toolsStart := strings.Index(html, `id="toolsTab"`)
	videoStart := strings.Index(html, `id="videoTab"`)
	audioStart := strings.Index(html, `id="audioTab"`)
	scheduleStart := strings.Index(html, `id="scheduleTab"`)
	if toolsStart < 0 || videoStart < 0 || audioStart < 0 || scheduleStart < 0 {
		t.Fatal("tab panel markers missing")
	}
	toolsSection := html[toolsStart:videoStart]
	videoSection := html[videoStart:audioStart]
	audioSection := html[audioStart:scheduleStart]
	scheduleSection := html[scheduleStart:]
	if !strings.Contains(toolsSection, `id="output"`) || strings.Contains(toolsSection, `id="streamImage"`) {
		t.Fatal("MCP tools tab should own the text result area only")
	}
	if !strings.Contains(videoSection, `id="streamImage"`) || strings.Contains(videoSection, `<pre`) || strings.Contains(videoSection, `<textarea`) {
		t.Fatal("video tab should render a video image area, not a text box")
	}
	if !strings.Contains(audioSection, `id="speakText"`) || strings.Contains(audioSection, `id="streamImage"`) {
		t.Fatal("audio tab should contain text sending controls only")
	}
	if !strings.Contains(scheduleSection, `id="schedules"`) || !strings.Contains(scheduleSection, `/admin/api/schedules`) {
		t.Fatal("schedule tab should render schedules from the schedules API")
	}
	if !strings.Contains(html, `/admin/api/speak/stop`) {
		t.Fatal("dashboard should wire speech stop control to the stop API")
	}
}

func TestSpeakStopAPIForwardsToBridge(t *testing.T) {
	var received map[string]string
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/bridge/speak/stop" {
			t.Fatalf("path = %s, want /bridge/speak/stop", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode bridge request: %v", err)
		}
		return jsonResponse(http.StatusOK, map[string]any{"ok": true, "status": "stopped"}), nil
	})}

	cfg := testConfig()
	srv := NewServer(cfg)
	srv.bridge = NewBridgeClient("http://bridge.local", httpClient)
	session, err := srv.signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  cfg.now().Unix(),
		"exp":  cfg.now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/admin/api/speak/stop", strings.NewReader(`{"device_id":"device-1"}`))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if received["device_id"] != "device-1" {
		t.Fatalf("device_id = %q, want device-1", received["device_id"])
	}
}

func TestSchedulesAPIIncludesBackgroundTasks(t *testing.T) {
	cfg := testConfig()
	cfg.StudyMonitorEnabled = true
	cfg.StudyMonitorTimezone = "Asia/Shanghai"
	cfg.StudyMonitorStartHour = 17
	cfg.StudyMonitorEndHour = 21
	cfg.StudyMonitorInterval = 5 * time.Minute
	cfg.StudyMonitorCameraTool = "self.camera.take_photo"
	cfg.MorningGreetingEnabled = true
	cfg.MorningGreetingTimezone = "Asia/Shanghai"
	cfg.MorningGreetingHour = 8
	cfg.MorningGreetingText = "早上好。"
	srv := NewServer(cfg)
	session, err := srv.signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  cfg.now().Unix(),
		"exp":  cfg.now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/admin/api/schedules", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload struct {
		Schedules []map[string]any `json:"schedules"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Schedules) != 2 {
		t.Fatalf("schedules length = %d, want 2", len(payload.Schedules))
	}
	task := payload.Schedules[0]
	if task["id"] != "study_monitor" || task["enabled"] != true || task["timezone"] != "Asia/Shanghai" {
		t.Fatalf("unexpected schedule task: %#v", task)
	}
	if task["interval_seconds"] != float64(300) || task["window"] != "17:00-21:00" {
		t.Fatalf("unexpected schedule timing: %#v", task)
	}
	greeting := payload.Schedules[1]
	if greeting["id"] != "morning_greeting" || greeting["enabled"] != true || greeting["timezone"] != "Asia/Shanghai" {
		t.Fatalf("unexpected greeting schedule: %#v", greeting)
	}
	if greeting["time"] != "08:00" || greeting["text"] != "早上好。" {
		t.Fatalf("unexpected greeting timing/text: %#v", greeting)
	}
}

func TestDashboardActionButtonsUseBusyState(t *testing.T) {
	html := dashboardHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`async function withBusy(button, busyText, action)`,
		`button.disabled = true`,
		`button.setAttribute("aria-busy", "true")`,
		`button.disabled = false`,
		`button.removeAttribute("aria-busy")`,
		`withBusy($("refresh")`,
		`withBusy($("call")`,
		`withBusy($("snapshot")`,
		`withBusy($("streamStart")`,
		`withBusy($("streamStop")`,
		`withBusy($("speak")`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing busy-state fragment %s", fragment)
		}
	}
}

func TestDashboardHasModelFreeSnapshotAction(t *testing.T) {
	html := dashboardHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`id="snapshot"`,
		`id="snapshotResolution"`,
		`value="vga"`,
		`value="uxga"`,
		`value="legacy_vga"`,
		`拍照`,
		`async function captureSnapshot()`,
		`setStreamStatus("正在拍照...")`,
		`await api("/admin/api/snapshot"`,
		`resolution: $("snapshotResolution").value`,
		`renderStreamImage((data.preview.images || [])[0]);`,
		`id="streamResolution"`,
		`value="qqvga"`,
		`resolution: $("streamResolution").value`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing snapshot fragment %s", fragment)
		}
	}
}

func TestDashboardLinksToMemoryViewer(t *testing.T) {
	html := dashboardHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`href="/admin/memory"`,
		`记忆查看`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing memory viewer fragment %s", fragment)
		}
	}
}

func TestMemoryPageRendersStandaloneViewer(t *testing.T) {
	cfg := testConfig()
	srv := NewServer(cfg)
	req := authenticatedRequest(t, srv, http.MethodGet, "/admin/memory", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	for _, fragment := range []string{
		`小李记忆查看`,
		`/admin/api/memory`,
		`/admin/api/memory/detail`,
		`id="memorySort"`,
		`class="memory-picker"`,
		`近到远`,
		`远到近`,
	} {
		if !strings.Contains(rr.Body.String(), fragment) {
			t.Fatalf("memory page missing fragment %s", fragment)
		}
	}
	html := rr.Body.String()
	pickerStart := strings.Index(html, `class="memory-picker"`)
	gridStart := strings.Index(html, `class="memory-grid"`)
	if pickerStart < 0 || gridStart < 0 || pickerStart > gridStart {
		t.Fatal("memory key list should live in the top filter section before the history grid")
	}
	historyGrid := html[gridStart:]
	if strings.Contains(historyGrid, `设备 / Redis Key`) {
		t.Fatal("history grid should only contain the message timeline and detail panels")
	}
}

func TestMemoryMessageButtonsUseValidInlineContent(t *testing.T) {
	html := memoryHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`const button = document.createElement("button");`,
		`const head = document.createElement("span");`,
		`const content = document.createElement("span");`,
		`.message { width: 100%;`,
		`.content { display: block;`,
		`overflow-wrap: anywhere;`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("memory page missing valid message layout fragment %s", fragment)
		}
	}
	for _, fragment := range []string{
		`const head = document.createElement("div");`,
		`const content = document.createElement("div");`,
	} {
		if strings.Contains(html, fragment) {
			t.Fatalf("message buttons should not contain block div children: %s", fragment)
		}
	}
}

func TestMemoryAPIsRequireAuth(t *testing.T) {
	srv := NewServer(testConfig())
	for _, path := range []string{
		"/admin/api/memory",
		"/admin/api/memory/detail?device_id=device-1",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rr := httptest.NewRecorder()
		srv.ServeHTTP(rr, req)
		if rr.Code != http.StatusUnauthorized {
			t.Fatalf("%s status = %d, want %d", path, rr.Code, http.StatusUnauthorized)
		}
	}
}

func TestMemoryListReturnsRedisKeysAndDeviceMetadata(t *testing.T) {
	cfg := testConfig()
	cfg.DirectDeviceServer = true
	cfg.RedisKeyPrefix = "xiaoli:cp:"
	srv := NewServer(cfg)
	srv.memory = fakeMemoryReader{
		prefix: "xiaoli:cp:",
		keys: []memoryKeyInfo{
			{Key: "xiaoli:cp:device-2", DeviceID: "device-2", TTLSeconds: 1800, Bytes: 20},
			{Key: "xiaoli:cp:device-1", DeviceID: "device-1", TTLSeconds: 3600, Bytes: 40},
		},
	}
	req := authenticatedRequest(t, srv, http.MethodGet, "/admin/api/memory", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload struct {
		Enabled  bool `json:"enabled"`
		Prefix   string
		Memories []struct {
			Key        string `json:"key"`
			DeviceID   string `json:"device_id"`
			TTLSeconds int64  `json:"ttl_seconds"`
			Bytes      int    `json:"bytes"`
			Online     bool   `json:"online"`
		} `json:"memories"`
		Devices []Device `json:"devices"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !payload.Enabled || payload.Prefix != "xiaoli:cp:" {
		t.Fatalf("unexpected memory metadata: %#v", payload)
	}
	if len(payload.Memories) != 2 {
		t.Fatalf("memories length = %d, want 2: %#v", len(payload.Memories), payload.Memories)
	}
	if payload.Memories[0].DeviceID != "device-1" || payload.Memories[1].DeviceID != "device-2" {
		t.Fatalf("memories should be sorted by device id: %#v", payload.Memories)
	}
	if payload.Memories[0].TTLSeconds != 3600 || payload.Memories[0].Bytes != 40 {
		t.Fatalf("first memory metadata = %#v, want ttl=3600 bytes=40", payload.Memories[0])
	}
	if len(payload.Devices) != 0 {
		t.Fatalf("direct test hub should have no online devices: %#v", payload.Devices)
	}
}

func TestMemoryDetailParsesNewestFirstByDefault(t *testing.T) {
	cfg := testConfig()
	cfg.RedisKeyPrefix = "xiaoli:cp:"
	srv := NewServer(cfg)
	srv.memory = fakeMemoryReader{
		prefix: "xiaoli:cp:",
		values: map[string]memoryValue{
			"device-1": {
				Key:        "xiaoli:cp:device-1",
				DeviceID:   "device-1",
				TTLSeconds: 3600,
				Raw:        []byte(`[{"role":"user","content":"old prompt"},{"role":"assistant","content":"middle answer","response_meta":{"finish_reason":"stop","usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}},{"role":"user","content":"new prompt"}]`),
			},
		},
	}
	req := authenticatedRequest(t, srv, http.MethodGet, "/admin/api/memory/detail?device_id=device-1", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload struct {
		Enabled      bool   `json:"enabled"`
		Key          string `json:"key"`
		DeviceID     string `json:"device_id"`
		Order        string `json:"order"`
		MessageCount int    `json:"message_count"`
		Messages     []struct {
			Index        int    `json:"index"`
			Role         string `json:"role"`
			Content      string `json:"content"`
			FinishReason string `json:"finish_reason"`
			Usage        struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			} `json:"usage"`
		} `json:"messages"`
		RawJSON string `json:"raw_json"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !payload.Enabled || payload.Key != "xiaoli:cp:device-1" || payload.DeviceID != "device-1" {
		t.Fatalf("unexpected detail metadata: %#v", payload)
	}
	if payload.Order != "newest" || payload.MessageCount != 3 || len(payload.Messages) != 3 {
		t.Fatalf("unexpected order/count/messages: %#v", payload)
	}
	if payload.Messages[0].Index != 2 || payload.Messages[0].Content != "new prompt" || payload.Messages[2].Content != "old prompt" {
		t.Fatalf("messages should default newest first: %#v", payload.Messages)
	}
	if payload.Messages[1].FinishReason != "stop" || payload.Messages[1].Usage.TotalTokens != 13 {
		t.Fatalf("assistant metadata was not summarized: %#v", payload.Messages[1])
	}
	if !strings.Contains(payload.RawJSON, `"old prompt"`) {
		t.Fatalf("raw_json missing original payload: %s", payload.RawJSON)
	}
}

func TestMemoryDetailCanReturnOldestFirst(t *testing.T) {
	cfg := testConfig()
	srv := NewServer(cfg)
	srv.memory = fakeMemoryReader{
		prefix: "xiaoli:cp:",
		values: map[string]memoryValue{
			"device-1": {
				Key:      "xiaoli:cp:device-1",
				DeviceID: "device-1",
				Raw:      []byte(`[{"role":"user","content":"old"},{"role":"assistant","content":"new"}]`),
			},
		},
	}
	req := authenticatedRequest(t, srv, http.MethodGet, "/admin/api/memory/detail?device_id=device-1&order=oldest", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload struct {
		Order    string `json:"order"`
		Messages []struct {
			Index   int    `json:"index"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Order != "oldest" || payload.Messages[0].Index != 0 || payload.Messages[0].Content != "old" {
		t.Fatalf("messages should be oldest first: %#v", payload)
	}
}

func TestMemoryAPIReturnsDisabledWhenRedisMemoryIsNotConfigured(t *testing.T) {
	srv := NewServer(testConfig())
	req := authenticatedRequest(t, srv, http.MethodGet, "/admin/api/memory", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload struct {
		Enabled  bool   `json:"enabled"`
		Prefix   string `json:"prefix"`
		Memories []any  `json:"memories"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Enabled || payload.Prefix != testConfig().RedisKeyPrefix || len(payload.Memories) != 0 {
		t.Fatalf("unexpected disabled memory response: %#v", payload)
	}
}

func TestMemoryDetailReturnsRawJSONWhenParsingFails(t *testing.T) {
	cfg := testConfig()
	srv := NewServer(cfg)
	srv.memory = fakeMemoryReader{
		prefix: "xiaoli:cp:",
		values: map[string]memoryValue{
			"device-1": {
				Key:      "xiaoli:cp:device-1",
				DeviceID: "device-1",
				Raw:      []byte(`{"not":"an array"`),
			},
		},
	}
	req := authenticatedRequest(t, srv, http.MethodGet, "/admin/api/memory/detail?device_id=device-1", nil)
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload struct {
		ParseError string `json:"parse_error"`
		RawJSON    string `json:"raw_json"`
		Messages   []any  `json:"messages"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.ParseError == "" || payload.RawJSON != `{"not":"an array"` || len(payload.Messages) != 0 {
		t.Fatalf("malformed JSON response = %#v", payload)
	}
}

func TestStreamStartAPIInvokesCameraStreamWithResolution(t *testing.T) {
	var received BridgeCallRequest
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/bridge/call" {
			t.Fatalf("path = %s, want /bridge/call", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode bridge request: %v", err)
		}
		return jsonResponse(http.StatusOK, BridgeCallResult{OK: true, Raw: `{"ok":true}`, Result: map[string]any{"ok": true}}), nil
	})}

	cfg := testConfig()
	srv := NewServer(cfg)
	srv.bridge = NewBridgeClient("http://bridge.local", httpClient)
	session, err := srv.signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  cfg.now().Unix(),
		"exp":  cfg.now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	body := `{"device_id":"device-1","fps":3,"duration_sec":60,"resolution":"qvga"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/stream/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if received.DeviceID != "device-1" || received.Tool != "self.camera.start_stream" {
		t.Fatalf("unexpected bridge request: %#v", received)
	}
	if received.Arguments["resolution"] != "qvga" {
		t.Fatalf("resolution argument = %#v, want qvga", received.Arguments["resolution"])
	}
	if received.Arguments["fps"] != float64(3) && received.Arguments["fps"] != 3 {
		t.Fatalf("fps argument = %#v, want 3", received.Arguments["fps"])
	}
}

func TestStreamStartAPIRequestsLanTransportByDefault(t *testing.T) {
	var received BridgeCallRequest
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode bridge request: %v", err)
		}
		return jsonResponse(http.StatusOK, BridgeCallResult{
			OK:     true,
			Raw:    `{"ok":true,"transport":"lan","mjpeg_url":"http://192.168.1.50:8081/stream"}`,
			Result: map[string]any{"ok": true, "transport": "lan", "mjpeg_url": "http://192.168.1.50:8081/stream"},
		}), nil
	})}

	cfg := testConfig()
	srv := NewServer(cfg)
	srv.bridge = NewBridgeClient("http://bridge.local", httpClient)
	session, err := srv.signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  cfg.now().Unix(),
		"exp":  cfg.now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	body := `{"device_id":"device-1","fps":1,"duration_sec":30,"resolution":"vga"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/stream/start", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if received.Arguments["transport"] != "lan" {
		t.Fatalf("transport argument = %#v, want lan", received.Arguments["transport"])
	}
	var payload BridgeCallResult
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	result, ok := payload.Result.(map[string]any)
	if !ok {
		t.Fatalf("response result has unexpected type: %#v", payload.Result)
	}
	if stringValue(result["mjpeg_url"]) != "http://192.168.1.50:8081/stream" {
		t.Fatalf("mjpeg_url was not returned to dashboard: %#v", result)
	}
}

func TestDashboardCanRenderLanMjpegStream(t *testing.T) {
	html := dashboardHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`let directStreamURL = "";`,
		`function renderDirectStream(url)`,
		`renderDirectStream(result.mjpeg_url);`,
		`transport: "lan"`,
		`setStreamStatus("局域网直连播放中")`,
		`setStreamStatus("退回后台中转流")`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing LAN stream fragment %s", fragment)
		}
	}
}

func TestDashboardBuildsDefaultToolArgumentsFromSchema(t *testing.T) {
	html := dashboardHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`function defaultValueForSchema(key, schema)`,
		`if (key === "question") return "请描述这张图片里的内容。";`,
		`if (["query", "prompt", "text", "message", "instruction"].includes(key)) return "请帮我执行这个工具。";`,
		`if (schema.minimum !== undefined) return schema.minimum;`,
		`if (schema.type === "array") return [];`,
		`function updateArgsFromTool()`,
		`const tool = tools.find(t => ((t.function || {}).name || "") === toolName);`,
		`const params = ((tool || {}).function || {}).parameters || {};`,
		`const required = new Set(params.required || []);`,
		`if (required.has(key) || schema.default !== undefined || Object.keys(props).length <= 8)`,
		`$("args").value = JSON.stringify(args, null, 2);`,
		`$("tool").onchange = updateArgsFromTool;`,
		`updateArgsFromTool();`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing schema argument fragment %s", fragment)
		}
	}
}

func TestNormalizeAdminToolsTranslatesRawMCPTools(t *testing.T) {
	tools := normalizeAdminTools([]map[string]any{
		{
			"name":        "self.camera.snapshot",
			"description": "拍照",
			"inputSchema": map[string]any{
				"type":     "object",
				"required": []any{"resolution"},
				"properties": map[string]any{
					"resolution": map[string]any{"type": "string", "default": "qvga"},
				},
			},
		},
		{"description": "missing name"},
	})

	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1", len(tools))
	}
	fn, ok := tools[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("function has unexpected type: %#v", tools[0]["function"])
	}
	if fn["name"] != "self.camera.snapshot" || fn["description"] != "拍照" {
		t.Fatalf("unexpected function metadata: %#v", fn)
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("parameters has unexpected type: %#v", fn["parameters"])
	}
	if params["type"] != "object" {
		t.Fatalf("parameters type = %#v, want object", params["type"])
	}
	if tools[0]["type"] != "function" {
		t.Fatalf("tool type = %#v, want function", tools[0]["type"])
	}
}

func TestDashboardUsesLongTimeoutForCameraTools(t *testing.T) {
	html := dashboardHTML(map[string]any{"sub": "logto-user"})

	for _, fragment := range []string{
		`function timeoutForTool(name)`,
		`camera|photo|vision|image|snapshot|拍照|摄像`,
		`timeout: timeoutForTool(name)`,
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing timeout fragment %s", fragment)
		}
	}
}

func TestBridgeClientCallsLocalBridge(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/bridge/call" {
			t.Fatalf("path = %s, want /bridge/call", r.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["device_id"] != "device-1" || body["tool"] != "self.get_device_status" {
			t.Fatalf("unexpected body: %#v", body)
		}
		return jsonResponse(http.StatusOK, map[string]any{
			"ok":         true,
			"raw":        `{"battery":90}`,
			"result":     map[string]any{"battery": 90},
			"elapsed_ms": 12,
		}), nil
	})}

	client := NewBridgeClient("http://bridge.local", httpClient)
	result, err := client.Call(context.Background(), BridgeCallRequest{
		DeviceID:  "device-1",
		Tool:      "self.get_device_status",
		Arguments: map[string]any{},
		Timeout:   10,
	})
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !result.OK || result.ElapsedMS != 12 {
		t.Fatalf("unexpected result: %#v", result)
	}
}

func TestCameraToolCallUsesLongTimeoutEvenWhenClientSendsShortTimeout(t *testing.T) {
	var received BridgeCallRequest
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/bridge/call" {
			t.Fatalf("path = %s, want /bridge/call", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode bridge request: %v", err)
		}
		return jsonResponse(http.StatusOK, BridgeCallResult{OK: true, Raw: `{}`, Result: map[string]any{}}), nil
	})}

	cfg := testConfig()
	srv := NewServer(cfg)
	srv.bridge = NewBridgeClient("http://bridge.local", httpClient)
	session, err := srv.signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  cfg.now().Unix(),
		"exp":  cfg.now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	body := `{"device_id":"device-1","tool":"self.camera.take_photo","arguments":{"question":"请描述这张图片里的内容。"},"timeout":30}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/call", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if received.Timeout != 120 {
		t.Fatalf("bridge timeout = %d, want 120", received.Timeout)
	}
}

func TestSnapshotAPIInvokesModelFreeSnapshotToolWithResolution(t *testing.T) {
	var received BridgeCallRequest
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path != "/bridge/call" {
			t.Fatalf("path = %s, want /bridge/call", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode bridge request: %v", err)
		}
		return jsonResponse(http.StatusOK, BridgeCallResult{
			OK:     true,
			Raw:    `{"ok":true}`,
			Result: map[string]any{"ok": true, "image_url": "/admin/api/images/img-1"},
		}), nil
	})}

	cfg := testConfig()
	srv := NewServer(cfg)
	srv.bridge = NewBridgeClient("http://bridge.local", httpClient)
	session, err := srv.signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  cfg.now().Unix(),
		"exp":  cfg.now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	body := `{"device_id":"device-1","resolution":"legacy_vga"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/api/snapshot", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if received.DeviceID != "device-1" || received.Tool != "self.camera.snapshot" {
		t.Fatalf("unexpected bridge request: %#v", received)
	}
	if received.Arguments["resolution"] != "legacy_vga" {
		t.Fatalf("resolution argument = %#v, want legacy_vga", received.Arguments["resolution"])
	}
	if received.Timeout != 60 {
		t.Fatalf("timeout = %d, want 60", received.Timeout)
	}
}

func TestVisionProxyStoresImageAndForwardsRequest(t *testing.T) {
	var forwardedPath string
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		forwardedPath = r.URL.RequestURI()
		return &http.Response{
			StatusCode: http.StatusAccepted,
			Header:     http.Header{"X-Upstream": {"ok"}},
			Body:       io.NopCloser(strings.NewReader("proxied")),
		}, nil
	})}

	cfg := testConfig()
	cfg.VisionProxyBaseURL = "http://vision.local"
	srv := NewServer(cfg)
	srv.httpClient = httpClient
	req := httptest.NewRequest(http.MethodPost, "/mcp/vision/explain?x=1", strings.NewReader("jpeg-bytes"))
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("device-id", "device-1")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusAccepted)
	}
	if forwardedPath != "/mcp/vision/explain?x=1" {
		t.Fatalf("forwarded path = %q", forwardedPath)
	}
	urls := srv.recentDeviceImageURLs("device-1", time.Unix(0, 0))
	if len(urls) != 1 || !strings.HasPrefix(urls[0], "/admin/api/images/") {
		t.Fatalf("recent urls = %#v", urls)
	}
}

func TestVisionSnapshotStoresImageWithoutForwardingToModel(t *testing.T) {
	httpClient := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatalf("snapshot request should not be forwarded to vision model: %s", r.URL.String())
		return nil, nil
	})}

	cfg := testConfig()
	cfg.VisionProxyBaseURL = "http://vision.local"
	srv := NewServer(cfg)
	srv.httpClient = httpClient
	req := httptest.NewRequest(http.MethodPost, "/mcp/vision/snapshot", strings.NewReader("jpeg-bytes"))
	req.Header.Set("Content-Type", "image/jpeg")
	req.Header.Set("Device-Id", "device-1")
	req.Header.Set("X-Xiaoli-Resolution", "vga")
	req.Header.Set("X-Xiaoli-Width", "640")
	req.Header.Set("X-Xiaoli-Height", "480")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["ok"] != true || payload["resolution"] != "vga" || payload["width"] != float64(640) || payload["height"] != float64(480) {
		t.Fatalf("unexpected snapshot response: %#v", payload)
	}
	if !strings.HasPrefix(stringValue(payload["image_url"]), "/admin/api/images/") {
		t.Fatalf("image_url = %#v, want admin image url", payload["image_url"])
	}
	urls := srv.recentDeviceImageURLs("device-1", time.Unix(0, 0))
	if len(urls) != 1 || urls[0] != payload["image_url"] {
		t.Fatalf("recent urls = %#v, response url = %#v", urls, payload["image_url"])
	}
	latest := srv.stream.latest("device-1")
	if latest == nil || latest.ContentType != "image/jpeg" || !strings.HasPrefix(latest.Image, "data:image/jpeg;base64,") {
		t.Fatalf("latest stream event = %#v", latest)
	}
}

func TestCameraPreviewShowsOnlyLatestStoredImage(t *testing.T) {
	cfg := testConfig()
	current := cfg.Now()
	cfg.Now = func() time.Time { return current }
	srv := NewServer(cfg)

	firstID := srv.storeVisionImage("device-1", "image/jpeg", []byte("first"))
	current = current.Add(time.Second)
	secondID := srv.storeVisionImage("device-1", "image/jpeg", []byte("second"))
	current = current.Add(time.Second)
	latestID := srv.storeVisionImage("device-1", "image/jpeg", []byte("latest"))

	preview := srv.buildResultPreviewForCall("device-1", "self.camera.snapshot", map[string]any{"ok": true}, time.Unix(0, 0))
	images, ok := preview["images"].([]string)
	if !ok {
		t.Fatalf("preview images has unexpected type: %#v", preview["images"])
	}
	if len(images) != 1 {
		t.Fatalf("preview images = %#v, want exactly one latest image", images)
	}
	want := "/admin/api/images/" + latestID
	if images[0] != want {
		t.Fatalf("preview image = %q, want %q; older ids were %s and %s", images[0], want, firstID, secondID)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func jsonResponse(status int, payload any) *http.Response {
	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(payload)
	return &http.Response{
		StatusCode: status,
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(&body),
	}
}

func TestInternalStreamFrameRequiresTokenAndPublishesLatest(t *testing.T) {
	srv := NewServer(testConfig())
	body := `{"device_id":"device-1","content_type":"image/jpeg","data":"abc123","stream_id":"s","seq":"7"}`
	req := httptest.NewRequest(http.MethodPost, "/admin/internal/stream/frame", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want %d", rr.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodPost, "/admin/internal/stream/frame", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Xiaoli-Internal-Token", testConfig().InternalStreamToken)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status with token = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	latest := srv.stream.latest("device-1")
	if latest == nil || latest.Seq != "7" {
		t.Fatalf("latest stream event = %#v", latest)
	}
}

func TestInternalLatestImageRequiresTokenAndReturnsNewestImage(t *testing.T) {
	cfg := testConfig()
	current := cfg.Now()
	cfg.Now = func() time.Time { return current }
	srv := NewServer(cfg)
	srv.storeVisionImage("device-1", "image/jpeg", []byte("old-image"))
	current = current.Add(time.Second)
	srv.storeVisionImage("device-1", "image/png", []byte("new-image"))

	req := httptest.NewRequest(http.MethodGet, "/admin/internal/images/latest?device_id=device-1", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status without token = %d, want %d", rr.Code, http.StatusUnauthorized)
	}

	req = httptest.NewRequest(http.MethodGet, "/admin/internal/images/latest?device_id=device-1", nil)
	req.Header.Set("X-Xiaoli-Internal-Token", cfg.InternalStreamToken)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status with token = %d, want %d body=%s", rr.Code, http.StatusOK, rr.Body.String())
	}
	if got := rr.Header().Get("Content-Type"); got != "image/png" {
		t.Fatalf("Content-Type = %q, want image/png", got)
	}
	if got := rr.Body.String(); got != "new-image" {
		t.Fatalf("body = %q, want latest image", got)
	}
}

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}

type fakeMemoryReader struct {
	prefix string
	keys   []memoryKeyInfo
	values map[string]memoryValue
}

func (f fakeMemoryReader) Enabled() bool { return true }

func (f fakeMemoryReader) Prefix() string { return f.prefix }

func (f fakeMemoryReader) List(ctx context.Context, limit int) ([]memoryKeyInfo, error) {
	if limit > 0 && len(f.keys) > limit {
		return f.keys[:limit], nil
	}
	return f.keys, nil
}

func (f fakeMemoryReader) LoadRaw(ctx context.Context, deviceID string) (memoryValue, error) {
	value, ok := f.values[deviceID]
	if !ok {
		return memoryValue{}, redisNilError{}
	}
	return value, nil
}

type redisNilError struct{}

func (redisNilError) Error() string { return "redis: nil" }

func authenticatedRequest(t *testing.T, srv *AdminServer, method string, target string, body io.Reader) *http.Request {
	t.Helper()
	session, err := srv.signer.sign(map[string]any{
		"user": map[string]any{"sub": "logto-user"},
		"iat":  srv.cfg.now().Unix(),
		"exp":  srv.cfg.now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("sign session: %v", err)
	}
	req := httptest.NewRequest(method, target, body)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: session})
	return req
}
