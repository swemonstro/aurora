// Package linuxprocess implements Linux-specific, observe-only process
// discovery. It never writes presence state or communicates with a relay.
package linuxprocess

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

var (
	ErrMalformedStat   = errors.New("malformed proc stat")
	ErrInvalidBootTime = errors.New("invalid proc boot time")
	ErrUnsafeProcEntry = errors.New("unsafe proc entry")
	ErrReadLimit       = errors.New("proc read limit exceeded")
)

type ReasonCode string

const (
	ReasonUnknownProcess        ReasonCode = "unknown_process"
	ReasonProcessDisappeared    ReasonCode = "process_disappeared_during_read"
	ReasonInvalidProcData       ReasonCode = "invalid_proc_data"
	ReasonPIDReused             ReasonCode = "pid_reused"
	ReasonPermissionDenied      ReasonCode = "permission_denied"
	ReasonArgvPrefixTruncated   ReasonCode = "argv_prefix_truncated"
	ReasonAncestryDepthExceeded ReasonCode = "ancestry_depth_exceeded"
)

type Diagnostic struct {
	Code  ReasonCode `json:"code"`
	Count uint64     `json:"count"`
}

type Sample struct {
	Snapshot      instancepresence.ProcessSnapshot
	Recognition   runtimerecognition.Snapshot
	Diagnostics   []Diagnostic
	uncertainPIDs map[uint64]struct{}
}

type Config struct {
	ProcRoot            string
	HostID              string
	BootID              instancepresence.BootIdentity
	Clock               instancepresence.Clock
	ClockTicks          uint64
	LaunchIdentityRules []runtimerecognition.LaunchIdentityRule
}

func (config Config) validate() error {
	if strings.TrimSpace(config.ProcRoot) == "" {
		return errors.New("proc root must not be empty")
	}
	if strings.TrimSpace(config.HostID) == "" {
		return errors.New("host ID must not be empty")
	}
	if config.BootID != "" {
		if err := config.BootID.Validate(); err != nil {
			return err
		}
	}
	if config.Clock == nil {
		return errors.New("clock must not be nil")
	}
	for index, rule := range config.LaunchIdentityRules {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("launch identity rule %d: %w", index, err)
		}
	}
	return nil
}

func diagnosticsFromCounts(counts map[ReasonCode]uint64) []Diagnostic {
	diagnostics := make([]Diagnostic, 0, len(counts))
	for code, count := range counts {
		if count > 0 {
			diagnostics = append(diagnostics, Diagnostic{Code: code, Count: count})
		}
	}
	sort.Slice(diagnostics, func(first, second int) bool {
		return diagnostics[first].Code < diagnostics[second].Code
	})
	return diagnostics
}

func processKey(identity instancepresence.ProcessIdentity) string {
	return fmt.Sprintf("%020d/%020d", identity.PID, identity.StartedAt.UnixNano())
}

func sameProcess(first, second instancepresence.ProcessIdentity) bool {
	return first.PID == second.PID && first.StartedAt.Equal(second.StartedAt)
}

func sortProcesses(processes []instancepresence.ProcessIdentity) {
	sort.Slice(processes, func(first, second int) bool {
		if processes[first].PID != processes[second].PID {
			return processes[first].PID < processes[second].PID
		}
		return processes[first].StartedAt.Before(processes[second].StartedAt)
	})
}

func sortReasonCodes(codes []ReasonCode) {
	sort.Slice(codes, func(first, second int) bool { return codes[first] < codes[second] })
}

func validObservedAt(value time.Time) error {
	if value.IsZero() {
		return errors.New("observation time must not be zero")
	}
	return nil
}
