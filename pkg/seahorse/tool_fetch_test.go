package seahorse

import (
	"context"
	"strings"
	"testing"

	"github.com/sipeed/picoclaw/pkg/providers/protocoltypes"
)

// fetchToolFromStore wraps a store in the minimal RetrievalEngine + tool needed
// for tool_fetch tests, so the tests can target the persisted row directly
// without going through Engine.Ingest.
func fetchToolFromStore(s *Store) *FetchMessageTool {
	return NewFetchMessageTool(&RetrievalEngine{store: s})
}

func TestFetchMessageTool_EmptyMessageID(t *testing.T) {
	s := openTestStore(t)
	tool := fetchToolFromStore(s)

	res := tool.Execute(context.Background(), map[string]any{"message_id": ""})
	if res == nil || !res.IsError {
		t.Fatalf("expected error result for empty message_id, got %+v", res)
	}
	if !strings.Contains(res.ForLLM, "message_id is required") {
		t.Errorf("expected 'message_id is required' in payload, got %q", res.ForLLM)
	}
}

func TestFetchMessageTool_MissingArg(t *testing.T) {
	s := openTestStore(t)
	tool := fetchToolFromStore(s)

	res := tool.Execute(context.Background(), map[string]any{})
	if res == nil || !res.IsError {
		t.Fatalf("expected error result for missing arg, got %+v", res)
	}
}

func TestFetchMessageTool_NotFound(t *testing.T) {
	s := openTestStore(t)
	tool := fetchToolFromStore(s)

	const ref = "telegram:404:0"
	res := tool.Execute(context.Background(), map[string]any{"message_id": ref})
	if res == nil || !res.IsError {
		t.Fatalf("expected error result for unknown message_id, got %+v", res)
	}
	if !strings.Contains(res.ForLLM, "message not found") || !strings.Contains(res.ForLLM, ref) {
		t.Errorf("expected not-found payload to mention %q, got %q", ref, res.ForLLM)
	}
}

func TestFetchMessageTool_TopicFormRef(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()

	conv, err := s.GetOrCreateConversation(ctx, "agent:fetch-topic-form")
	if err != nil {
		t.Fatalf("GetOrCreateConversation: %v", err)
	}

	const (
		topicRef    = "12345:9:67"
		bodyContent = "topic-form ref body"
	)
	md := &protocoltypes.MessageMetadata{
		SenderID:          "telegram:42",
		SenderDisplayName: "Bob",
	}

	if _, err := s.AddMessageWithReasoning(
		ctx, conv.ConversationID, "user", bodyContent, "", topicRef, md, 0,
	); err != nil {
		t.Fatalf("AddMessageWithReasoning: %v", err)
	}

	tool := fetchToolFromStore(s)
	res := tool.Execute(ctx, map[string]any{"message_id": topicRef})
	if res == nil || res.IsError {
		t.Fatalf("expected success for topic-form ref, got %+v", res)
	}
	for _, want := range []string{
		"Message ID: " + topicRef,
		"Role: user",
		"Sender: Bob",
		"Sender ID: telegram:42",
		bodyContent,
	} {
		if !strings.Contains(res.ForLLM, want) {
			t.Errorf("payload missing %q: %q", want, res.ForLLM)
		}
	}
}
