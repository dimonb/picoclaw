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

func TestBuildWSContentPartsSkipsSVG(t *testing.T) {
	t.Parallel()

	svgURL := "data:" + "image/svg+xml;base64,PHN2Zz48L3N2Zz4="
	parts := buildWSContentParts(Message{
		Content: "what is this?",
		Media:   []string{svgURL},
	})

	if len(parts) != 1 {
		t.Fatalf("len(parts) = %d, want 1 text part", len(parts))
	}
	if got := parts[0]["type"]; got != "input_text" {
		t.Fatalf("parts[0].type = %v, want input_text", got)
	}
}

func TestSupportedCodexImageDataURL(t *testing.T) {
	t.Parallel()

	pngURL := "data:" + "image/png;base64,abc"
	if !isSupportedCodexImageDataURL(pngURL) {
		t.Fatal("png should be supported")
	}

	svgURL := "data:" + "image/svg+xml;base64,abc"
	if isSupportedCodexImageDataURL(svgURL) {
		t.Fatal("svg should not be sent as input_image")
	}
}
