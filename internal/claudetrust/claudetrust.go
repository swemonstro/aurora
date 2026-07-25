// Package claudetrust observes Claude project presence in ~/.claude.json via a
// verified Claude root process namespace (/proc/<pid>/root/...). It reads only
// the top-level "projects" keys and never logs file contents.
package claudetrust

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Status is the three-valued Claude trust observation used for startup-pending.
type Status uint8

const (
	Unknown Status = iota
	ProjectMissing
	ProjectPresent
)

const defaultMaxBytes = int64(1 << 20) // 1 MiB

// Observer reads and classifies ~/.claude.json through a process root.
// ProcRoot must point at a procfs-like root (production: /proc).
type Observer struct {
	ProcRoot string
	MaxBytes int64
}

// Observe returns ProjectMissing when the exact cleaned cwd is absent or its
// hasTrustDialogAccepted value is explicitly false. An existing project with
// true, null, or no trust field returns ProjectPresent. Any parse/path/I/O
// uncertainty returns Unknown.
func (o Observer) Observe(pid uint64, userHome, cwd string) Status {
	procRoot := strings.TrimSpace(o.ProcRoot)
	if procRoot == "" {
		procRoot = "/proc"
	}
	procRoot = filepath.Clean(procRoot)
	if !filepath.IsAbs(procRoot) {
		return Unknown
	}
	if pid == 0 {
		return Unknown
	}

	userHome, ok := cleanAbsPath(userHome)
	if !ok {
		return Unknown
	}
	cwd, ok = cleanAbsPath(cwd)
	if !ok {
		return Unknown
	}

	maxBytes := o.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxBytes > 32*defaultMaxBytes {
		// Avoid unbounded callers; exact number is not important, only that it is bounded.
		maxBytes = 32 * defaultMaxBytes
	}

	relHome := strings.TrimPrefix(userHome, string(filepath.Separator))
	if relHome == "" {
		return Unknown
	}

	claudeJSON := filepath.Join(procRoot, strconv.FormatUint(pid, 10), "root", relHome, ".claude.json")
	data, err := readBoundedFile(claudeJSON, maxBytes)
	if err != nil {
		return Unknown
	}

	var top struct {
		Projects map[string]json.RawMessage `json:"projects"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return Unknown
	}
	if top.Projects == nil {
		return Unknown
	}

	want := filepath.Clean(cwd)
	for key, rawProject := range top.Projects {
		k := strings.TrimSpace(key)
		if k == "" || filepath.Clean(k) != want {
			continue
		}

		var project map[string]json.RawMessage
		if err := json.Unmarshal(rawProject, &project); err != nil || project == nil {
			return Unknown
		}

		rawAccepted, exists := project["hasTrustDialogAccepted"]
		if !exists {
			return ProjectPresent
		}

		var accepted *bool
		if err := json.Unmarshal(rawAccepted, &accepted); err != nil {
			return Unknown
		}
		if accepted != nil && !*accepted {
			return ProjectMissing
		}
		return ProjectPresent
	}
	return ProjectMissing
}

func cleanAbsPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	if p == "" || strings.ContainsRune(p, 0) {
		return "", false
	}
	cleaned := filepath.Clean(p)
	if cleaned == "." || !filepath.IsAbs(cleaned) {
		return "", false
	}
	return cleaned, true
}

func readBoundedFile(path string, maxBytes int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	r := io.LimitReader(f, maxBytes+1)
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maxBytes {
		return nil, io.ErrUnexpectedEOF
	}
	return data, nil
}
