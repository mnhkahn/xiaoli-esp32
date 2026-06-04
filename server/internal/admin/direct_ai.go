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
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cloudwego/eino-ext/components/model/openai"
	"xiaoli/server/pkg/langsmith"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/components/tool"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"

	einojsonschema "github.com/eino-contrib/jsonschema"
)

// ---------------------------------------------------------------------------
// Interfaces kept for ASR / Vision (not migrated to Eino yet)
// ---------------------------------------------------------------------------

type SpeechRecognizer interface {
	Transcribe(ctx context.Context, oggOpus []byte) (string, error)
}

type VisionAnalyzer interface {
	Analyze(ctx context.Context, question string, contentType string, image []byte) (string, error)
}

// ---------------------------------------------------------------------------
// ASR implementation (hand-rolled HTTP, kept as-is)
// ---------------------------------------------------------------------------

type openAITranscriber struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

func newOpenAITranscriber(cfg Config) SpeechRecognizer {
	if cfg.GoASRAPIKey == "" {
		return nil
	}
	return &openAITranscriber{
		url:    cfg.GoASRURL,
		apiKey: cfg.GoASRAPIKey,
		model:  cfg.GoASRModel,
		client: &http.Client{Timeout: cfg.GoASRTimeout},
	}
}

func (c *openAITranscriber) Transcribe(ctx context.Context, oggOpus []byte) (string, error) {
	if c == nil || c.apiKey == "" {
		return "", errors.New("ASR is not configured")
	}
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	if err := writer.WriteField("model", c.model); err != nil {
		return "", err
	}
	_ = writer.WriteField("response_format", "json")
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="speech.ogg"`)
	header.Set("Content-Type", "audio/ogg")
	part, err := writer.CreatePart(header)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(oggOpus); err != nil {
		return "", err
	}
	if err := writer.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, &body)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 600))
		return "", fmt.Errorf("ASR request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(errorBody)))
	}
	var payload map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	text := strings.TrimSpace(stringValue(payload["text"]))
	if text == "" {
		return "", errors.New("ASR returned empty text")
	}
	return text, nil
}

// ---------------------------------------------------------------------------
// Vision implementation (hand-rolled HTTP, kept as-is)
// ---------------------------------------------------------------------------

type openAIVisionClient struct {
	url    string
	apiKey string
	model  string
	client *http.Client
}

func newGoVisionClient(cfg Config) VisionAnalyzer {
	if cfg.GoVLLMAPIKey == "" {
		return nil
	}
	return &openAIVisionClient{
		url:    cfg.GoVLLMURL,
		apiKey: cfg.GoVLLMAPIKey,
		model:  cfg.GoVLLMModel,
		client: &http.Client{Timeout: cfg.GoVLLMTimeout},
	}
}

func (c *openAIVisionClient) Analyze(ctx context.Context, question string, contentType string, image []byte) (string, error) {
	if c == nil || c.apiKey == "" {
		return "", errors.New("vision model is not configured")
	}
	if question == "" {
		question = "请描述这张图片里的内容。"
	}
	dataURL := "data:" + normalizeImageContentType(contentType, "image/jpeg") + ";base64," + base64.StdEncoding.EncodeToString(image)
	payload := map[string]any{
		"model": c.model,
		"messages": []map[string]any{
			{"role": "system", "content": "你是一个视觉助手。回答要简短、直接，适合通过语音播放。"},
			{"role": "user", "content": []map[string]any{
				{"type": "text", "text": question},
				{"type": "image_url", "image_url": map[string]any{"url": dataURL}},
			}},
		},
		"temperature": 0.2,
		"max_tokens":  300,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		errorBody, _ := io.ReadAll(io.LimitReader(resp.Body, 800))
		return "", fmt.Errorf("vision request failed: status %d: %s", resp.StatusCode, strings.TrimSpace(string(errorBody)))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content any `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return "", err
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("vision model returned no choices")
	}
	answer := strings.TrimSpace(contentText(parsed.Choices[0].Message.Content))
	if answer == "" {
		return "", errors.New("vision model returned empty answer")
	}
	return answer, nil
}

