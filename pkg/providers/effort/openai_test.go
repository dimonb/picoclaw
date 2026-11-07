package effort

import "testing"

func TestOpenAIEffort(t *testing.T) {
	for _, tt := range []struct{ model, level, want string }{
		{"o1", "low", "low"}, {"o3", "max", "high"}, {"o4-mini", "off", "low"},
		{"o3-mini-2025-01-31", "xhigh", "high"}, {"gpt-5", "off", "minimal"},
		{"gpt-5-mini", "max", "high"}, {"gpt-5-pro", "low", "high"}, {"gpt-5-pro", "off", "high"},
		{"gpt-5.1", "off", "none"}, {"gpt-5.1", "xhigh", "high"},
		{"gpt-5.1-codex-max", "max", "xhigh"}, {"gpt-5.1-codex-max", "off", "low"},
		{" GPT-5.2-2025-12-11 ", " MAX ", "xhigh"}, {"gpt-5.2", "off", "none"},
		{"gpt-4o", "high", ""}, {"gpt-5.2-chat-latest", "high", ""},
		{"gpt-5.2-pro", "low", ""}, {"gpt-6.1-sol", "max", ""}, {"o3-pro", "low", ""},
		{"my-azure-deployment", "high", ""}, {"gpt-5.2", "adaptive", ""}, {"gpt-5.2", "", ""},
		{"gpt-5.2", "bogus", ""}, {"gpt-5.2-unrecognized", "max", ""},
	} {
		t.Run(tt.model+"/"+tt.level, func(t *testing.T) {
			got, ok := OpenAI(tt.model, tt.level)
			if got != tt.want || ok != (tt.want != "") {
				t.Fatalf("got %q,%v want %q", got, ok, tt.want)
			}
		})
	}
}
