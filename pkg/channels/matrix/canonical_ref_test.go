package matrix

import "testing"

// Review 3, fix 2: Matrix references in one canonical "room event" form, so
// own-message checks compare the full event ID (never cut at its ':').
func TestCanonicalMessageRef(t *testing.T) {
	c := &MatrixChannel{}
	for _, tc := range []struct{ chatID, ref, want string }{
		{"!r:example.org", "!r:example.org $e:example.org", "!r:example.org $e:example.org"},
		{"!r:example.org", "$e:example.org", "!r:example.org $e:example.org"},
		{"!other:example.org", "!r:example.org $e:example.org", "!r:example.org $e:example.org"},
		{"", "$e:example.org", ""},
		{"!r:example.org", "not-an-event", ""},
	} {
		if got := c.CanonicalMessageRef(tc.chatID, tc.ref); got != tc.want {
			t.Errorf("CanonicalMessageRef(%q, %q) = %q, want %q", tc.chatID, tc.ref, got, tc.want)
		}
	}
}
