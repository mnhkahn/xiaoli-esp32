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
	"sync"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/cloudwego/eino-ext/components/model/openai"
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

type memoryReader interface {
	Enabled() bool
	Prefix() string
	List(ctx context.Context, limit int) ([]memoryKeyInfo, error)
	LoadRaw(ctx context.Context, deviceID string) (memoryValue, error)
}

type memoryKeyInfo struct {
	Key        string `json:"key"`
	DeviceID   string `json:"device_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
	Bytes      int    `json:"bytes"`
}

type memoryValue struct {
	Key        string `json:"key"`
	DeviceID   string `json:"device_id"`
	TTLSeconds int64  `json:"ttl_seconds"`
	Bytes      int    `json:"bytes"`
	Raw        []byte `json:"-"`
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

func (m *redisMemory) Enabled() bool {
	return m != nil && m.client != nil
}

func (m *redisMemory) Prefix() string {
	if m == nil {
		return ""
	}
	return m.prefix
}

func (m *redisMemory) List(ctx context.Context, limit int) ([]memoryKeyInfo, error) {
	if !m.Enabled() {
		return nil, errors.New("redis memory is not configured")
	}
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	pattern := m.prefix + "*"
	var cursor uint64
	items := make([]memoryKeyInfo, 0)
	for {
		keys, next, err := m.client.Scan(ctx, cursor, pattern, 100).Result()
		if err != nil {
			return nil, err
		}
		for _, key := range keys {
			info := memoryKeyInfo{
				Key:      key,
				DeviceID: strings.TrimPrefix(key, m.prefix),
			}
			if ttl, err := m.client.TTL(ctx, key).Result(); err == nil {
				info.TTLSeconds = ttlSeconds(ttl)
			}
			if size, err := m.client.StrLen(ctx, key).Result(); err == nil {
				info.Bytes = int(size)
			}
			items = append(items, info)
			if len(items) >= limit {
				return items, nil
			}
		}
		cursor = next
		if cursor == 0 {
			break
		}
	}
	return items, nil
}

func (m *redisMemory) LoadRaw(ctx context.Context, deviceID string) (memoryValue, error) {
	if !m.Enabled() {
		return memoryValue{}, errors.New("redis memory is not configured")
	}
	key := m.prefix + deviceID
	data, err := m.client.Get(ctx, key).Bytes()
	if err != nil {
		return memoryValue{}, err
	}
	value := memoryValue{
		Key:      key,
		DeviceID: deviceID,
		Bytes:    len(data),
		Raw:      data,
	}
	if ttl, err := m.client.TTL(ctx, key).Result(); err == nil {
		value.TTLSeconds = ttlSeconds(ttl)
	}
	return value, nil
}

func ttlSeconds(ttl time.Duration) int64 {
	if ttl < 0 {
		return int64(ttl)
	}
	return int64(ttl.Seconds())
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
		Tool:      t.info.Name,
		Arguments: args,
		Timeout:   30,
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
// external MCP client — automatically injects tools from remote MCP servers
// ---------------------------------------------------------------------------

type externalMCPTool struct {
	info     *schema.ToolInfo
	client   *externalMCPClient
	toolName string
}

func (t *externalMCPTool) Info(_ context.Context) (*schema.ToolInfo, error) {
	return t.info, nil
}

func (t *externalMCPTool) InvokableRun(ctx context.Context, argumentsInJSON string, _ ...tool.Option) (string, error) {
	var args map[string]any
	if err := json.Unmarshal([]byte(argumentsInJSON), &args); err != nil {
		args = map[string]any{}
	}
	return t.client.call(ctx, t.toolName, args)
}

type externalMCPClient struct {
	url       string
	sessionID string
	mu        sync.Mutex
}

func newExternalMCPClient(ctx context.Context, url string) (*externalMCPClient, error) {
	c := &externalMCPClient{url: url}
	sid, err := c.mcpInit(ctx)
	if err != nil {
		return nil, fmt.Errorf("mcp connect %s: %w", url, err)
	}
	c.sessionID = sid
	return c, nil
}

// listTools queries the MCP server for all available tools and wraps each as an Eino BaseTool.
func (c *externalMCPClient) listTools(ctx context.Context) ([]tool.BaseTool, error) {
	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/list",
		"params":  map[string]any{},
	})
	bodyStr, err := c.mcpPost(ctx, payload)
	if err != nil {
		return nil, err
	}

	var resp struct {
		Result *struct {
			Tools []map[string]any `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(bodyStr), &resp); err != nil {
		return nil, fmt.Errorf("parse tools/list: %w", err)
	}
	if resp.Result == nil {
		return nil, fmt.Errorf("tools/list: no result")
	}

	var tools []tool.BaseTool
	for _, raw := range resp.Result.Tools {
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

		tools = append(tools, &externalMCPTool{
			info: &schema.ToolInfo{
				Name:        name,
				Desc:        desc,
				ParamsOneOf: paramsOneOf,
			},
			client:   c,
			toolName: name,
		})
	}
	return tools, nil
}

