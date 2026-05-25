package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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
		`id="speakText"`,
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
}

func TestSchedulesAPIIncludesStudyMonitorTask(t *testing.T) {
	cfg := testConfig()
	cfg.StudyMonitorEnabled = true
	cfg.StudyMonitorTimezone = "Asia/Shanghai"
	cfg.StudyMonitorStartHour = 17
	cfg.StudyMonitorEndHour = 21
	cfg.StudyMonitorInterval = 5 * time.Minute
	cfg.StudyMonitorCameraTool = "self.camera.take_photo"
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
	if len(payload.Schedules) != 1 {
		t.Fatalf("schedules length = %d, want 1", len(payload.Schedules))
	}
	task := payload.Schedules[0]
	if task["id"] != "study_monitor" || task["enabled"] != true || task["timezone"] != "Asia/Shanghai" {
		t.Fatalf("unexpected schedule task: %#v", task)
	}
	if task["interval_seconds"] != float64(300) || task["window"] != "17:00-21:00" {
		t.Fatalf("unexpected schedule timing: %#v", task)
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
	} {
		if !strings.Contains(html, fragment) {
			t.Fatalf("dashboard HTML missing snapshot fragment %s", fragment)
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

func findCookie(cookies []*http.Cookie, name string) *http.Cookie {
	for _, cookie := range cookies {
		if cookie.Name == name {
			return cookie
		}
	}
	return nil
}
