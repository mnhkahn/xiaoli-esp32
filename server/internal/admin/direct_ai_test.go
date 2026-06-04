package admin

import (
	"context"
	"fmt"
	"log"
	"os"
	"testing"
	"time"

	einomodel "github.com/cloudwego/eino-ext/components/model/openai"
	"xiaoli/server/pkg/langsmith"
	"github.com/cloudwego/eino/adk"
	"github.com/cloudwego/eino/callbacks"
	"github.com/cloudwego/eino/components"
	"github.com/cloudwego/eino/compose"
	"github.com/cloudwego/eino/schema"
)

func TestEinoAgentChatWithLangSmith(t *testing.T) {
	apiKey := os.Getenv("SILICONFLOW_API_KEY")
	if apiKey == "" {
		t.Skip("SILICONFLOW_API_KEY not set")
	}
	langsmithKey := os.Getenv("LANGSMITH_API_KEY")
	if langsmithKey == "" {
		t.Skip("LANGSMITH_API_KEY not set")
	}

	ctx := context.Background()
	baseURL := "https://api.siliconflow.cn/v1"
	model := "Qwen/Qwen3-8B"
	temp := float32(0.2)
	maxTokens := 180

	chatModel, err := einomodel.NewChatModel(ctx, &einomodel.ChatModelConfig{
		BaseURL:     baseURL,
		APIKey:      apiKey,
		Model:       model,
		Timeout:     45 * time.Second,
		Temperature: &temp,
		MaxTokens:   &maxTokens,
	})
	if err != nil {
		t.Fatalf("create chat model: %v", err)
	}

	lsHandler, err := langsmith.NewLangsmithHandler(&langsmith.Config{
		APIKey: langsmithKey,
	})
	if err != nil {
		t.Fatalf("create langsmith handler: %v", err)
	}
	log.Printf("langsmith handler created")

	// Test 1: Direct chatModel.Generate with callbacks
	t.Run("DirectChatModel", func(t *testing.T) {
		ctx2 := langsmith.SetTrace(context.Background(), langsmith.WithSessionName("xiaoli-server-test"))
		ri := &callbacks.RunInfo{
			Name:      "Qwen/Qwen3-8B",
			Type:      "ChatModel",
			Component: components.ComponentOfChatModel,
		}
		ctx2 = callbacks.InitCallbacks(ctx2, ri, lsHandler)

		msgs := []*schema.Message{
			schema.SystemMessage("你是一个叫小李的中文语音助手。回答要简短。"),
			schema.UserMessage("一加一等于几？"),
		}

		result, err := chatModel.Generate(ctx2, msgs)
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		log.Printf("direct answer: %q", result.Content)
	})

	// Test 2: adk.ChatModelAgent with WithCallbacks
	t.Run("AgentWithCallbacks", func(t *testing.T) {
		agent, err := adk.NewChatModelAgent(ctx, &adk.ChatModelAgentConfig{
			Name:          "test-xiaoli",
			Model:         chatModel,
			MaxIterations: 10,
			ToolsConfig: adk.ToolsConfig{
				ToolsNodeConfig: compose.ToolsNodeConfig{},
			},
		})
		if err != nil {
			t.Fatalf("create agent: %v", err)
		}

		ctx2 := langsmith.SetTrace(context.Background(), langsmith.WithSessionName("xiaoli-server-test"))
		runOpts := []adk.AgentRunOption{adk.WithCallbacks(lsHandler)}

		msgs := []*schema.Message{
			schema.SystemMessage("你是一个叫小李的中文语音助手。回答要简短。"),
			schema.UserMessage("二加二等于几？"),
		}

		iter := agent.Run(ctx2, &adk.AgentInput{Messages: msgs}, runOpts...)

		var result *schema.Message
		eventCount := 0
		for {
			event, ok := iter.Next()
			if !ok {
				break
			}
			eventCount++
			if event.Err != nil {
				t.Fatalf("agent error: %v", event.Err)
			}
			log.Printf("event[%d]: output=%v", eventCount, event.Output != nil)
			if event.Output != nil && event.Output.MessageOutput != nil &&
				event.Output.MessageOutput.Message != nil &&
				event.Output.MessageOutput.Role == schema.Assistant {
				result = event.Output.MessageOutput.Message
				log.Printf("assistant: %q", result.Content)
			}
		}

		log.Printf("agent done: events=%d hasResult=%v", eventCount, result != nil)
		if result == nil || result.Content == "" {
			t.Fatal("agent returned empty response")
		}
	})
}

func TestLangSmithCallbackFires(t *testing.T) {
	langsmithKey := os.Getenv("LANGSMITH_API_KEY")
	if langsmithKey == "" {
		t.Skip("LANGSMITH_API_KEY not set")
	}

	handler, err := langsmith.NewLangsmithHandler(&langsmith.Config{
		APIKey: langsmithKey,
	})
	if err != nil {
		t.Fatalf("create handler: %v", err)
	}

	ctx := context.Background()
	ctx = langsmith.SetTrace(ctx, langsmith.WithSessionName("xiaoli-server-test"))

	ri := &callbacks.RunInfo{
		Name:      "test-run",
		Type:      "ChatModel",
		Component: components.ComponentOfChatModel,
	}
	ctx = callbacks.InitCallbacks(ctx, ri, handler)

	ctx = handler.OnStart(ctx, ri, callbacks.CallbackInput("hello"))
	fmt.Printf("[test] OnStart called\n")

	handler.OnEnd(ctx, ri, callbacks.CallbackOutput("world"))
	fmt.Printf("[test] OnEnd called\n")
}
