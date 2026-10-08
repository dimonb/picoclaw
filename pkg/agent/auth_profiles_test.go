package agent

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sipeed/picoclaw/pkg/auth"
	"github.com/sipeed/picoclaw/pkg/config"
	"github.com/sipeed/picoclaw/pkg/providers"
)

func TestAuthProfilesFailOverAcrossLoginsBeforeModels(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	for _, profile := range []string{"default", "b"} {
		if err := auth.SetCredential(auth.ProfileKey("openai", profile), &auth.AuthCredential{
			AccessToken: "token-" + profile, AccountID: "acct-" + profile, AuthMethod: "oauth",
		}); err != nil {
			t.Fatalf("SetCredential(%s): %v", profile, err)
		}
	}

	configPath := filepath.Join(home, "config.json")
	raw := `{
		"version": 3,
		"model_list": [
			{"model_name": "sol61", "provider": "codex-ws", "model": "gpt-6.1-sol",
			 "auth_method": "oauth", "auth_profiles": ["default", "b"]},
			{"model_name": "gpt-5.5", "provider": "codex-ws", "model": "gpt-5.5",
			 "auth_method": "oauth", "auth_profiles": ["default", "b"]}
		]
	}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	cfg, err := config.LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	candidates := resolveModelCandidates(cfg, "", "sol61", []string{"gpt-5.5"})
	order := make([]string, 0, len(candidates))
	for _, c := range candidates {
		order = append(order, c.DisplayName)
	}
	if want := []string{"sol61", "sol61@b", "gpt-5.5", "gpt-5.5@b"}; !reflect.DeepEqual(order, want) {
		t.Fatalf("candidate order = %v, want %v", order, want)
	}

	out := map[string]providers.LLMProvider{}
	populateCandidateProvidersFromNames(cfg, home, authProfileModelNames(cfg), out)
	agent := &AgentInstance{CandidateProviders: out}
	t.Cleanup(func() {
		for _, p := range out {
			closeProviderIfStateful(p)
		}
	})

	seen := map[providers.LLMProvider]string{}
	for _, c := range candidates {
		p, err := providerForFallbackCandidate(agent, nil, candidates, c)
		if err != nil {
			t.Fatalf("%s: providerForFallbackCandidate: %v", c.DisplayName, err)
		}
		if other, dup := seen[p]; dup {
			t.Fatalf("%s and %s share one provider; each login needs its own", other, c.DisplayName)
		}
		seen[p] = c.DisplayName
	}
}
