package providers

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestNormalizeCodexWSReplayCursor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		sentMsgCount int
		historyLen   int
		wantCount    int
		wantReset    bool
	}{
		{name: "valid cursor", sentMsgCount: 3, historyLen: 7, wantCount: 3},
		{name: "cursor exceeds history", sentMsgCount: 70, historyLen: 1, wantCount: 0, wantReset: true},
		{name: "negative cursor", sentMsgCount: -1, historyLen: 4, wantCount: 0, wantReset: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotCount, gotReset := normalizeCodexWSReplayCursor(tt.sentMsgCount, tt.historyLen)
			if gotCount != tt.wantCount || gotReset != tt.wantReset {
				t.Fatalf("normalizeCodexWSReplayCursor(%d, %d) = (%d, %t), want (%d, %t)",
					tt.sentMsgCount, tt.historyLen, gotCount, gotReset, tt.wantCount, tt.wantReset)
			}
		})
	}
}

func TestParseWSResponseUsageDetails(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		event string
		want  *UsageInfo
	}{
		{
			name: "cached and reasoning details are surfaced",
			event: `{"type":"response.completed","response":{"id":"resp_1","status":"completed",
				"output":[{"type":"message","id":"msg_1","content":[{"type":"output_text","text":"pong"}]}],
				"usage":{"input_tokens":1200,"input_tokens_details":{"cached_tokens":1024},
				"output_tokens":40,"output_tokens_details":{"reasoning_tokens":16},"total_tokens":1240}}}`,
			want: &UsageInfo{
				PromptTokens:       1200,
				CompletionTokens:   40,
				TotalTokens:        1240,
				CachedPromptTokens: 1024,
				ReasoningTokens:    16,
			},
		},
		{
			name: "details absent leave the breakdown at zero",
			event: `{"type":"response.completed","response":{"id":"resp_2","status":"completed","output":[],
				"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}`,
			want: &UsageInfo{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		},
		{
			name:  "usage omitted stays unknown",
			event: `{"type":"response.completed","response":{"id":"resp_3","status":"completed","output":[]}}`,
			want:  nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var evt wsEvent
			if err := json.Unmarshal([]byte(tt.event), &evt); err != nil {
				t.Fatalf("unmarshal event: %v", err)
			}
			if evt.Response == nil {
				t.Fatal("event carries no response object")
			}
			got := parseWSResponse(evt.Response.Output, evt.Response.Usage)
			if !reflect.DeepEqual(got.Usage, tt.want) {
				t.Fatalf("Usage = %+v, want %+v", got.Usage, tt.want)
			}
		})
	}
}
