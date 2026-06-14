package admin

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Host                     string
	Port                     int
	PublicBaseURL            string
	SessionSecret            string
	SessionMaxAge            time.Duration
	LogtoEndpoint            string
	LogtoAppID               string
	LogtoAppSecret           string
	AllowedUsers             []string
	DirectDeviceServer       bool
	DeviceAuthEnabled        bool
	DeviceAuthKey            string
	AllowedDeviceIDs         []string
	BridgeBaseURL            string
	VisionProxyBaseURL       string
	InternalStreamToken      string
	MCPReadyWait             time.Duration
	GoASRURL                 string
	GoASRAPIKey              string
	GoASRModel               string
	GoASRTimeout             time.Duration
	GoLLMURL                 string
	GoLLMAPIKey              string
	GoLLMModel               string
	GoLLMPrompt              string
	GoLLMTimeout             time.Duration
	GoVLLMURL                string
	GoVLLMAPIKey             string
	GoVLLMModel              string
	GoVLLMTimeout            time.Duration
	GoTTSURL                 string
	GoTTSAPIKey              string
	GoTTSModel               string
	GoTTSVoice               string
	GoTTSResponseFormat      string
	GoTTSTimeout             time.Duration
	ExternalMCPURLs          []string
	MCPConfigPath            string
	StudyMonitorEnabled      bool
	StudyMonitorTimezone     string
	StudyMonitorStartHour    int
	StudyMonitorEndHour      int
	StudyMonitorInterval     time.Duration
	StudyMonitorCameraTool   string
	StudyMonitorReminder     string
	StudyMonitorToolTimeout  time.Duration
	StudyMonitorDeviceIDs    []string
	MorningGreetingEnabled   bool
	MorningGreetingTimezone  string
	MorningGreetingHour      int
	MorningGreetingMinute    int
	MorningGreetingText      string
	MorningGreetingDeviceIDs []string
	LarkWebhookURL           string
	LarkAppID                string
	LarkAppToken             string
	RedisURL                 string
	RedisKeyPrefix           string
	MemoryTTL                time.Duration
	Now                      func() time.Time
}

