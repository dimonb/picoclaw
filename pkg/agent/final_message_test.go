// PicoClaw - Ultra-lightweight personal AI agent

package agent

import "testing"

func TestStripLeadingMessageAnnotations(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantContent string
		wantStrip   string
	}{
		{
			name:        "msgs only",
			in:          `[msgs:#35243507:644] Готово.`,
			wantContent: `Готово.`,
			wantStrip:   `[msgs:#35243507:644] `,
		},
		{
			name:        "reply_to only",
			in:          `[reply_to:#999] Нашёл.`,
			wantContent: `Нашёл.`,
			wantStrip:   `[reply_to:#999] `,
		},
		{
			name:        "from msgs and reply_to",
			in:          `[from:Dmitrii Balabanov (@dimonb); msgs:#1042, reply_to:#999] Нашёл.`,
			wantContent: `Нашёл.`,
			wantStrip:   `[from:Dmitrii Balabanov (@dimonb); msgs:#1042, reply_to:#999] `,
		},
		{
			name:        "regular bracketed text preserved",
			in:          `[Черновик] Нашёл.`,
			wantContent: `[Черновик] Нашёл.`,
			wantStrip:   ``,
		},
		{
			name:        "no leading bracket",
			in:          `Просто текст [msgs:#1] внутри.`,
			wantContent: `Просто текст [msgs:#1] внутри.`,
			wantStrip:   ``,
		},
		{
			name:        "empty",
			in:          ``,
			wantContent: ``,
			wantStrip:   ``,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			content, stripped := stripLeadingMessageAnnotations(tt.in)
			if content != tt.wantContent {
				t.Fatalf("content = %q, want %q", content, tt.wantContent)
			}
			if stripped != tt.wantStrip {
				t.Fatalf("stripped = %q, want %q", stripped, tt.wantStrip)
			}
		})
	}
}
