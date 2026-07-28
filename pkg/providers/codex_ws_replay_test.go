package providers

import "testing"

func msg(role, content string) Message {
	return Message{Role: role, Content: content}
}

func TestCodexWSReplayResetReason(t *testing.T) {
	t.Parallel()

	convo := []Message{
		msg("user", "hello"),
		msg("assistant", "hi"),
		msg("user", "how are you"),
	}
	sentAll := historyPrefixDigest(convo)
	sentTwo := historyPrefixDigest(convo[:2])

	tests := []struct {
		name         string
		sentMsgCount int
		sentDigest   string
		convMsgs     []Message
		want         string
	}{
		{
			name:         "fresh session sends everything",
			sentMsgCount: 0,
			convMsgs:     convo,
			want:         "",
		},
		{
			name:         "append-only growth keeps the cursor",
			sentMsgCount: 2,
			sentDigest:   sentTwo,
			convMsgs:     convo,
			want:         "",
		},
		{
			name:         "compaction shrinks history",
			sentMsgCount: 3,
			sentDigest:   sentAll,
			convMsgs:     convo[:2],
			want:         "history_shrank",
		},
		{
			name:         "prefix rewritten at same length",
			sentMsgCount: 2,
			sentDigest:   sentTwo,
			convMsgs: []Message{
				msg("user", "summarize this other thing"),
				msg("assistant", "hi"),
				msg("user", "how are you"),
			},
			want: "prefix_rewritten",
		},
		{
			name: "one-shot prompt reusing the session key",
			// Two unrelated single-message prompts look identical by count.
			sentMsgCount: 1,
			sentDigest:   historyPrefixDigest([]Message{msg("user", "summarize chunk A")}),
			convMsgs:     []Message{msg("user", "summarize chunk B")},
			want:         "prefix_rewritten",
		},
		{
			name:         "repeat of an already-sent conversation",
			sentMsgCount: 3,
			sentDigest:   sentAll,
			convMsgs:     convo,
			want:         "no_new_messages",
		},
		{
			name:         "missing digest falls back to length check",
			sentMsgCount: 2,
			sentDigest:   "",
			convMsgs:     convo,
			want:         "",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := codexWSReplayResetReason(tt.sentMsgCount, tt.sentDigest, tt.convMsgs)
			if got != tt.want {
				t.Fatalf("codexWSReplayResetReason = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestHistoryPrefixDigestCoversToolCalls(t *testing.T) {
	t.Parallel()

	base := []Message{
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "call_1", Function: &FunctionCall{Name: "exec", Arguments: `{"cmd":"ls"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "file.txt"},
	}
	changedArgs := []Message{
		{
			Role: "assistant",
			ToolCalls: []ToolCall{
				{ID: "call_1", Function: &FunctionCall{Name: "exec", Arguments: `{"cmd":"rm -rf /"}`}},
			},
		},
		{Role: "tool", ToolCallID: "call_1", Content: "file.txt"},
	}

	if historyPrefixDigest(base) == historyPrefixDigest(changedArgs) {
		t.Fatal("digest ignores tool call arguments, so a rewritten tool call would go undetected")
	}
	if historyPrefixDigest(base) != historyPrefixDigest(base) {
		t.Fatal("digest is not stable for identical input")
	}
}
