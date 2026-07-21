package codexhook

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	DefaultRelayURL  = "http://127.0.0.1:8080"
	RelayURLEnv      = "AURORA_RELAY_URL"
	SourceEnv        = "AURORA_CODEX_SOURCE"
	StateFileEnv     = "AURORA_CODEX_STATE_FILE"
	SessionTTLEnv    = "AURORA_CODEX_SESSION_TTL"
	SessionIDFileEnv = "AURORA_CODEX_SESSION_ID_FILE"
	DefaultSource    = "codex"
	DefaultStateName = "codex-sessions.json"
)

type Config struct {
	RelayURL      string
	Source        string
	StatePath     string
	SessionIDPath string
	TTL           time.Duration
}

func ConfigFromEnv(
	getenv func(string) string,
	userHomeDir func() (string, error),
) (Config, error) {
	home, err := userHomeDir()
	if err != nil {
		return Config{}, fmt.Errorf("resolve user home directory: %w", err)
	}

	config := Config{
		RelayURL: DefaultRelayURL,
		Source:   DefaultSource,
		StatePath: filepath.Join(
			home,
			".local",
			"state",
			"aurora",
			DefaultStateName,
		),
		TTL: DefaultSessionTTL,
	}

	if value := strings.TrimSpace(getenv(RelayURLEnv)); value != "" {
		config.RelayURL = value
	}
	if value := strings.TrimSpace(getenv(SourceEnv)); value != "" {
		config.Source = value
	}
	if value := strings.TrimSpace(getenv(StateFileEnv)); value != "" {
		config.StatePath, err = expandHome(value, home)
		if err != nil {
			return Config{}, err
		}
	}
	if value := strings.TrimSpace(getenv(SessionIDFileEnv)); value != "" {
		config.SessionIDPath, err = expandHome(value, home)
		if err != nil {
			return Config{}, err
		}
	}
	if value := strings.TrimSpace(getenv(SessionTTLEnv)); value != "" {
		if parsed, parseErr := time.ParseDuration(value); parseErr == nil && parsed > 0 {
			config.TTL = parsed
		}
	}

	return config, nil
}

func expandHome(path string, home string) (string, error) {
	if path == "~" {
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	if strings.HasPrefix(path, "~") {
		return "", fmt.Errorf("unsupported home-relative state path %q", path)
	}

	return path, nil
}
