package codexproducer

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SourceLabel names one explicitly configured CODEX_HOME root (e.g.
// "business", "api"). It is operator-assigned, opaque to this package beyond
// validation, and never derived from an observed path.
type SourceLabel string

func (label SourceLabel) Validate() error {
	value := string(label)
	if value == "" {
		return fmt.Errorf("source label must not be empty")
	}
	for _, character := range value {
		switch {
		case character >= 'a' && character <= 'z':
		case character >= 'A' && character <= 'Z':
		case character >= '0' && character <= '9':
		case character == '-' || character == '_':
		default:
			return fmt.Errorf("source label contains unsupported characters")
		}
	}
	return nil
}

// SourceSet is the explicit, closed set of CODEX_HOME roots this producer is
// configured to observe. There is no ambient fallback: a process whose
// observed CODEX_HOME does not exactly match one of these configured roots
// (see Match) is never attributed to any source, and is therefore ignored
// rather than guessed at.
//
// Every configured source is canonicalized once, at construction time (see
// NewSourceSet), so two labels can never secretly refer to the same real
// directory through a relative/absolute spelling difference, a trailing
// slash, a "." or ".." component, a symlink, two different symlinks to the
// same target, or a bind mount — os.Stat + os.SameFile compares actual
// filesystem identity (device + inode under the hood), not path text, so it
// catches aliasing that even filepath.EvalSymlinks alone would miss (e.g.
// two independent symlinks to one target, compared directly rather than by
// first resolving each and hoping the resulting strings are byte-identical).
type SourceSet struct {
	// byRawPath is keyed by each entry's own absolute, clean path exactly as
	// configured — the common-case, syscall-free lookup Match tries first,
	// since an observed CODEX_HOME is virtually always textually identical
	// to how the operator configured it.
	byRawPath map[string]SourceLabel
	// byCanonicalPath is keyed by each entry's filepath.EvalSymlinks result
	// (falling back to the raw path if EvalSymlinks fails at construction
	// time, which cannot happen — see NewSourceSet's existence check —
	// except in an unrepresentable race). Match falls back to this, also
	// via EvalSymlinks, only when the fast raw-path lookup misses, so a
	// process whose CODEX_HOME is textually a symlink alias of a configured
	// source still resolves to it.
	byCanonicalPath map[string]SourceLabel
	defaultLabel    SourceLabel
	hasDefault      bool
}

// SourceEntry is one parsed "-source LABEL=PATH" flag occurrence.
type SourceEntry struct {
	Label SourceLabel
	Path  string
}

// ParseSourceFlag parses one "-source" flag value of the form "label=/abs/path".
func ParseSourceFlag(value string) (SourceEntry, error) {
	parts := strings.SplitN(value, "=", 2)
	if len(parts) != 2 {
		return SourceEntry{}, fmt.Errorf("source must be in LABEL=PATH form")
	}
	label := SourceLabel(strings.TrimSpace(parts[0]))
	if err := label.Validate(); err != nil {
		return SourceEntry{}, fmt.Errorf("source label: %w", err)
	}
	path := strings.TrimSpace(parts[1])
	if path == "" {
		return SourceEntry{}, fmt.Errorf("source path must not be empty")
	}
	if !filepath.IsAbs(path) {
		return SourceEntry{}, fmt.Errorf("source path must be absolute")
	}
	return SourceEntry{Label: label, Path: filepath.Clean(path)}, nil
}

// NewSourceSet builds a closed, validated, canonicalized set of CODEX_HOME
// sources. defaultLabel, when non-empty, must name one of entries and is the
// only source a process with an EMPTY observed CODEX_HOME environment
// variable (i.e. Codex's own built-in default location) may match — this is
// an explicit operator opt-in, never an implicit guess (see Match). Passing
// an empty defaultLabel means processes with no CODEX_HOME env var are
// always ignored as unconfigured.
//
// Every configured path must exist and be a directory at construction time:
// a configured source that cannot be resolved fails the whole configuration
// closed (an explicit startup error) rather than silently running with an
// unclear or partially-resolved source. Two entries that resolve to the
// same actual directory (by any means — see SourceSet's doc comment) are
// also rejected outright: this package never arbitrarily picks one label
// over the other, and never echoes the actual path in the resulting error.
func NewSourceSet(entries []SourceEntry, defaultLabel SourceLabel) (*SourceSet, error) {
	if len(entries) == 0 {
		return nil, fmt.Errorf("at least one -source must be configured")
	}
	byRawPath := make(map[string]SourceLabel, len(entries))
	byCanonicalPath := make(map[string]SourceLabel, len(entries))
	seenLabels := make(map[SourceLabel]struct{}, len(entries))
	type resolvedEntry struct {
		label SourceLabel
		info  os.FileInfo
	}
	resolved := make([]resolvedEntry, 0, len(entries))

	for _, entry := range entries {
		if err := entry.Label.Validate(); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(entry.Path) || filepath.Clean(entry.Path) != entry.Path {
			return nil, fmt.Errorf("source path for %q must be an absolute, clean path", entry.Label)
		}
		if _, exists := seenLabels[entry.Label]; exists {
			return nil, fmt.Errorf("duplicate source label %q", entry.Label)
		}
		seenLabels[entry.Label] = struct{}{}
		if existingLabel, exists := byRawPath[entry.Path]; exists {
			return nil, fmt.Errorf("source %q and %q are configured with the identical path string; Business and API must never share a path", existingLabel, entry.Label)
		}

		// Fail closed on a source that does not (yet) exist at startup,
		// rather than run with an unresolvable or ambiguous source: an
		// operator error here should be loud and immediate, not a silently
		// never-matching source discovered only much later in shadow mode.
		info, statErr := os.Stat(entry.Path)
		if statErr != nil {
			return nil, fmt.Errorf("source %q: configured path does not exist or is inaccessible", entry.Label)
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("source %q: configured path is not a directory", entry.Label)
		}

		// os.SameFile compares actual filesystem identity (the OS's own
		// device+inode notion under the hood), not path text: this is what
		// catches two different symlinks to one target, a bind mount, or
		// any other alias filepath.EvalSymlinks alone might not normalize
		// to byte-identical strings.
		for _, previous := range resolved {
			if os.SameFile(info, previous.info) {
				return nil, fmt.Errorf("source %q and %q resolve to the same actual directory; Business and API must never share a real CODEX_HOME", previous.label, entry.Label)
			}
		}
		resolved = append(resolved, resolvedEntry{label: entry.Label, info: info})
		byRawPath[entry.Path] = entry.Label

		canonicalPath := entry.Path
		if evaluated, evalErr := filepath.EvalSymlinks(entry.Path); evalErr == nil {
			canonicalPath = evaluated
		}
		if existingLabel, exists := byCanonicalPath[canonicalPath]; exists {
			return nil, fmt.Errorf("source %q and %q resolve to the same canonical path; Business and API must never share a real CODEX_HOME", existingLabel, entry.Label)
		}
		byCanonicalPath[canonicalPath] = entry.Label
	}

	set := &SourceSet{byRawPath: byRawPath, byCanonicalPath: byCanonicalPath}
	if defaultLabel != "" {
		if err := defaultLabel.Validate(); err != nil {
			return nil, fmt.Errorf("default source: %w", err)
		}
		if _, exists := seenLabels[defaultLabel]; !exists {
			return nil, fmt.Errorf("default source %q must match one configured -source label", defaultLabel)
		}
		set.defaultLabel, set.hasDefault = defaultLabel, true
	}
	return set, nil
}

// Match resolves an observed, already-absolute CODEX_HOME path (or empty,
// meaning the process had no CODEX_HOME environment variable set) to exactly
// one configured source label. It never falls back to ambient HOME or
// guesses: an empty envCodexHome only matches when the operator explicitly
// configured a default source, and any non-empty value must equal one
// configured source's raw path, or (falling back, best-effort) its
// canonical (symlink-resolved) path, or the process is unconfigured.
func (set *SourceSet) Match(envCodexHome string) (SourceLabel, bool) {
	if set == nil {
		return "", false
	}
	if envCodexHome == "" {
		if set.hasDefault {
			return set.defaultLabel, true
		}
		return "", false
	}
	if !filepath.IsAbs(envCodexHome) {
		return "", false
	}
	cleaned := filepath.Clean(envCodexHome)
	if label, exists := set.byRawPath[cleaned]; exists {
		return label, true
	}
	// Fall back to canonical resolution only when the fast exact-path
	// lookup misses: this is what lets a process whose CODEX_HOME is
	// textually a symlink alias of a configured source still resolve to
	// it. A failure here (path no longer exists, permission denied, ...)
	// is treated exactly like "unconfigured" — never an error, never a
	// crash, never a guess.
	if canonical, err := filepath.EvalSymlinks(cleaned); err == nil {
		if label, exists := set.byCanonicalPath[canonical]; exists {
			return label, true
		}
	}
	return "", false
}

// Labels returns every configured source label, sorted is not guaranteed;
// callers that need determinism should sort the result themselves.
func (set *SourceSet) Labels() []SourceLabel {
	labels := make([]SourceLabel, 0, len(set.byRawPath))
	for _, label := range set.byRawPath {
		labels = append(labels, label)
	}
	return labels
}
