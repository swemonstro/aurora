package codextrust

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProjectTrustTrustedAndNotTrusted(t *testing.T) {
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(project, 0o700); err != nil {
		t.Fatal(err)
	}
	// Missing config → not_trusted (known paths).
	if got := ProjectTrust(home, project, ""); got != NotTrusted {
		t.Fatalf("missing config = %q, want not_trusted", got)
	}
	// Wrong trust_level.
	writeConfig(t, home, project, "untrusted")
	if got := ProjectTrust(home, project, ""); got != NotTrusted {
		t.Fatalf("untrusted = %q", got)
	}
	// Exact trusted.
	writeConfig(t, home, project, "trusted")
	if got := ProjectTrust(home, project, ""); got != Trusted {
		t.Fatalf("trusted = %q", got)
	}
	// Other project directory remains not_trusted.
	other := filepath.Join(t.TempDir(), "other")
	_ = os.MkdirAll(other, 0o700)
	if got := ProjectTrust(home, other, ""); got != NotTrusted {
		t.Fatalf("other project = %q", got)
	}
}

func TestProjectTrustUnknownCases(t *testing.T) {
	if got := ProjectTrust("/tmp/home", "relative", ""); got != Unknown {
		t.Fatalf("relative project = %q", got)
	}
	if got := ProjectTrust("relative-home", "/abs/project", ""); got != Unknown {
		t.Fatalf("relative CODEX_HOME = %q", got)
	}
	// Unreadable / invalid TOML.
	home := t.TempDir()
	project := filepath.Join(t.TempDir(), "repo")
	_ = os.MkdirAll(project, 0o700)
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte("[[[not-toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := ProjectTrust(home, project, ""); got != Unknown {
		t.Fatalf("bad toml = %q", got)
	}
}

func TestResolveCodexHomeFallback(t *testing.T) {
	user := t.TempDir()
	got, ok := ResolveCodexHome("", user)
	if !ok || got != filepath.Join(user, ".codex") {
		t.Fatalf("fallback = %q ok=%t", got, ok)
	}
	explicit := filepath.Join(t.TempDir(), "profile")
	got, ok = ResolveCodexHome(explicit, user)
	if !ok || got != explicit {
		t.Fatalf("explicit = %q ok=%t", got, ok)
	}
}

func TestInteractiveArgv(t *testing.T) {
	for _, test := range []struct {
		name string
		argv []string
		want bool
	}{
		{name: "bare", argv: []string{"codex"}, want: true},
		{name: "path bare", argv: []string{"/usr/bin/codex"}, want: true},
		{name: "resume", argv: []string{"codex", "resume", "sess"}, want: true},
		{name: "fork", argv: []string{"codex", "fork"}, want: true},
		{name: "exec", argv: []string{"codex", "exec", "ls"}, want: false},
		{name: "login", argv: []string{"codex", "login"}, want: false},
		{name: "config", argv: []string{"codex", "config", "show"}, want: false},
		{name: "status", argv: []string{"codex", "status"}, want: false},
		{name: "app", argv: []string{"codex", "app"}, want: false},
		{name: "help long", argv: []string{"codex", "--help"}, want: false},
		{name: "help short", argv: []string{"codex", "-h"}, want: false},
		{name: "version long", argv: []string{"codex", "--version"}, want: false},
		{name: "version short", argv: []string{"codex", "-V"}, want: false},
		{name: "profile then exec", argv: []string{"codex", "--profile", "business", "exec", "ls"}, want: false},
		{name: "config flag then login", argv: []string{"codex", "-c", "key=value", "login"}, want: false},
		{name: "model flag only", argv: []string{"codex", "--model", "gpt"}, want: true},
		{name: "model then free prompt", argv: []string{"codex", "--model", "gpt", "hemlig prompt"}, want: true},
		{name: "sanitized help token", argv: []string{"codex", "help"}, want: false},
		{name: "sanitized version token", argv: []string{"codex", "version"}, want: false},
		{name: "sanitized exec only", argv: []string{"codex", "exec"}, want: false},
		{
			name: "node package wrapper exec",
			argv: []string{"node", "/opt/node_modules/@openai/codex/bin/codex.js", "exec", "ls"},
			want: false,
		},
		{
			name: "node package wrapper interactive",
			argv: []string{"node", "/opt/node_modules/@openai/codex/bin/codex.js"},
			want: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := InteractiveArgv(test.argv); got != test.want {
				t.Fatalf("InteractiveArgv(%q) = %t, want %t", test.argv, got, test.want)
			}
		})
	}
}

func writeConfig(t *testing.T, home, project, trust string) {
	t.Helper()
	// TOML table key with quotes for absolute path.
	body := "[projects." + quoteTOML(project) + "]\ntrust_level = " + quoteTOML(trust) + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func quoteTOML(value string) string {
	return `"` + value + `"`
}
