package providers

import "testing"

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
