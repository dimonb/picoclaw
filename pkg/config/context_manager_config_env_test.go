package config

import (
	"os"
	"path/filepath"
	"testing"
)

const envContextManagerConfig = "PICOCLAW_AGENTS_DEFAULTS_CONTEXT_MANAGER_CONFIG"

func writeMinimalV2Config(t *testing.T) string {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "config.json")
	raw := `{"version":2,"gateway":{"host":"127.0.0.1","port":18790}}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatalf("WriteFile(configPath): %v", err)
	}
	return configPath
}

// Regression: ContextManagerConfig is a json.RawMessage, which the caarlos0/env
// loader cannot decode from a string without a custom parser. Before the fix,
// setting this env crashed config load with "strconv.ParseUint ... invalid
// syntax". Now it must round-trip the raw JSON blob unchanged.
func TestLoadConfig_ContextManagerConfigEnvParsesJSON(t *testing.T) {
	configPath := writeMinimalV2Config(t)
	blob := `{"leafSummaryCompression":"relaxed"}`
	t.Setenv(envContextManagerConfig, blob)

	cfg, err := LoadConfig(configPath)
	if err != nil {
		t.Fatalf("LoadConfig() error: %v", err)
	}
	if got := string(cfg.Agents.Defaults.ContextManagerConfig); got != blob {
		t.Fatalf("ContextManagerConfig = %q, want %q", got, blob)
	}
}

func TestLoadConfig_ContextManagerConfigEnvRejectsInvalidJSON(t *testing.T) {
	configPath := writeMinimalV2Config(t)
	t.Setenv(envContextManagerConfig, "not-json")

	if _, err := LoadConfig(configPath); err == nil {
		t.Fatal("LoadConfig() expected error for invalid JSON env, got nil")
	}
}
