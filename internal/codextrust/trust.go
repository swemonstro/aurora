// Package codextrust observes Codex project trust from CODEX_HOME/config.toml.
// It never reads credentials, auth.json, or free-form prompt text.
package codextrust

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// Status is the three-valued trust observation used for startup-pending.
type Status string

const (
	// Trusted means [projects."<dir>"].trust_level is exactly "trusted".
	Trusted Status = "trusted"
	// NotTrusted means the project entry is missing or has another trust_level.
	NotTrusted Status = "not_trusted"
	// Unknown means home/cwd/config could not be established safely.
	Unknown Status = "unknown"
)

// ProjectTrust reads CODEX_HOME/config.toml (or default ~/.codex) and classifies
// the project directory. Paths are cleaned; no content is logged.
func ProjectTrust(codexHomeEnv, projectDir, userHome string) Status {
	projectDir = strings.TrimSpace(projectDir)
	if projectDir == "" || !filepath.IsAbs(projectDir) {
		return Unknown
	}
	projectDir = filepath.Clean(projectDir)

	home, ok := ResolveCodexHome(codexHomeEnv, userHome)
	if !ok {
		return Unknown
	}
	configPath := filepath.Join(home, "config.toml")
	data, err := os.ReadFile(configPath)
	if err != nil {
		// Missing file is treated as not_trusted for a known project path;
		// other I/O errors are unknown so we do not invent attention or clear pending.
		if os.IsNotExist(err) {
			return NotTrusted
		}
		return Unknown
	}
	return classifyConfig(data, projectDir)
}

// ResolveCodexHome returns the absolute Codex home directory.
// codexHomeEnv is process CODEX_HOME (may be empty). userHome is os user home
// for the ~/.codex fallback.
func ResolveCodexHome(codexHomeEnv, userHome string) (string, bool) {
	if value := strings.TrimSpace(codexHomeEnv); value != "" {
		if !filepath.IsAbs(value) {
			return "", false
		}
		return filepath.Clean(value), true
	}
	userHome = strings.TrimSpace(userHome)
	if userHome == "" || !filepath.IsAbs(userHome) {
		return "", false
	}
	return filepath.Join(filepath.Clean(userHome), ".codex"), true
}

// InteractiveArgv reports whether argv looks like a long-lived interactive
// Codex session rather than a short-lived subcommand (exec, login, config, …).
// Classification uses structural tokens only — never terminal display text.
//
// Accepts either raw process argv or the sanitized structural tokens produced
// by linuxprocess (basename + optional allowlisted command token).
func InteractiveArgv(argv []string) bool {
	command := firstStructuralCommand(argv)
	if command == "" {
		// Bare `codex` / launcher without a structural command is the TUI.
		return true
	}
	switch strings.ToLower(command) {
	case "exec", "e", "login", "logout", "config", "completion", "apply", "a",
		"sandbox", "debug", "mcp", "mcp-server", "app-server", "app",
		"plugin", "remote-control", "exec-server", "cloud",
		"help", "features", "proto", "review", "status", "version",
		"uninstall", "update", "doctor", "archive", "delete", "unarchive":
		return false
	case "resume", "fork":
		// Interactive session pickers / TUI continuations.
		return true
	default:
		// Unknown bare token is typically an interactive prompt string.
		return true
	}
}

// firstStructuralCommand returns the first allowlisted command token after a
// Codex entrypoint (direct binary, aurora-codex, or package path basename).
// Help/version flags map to synthetic names. Flag values, absolute script
// paths, and free-form prompts are never returned.
func firstStructuralCommand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	sawCodex := false
	for i := 0; i < len(argv); i++ {
		arg := strings.TrimSpace(argv[i])
		if arg == "" {
			continue
		}
		if arg == "--" {
			return ""
		}
		if mapped := structuralFlagToken(arg); mapped != "" {
			return mapped
		}
		if strings.HasPrefix(arg, "-") {
			if strings.Contains(arg, "=") {
				continue
			}
			if flagTakesValue(arg) && i+1 < len(argv) && !strings.HasPrefix(strings.TrimSpace(argv[i+1]), "-") {
				i++
			}
			continue
		}
		base := strings.ToLower(filepath.Base(arg))
		switch base {
		case "node", "nodejs", "npx":
			continue
		case "codex", "aurora-codex", "codex.js":
			sawCodex = true
			continue
		}
		if strings.HasPrefix(base, "codex.") && strings.HasSuffix(base, ".js") {
			sawCodex = true
			continue
		}
		if token := structuralBareToken(arg); token != "" {
			if sawCodex || i > 0 {
				return token
			}
			return ""
		}
		// Free-form prompt after a codex entrypoint: interactive TUI path.
		return ""
	}
	return ""
}

func structuralFlagToken(arg string) string {
	switch arg {
	case "--help", "-h":
		return "help"
	case "--version", "-V":
		return "version"
	default:
		return ""
	}
}

func structuralBareToken(arg string) string {
	arg = strings.TrimSpace(arg)
	if arg == "" {
		return ""
	}
	lower := strings.ToLower(arg)
	switch lower {
	case "exec", "e", "login", "logout", "config", "completion", "apply", "a",
		"sandbox", "debug", "mcp", "mcp-server", "app-server", "app",
		"plugin", "remote-control", "exec-server", "cloud",
		"help", "features", "proto", "review", "status", "version",
		"uninstall", "update", "doctor", "archive", "delete", "unarchive",
		"resume", "fork":
		return lower
	default:
		return ""
	}
}

func flagTakesValue(flag string) bool {
	switch flag {
	case "-m", "--model", "-c", "--config", "-C", "--cd",
		"--profile", "-p", "--sandbox", "--ask-for-approval", "-a",
		"--output-schema", "--output-last-message", "--image", "-i",
		"--enable", "--disable", "--remote", "--remote-auth-token-env",
		"--local-provider", "--oss":
		// --oss is boolean in help; listed without consuming values via separate path.
		return flag != "--oss"
	default:
		return false
	}
}

type fileConfig struct {
	Projects map[string]projectEntry `toml:"projects"`
}

type projectEntry struct {
	TrustLevel string `toml:"trust_level"`
}

func classifyConfig(data []byte, projectDir string) Status {
	var cfg fileConfig
	if _, err := toml.Decode(string(data), &cfg); err != nil {
		return Unknown
	}
	if cfg.Projects == nil {
		return NotTrusted
	}
	// Exact key match after Clean; also try trailing-slash-normalized variants.
	candidates := []string{projectDir}
	if !strings.HasSuffix(projectDir, string(filepath.Separator)) {
		candidates = append(candidates, projectDir+string(filepath.Separator))
	}
	for key, entry := range cfg.Projects {
		cleaned := filepath.Clean(strings.TrimSpace(key))
		for _, want := range candidates {
			if cleaned == want || cleaned == filepath.Clean(want) {
				if strings.TrimSpace(entry.TrustLevel) == "trusted" {
					return Trusted
				}
				return NotTrusted
			}
		}
	}
	return NotTrusted
}

// FormatConfigPath is exported for tests.
func FormatConfigPath(codexHome string) string {
	return filepath.Join(codexHome, "config.toml")
}