// call invokes a tool on the remote MCP server.
func (c *externalMCPClient) call(ctx context.Context, toolName string, args map[string]any) (string, error) {
	c.mu.Lock()
	sessionID := c.sessionID
	c.mu.Unlock()

	payload, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      toolName,
			"arguments": args,
		},
	})
	bodyStr, err := c.mcpPost(ctx, payload, "Mcp-Session-Id", sessionID)
	if err != nil {
		// session might have expired — re-init and retry once
		if sid, reErr := c.reinit(ctx); reErr == nil {
			c.mu.Lock()
			c.sessionID = sid
			c.mu.Unlock()
			return c.call(ctx, toolName, args)
		}
		return fmt.Sprintf(`{"error":"tool call failed: %v"}`, err), nil
	}

	var mcpResp struct {
		Result *struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(bodyStr), &mcpResp); err != nil {
		return fmt.Sprintf(`{"error":"parse response: %v"}`, err), nil
	}
	if mcpResp.Error != nil {
		return fmt.Sprintf(`{"error":"MCP error code=%d: %s"}`, mcpResp.Error.Code, mcpResp.Error.Message), nil
	}
	if mcpResp.Result == nil || len(mcpResp.Result.Content) == 0 {
		return `{"error":"empty result"}`, nil
	}
	return mcpResp.Result.Content[0].Text, nil
}

func (c *externalMCPClient) mcpInit(ctx context.Context) (string, error) {
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"xiaoli-server","version":"1.0"}}}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	sessionID := resp.Header.Get("Mcp-Session-Id")
	if sessionID == "" {
		raw, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("MCP init: no session ID, body: %s", string(raw))
	}
	return sessionID, nil
}

func (c *externalMCPClient) reinit(ctx context.Context) (string, error) {
	sid, err := c.mcpInit(ctx)
	if err != nil {
		return "", err
	}
	return sid, nil
}

func (c *externalMCPClient) mcpPost(ctx context.Context, payload []byte, extraHeaders ...string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for i := 0; i+1 < len(extraHeaders); i += 2 {
		req.Header.Set(extraHeaders[i], extraHeaders[i+1])
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(resp.Body)
	bodyStr := string(raw)
	if idx := strings.Index(bodyStr, "data: "); idx >= 0 {
		bodyStr = bodyStr[idx+6:]
	}
	return bodyStr, nil
}

// ---------------------------------------------------------------------------
// EinoAgent — Eino-powered LLM with memory and skill support
// ---------------------------------------------------------------------------

type EinoAgent struct {
	chatModel   *openai.ChatModel
	memory      *redisMemory
	cfg         Config
	hub         *DeviceHub
	extMCPs     []*externalMCPClient
	extToolSets [][]tool.BaseTool
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
		BaseURL:     baseURL,
		APIKey:      cfg.GoLLMAPIKey,
		Model:       cfg.GoLLMModel,
		Timeout:     cfg.GoLLMTimeout,
		Temperature: &temp,
		MaxTokens:   &maxTokens,
	})
	if err != nil {
		log.Printf("eino chat model init failed: %v", err)
		return nil
	}

	memory := newRedisMemory(cfg)

	// Connect to external MCP servers and discover their tools
	var extMCPs []*externalMCPClient
	var extToolSets [][]tool.BaseTool
	for _, mcpURL := range cfg.ExternalMCPURLs {
		mcpURL = strings.TrimSpace(mcpURL)
		if mcpURL == "" {
			continue
		}
		client, err := newExternalMCPClient(ctx, mcpURL)
		if err != nil {
			log.Printf("ext MCP connect failed %s: %v", mcpURL, err)
			continue
		}
		tools, err := client.listTools(ctx)
		if err != nil {
			log.Printf("ext MCP list tools failed %s: %v", mcpURL, err)
			continue
		}
		extMCPs = append(extMCPs, client)
		extToolSets = append(extToolSets, tools)
		log.Printf("ext MCP ready: %s tools=%d", mcpURL, len(tools))
	}

	log.Printf("eino agent ready: model=%s base=%s redis=%v extMCPs=%d", cfg.GoLLMModel, baseURL, memory != nil, len(extMCPs))
	return &EinoAgent{chatModel: chatModel, memory: memory, cfg: cfg, extMCPs: extMCPs, extToolSets: extToolSets}
}

