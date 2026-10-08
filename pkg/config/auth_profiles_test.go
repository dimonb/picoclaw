package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestLoadConfig_AuthProfilesExpandIntoFallbackEntries(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	raw := `{
		"version": 3,
		"model_list": [
			{"model_name": "sol61", "provider": "codex-ws", "model": "gpt-6.1-sol",
			 "auth_method": "oauth", "thinking_level": "medium",
			 "auth_profiles": ["default", "B", "b", "c"]},
			{"model_name": "gpt-5.5", "provider": "codex-ws", "model": "gpt-5.5", "auth_method": "oauth"}
		]
	}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	var names []string
	for _, m := range cfg.ModelList {
		names = append(names, m.ModelName)
	}
	if want := []string{"sol61", "sol61@b", "sol61@c", "gpt-5.5"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("model_list names = %v, want %v", names, want)
	}

	primary, err := cfg.GetModelConfig("sol61")
	if err != nil {
		t.Fatalf("GetModelConfig(sol61): %v", err)
	}
	if got := primary.AuthProfile(); got != "default" {
		t.Errorf("primary AuthProfile() = %q, want default", got)
	}
	if got, want := primary.ProfileFallbacks(), []string{"sol61@b", "sol61@c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("primary ProfileFallbacks() = %v, want %v", got, want)
	}
	if primary.IsVirtual() {
		t.Error("primary entry must not be virtual")
	}

	b, err := cfg.GetModelConfig("sol61@b")
	if err != nil {
		t.Fatalf("GetModelConfig(sol61@b): %v", err)
	}
	if got := b.AuthProfile(); got != "b" {
		t.Errorf("sol61@b AuthProfile() = %q, want b", got)
	}
	if !b.IsVirtual() || len(b.ProfileFallbacks()) != 0 {
		t.Errorf("sol61@b: virtual=%v profileFallbacks=%v, want virtual with none", b.IsVirtual(), b.ProfileFallbacks())
	}
	if b.Provider != "codex-ws" || b.Model != "gpt-6.1-sol" || b.ThinkingLevel != "medium" {
		t.Errorf("sol61@b lost the entry's settings: %+v", b)
	}

	if err := SaveConfig(configPath, cfg); err != nil {
		t.Fatalf("SaveConfig: %v", err)
	}
	saved, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if strings.Contains(string(saved), "sol61@") {
		t.Errorf("saved config contains expanded profile entries:\n%s", saved)
	}

	reloaded, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig after save: %v", err)
	}
	again, err := reloaded.GetModelConfig("sol61")
	if err != nil {
		t.Fatalf("GetModelConfig(sol61) after save: %v", err)
	}
	if got, want := again.ProfileFallbacks(), []string{"sol61@b", "sol61@c"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProfileFallbacks() after save+load = %v, want %v", got, want)
	}
}

func TestExpandAuthProfileModels_SingleProfileStaysOneEntry(t *testing.T) {
	m := &ModelConfig{ModelName: "sol61", Model: "gpt-6.1-sol", AuthProfiles: []string{"b"}}
	out := expandAuthProfileModels([]*ModelConfig{m})
	if len(out) != 1 || out[0] != m {
		t.Fatalf("expected the entry unchanged, got %v", out)
	}
	if got := out[0].AuthProfile(); got != "b" {
		t.Errorf("AuthProfile() = %q, want b", got)
	}
}
