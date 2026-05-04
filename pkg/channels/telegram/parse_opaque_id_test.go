package telegram

import "testing"

func TestParseTelegramOpaqueID(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantChat int64
		wantTopc int
		wantMsg  int
		wantErr  bool
	}{
		{
			name:     "plain chat_id:msg_id",
			input:    "12345:67",
			wantChat: 12345,
			wantMsg:  67,
		},
		{
			name:     "with topic chat_id:topic_id:msg_id",
			input:    "12345:9:67",
			wantChat: 12345,
			wantTopc: 9,
			wantMsg:  67,
		},
		{
			name:     "negative supergroup chat_id",
			input:    "-1001234567890:42",
			wantChat: -1001234567890,
			wantMsg:  42,
		},
		{
			name:     "negative supergroup with topic",
			input:    "-1001234567890:5:42",
			wantChat: -1001234567890,
			wantTopc: 5,
			wantMsg:  42,
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
		{
			name:    "missing message id",
			input:   "12345",
			wantErr: true,
		},
		{
			name:    "too many segments",
			input:   "1:2:3:4",
			wantErr: true,
		},
		{
			name:    "non-numeric chat",
			input:   "abc:42",
			wantErr: true,
		},
		{
			name:    "non-numeric message",
			input:   "12345:xyz",
			wantErr: true,
		},
		{
			name:    "non-numeric topic",
			input:   "12345:abc:42",
			wantErr: true,
		},
		{
			name:    "non-numeric message in topic form",
			input:   "12345:9:xyz",
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chatID, threadID, msgID, err := parseTelegramOpaqueID(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got chat=%d thread=%d msg=%d",
						tt.input, chatID, threadID, msgID)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if chatID != tt.wantChat {
				t.Errorf("chatID = %d, want %d", chatID, tt.wantChat)
			}
			if threadID != tt.wantTopc {
				t.Errorf("threadID = %d, want %d", threadID, tt.wantTopc)
			}
			if msgID != tt.wantMsg {
				t.Errorf("messageID = %d, want %d", msgID, tt.wantMsg)
			}
		})
	}
}