func contentText(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case []any:
		var parts []string
		for _, item := range v {
			if m, ok := item.(map[string]any); ok {
				if text := stringValue(m["text"]); text != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	default:
		return stringValue(value)
	}
}

// ---------------------------------------------------------------------------
// Conversation memory backed by Redis
// ---------------------------------------------------------------------------

type redisMemory struct {
	client *redis.Client
	prefix string
	ttl    time.Duration
}

func newRedisMemory(cfg Config) *redisMemory {
	if cfg.RedisURL == "" {
		return nil
	}
	opts, err := redis.ParseURL(cfg.RedisURL)
	if err != nil {
		log.Printf("redis url parse failed: %v", err)
		return nil
	}
	client := redis.NewClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := client.Ping(ctx).Err(); err != nil {
		log.Printf("redis ping failed: %v", err)
		return nil
	}
	log.Printf("redis memory connected, prefix=%s ttl=%s", cfg.RedisKeyPrefix, cfg.MemoryTTL)
	return &redisMemory{client: client, prefix: cfg.RedisKeyPrefix, ttl: cfg.MemoryTTL}
}

const maxHistoryMessages = 40

func (m *redisMemory) Load(ctx context.Context, deviceID string) []*schema.Message {
	if m == nil {
		return nil
	}
	data, err := m.client.Get(ctx, m.prefix+deviceID).Bytes()
	if err == redis.Nil {
		return nil
	}
	if err != nil {
		log.Printf("redis load memory for %s: %v", deviceID, err)
		return nil
	}
	var msgs []*schema.Message
	if err := json.Unmarshal(data, &msgs); err != nil {
		log.Printf("redis unmarshal memory for %s: %v", deviceID, err)
		return nil
	}
	return msgs
}

func (m *redisMemory) Save(ctx context.Context, deviceID string, msgs []*schema.Message) {
	if m == nil {
		return
	}
	// Keep only last N messages to stay within budget
	if len(msgs) > maxHistoryMessages {
		msgs = msgs[len(msgs)-maxHistoryMessages:]
	}
	data, err := json.Marshal(msgs)
	if err != nil {
		log.Printf("redis marshal memory for %s: %v", deviceID, err)
		return
	}
	if err := m.client.Set(ctx, m.prefix+deviceID, data, m.ttl).Err(); err != nil {
		log.Printf("redis save memory for %s: %v", deviceID, err)
	}
}

// ---------------------------------------------------------------------------
// MCP Tool wrapper — bridges device MCP tools into Eino tool.BaseTool
// ---------------------------------------------------------------------------

type mcpTool struct {
	info     *schema.ToolInfo
	hub      *DeviceHub
	deviceID string
}

func (t *mcpTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *mcpTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		args = map[string]any{}
	}
	result, err := t.hub.Call(ctx, BridgeCallRequest{
		DeviceID:  t.deviceID,
		Tool:     t.info.Name,
		Arguments: args,
		Timeout:  30,
	})
	if err != nil {
		return fmt.Sprintf("tool call error: %v", err), nil
	}
	if result.Error != "" {
		return fmt.Sprintf("tool error: %s", result.Error), nil
	}
	raw, _ := json.Marshal(result.Result)
	return string(raw), nil
}

func mcpToolsToEinoTools(session *deviceSession, hub *DeviceHub) []tool.BaseTool {
	session.mu.Lock()
	rawTools := make([]map[string]any, len(session.tools))
	copy(rawTools, session.tools)
	session.mu.Unlock()

	var tools []tool.BaseTool
	for _, raw := range rawTools {
		name, _ := raw["name"].(string)
		if name == "" {
			continue
		}
		desc, _ := raw["description"].(string)
		if desc == "" {
			desc = name
		}

		var paramsOneOf *schema.ParamsOneOf
		if inputSchema, ok := raw["inputSchema"]; ok && inputSchema != nil {
			schemaBytes, err := json.Marshal(inputSchema)
			if err == nil {
				var js einojsonschema.Schema
				if err := json.Unmarshal(schemaBytes, &js); err == nil {
					paramsOneOf = schema.NewParamsOneOfByJSONSchema(&js)
				}
			}
		}

		tools = append(tools, &mcpTool{
			info: &schema.ToolInfo{
				Name:        name,
				Desc:        desc,
				ParamsOneOf: paramsOneOf,
			},
			hub:      hub,
			deviceID: session.deviceID,
		})
	}
	return tools
}

// ---------------------------------------------------------------------------
// EinoAgent — Eino-powered LLM with memory and skill support
// ---------------------------------------------------------------------------

type EinoAgent struct {
	chatModel  *openai.ChatModel
	memory     *redisMemory
	cfg        Config
	hub        *DeviceHub
	langsmith  *langsmith.CallbackHandler
}

