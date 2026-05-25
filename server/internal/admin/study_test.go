package admin

import (
	"testing"
	"time"
)

func TestParseStudyDecisionFromJSON(t *testing.T) {
	srv := NewServer(testConfig())
	value := map[string]any{
		"need_reminder": true,
		"posture":       "低头过近",
		"focus":         "正在写作业",
		"summary":       "需要调整坐姿",
		"reminder_text": "抬头一点。",
	}

	decision := srv.parseStudyDecision(value)

	if !decision.NeedReminder {
		t.Fatal("NeedReminder = false, want true")
	}
	if decision.ReminderText != "抬头一点。" {
		t.Fatalf("ReminderText = %q", decision.ReminderText)
	}
	if decision.AnalysisText == "" {
		t.Fatal("AnalysisText is empty")
	}
}

func TestParseStudyDecisionFromNestedJSONText(t *testing.T) {
	srv := NewServer(testConfig())
	value := map[string]any{
		"result": `{"need_reminder":false,"summary":"坐姿端正，认真学习"}`,
	}

	decision := srv.parseStudyDecision(value)

	if decision.NeedReminder {
		t.Fatal("NeedReminder = true, want false")
	}
	if decision.AnalysisText != "坐姿端正，认真学习" {
		t.Fatalf("AnalysisText = %q", decision.AnalysisText)
	}
}

func TestStudyMonitorSlotUsesConfiguredWindow(t *testing.T) {
	cfg := testConfig()
	cfg.StudyMonitorTimezone = "Asia/Shanghai"
	cfg.StudyMonitorStartHour = 17
	cfg.StudyMonitorEndHour = 21
	cfg.StudyMonitorInterval = 5 * time.Minute
	srv := NewServer(cfg)
	inWindow := time.Date(2026, 5, 24, 17, 3, 20, 0, time.FixedZone("CST", 8*3600))
	outWindow := time.Date(2026, 5, 24, 21, 0, 0, 0, time.FixedZone("CST", 8*3600))

	if slot := srv.studyMonitorSlot(inWindow); slot == nil {
		t.Fatal("slot is nil inside monitor window")
	}
	if slot := srv.studyMonitorSlot(outWindow); slot != nil {
		t.Fatalf("slot = %v outside monitor window", *slot)
	}
}

func TestBuildLarkPostPayloadIncludesImageAndReminder(t *testing.T) {
	srv := NewServer(testConfig())
	payload := srv.buildLarkPostPayload(studyLarkPayloadInput{
		DeviceID:       "device-1",
		AnalysisText:   "低头过近",
		NeedReminder:   true,
		ReminderText:   "请坐直",
		ImageKey:       "img-key",
		CheckedAt:      time.Date(2026, 5, 24, 18, 30, 0, 0, time.FixedZone("CST", 8*3600)),
		ReminderResult: "queued",
		ElapsedMS:      123,
	})

	content := payload["content"].(map[string]any)
	post := content["post"].(map[string]any)
	zh := post["zh_cn"].(map[string]any)
	if zh["title"] != "学习状态巡检 2026-05-24 18:30" {
		t.Fatalf("title = %q", zh["title"])
	}
	lines := zh["content"].([][]map[string]string)
	foundImage := false
	for _, line := range lines {
		for _, item := range line {
			if item["tag"] == "img" && item["image_key"] == "img-key" {
				foundImage = true
			}
		}
	}
	if !foundImage {
		t.Fatal("lark post payload did not include image")
	}
}
