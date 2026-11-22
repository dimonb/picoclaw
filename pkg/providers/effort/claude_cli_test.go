package effort

import "testing"

func TestClaudeCLI(t *testing.T) {
	cases := map[string]string{
		"": "", "adaptive": "", "bogus": "",
		"off": "low", "low": "low", "medium": "medium", " HIGH ": "high", "xhigh": "xhigh", "max": "max",
	}
	for in, want := range cases {
		got, ok := ClaudeCLI(in)
		if got != want || ok != (want != "") {
			t.Errorf("ClaudeCLI(%q) = %q,%v; want %q", in, got, ok, want)
		}
	}
}
