package codexproducer

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseSourceFlag(t *testing.T) {
	entry, err := ParseSourceFlag("business=/home/carl/.codex-business")
	if err != nil {
		t.Fatalf("ParseSourceFlag: %v", err)
	}
	if entry.Label != "business" || entry.Path != "/home/carl/.codex-business" {
		t.Fatalf("got %+v", entry)
	}
}

func TestParseSourceFlag_RejectsRelativePath(t *testing.T) {
	if _, err := ParseSourceFlag("business=relative/path"); err == nil {
		t.Fatalf("expected error for relative path")
	}
}

func TestParseSourceFlag_RejectsMissingEquals(t *testing.T) {
	if _, err := ParseSourceFlag("business"); err == nil {
		t.Fatalf("expected error for missing '='")
	}
}

func TestParseSourceFlag_RejectsEmptyLabel(t *testing.T) {
	if _, err := ParseSourceFlag("=/home/carl/.codex"); err == nil {
		t.Fatalf("expected error for empty label")
	}
}

// mkTestDir creates and returns a fresh real directory under t.TempDir().
func mkTestDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestNewSourceSet_RejectsDuplicateLabel(t *testing.T) {
	business := mkTestDir(t, "business")
	other := mkTestDir(t, "other")
	_, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: business},
		{Label: "business", Path: other},
	}, "")
	if err == nil {
		t.Fatalf("expected error for duplicate label")
	}
}

func TestNewSourceSet_RejectsSharedPath(t *testing.T) {
	shared := mkTestDir(t, "shared")
	_, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: shared},
		{Label: "api", Path: shared},
	}, "")
	if err == nil {
		t.Fatalf("expected error: business and api must never share a path")
	}
}

func TestNewSourceSet_RejectsEmpty(t *testing.T) {
	if _, err := NewSourceSet(nil, ""); err == nil {
		t.Fatalf("expected error for zero configured sources")
	}
}

func TestNewSourceSet_RejectsDefaultNotMatchingAnyLabel(t *testing.T) {
	api := mkTestDir(t, "api")
	_, err := NewSourceSet([]SourceEntry{{Label: "api", Path: api}}, "business")
	if err == nil {
		t.Fatalf("expected error: default-source must match a configured label")
	}
}

// TestNewSourceSet_RejectsMissingPathAtStartup is the explicit fail-closed
// behavior decision for a configured source path that does not exist:
// NewSourceSet must refuse to start with an unresolvable source rather than
// silently accept a source that can never match anything.
func TestNewSourceSet_RejectsMissingPathAtStartup(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	_, err := NewSourceSet([]SourceEntry{{Label: "business", Path: missing}}, "")
	if err == nil {
		t.Fatalf("expected error for a configured path that does not exist at startup")
	}
	if strings.Contains(err.Error(), missing) {
		t.Fatalf("missing-path error must never echo the CODEX_HOME path: %v", err)
	}
}

func TestNewSourceSet_RejectsPathThatIsNotADirectory(t *testing.T) {
	file := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := NewSourceSet([]SourceEntry{{Label: "business", Path: file}}, "")
	if err == nil {
		t.Fatalf("expected error for a configured path that is not a directory")
	}
	if strings.Contains(err.Error(), file) {
		t.Fatalf("not-a-directory error must never echo the CODEX_HOME path: %v", err)
	}
}

// TestNewSourceSet_RejectsTwoSymlinksToSameTarget is the core regression
// test for the G.4 pre-commit source-alias finding: two configured sources
// whose paths are textually completely different, but both resolve (via
// independent symlinks) to the same real directory, must be rejected —
// Business and API must never secretly share a real CODEX_HOME.
func TestNewSourceSet_RejectsTwoSymlinksToSameTarget(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real-codex-home")
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	linkA := filepath.Join(root, "business-link")
	linkB := filepath.Join(root, "api-link")
	if err := os.Symlink(target, linkA); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linkB); err != nil {
		t.Fatal(err)
	}
	_, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: linkA},
		{Label: "api", Path: linkB},
	}, "")
	if err == nil {
		t.Fatalf("expected error: two symlinks to the same target must be rejected as aliasing")
	}
	if strings.Contains(err.Error(), target) || strings.Contains(err.Error(), linkA) || strings.Contains(err.Error(), linkB) {
		t.Fatalf("symlink-alias error must never echo any CODEX_HOME path: %v", err)
	}
}