func (a *EinoAgent) SetHub(hub *DeviceHub) {
	a.hub = hub
}

// Chat sends userText through the Eino agent with memory and optional MCP tools.
func (a *EinoAgent) Chat(ctx context.Context, deviceID string, userText string) (string, error) {
	return a.ChatWithContext(ctx, deviceID, deviceID, userText)
}

// ChatWithContext separates the conversation memory key from the optional
// device ID used for MCP tools. Device voice turns use the same value for both;
// text-only channels such as Lark use their channel conversation ID and leave
// deviceID empty unless they intentionally bind to a device.
func (a *EinoAgent) ChatWithContext(ctx context.Context, conversationID string, deviceID string, userText string) (string, error) {
	if conversationID == "" {
		conversationID = deviceID
	}
	log.Printf("EinoAgent.Chat called: conversation=%s device=%s text=%q", conversationID, deviceID, userText)

	// Load conversation history
	history := a.memory.Load(ctx, conversationID)

	// Build message list: system + history + new user message
	msgs := make([]*schema.Message, 0, len(history)+2)
	if a.cfg.GoLLMPrompt != "" {
		msgs = append(msgs, schema.SystemMessage(a.cfg.GoLLMPrompt))
	}
	msgs = append(msgs, history...)
	msgs = append(msgs, schema.UserMessage(userText))

	// Build tool list: device MCP tools + external MCP tools
	var einoTools []tool.BaseTool
	if a.hub != nil && deviceID != "" {
		if session := a.hub.session(deviceID); session != nil {
			einoTools = mcpToolsToEinoTools(session, a.hub)
		}
	}
	for _, tools := range a.extToolSets {
		einoTools = append(einoTools, tools...)
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

	runner := adk.NewRunner(ctx, adk.RunnerConfig{
		Agent: agent,
	})

	iter := runner.Run(ctx, msgs)

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
	a.memory.Save(ctx, conversationID, updated)

	return result.Content, nil
}

// Generate sends a system + user message to the LLM with external MCP tools but no history.
func (a *EinoAgent) Generate(ctx context.Context, system, user string) (string, error) {
	msgs := make([]*schema.Message, 0, 2)
	if system != "" {
		msgs = append(msgs, schema.SystemMessage(system))
	}
	msgs = append(msgs, schema.UserMessage(user))

	var einoTools []tool.BaseTool
	for _, tools := range a.extToolSets {
		einoTools = append(einoTools, tools...)
	}

	cfg := &adk.ChatModelAgentConfig{
		Name:  "xiaoli",
		Model: a.chatModel,
	}
	if len(einoTools) > 0 {
		cfg.ToolsConfig = adk.ToolsConfig{
			ToolsNodeConfig: compose.ToolsNodeConfig{
				Tools: einoTools,
			},
		}
	}

	agent, err := adk.NewChatModelAgent(ctx, cfg)
	if err != nil {
		return "", fmt.Errorf("create agent: %w", err)
	}

	runner := adk.NewRunner(ctx, adk.RunnerConfig{Agent: agent})
	iter := runner.Run(ctx, msgs)

	var result string
	for {
		event, ok := iter.Next()
		if !ok {
			break
		}
		if event.Err != nil {
			log.Printf("EinoAgent.Generate event error: %v", event.Err)
			return "", fmt.Errorf("agent error: %w", event.Err)
		}
		if event.Output != nil && event.Output.MessageOutput != nil &&
			event.Output.MessageOutput.Message != nil &&
			event.Output.MessageOutput.Role == schema.Assistant {
			result = event.Output.MessageOutput.Message.Content
		}
	}
	if result == "" {
		return "", fmt.Errorf("agent returned empty response")
	}
	return result, nil
}
