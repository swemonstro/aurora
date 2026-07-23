// Package runtimerecognition derives observe-only runtime candidates from
// internal, OS-neutral recognition input. It has no transport, persistence,
// registry, or platform dependency.
package runtimerecognition

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

type LaunchRuleMode string

const (
	LaunchRuleExactBasename LaunchRuleMode = "exact_basename"
	LaunchRulePackagePath   LaunchRuleMode = "package_path"
)

// LaunchArgumentRole limits where a configured launch identity may be
// recognized. It prevents arbitrary option values from becoming identity
// evidence.
type LaunchArgumentRole string

const (
	LaunchArgumentArgv0      LaunchArgumentRole = "argv0"
	LaunchArgumentEntrypoint LaunchArgumentRole = "entrypoint"
)

// LaunchIdentityRule is composition-supplied recognition input. It is never
// serialized as part of the public ProcessObservation contract.
type LaunchIdentityRule struct {
	Mode      LaunchRuleMode
	Value     string
	Identity  instancepresence.OpaqueIdentity
	Argument  LaunchArgumentRole
	Launchers []string
}

func (rule LaunchIdentityRule) Validate() error {
	if strings.TrimSpace(rule.Value) == "" || strings.TrimSpace(rule.Value) != rule.Value {
		return errors.New("launch identity rule value must not be empty")
	}
	if err := validateOpaqueIdentity("launch identity rule identity", rule.Identity); err != nil {
		return err
	}
	switch rule.Mode {
	case LaunchRuleExactBasename, LaunchRulePackagePath:
	default:
		return fmt.Errorf("unsupported launch identity rule mode %q", rule.Mode)
	}
	switch rule.Argument {
	case LaunchArgumentArgv0:
		if len(rule.Launchers) != 0 {
			return errors.New("argv0 launch identity rule must not declare launchers")
		}
	case LaunchArgumentEntrypoint:
		if len(rule.Launchers) == 0 {
			return errors.New("entrypoint launch identity rule must declare launchers")
		}
		for index, launcher := range rule.Launchers {
			if launcher != strings.TrimSpace(launcher) || launcher == "" || filepath.Base(launcher) != launcher {
				return fmt.Errorf("launch identity rule launcher %d is invalid", index)
			}
		}
	default:
		return fmt.Errorf("unsupported launch identity rule argument role %q", rule.Argument)
	}
	return nil
}

// Matches accepts only argv[0], or argv[1] when its launcher is explicitly
// allowlisted by an entrypoint rule. Callers discard the raw argv immediately.
func (rule LaunchIdentityRule) Matches(arguments []string, index int) bool {
	if err := rule.Validate(); err != nil {
		return false
	}
	if index < 0 || index >= len(arguments) {
		return false
	}
	switch rule.Argument {
	case LaunchArgumentArgv0:
		if index != 0 {
			return false
		}
	case LaunchArgumentEntrypoint:
		if index != 1 || len(arguments) == 0 || !rule.matchesLauncher(arguments[0]) {
			return false
		}
	}
	argument := arguments[index]
	if strings.HasPrefix(argument, "-") || strings.Contains(argument, "=") {
		return false
	}
	switch rule.Mode {
	case LaunchRuleExactBasename:
		return strings.EqualFold(filepath.Base(argument), rule.Value)
	case LaunchRulePackagePath:
		normalized := "/" + strings.Trim(strings.ReplaceAll(strings.ToLower(argument), "\\", "/"), "/") + "/"
		want := "/" + strings.Trim(strings.ToLower(rule.Value), "/") + "/"
		return strings.Contains(normalized, want)
	default:
		return false
	}
}

func (rule LaunchIdentityRule) matchesLauncher(argument string) bool {
	base := strings.ToLower(filepath.Base(argument))
	for _, launcher := range rule.Launchers {
		if base == strings.ToLower(launcher) {
			return true
		}
	}
	return false
}

// ProcessObservation carries local-only data needed for recognition. It is
// intentionally separate from instancepresence.ProcessObservation so public
// JSON round-trips cannot silently discard recognition evidence.
type ProcessObservation struct {
	Process             instancepresence.ProcessIdentity
	Parent              *instancepresence.ProcessIdentity
	ParentPIDHint       uint64
	CommIdentity        instancepresence.OpaqueIdentity
	ExecutableIdentity  instancepresence.OpaqueIdentity
	LaunchIdentities    []instancepresence.OpaqueIdentity
	ProcessGroupOrJob   instancepresence.OpaqueIdentity
	OSSession           instancepresence.OpaqueIdentity
	TerminalFingerprint instancepresence.OpaqueIdentity
	OwnerIdentity       instancepresence.OpaqueIdentity
	// Suspended is true when the process is stopped (SIGTSTP/SIGSTOP or
	// tracing). It is recognition-local only and never appears on public
	// instancepresence process snapshots or presence wire formats.
	Suspended bool
}

func (observation ProcessObservation) Validate() error {
	if err := observation.Process.Validate(); err != nil {
		return err
	}
	if observation.Parent != nil {
		if err := observation.Parent.Validate(); err != nil {
			return fmt.Errorf("parent process: %w", err)
		}
		if observation.Parent.PID == observation.Process.PID {
			return errors.New("parent process must not share the child PID")
		}
		if observation.Parent.StartedAt.After(observation.Process.StartedAt) {
			return errors.New("parent process must not start after the child")
		}
		if observation.ParentPIDHint != 0 && observation.ParentPIDHint != observation.Parent.PID {
			return errors.New("parent PID hint does not match parent process")
		}
	}
	if err := validateOpaqueIdentity("comm identity", observation.CommIdentity); err != nil {
		return err
	}
	if err := validateOpaqueIdentity("executable identity", observation.ExecutableIdentity); err != nil {
		return err
	}
	if err := validateOpaqueIdentity("owner identity", observation.OwnerIdentity); err != nil {
		return err
	}
	seen := make(map[instancepresence.OpaqueIdentity]struct{}, len(observation.LaunchIdentities))
	for index, identity := range observation.LaunchIdentities {
		if err := validateOpaqueIdentity(fmt.Sprintf("launch identity %d", index), identity); err != nil {
			return err
		}
		if _, exists := seen[identity]; exists {
			return fmt.Errorf("duplicate launch identity %q", identity)
		}
		seen[identity] = struct{}{}
	}
	return nil
}

func validateOpaqueIdentity(name string, identity instancepresence.OpaqueIdentity) error {
	value := string(identity)
	if value == "" || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsSpace) >= 0 {
		return fmt.Errorf("%s must not be empty or contain whitespace", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._:@-", character) {
			continue
		}
		return fmt.Errorf("%s contains unsupported characters", name)
	}
	return nil
}

type Snapshot struct {
	ObservedAt time.Time
	BootID     instancepresence.BootIdentity
	Processes  []ProcessObservation
}

func (snapshot Snapshot) Validate() error {
	if snapshot.ObservedAt.IsZero() {
		return errors.New("runtime snapshot observation time must not be zero")
	}
	if err := snapshot.BootID.Validate(); err != nil {
		return fmt.Errorf("runtime snapshot boot identity: %w", err)
	}
	seen := make(map[string]struct{}, len(snapshot.Processes))
	for index, process := range snapshot.Processes {
		if err := process.Validate(); err != nil {
			return fmt.Errorf("runtime snapshot process %d: %w", index, err)
		}
		key := processIdentityKey(process.Process)
		if _, exists := seen[key]; exists {
			return fmt.Errorf("duplicate runtime snapshot process %q", key)
		}
		seen[key] = struct{}{}
	}
	return nil
}
