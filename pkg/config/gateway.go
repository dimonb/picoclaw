package config

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"

	"github.com/sipeed/picoclaw/pkg/logger"
	"github.com/sipeed/picoclaw/pkg/netbind"
)

const DefaultGatewayLogLevel = "warn"

type GatewayConfig struct {
	Host      string `json:"host"                env:"PICOCLAW_GATEWAY_HOST"`
	Port      int    `json:"port"                env:"PICOCLAW_GATEWAY_PORT"`
	HotReload bool   `json:"hot_reload"          env:"PICOCLAW_GATEWAY_HOT_RELOAD"`
	LogLevel  string `json:"log_level,omitempty" env:"PICOCLAW_LOG_LEVEL"`
	// LogMaxSizeMB and LogMaxFiles bound gateway.log, which is otherwise an
	// unbounded append file — on a persistent volume it outlives restarts and
	// just keeps growing. Zero means the logger's defaults.
	LogMaxSizeMB int `json:"log_max_size_mb,omitempty" env:"PICOCLAW_LOG_MAX_SIZE_MB"`
	LogMaxFiles  int `json:"log_max_files,omitempty"   env:"PICOCLAW_LOG_MAX_FILES"`
}

func canonicalGatewayLogLevel(level logger.LogLevel) string {
	switch level {
	case logger.DEBUG:
		return "debug"
	case logger.INFO:
		return "info"
	case logger.WARN:
		return "warn"
	case logger.ERROR:
		return "error"
	case logger.FATAL:
		return "fatal"
	default:
		return DefaultGatewayLogLevel
	}
}

func normalizeGatewayLogLevel(logLevel string) string {
	if level, ok := logger.ParseLevel(logLevel); ok {
		return canonicalGatewayLogLevel(level)
	}
	return DefaultGatewayLogLevel
}

// EffectiveGatewayLogLevel returns the normalized runtime log level from a loaded config.
// Invalid or empty values fall back to the package default.
func EffectiveGatewayLogLevel(cfg *Config) string {
	if cfg == nil {
		return DefaultGatewayLogLevel
	}
	return normalizeGatewayLogLevel(cfg.Gateway.LogLevel)
}

func resolveGatewayHostFromEnv(baseHost string) (string, error) {
	envHost, ok := os.LookupEnv(EnvGatewayHost)
	if !ok {
		return normalizeGatewayHostInput(baseHost)
	}

	envHost = strings.TrimSpace(envHost)
	if envHost == "" {
		return normalizeGatewayHostInput(baseHost)
	}

	return normalizeGatewayHostInput(envHost)
}

func normalizeGatewayHostInput(host string) (string, error) {
	host = strings.TrimSpace(host)
	if host == "" {
		host = strings.TrimSpace(DefaultConfig().Gateway.Host)
	}
	if host == "" {
		host = "localhost"
	}
	return netbind.NormalizeHostInput(host)
}

// ResolveGatewayLogLevel reads the configured gateway log level without triggering
// the full config loader, so startup code can apply logging before config load logs run.
// The PICOCLAW_LOG_LEVEL environment variable overrides the file value.
func ResolveGatewayLogLevel(path string) string {
	cfg := struct {
		Gateway GatewayConfig `json:"gateway"`
	}{
		Gateway: GatewayConfig{LogLevel: DefaultGatewayLogLevel},
	}

	data, err := os.ReadFile(path)
	if err == nil {
		if err := json.Unmarshal(data, &cfg); err != nil {
			logger.WarnCF("config", "failed to parse gateway config, using defaults", map[string]any{
				"path":  path,
				"error": err.Error(),
			})
		}
	}

	if envLevel := os.Getenv("PICOCLAW_LOG_LEVEL"); envLevel != "" {
		cfg.Gateway.LogLevel = envLevel
	}

	return normalizeGatewayLogLevel(cfg.Gateway.LogLevel)
}

// ResolveGatewayLogRotation reads the log rotation bounds the same way, and for
// the same reason: file logging is enabled before the config loader runs, so
// the loader's own output already needs somewhere bounded to go. Zero values
// leave the logger on its defaults.
func ResolveGatewayLogRotation(path string) logger.FileLoggingOptions {
	var cfg struct {
		Gateway GatewayConfig `json:"gateway"`
	}

	if data, err := os.ReadFile(path); err == nil {
		// A parse error is already reported by ResolveGatewayLogLevel, which
		// runs alongside this; staying quiet here avoids duplicating it.
		_ = json.Unmarshal(data, &cfg)
	}

	opts := logger.FileLoggingOptions{
		MaxSizeMB: cfg.Gateway.LogMaxSizeMB,
		MaxFiles:  cfg.Gateway.LogMaxFiles,
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PICOCLAW_LOG_MAX_SIZE_MB"))); err == nil && v > 0 {
		opts.MaxSizeMB = v
	}
	if v, err := strconv.Atoi(strings.TrimSpace(os.Getenv("PICOCLAW_LOG_MAX_FILES"))); err == nil && v > 0 {
		opts.MaxFiles = v
	}
	return opts
}
