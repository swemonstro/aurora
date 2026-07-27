// Package grokpresence contains the Grok-specific runtime adapter.
package grokpresence

import (
	"strings"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

// linuxCommMaxLen is the maximum length of Linux /proc/<pid>/comm
// (TASK_COMM_LEN-1). Longer executable basenames appear truncated here.
const linuxCommMaxLen = 15

const (
	officialGrokName              = "grok"
	officialGrokLinuxAMD64        = "grok-linux-x86_64"
	officialVersionedGrokPrefix   = "grok-"
	officialVersionedGrokPlatform = "-linux-x86_64"
)

// RuntimeRecognizer recognizes the interactive Grok CLI process.
func RuntimeRecognizer() runtimerecognition.AgentRuntimeRecognizer {
	return runtimeRecognizer{}
}

type runtimeRecognizer struct{}

func (runtimeRecognizer) Recognize(
	process runtimerecognition.ProcessObservation,
) (runtimerecognition.Recognition, bool) {
	// Executable basenames must be complete official names only.
	// Truncated Linux comm forms are never valid executable identities.
	if isAllowedGrokExecutableName(normalizeGrokIdentity(process.ExecutableIdentity)) {
		return grokRecognition(), true
	}
	// Comm may be a full official name or a 15-char Linux truncation of one.
	if isAllowedGrokCommName(normalizeGrokIdentity(process.CommIdentity)) {
		return grokRecognition(), true
	}
	return runtimerecognition.Recognition{}, false
}

func grokRecognition() runtimerecognition.Recognition {
	return runtimerecognition.Recognition{
		Tool:     instancepresence.ToolGrok,
		Role:     runtimerecognition.RoleDirect,
		Priority: runtimerecognition.PriorityExecutable,
	}
}

func normalizeGrokIdentity(identity instancepresence.OpaqueIdentity) string {
	return strings.TrimPrefix(strings.ToLower(string(identity)), "exe:")
}

// isAllowedGrokExecutableName reports whether name is a complete allowlisted
// Grok CLI executable basename. Truncated Linux comm forms are not accepted.
//
// Allowed forms:
//   - exact "grok" (symlink / unversioned entrypoint)
//   - exact "grok-linux-x86_64" (official local download basename)
//   - "grok-<major>.<minor>.<patch>-linux-x86_64" (official versioned basename)
//
// Deliberately not accepted: arbitrary "grok-" prefixes, helpers, lookalikes,
// or 15-character Linux-comm truncations.
func isAllowedGrokExecutableName(name string) bool {
	if name == officialGrokName {
		return true
	}
	if name == officialGrokLinuxAMD64 {
		return true
	}
	return isOfficialVersionedGrokLinuxAMD64(name)
}

// isAllowedGrokCommName reports whether name is an allowlisted Grok CLI
// identity as observed in Linux comm (or an untruncated full name that
// may also appear there).
//
// Allowed forms:
//   - any complete form accepted by isAllowedGrokExecutableName
//   - exactly 15 characters that are a Linux-comm truncation of an official
//     long form (versioned or unversioned)
func isAllowedGrokCommName(name string) bool {
	if isAllowedGrokExecutableName(name) {
		return true
	}
	if len(name) != linuxCommMaxLen {
		return false
	}
	if isTruncatedVersionedGrokLinuxAMD64Comm(name) {
		return true
	}
	// Truncation of the exact unversioned official download name.
	return strings.HasPrefix(officialGrokLinuxAMD64, name)
}

// isOfficialVersionedGrokLinuxAMD64 reports whether name matches the official
// versioned Grok Linux amd64 download basename:
//
//	grok-<numeric major>.<minor>.<patch>-linux-x86_64
func isOfficialVersionedGrokLinuxAMD64(name string) bool {
	if !strings.HasPrefix(name, officialVersionedGrokPrefix) ||
		!strings.HasSuffix(name, officialVersionedGrokPlatform) {
		return false
	}
	// Require a non-empty middle so prefix/suffix cannot overlap
	// (e.g. "grok-linux-x86_64" must not be treated as versioned).
	if len(name) <= len(officialVersionedGrokPrefix)+len(officialVersionedGrokPlatform) {
		return false
	}
	version := name[len(officialVersionedGrokPrefix) : len(name)-len(officialVersionedGrokPlatform)]
	return isNumericSemverTriple(version)
}

// isNumericSemverTriple reports whether version is exactly three non-empty
// all-digit components separated by dots (e.g. "0.2.112").
func isNumericSemverTriple(version string) bool {
	parts := strings.Split(version, ".")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" || !isAllDigits(part) {
			return false
		}
	}
	return true
}

// isTruncatedVersionedGrokLinuxAMD64Comm reports whether name is exactly a
// 15-character Linux comm truncation of an official versioned basename
// grok-<major>.<minor>.<patch>-linux-x86_64.
//
// The truncated form must contain:
//   - the prefix "grok-"
//   - exactly three complete, non-empty numeric version components
//   - a non-empty proper prefix of "-linux-x86_64"
//
// Names that end mid-version (e.g. "grok-1234567890", "grok-0.2.123456")
// are rejected.
func isTruncatedVersionedGrokLinuxAMD64Comm(name string) bool {
	if len(name) != linuxCommMaxLen {
		return false
	}
	if !strings.HasPrefix(name, officialVersionedGrokPrefix) {
		return false
	}
	rest := name[len(officialVersionedGrokPrefix):]

	pos := 0
	for component := 0; component < 3; component++ {
		if component > 0 {
			if pos >= len(rest) || rest[pos] != '.' {
				return false
			}
			pos++
		}
		start := pos
		for pos < len(rest) && isDigit(rest[pos]) {
			pos++
		}
		if pos == start {
			// Empty version component (or input ended before digits).
			return false
		}
	}

	platformPart := rest[pos:]
	// Must have reached the platform suffix and truncated inside it.
	if platformPart == "" || len(platformPart) >= len(officialVersionedGrokPlatform) {
		return false
	}
	return officialVersionedGrokPlatform[:len(platformPart)] == platformPart
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			return false
		}
	}
	return true
}

func isDigit(b byte) bool {
	return b >= '0' && b <= '9'
}
