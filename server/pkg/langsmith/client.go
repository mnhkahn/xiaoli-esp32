package langsmith

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/bytedance/sonic"
)

type Langsmith interface {
	CreateRun(ctx context.Context, run *Run) error
	UpdateRun(ctx context.Context, runID string, patch *RunPatch) error
}

const (
	DefaultLangsmithAPIURL = "https://api.smith.langchain.com"
)

type RunType string

const (
	RunTypeChain RunType = "chain"
	RunTypeLLM   RunType = "llm"
	RunTypeTool  RunType = "tool"
)

type Run struct {
	ID                 string                 `json:"id"`
	Name               string                 `json:"name"`
	RunType            RunType                `json:"run_type"`
	StartTime          time.Time              `json:"start_time"`
	EndTime            *time.Time             `json:"end_time,omitempty"`
	Inputs             map[string]interface{} `json:"inputs"`
	Outputs            map[string]interface{} `json:"outputs,omitempty"`
	Error              *string                `json:"error,omitempty"`
	ParentRunID        *string                `json:"parent_run_id,omitempty"`
	TraceID            string                 `json:"trace_id,omitempty"`
	Extra              map[string]interface{} `json:"extra,omitempty"`
	SessionName        string                 `json:"session_name,omitempty"`
	ReferenceExampleID *string                `json:"reference_example_id,omitempty"`
	DottedOrder        string                 `json:"dotted_order,omitempty"`
	Tags               []string               `json:"tags,omitempty"`
}

type RunPatch struct {
	EndTime *time.Time             `json:"end_time,omitempty"`
	Inputs  map[string]interface{} `json:"inputs,omitempty"`
	Outputs map[string]interface{} `json:"outputs,omitempty"`
	Error   *string                `json:"error,omitempty"`
	Extra   map[string]interface{} `json:"extra,omitempty"`
}

type langsmithClient struct {
	apiKey     string
	baseURL    string
	httpClient *http.Client
}

func NewLangsmithWithTimeout(apiKey, apiUrl string, timeout time.Duration) Langsmith {
	if apiUrl == "" {
		apiUrl = DefaultLangsmithAPIURL
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &langsmithClient{
		apiKey:     apiKey,
		baseURL:    apiUrl,
		httpClient: &http.Client{Timeout: timeout},
	}
}

func (c *langsmithClient) CreateRun(ctx context.Context, run *Run) error {
	jsonData, err := sonic.Marshal(run)
	if err != nil {
		return fmt.Errorf("failed to marshal run data: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.baseURL+"/runs", bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("failed to create run, status: %s, body: %s", resp.Status, string(body))
	}

	err = sonic.Unmarshal(body, run)
	if err != nil {
		return fmt.Errorf("failed to decode response body: %w", err)
	}

	return nil
}

func (c *langsmithClient) UpdateRun(ctx context.Context, runID string, patch *RunPatch) error {
	jsonData, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("failed to marshal patch data: %w", err)
	}

	url := fmt.Sprintf("%s/runs/%s", c.baseURL, runID)
	req, err := http.NewRequestWithContext(ctx, "PATCH", url, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", c.apiKey)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("failed to update run, status: %s, body: %s", resp.Status, string(body))
	}

	return nil
}