// TestNewSourceSet_RejectsSymlinkAliasingARawConfiguredPath covers the
// asymmetric case: one source configured by its real path, the other by a
// symlink pointing at that same real path.
func TestNewSourceSet_RejectsSymlinkAliasingARawConfiguredPath(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-codex-home")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	_, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: real},
		{Label: "api", Path: link},
	}, "")
	if err == nil {
		t.Fatalf("expected error: a symlink aliasing a raw configured path must be rejected")
	}
}

// TestNewSourceSet_AllowsTwoGenuinelyDifferentDirectories is the negative
// control: two real, unrelated directories must never be rejected.
func TestNewSourceSet_AllowsTwoGenuinelyDifferentDirectories(t *testing.T) {
	business := mkTestDir(t, "business")
	api := mkTestDir(t, "api")
	set, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: business},
		{Label: "api", Path: api},
	}, "")
	if err != nil {
		t.Fatalf("expected two genuinely different directories to be accepted: %v", err)
	}
	if label, ok := set.Match(business); !ok || label != "business" {
		t.Fatalf("got label=%q ok=%v, want business/true", label, ok)
	}
	if label, ok := set.Match(api); !ok || label != "api" {
		t.Fatalf("got label=%q ok=%v, want api/true", label, ok)
	}
}

func TestSourceSet_Match(t *testing.T) {
	business := mkTestDir(t, "business")
	api := mkTestDir(t, "api")
	set, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: business},
		{Label: "api", Path: api},
	}, "")
	if err != nil {
		t.Fatalf("NewSourceSet: %v", err)
	}

	if label, ok := set.Match(business); !ok || label != "business" {
		t.Fatalf("got label=%q ok=%v, want business/true", label, ok)
	}
	if label, ok := set.Match(api); !ok || label != "api" {
		t.Fatalf("got label=%q ok=%v, want api/true", label, ok)
	}
	if _, ok := set.Match(filepath.Join(t.TempDir(), "unconfigured")); ok {
		t.Fatalf("unconfigured CODEX_HOME must not match")
	}
	if _, ok := set.Match(""); ok {
		t.Fatalf("empty CODEX_HOME must not match without an explicit default source")
	}
	if _, ok := set.Match("relative/path"); ok {
		t.Fatalf("a non-absolute observed CODEX_HOME must never match")
	}
	// Trailing slash / non-clean forms must still match after Clean.
	if label, ok := set.Match(business + "/"); !ok || label != "business" {
		t.Fatalf("expected trailing-slash path to still match after Clean, got label=%q ok=%v", label, ok)
	}
}

// TestSourceSet_MatchResolvesSymlinkAliasOfAConfiguredSource is the
// canonicalization-tolerant matching test: a process reporting its
// CODEX_HOME via a symlink that resolves to a configured source's real
// directory must still be attributed to that source, not ignored as
// unconfigured.
func TestSourceSet_MatchResolvesSymlinkAliasOfAConfiguredSource(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real-codex-home")
	if err := os.MkdirAll(real, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "alias-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	set, err := NewSourceSet([]SourceEntry{{Label: "business", Path: real}}, "")
	if err != nil {
		t.Fatalf("NewSourceSet: %v", err)
	}
	if label, ok := set.Match(link); !ok || label != "business" {
		t.Fatalf("expected a symlink alias of the configured path to resolve to business, got label=%q ok=%v", label, ok)
	}
}

func TestSourceSet_MatchNilIsSafe(t *testing.T) {
	var set *SourceSet
	if _, ok := set.Match("/home/carl/.codex"); ok {
		t.Fatalf("nil SourceSet must never match")
	}
}

// TestSourceSet_InstanceIDStillUsesValidatedLabelNotPath verifies that,
// after canonicalization, DeriveInstanceID (and therefore correlation and
// revision state) is still keyed by the operator-chosen, validated label —
// never by the raw or canonical path — so Business and API can never
// collide even though both now carry richer path-identity bookkeeping.
func TestSourceSet_InstanceIDStillUsesValidatedLabelNotPath(t *testing.T) {
	business := mkTestDir(t, "business")
	api := mkTestDir(t, "api")
	set, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: business},
		{Label: "api", Path: api},
	}, "")
	if err != nil {
		t.Fatalf("NewSourceSet: %v", err)
	}
	businessLabel, _ := set.Match(business)
	apiLabel, _ := set.Match(api)

	startedAt := time.Now().UTC()
	businessID := DeriveInstanceID(businessLabel, 555, startedAt)
	apiID := DeriveInstanceID(apiLabel, 555, startedAt)
	if businessID == apiID {
		t.Fatalf("business and api instance ids must never collide for the same PID+StartedAt: %q", businessID)
	}
}