func LoadConfig() Config {
	sessionSecret := env("ADMIN_SESSION_SECRET", "")
	cfg := Config{
		Host:                     env("XIAOLI_ADMIN_HOST", "0.0.0.0"),
		Port:                     envInt("XIAOLI_ADMIN_PORT", 8004),
		PublicBaseURL:            strings.TrimRight(env("ADMIN_PUBLIC_BASE_URL", env("PUBLIC_BASE_URL", "https://xiaoli-server.fly.dev")), "/"),
		SessionSecret:            sessionSecret,
		SessionMaxAge:            time.Duration(envInt("ADMIN_SESSION_MAX_AGE_SECONDS", 604800)) * time.Second,
		LogtoEndpoint:            strings.TrimRight(env("LOGTO_ENDPOINT", ""), "/") + "/",
		LogtoAppID:               env("LOGTO_APP_ID", ""),
		LogtoAppSecret:           env("LOGTO_APP_SECRET", ""),
		AllowedUsers:             csv(env("ADMIN_ALLOWED_USERS", "")),
		DirectDeviceServer:       envBool("XIAOLI_DIRECT_DEVICE_SERVER", false),
		DeviceAuthEnabled:        envBool("ENABLE_SERVER_AUTH", false),
		DeviceAuthKey:            env("SERVER_AUTH_KEY", ""),
		AllowedDeviceIDs:         csv(firstNonEmptyEnv("ALLOWED_DEVICE_IDS", "ALLOWED_DEVICE_ID", "SERVER_AUTH_ALLOWED_DEVICE_IDS")),
		BridgeBaseURL:            strings.TrimRight(env("XIAOLI_BRIDGE_BASE_URL", "http://127.0.0.1:8005"), "/"),
		VisionProxyBaseURL:       strings.TrimRight(env("XIAOLI_VISION_PROXY_BASE_URL", "http://127.0.0.1:8003"), "/"),
		InternalStreamToken:      env("XIAOLI_ADMIN_INTERNAL_TOKEN", sessionSecret),
		MCPReadyWait:             time.Duration(envFloat("ADMIN_MCP_READY_WAIT_SECONDS", 5)) * time.Second,
		GoASRURL:                 env("XIAOLI_GO_ASR_URL", env("OPENAI_ASR_BASE_URL", "https://api.siliconflow.cn/v1/audio/transcriptions")),
		GoASRAPIKey:              env("XIAOLI_GO_ASR_API_KEY", firstNonEmptyEnv("SILICONFLOW_API_KEY", "OPENAI_API_KEY", "GROQ_API_KEY")),
		GoASRModel:               env("XIAOLI_GO_ASR_MODEL", env("SILICONFLOW_ASR_MODEL", "FunAudioLLM/SenseVoiceSmall")),
		GoASRTimeout:             time.Duration(envInt("XIAOLI_GO_ASR_TIMEOUT_SECONDS", 45)) * time.Second,
		GoLLMURL:                 env("XIAOLI_GO_LLM_URL", "https://api.siliconflow.cn/v1/chat/completions"),
		GoLLMAPIKey:              env("XIAOLI_GO_LLM_API_KEY", firstNonEmptyEnv("SILICONFLOW_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY")),
		GoLLMModel:               env("XIAOLI_GO_LLM_MODEL", env("SILICONFLOW_LLM_MODEL", "Qwen/Qwen3-8B")),
		GoLLMPrompt:              env("XIAOLI_GO_LLM_PROMPT", "你是一个叫小李的中文语音助手。回答要简短、自然、适合通过扬声器播放。"),
		GoLLMTimeout:             time.Duration(envInt("XIAOLI_GO_LLM_TIMEOUT_SECONDS", 45)) * time.Second,
		GoVLLMURL:                env("XIAOLI_GO_VLLM_URL", "https://api.siliconflow.cn/v1/chat/completions"),
		GoVLLMAPIKey:             env("XIAOLI_GO_VLLM_API_KEY", firstNonEmptyEnv("SILICONFLOW_API_KEY", "OPENROUTER_API_KEY", "OPENAI_API_KEY")),
		GoVLLMModel:              env("XIAOLI_GO_VLLM_MODEL", env("SILICONFLOW_VLLM_MODEL", "Qwen/Qwen3-VL-8B-Instruct")),
		GoVLLMTimeout:            time.Duration(envInt("XIAOLI_GO_VLLM_TIMEOUT_SECONDS", 60)) * time.Second,
		GoTTSURL:                 env("XIAOLI_GO_TTS_URL", "https://api.siliconflow.cn/v1/audio/speech"),
		GoTTSAPIKey:              env("XIAOLI_GO_TTS_API_KEY", env("SILICONFLOW_API_KEY", "")),
		GoTTSModel:               env("XIAOLI_GO_TTS_MODEL", env("SILICONFLOW_TTS_MODEL", "FunAudioLLM/CosyVoice2-0.5B")),
		GoTTSVoice:               env("XIAOLI_GO_TTS_VOICE", env("SILICONFLOW_TTS_VOICE", "FunAudioLLM/CosyVoice2-0.5B:anna")),
		GoTTSResponseFormat:      env("XIAOLI_GO_TTS_RESPONSE_FORMAT", "opus"),
		GoTTSTimeout:             time.Duration(envInt("XIAOLI_GO_TTS_TIMEOUT_SECONDS", 30)) * time.Second,
		MCPConfigPath:            env("MCP_SERVERS_CONFIG", "mcp-servers.json"),
		ExternalMCPURLs:          loadMCPConfigFile(env("MCP_SERVERS_CONFIG", "mcp-servers.json")),
		StudyMonitorEnabled:      envBool("STUDY_MONITOR_ENABLED", false),
		StudyMonitorTimezone:     env("STUDY_MONITOR_TIMEZONE", "Asia/Shanghai"),
		StudyMonitorStartHour:    envInt("STUDY_MONITOR_START_HOUR", 17),
		StudyMonitorEndHour:      envInt("STUDY_MONITOR_END_HOUR", 21),
		StudyMonitorInterval:     time.Duration(envInt("STUDY_MONITOR_INTERVAL_SECONDS", 300)) * time.Second,
		StudyMonitorCameraTool:   env("STUDY_MONITOR_CAMERA_TOOL", "self.camera.take_photo"),
		StudyMonitorReminder:     env("STUDY_MONITOR_REMINDER_TEXT", "请坐直，认真学习。"),
		StudyMonitorToolTimeout:  time.Duration(envInt("STUDY_MONITOR_TOOL_TIMEOUT_SECONDS", 120)) * time.Second,
		StudyMonitorDeviceIDs:    csv(env("STUDY_MONITOR_DEVICE_IDS", "")),
		MorningGreetingEnabled:   envBool("MORNING_GREETING_ENABLED", true),
		MorningGreetingTimezone:  env("MORNING_GREETING_TIMEZONE", "Asia/Shanghai"),
		MorningGreetingHour:      envInt("MORNING_GREETING_HOUR", 8),
		MorningGreetingMinute:    envInt("MORNING_GREETING_MINUTE", 0),
		MorningGreetingText:      env("MORNING_GREETING_TEXT", "早上好。"),
		MorningGreetingDeviceIDs: csv(env("MORNING_GREETING_DEVICE_IDS", "")),
		LarkWebhookURL:           env("LARK_BOT_WEBHOOK_URL", ""),
		LarkAppID:                env("LARK_APP_ID", ""),
		LarkAppToken:             env("LARK_APP_TOKEN", ""),
		RedisURL:                 env("XIAOLI_REDIS_URL", ""),
		RedisKeyPrefix:           env("XIAOLI_REDIS_KEY_PREFIX", "xiaoli:cp:"),
		MemoryTTL:                time.Duration(envInt("XIAOLI_MEMORY_TTL_HOURS", 24)) * time.Hour,
	}
	return cfg
}

func (c Config) LarkEnabled() bool {
	return strings.TrimSpace(c.LarkAppID) != "" && strings.TrimSpace(c.LarkAppToken) != ""
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

func firstNonEmptyEnv(names ...string) string {
	for _, name := range names {
		if value := os.Getenv(name); value != "" {
			return value
		}
	}
	return ""
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

type mcpServerConfig struct {
	MCPServers []struct {
		Name string `json:"name"`
		URL  string `json:"url"`
	} `json:"mcp_servers"`
}

func loadMCPConfigFile(path string) []string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var cfg mcpServerConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		log.Printf("mcp config: skip %s (parse error: %v)", path, err)
		return nil
	}
	var urls []string
	for _, s := range cfg.MCPServers {
		if s.URL != "" {
			urls = append(urls, s.URL)
		}
	}
	return urls
}
