package bus

import "testing"

func TestIsSilentReply(t *testing.T) {
	for _, in := range []string{"NO_REPLY", " NO_REPLY\n", "NO_REPLY.", "`NO_REPLY`", "**NO_REPLY**", "NO_REPLY!"} {
		if !IsSilentReply(in) {
			t.Errorf("IsSilentReply(%q) = false", in)
		}
	}
	for _, in := range []string{"", "no_reply", "NO_REPLY because", "Sure. NO_REPLY", "NO_REPL"} {
		if IsSilentReply(in) {
			t.Errorf("IsSilentReply(%q) = true", in)
		}
	}
}

func TestIsSilentReplyPrefix(t *testing.T) {
	for _, in := range []string{"", " ", "N", "NO_", "NO_REPLY", "NO_REPLY.", "`NO"} {
		if !IsSilentReplyPrefix(in) {
			t.Errorf("IsSilentReplyPrefix(%q) = false, want hold back", in)
		}
	}
	for _, in := range []string{"Hello", "No", "NO_REPLY and more", "NOT", "N O"} {
		if IsSilentReplyPrefix(in) {
			t.Errorf("IsSilentReplyPrefix(%q) = true, want stream", in)
		}
	}
}
