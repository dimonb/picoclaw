package telegram

import "testing"

func TestMentionsNoisyMethod(t *testing.T) {
	tests := []struct {
		name   string
		format string
		args   []any
		want   bool
	}{
		{
			name:   "response for muted method",
			format: "API response %s: %s",
			args:   []any{"sendChatAction", "Ok: true, Err: [<nil>], Result: true"},
			want:   true,
		},
		{
			name:   "call to muted method",
			format: "API call to: %q, with data: %s",
			args:   []any{"https://api.telegram.org/bot123:secret/getUpdates", "{}"},
			want:   true,
		},
		{
			name:   "response for method we care about",
			format: "API response %s: %s",
			args:   []any{"sendMessage", "Ok: true"},
			want:   false,
		},
		{
			name:   "call to method we care about",
			format: "API call to: %q, with data: %s",
			args:   []any{"https://api.telegram.org/bot123:secret/sendMessage", "{}"},
			want:   false,
		},
		{
			name:   "muted method name inside a non-API line stays visible",
			format: "Webhook request with data: %s",
			args:   []any{"sendChatAction"},
			want:   false,
		},
		{
			name:   "no args",
			format: "API something",
			args:   nil,
			want:   false,
		},
		{
			name:   "non-string first arg",
			format: "API response %s: %s",
			args:   []any{42, "x"},
			want:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mentionsNoisyMethod(tt.format, tt.args); got != tt.want {
				t.Errorf("mentionsNoisyMethod(%q, %v) = %v, want %v", tt.format, tt.args, got, tt.want)
			}
		})
	}
}

func TestIsTruthyEnv(t *testing.T) {
	for _, v := range []string{"1", "true", "TRUE", "yes", "on", " on "} {
		if !isTruthyEnv(v) {
			t.Errorf("isTruthyEnv(%q) = false, want true", v)
		}
	}
	for _, v := range []string{"", "0", "false", "no", "off", "maybe"} {
		if isTruthyEnv(v) {
			t.Errorf("isTruthyEnv(%q) = true, want false", v)
		}
	}
}
