package effort

import "testing"

func TestNormalizeEffort(t *testing.T) {
	for _, raw := range []string{"", "off", "low", "medium", "high", "xhigh", "max", "adaptive"} {
		t.Run(raw, func(t *testing.T) {
			got, err := Normalize(raw)
			if err != nil || got != raw {
				t.Fatalf("Normalize(%q) = %q, %v", raw, got, err)
			}
		})
	}
	for raw, want := range map[string]string{"  HIGH\n": "high", "\tAdApTiVe ": "adaptive", "   ": "", " MAX ": "max"} {
		if got, err := Normalize(raw); err != nil || got != want {
			t.Errorf("Normalize(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"auto", "none", "minimal", "invalid", "high low", "1"} {
		if got, err := Normalize(raw); err == nil || got != "" {
			t.Errorf("Normalize(%q) = %q, %v; want empty/error", raw, got, err)
		}
	}
}