func newEinoAgent(cfg Config) *EinoAgent {
	if cfg.GoLLMAPIKey == "" {
		return nil
	}
	baseURL := strings.TrimSuffix(cfg.GoLLMURL, "/chat/completions")
	baseURL = strings.TrimRight(baseURL, "/")

	ctx := context.Background()
	temp := float32(0.2)
	maxTokens := 180
	chatModel, err := openai.NewChatModel(ctx, &openai.ChatModelConfig{
		BaseURL:      baseURL,
		APIKey:       cfg.GoLLMAPIKey,
		Model:        cfg.GoLLMModel,
		Timeout:      cfg.GoLLMTimeout,
		Temperature:  &temp,
		MaxTokens:    &maxTokens,
	})
	if err != nil {
		log.Printf("eino chat model init failed: %v", err)
		return nil
	}

	memory := newRedisMemory(cfg)

	var lsHandler *langsmith.CallbackHandler
	if cfg.LangSmithTracing && cfg.LangSmithAPIKey != "" {
		var err2 error
		lsHandler, err2 = langsmith.NewLangsmithHandler(&langsmith.Config{
			APIKey:  cfg.LangSmithAPIKey,
			Timeout: 30 * time.Second,
		})
		if err2 != nil {
			log.Printf("langsmith handler init failed: %v", err2)
		} else {
			log.Printf("langsmith tracing enabled, project=%s", cfg.LangSmithProject)
		}
	}

	log.Printf("eino agent ready: model=%s base=%s redis=%v langsmith=%v", cfg.GoLLMModel, baseURL, memory != nil, lsHandler != nil)
	return &EinoAgent{chatModel: chatModel, memory: memory, cfg: cfg, langsmith: lsHandler}
}

func (a *EinoAgent) SetHub(hub *DeviceHub) {
	a.hub = hub
}

// Chat sends userText through the Eino agent with memory and optional MCP tools.
func (a *EinoAgent) Chat(ctx context.Context, deviceID string, userText string) (string, error) {
	log.Printf("EinoAgent.Chat called: device=%s text=%q langsmith=%v", deviceID, userText, a.langsmith != nil)

	// Load conversation history
	history := a.memory.Load(ctx, deviceID)

	// Build message list: system + history + new user message
	msgs := make([]*schema.Message, 0, len(history)+2)
	if a.cfg.GoLLMPrompt != "" {
		msgs = append(msgs, schema.SystemMessage(a.cfg.GoLLMPrompt))
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, schema.UserMessage(userText))

	// Build tool list from device's MCP tools
	var einoTools []tool.BaseTool
	if a.hub != nil {
		if session := a.hub.session(deviceID); session != nil {
			einoTools = mcpToolsToEinoTools(session, a.hub)
		}
	}

	// Create agent with tools
	agentCfg := &adk.ChatModelAgentConfig{
		Name:          "xiaoli",
		Instruction:   "", // already prepended as system message
		Model:         a.chatModel,
		MaxIterations: 10,
	}
	if len(einoTools) > 0 {
		agentCfg.ToolsConfig = adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: einoTools,
			},
		}
	}

	agent, err := adk.NewChatModelAgent(ctx, agentCfg)
	if err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}

	// Attach LangSmith tracing if configured — use Runner so callbacks
	// go through flowAgent.initAgentCallbacks into the context.
	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: agent,
	})
	var runOpts []adk.AgentRunOption
	if a.langsmith != nil {
		ctx = langsmith.SetTrace(ctx, langsmith.WithSessionName(a.cfg.LangSmithProject))
		runOpts = append(runOpts, adk.WithCallbacks(a.langsmith))
	}

	iter := runner.Run(ctx, msgs, runOpts...)

	var result *schema.Message
	eventCount := 0
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		eventCount++
		if event.Err != nil {
			log.Printf("EinoAgent.Chat event error: %v", event.Err)
			return "", fmt.Errorf("agent error: %w", event.Err)
		}
		log.Printf("EinoAgent.Chat event[%d]: output=%v", eventCount, event.Output != nil)
		if event.Output != nil && event.Output.MessageOutput != nil &&
			event.Output.MessageOutput.Message != nil &&
			event.Output.MessageOutput.Role == schema.Assistant {
			result = event.Output.MessageOutput.Message
			log.Printf("EinoAgent.Chat assistant: %q", result.Content)
		}
	}
	log.Printf("EinoAgent.Chat done: events=%d hasResult=%v", eventCount, result != nil)
	if result == nil || result.Content == "" {
		return "", fmt.Errorf("agent returned empty response")
	}

	// Update conversation history
	updated := append(history,
		schema.UserMessage(userText),
		result,
	)
	a.memory.Save(ctx, deviceID, updated)

	return result.Content, nil
}
