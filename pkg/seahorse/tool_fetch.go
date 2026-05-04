package seahorse

import (
	"context"
	"fmt"
	"strings"

	toolshared "github.com/sipeed/picoclaw/pkg/tools/shared"
)

type FetchMessageTool struct {
	retrieval *RetrievalEngine
}

func NewFetchMessageTool(retrieval *RetrievalEngine) *FetchMessageTool {
	return &FetchMessageTool{retrieval: retrieval}
}

func (t *FetchMessageTool) Name() string {
	return "fetch_message"
}

func (t *FetchMessageTool) Description() string {
	return "Retrieve the full content and metadata of a previous message using its message_id. " +
		"Use this to recover details from messages that have been summarized or to see reply chains."
}

func (t *FetchMessageTool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"message_id": map[string]any{
				"type":        "string",
				"description": "The opaque channel-native message ID (e.g., 'chat_id:msg_id')",
			},
		},
		"required": []string{"message_id"},
	}
}

func (t *FetchMessageTool) Execute(ctx context.Context, args map[string]any) *toolshared.ToolResult {
	messageID, ok := args["message_id"].(string)
	if !ok || messageID == "" {
		return toolshared.ErrorResult("message_id is required")
	}

	msg, err := t.retrieval.store.GetMessageByChannelMessageID(ctx, messageID)
	if err != nil {
		return toolshared.ErrorResult(fmt.Sprintf("database error: %v", err))
	}
	if msg == nil {
		return toolshared.ErrorResult(fmt.Sprintf("message not found: %s", messageID))
	}

	var b strings.Builder
	fmt.Fprintf(&b, "Message ID: %s\n", msg.ChannelMessageID)
	fmt.Fprintf(&b, "Role: %s\n", msg.Role)
	if md := msg.Metadata; md != nil {
		if md.SenderDisplayName != "" {
			fmt.Fprintf(&b, "Sender: %s\n", md.SenderDisplayName)
		}
		if md.SenderID != "" {
			fmt.Fprintf(&b, "Sender ID: %s\n", md.SenderID)
		}
		if md.ReplyToMessageID != "" {
			fmt.Fprintf(&b, "Reply To: %s\n", md.ReplyToMessageID)
		}
	}
	fmt.Fprintf(&b, "Time: %s\n\n%s", msg.CreatedAt.Format("2006-01-02 15:04:05"), msg.Content)

	return toolshared.NewToolResult(b.String())
}
