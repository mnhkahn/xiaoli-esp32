package admin

import (
	"context"
	"errors"
	"log"
	"strings"
)

type ConversationChannel string

const (
	ChannelDeviceVoice ConversationChannel = "device_voice"
	ChannelLarkText    ConversationChannel = "lark_text"
)

type ConversationTurn struct {
	Channel        ConversationChannel
	ConversationID string
	DeviceID       string
	Text           string
	UseDeviceTools bool
}

type ConversationReply struct {
	Text string
}

type DeviceVoiceFactory struct{}

func (DeviceVoiceFactory) Build(session *deviceSession, text string) ConversationTurn {
	return ConversationTurn{
		Channel:        ChannelDeviceVoice,
		ConversationID: session.deviceID,
		DeviceID:       session.deviceID,
		Text:           text,
		UseDeviceTools: true,
	}
}

type LarkTextFactory struct{}

func (LarkTextFactory) Build(chatID string, senderID string, text string) ConversationTurn {
	return ConversationTurn{
		Channel:        ChannelLarkText,
		ConversationID: "lark:" + chatID + ":" + senderID,
		Text:           text,
	}
}

type conversationChat interface {
	Chat(ctx context.Context, turn ConversationTurn) (string, error)
}

type conversationChatFunc func(ctx context.Context, turn ConversationTurn) (string, error)

func (f conversationChatFunc) Chat(ctx context.Context, turn ConversationTurn) (string, error) {
	return f(ctx, turn)
}

type ConversationPipeline struct {
	chat    conversationChat
	devices DeviceController
}

func newConversationPipeline(agent *EinoAgent, devices DeviceController) *ConversationPipeline {
	var chat conversationChat
	if agent != nil {
		chat = einoConversationChat{agent: agent}
	}
	return &ConversationPipeline{chat: chat, devices: devices}
}

func (p *ConversationPipeline) Run(ctx context.Context, turn ConversationTurn) (ConversationReply, error) {
	turn.Text = strings.TrimSpace(turn.Text)
	if turn.Text == "" {
		return ConversationReply{}, errors.New("conversation text is empty")
	}
	if turn.ConversationID == "" {
		turn.ConversationID = turn.DeviceID
	}
	if turn.UseDeviceTools && turn.DeviceID != "" && p.devices != nil && needsVision(turn.Text) {
		result, err := p.devices.Call(ctx, BridgeCallRequest{
			DeviceID: turn.DeviceID,
			Tool:     "self.camera.take_photo",
			Arguments: map[string]any{
				"question": turn.Text,
			},
			Timeout: 120,
		})
		if err == nil && result.Error == "" {
			if text := strings.TrimSpace(extractMCPText(result.Result)); text != "" {
				return ConversationReply{Text: text}, nil
			}
		}
		if err != nil {
			return ConversationReply{Text: "我现在看不了摄像头，原因是" + err.Error()}, nil
		}
	}
	if p.chat == nil {
		return ConversationReply{Text: "我现在还没有配置语言模型。"}, nil
	}
	answer, err := p.chat.Chat(ctx, turn)
	if err != nil {
		log.Printf("conversation chat failed channel=%s conversation=%s device=%s: %v", turn.Channel, turn.ConversationID, turn.DeviceID, err)
		return ConversationReply{Text: "我现在回答不了，语言模型调用失败。"}, nil
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		answer = "我现在还没想好怎么回答。"
	}
	return ConversationReply{Text: answer}, nil
}

type einoConversationChat struct {
	agent *EinoAgent
}

func (c einoConversationChat) Chat(ctx context.Context, turn ConversationTurn) (string, error) {
	return c.agent.ChatWithContext(ctx, turn.ConversationID, turn.DeviceID, turn.Text)
}
