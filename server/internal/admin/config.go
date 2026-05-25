package admin

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host                    string
	Port                    int
	PublicBaseURL           string
	SessionSecret           string
	SessionMaxAge           time.Duration
	LogtoEndpoint           string
	LogtoAppID              string
	LogtoAppSecret          string
	AllowedUsers            []string
	BridgeBaseURL           string
	VisionProxyBaseURL      string
	InternalStreamToken     string
	MCPReadyWait            time.Duration
	StudyMonitorEnabled     bool
	StudyMonitorTimezone    string
	StudyMonitorStartHour   int
	StudyMonitorEndHour     int
	StudyMonitorInterval    time.Duration
	StudyMonitorCameraTool  string
	StudyMonitorReminder    string
	StudyMonitorToolTimeout time.Duration
	LarkWebhookURL          string
	LarkAppID               string
	LarkAppSecret           string
	Now                     func() time.Time
}

func LoadConfig() Config {
	sessionSecret := env("ADMIN_SESSION_SECRET", "")
	cfg := Config{
		Host:                    env("XIAOLI_ADMIN_HOST", "0.0.0.0"),
		Port:                    envInt("XIAOLI_ADMIN_PORT", 8004),
		PublicBaseURL:           strings.TrimRight(env("ADMIN_PUBLIC_BASE_URL", env("PUBLIC_BASE_URL", "https://xiaoli-server.fly.dev")), "/"),
		SessionSecret:           sessionSecret,
		SessionMaxAge:           time.Duration(envInt("ADMIN_SESSION_MAX_AGE_SECONDS", 604800)) * time.Second,
		LogtoEndpoint:           strings.TrimRight(env("LOGTO_ENDPOINT", ""), "/") + "/",
		LogtoAppID:              env("LOGTO_APP_ID", ""),
		LogtoAppSecret:          env("LOGTO_APP_SECRET", ""),
		AllowedUsers:            csv(env("ADMIN_ALLOWED_USERS", "")),
		BridgeBaseURL:           strings.TrimRight(env("XIAOLI_BRIDGE_BASE_URL", "http://127.0.0.1:8005"), "/"),
		VisionProxyBaseURL:      strings.TrimRight(env("XIAOLI_VISION_PROXY_BASE_URL", "http://127.0.0.1:8003"), "/"),
		InternalStreamToken:     env("XIAOLI_ADMIN_INTERNAL_TOKEN", sessionSecret),
		MCPReadyWait:            time.Duration(envFloat("ADMIN_MCP_READY_WAIT_SECONDS", 5)) * time.Second,
		StudyMonitorEnabled:     envBool("STUDY_MONITOR_ENABLED", false),
		StudyMonitorTimezone:    env("STUDY_MONITOR_TIMEZONE", "Asia/Shanghai"),
		StudyMonitorStartHour:   envInt("STUDY_MONITOR_START_HOUR", 17),
		StudyMonitorEndHour:     envInt("STUDY_MONITOR_END_HOUR", 21),
		StudyMonitorInterval:    time.Duration(envInt("STUDY_MONITOR_INTERVAL_SECONDS", 300)) * time.Second,
		StudyMonitorCameraTool:  env("STUDY_MONITOR_CAMERA_TOOL", "self.camera.take_photo"),
		StudyMonitorReminder:    env("STUDY_MONITOR_REMINDER_TEXT", "请坐直，认真学习。"),
		StudyMonitorToolTimeout: time.Duration(envInt("STUDY_MONITOR_TOOL_TIMEOUT_SECONDS", 120)) * time.Second,
		LarkWebhookURL:          env("LARK_BOT_WEBHOOK_URL", ""),
		LarkAppID:               env("LARK_APP_ID", ""),
		LarkAppSecret:           env("LARK_APP_SECRET", ""),
	}
	return cfg
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envFloat(name string, fallback float64) float64 {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func envBool(name string, fallback bool) bool {
	value := strings.TrimSpace(strings.ToLower(os.Getenv(name)))
	if value == "" {
		return fallback
	}
	return value == "1" || value == "true" || value == "yes" || value == "on"
}

func csv(value string) []string {
	if value == "" {
		return nil
	}
	var items []string
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}
