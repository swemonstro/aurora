package codexhook

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConfigDefaults(t *testing.T) {
	config, err := ConfigFromEnv(
		func(string) string { return "" },
		func() (string, error) { return "/home/test", nil },
	)
	if err != nil {
		t.Fatalf("ConfigFromEnv returned error: %v", err)
	}

	if config.RelayURL != DefaultRelayURL {
		t.Fatalf("RelayURL = %q", config.RelayURL)
	}
	if config.Source != DefaultSource {
		t.Fatalf("Source = %q", config.Source)
	}
	if config.StatePath != filepath.Join(
		"/home/test",
		".local",
		"state",
		"aurora",
		DefaultStateName,
	) {
		t.Fatalf("StatePath = %q", config.StatePath)
	}
	if config.TTL != DefaultSessionTTL {
		t.Fatalf("TTL = %s", config.TTL)
	}
}

func TestConfigOverrides(t *testing.T) {
	values := map[string]string{
		RelayURLEnv:   " http://relay:8080 ",
		SourceEnv:     " codex-business ",
		StateFileEnv:  " ~/.local/state/aurora/business.json ",
		SessionTTLEnv: " 2h ",
	}

	config, err := ConfigFromEnv(
		func(key string) string { return values[key] },
		func() (string, error) { return "/home/test", nil },
	)
	if err != nil {
		t.Fatalf("ConfigFromEnv returned error: %v", err)
	}

	if config.RelayURL != "http://relay:8080" {
		t.Fatalf("RelayURL = %q", config.RelayURL)
	}
	if config.Source != "codex-business" {
		t.Fatalf("Source = %q", config.Source)
	}
	if config.StatePath != "/home/test/.local/state/aurora/business.json" {
		t.Fatalf("StatePath = %q", config.StatePath)
	}
	if config.TTL != 2*time.Hour {
		t.Fatalf("TTL = %s", config.TTL)
	}
}

func TestInvalidTTLFallsBackToDefault(t *testing.T) {
	for _, value := range []string{"invalid", "0", "-1h"} {
		t.Run(value, func(t *testing.T) {
			config, err := ConfigFromEnv(
				func(key string) string {
					if key == SessionTTLEnv {
						return value
					}
					return ""
				},
				func() (string, error) { return "/home/test", nil },
			)
			if err != nil {
				t.Fatalf("ConfigFromEnv returned error: %v", err)
			}
			if config.TTL != DefaultSessionTTL {
				t.Fatalf("TTL = %s", config.TTL)
			}
		})
	}
}
