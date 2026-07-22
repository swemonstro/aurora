package linuxprocess

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

const (
	defaultClockTicks = 100
	statReadLimit     = 16 * 1024
	commReadLimit     = 256
	cmdlineReadLimit  = 1024
	statusReadLimit   = 16 * 1024
	bootIDReadLimit   = 256
)

type readerFactory func() (procReader, error)

type Adapter struct {
	config Config
	open   readerFactory
}

var _ instancepresence.ProcessAdapter = (*Adapter)(nil)

func New(config Config) (*Adapter, error) {
	if err := config.validate(); err != nil {
		return nil, fmt.Errorf("linux process adapter config: %w", err)
	}
	if config.ClockTicks == 0 {
		config.ClockTicks = defaultClockTicks
	}
	// Resolve and validate the configured root now; each sample opens a fresh
	// descriptor so no directory handle survives between bounded observations.
	reader, err := openProcRoot(config.ProcRoot)
	if err != nil {
		return nil, err
	}
	if err := reader.Close(); err != nil {
		return nil, fmt.Errorf("close proc root after validation: %w", err)
	}
	return &Adapter{config: config, open: func() (procReader, error) {
		return openProcRoot(config.ProcRoot)
	}}, nil
}

func newWithReader(config Config, open readerFactory) (*Adapter, error) {
	if err := config.validate(); err != nil {
		return nil, err
	}
	if open == nil {
		return nil, errors.New("proc reader factory must not be nil")
	}
	if config.ClockTicks == 0 {
		config.ClockTicks = defaultClockTicks
	}
	return &Adapter{config: config, open: open}, nil
}

func (adapter *Adapter) BootIdentity(ctx context.Context) (instancepresence.BootIdentity, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if adapter.config.BootID != "" {
		return adapter.config.BootID, nil
	}
	reader, err := adapter.open()
	if err != nil {
		return "", err
	}
	defer reader.Close()
	return readBootIdentity(reader)
}

func (adapter *Adapter) Snapshot(ctx context.Context) (instancepresence.ProcessSnapshot, error) {
	sample, err := adapter.Observe(ctx)
	return sample.Snapshot, err
}

func (adapter *Adapter) Observe(ctx context.Context) (Sample, error) {
	if err := ctx.Err(); err != nil {
		return Sample{}, err
	}
	observedAt := adapter.config.Clock.Now()
	if err := validObservedAt(observedAt); err != nil {
		return Sample{}, fmt.Errorf("linux process adapter: %w", err)
	}
	reader, err := adapter.open()
	if err != nil {
		return Sample{}, err
	}
	defer reader.Close()

	bootID := adapter.config.BootID
	if bootID == "" {
		bootID, err = readBootIdentity(reader)
		if err != nil {
			return Sample{}, err
		}
	}
	bootData, err := reader.ReadFile("stat", statReadLimit)
	if err != nil {
		return Sample{}, fmt.Errorf("read proc boot time: %w", err)
	}
	bootTime, err := parseBootTime(bootData)
	if err != nil {
		return Sample{}, err
	}
	entries, err := reader.ReadDir(".")
	if err != nil {
		return Sample{}, fmt.Errorf("list proc root: %w", err)
	}

	counts := make(map[ReasonCode]uint64)
	uncertainPIDs := make(map[uint64]struct{})
	records := make([]rawProcess, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return Sample{}, err
		}
		pid, ok := numericPID(entry.Name())
		if !ok || entry.Type()&fs.ModeSymlink != 0 {
			continue
		}
		record, outcome := readProcess(reader, pid, bootTime, adapter.config.ClockTicks)
		for code, count := range outcome.counts {
			counts[code] += count
		}
		if outcome.uncertain {
			uncertainPIDs[pid] = struct{}{}
		}
		if outcome.accepted {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(first, second int) bool {
		return processKey(records[first].identity) < processKey(records[second].identity)
	})

	byPID := make(map[uint64]*rawProcess, len(records))
	for index := range records {
		byPID[records[index].identity.PID] = &records[index]
	}
	observations := make([]instancepresence.ProcessObservation, 0, len(records))
	unknown := uint64(0)
	for index := range records {
		record := &records[index]
		record.classification = classify(*record)
		if record.classification.tool == "" {
			unknown++
		}
		var parent *instancepresence.ProcessIdentity
		if parentRecord := byPID[record.stat.ParentPID]; parentRecord != nil {
			parentIdentity := parentRecord.identity
			parent = &parentIdentity
		}
		observation := instancepresence.ProcessObservation{
			Process: record.identity, Parent: parent,
			ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + sanitizeName(record.executableBase)),
			OwnerIdentity:      instancepresence.OpaqueIdentity(record.ownerIdentity),
		}
		if record.stat.GroupID != 0 {
			observation.ProcessGroupOrJob = instancepresence.OpaqueIdentity(fmt.Sprintf("pgrp:%d", record.stat.GroupID))
		}
		if record.stat.SessionID != 0 {
			observation.OSSession = instancepresence.OpaqueIdentity(fmt.Sprintf("session:%d", record.stat.SessionID))
		}
		if record.stat.TTY != 0 {
			observation.TerminalFingerprint = instancepresence.OpaqueIdentity(fmt.Sprintf("tty:%d", record.stat.TTY))
		}
		observations = append(observations, observation)
	}
	snapshot := instancepresence.ProcessSnapshot{ObservedAt: observedAt, Processes: observations}
	if err := snapshot.Validate(); err != nil {
		return Sample{}, fmt.Errorf("linux process snapshot invariant: %w", err)
	}

	families, uncertainFamilies := buildFamilies(adapter.config.HostID, bootID, records)
	for _, family := range families {
		for _, code := range family.ReasonCodes {
			counts[code]++
		}
	}
	for _, family := range uncertainFamilies {
		for _, code := range family.ReasonCodes {
			counts[code]++
		}
	}
	counts[ReasonUnknownProcess] += unknown
	summary := Summary{
		ObservedProcesses: uint64(len(observations)), UnknownProcesses: unknown,
		AmbiguousFamilies: uint64(len(uncertainFamilies)),
	}
	for _, family := range families {
		switch family.Candidate.Tool {
		case instancepresence.ToolClaude:
			summary.ClaudeFamilies++
		case instancepresence.ToolCodex:
			summary.CodexFamilies++
		}
	}
	return Sample{
		Snapshot: snapshot, Families: families, UncertainFamilies: uncertainFamilies,
		Diagnostics: diagnosticsFromCounts(counts), Summary: summary, uncertainPIDs: uncertainPIDs,
	}, nil
}

type rawProcess struct {
	stat           procStat
	identity       instancepresence.ProcessIdentity
	executableBase string
	argvPrefix     []string
	ownerIdentity  string
	classification classification
}

type readOutcome struct {
	accepted  bool
	uncertain bool
	counts    map[ReasonCode]uint64
}

func readProcess(reader procReader, pid uint64, bootTime time.Time, clockTicks uint64) (rawProcess, readOutcome) {
	outcome := readOutcome{counts: make(map[ReasonCode]uint64)}
	statPath := path.Join(strconv.FormatUint(pid, 10), "stat")
	initialData, err := reader.ReadFile(statPath, statReadLimit)
	if err != nil {
		outcome.uncertain = true
		outcome.counts[classifyReadError(err)]++
		return rawProcess{}, outcome
	}
	initial, err := parseProcStat(initialData)
	if err != nil || initial.PID != pid {
		outcome.uncertain = true
		outcome.counts[ReasonInvalidProcData]++
		return rawProcess{}, outcome
	}

	directory := strconv.FormatUint(pid, 10)
	comm := optionalText(reader, path.Join(directory, "comm"), commReadLimit, outcome.counts)
	if comm == "" {
		comm = initial.Comm
	}
	argvData := optionalBytes(reader, path.Join(directory, "cmdline"), cmdlineReadLimit, outcome.counts)
	owner := ownerFromStatus(optionalBytes(reader, path.Join(directory, "status"), statusReadLimit, outcome.counts))
	if owner == "" {
		owner = "uid:unknown"
	}

	finalData, err := reader.ReadFile(statPath, statReadLimit)
	if err != nil {
		outcome.uncertain = true
		outcome.counts[classifyReadError(err)]++
		return rawProcess{}, outcome
	}
	final, err := parseProcStat(finalData)
	if err != nil || final.PID != pid {
		outcome.uncertain = true
		outcome.counts[ReasonInvalidProcData]++
		return rawProcess{}, outcome
	}
	if initial.StartTicks != final.StartTicks {
		outcome.uncertain = true
		outcome.counts[ReasonPIDReused]++
		return rawProcess{}, outcome
	}
	executable := comm
	arguments := argvSignals(argvData)
	if len(arguments) > 0 {
		if base := filepath.Base(arguments[0]); base != "." && base != string(filepath.Separator) && base != "" {
			executable = base
		}
	}
	outcome.accepted = true
	return rawProcess{
		stat: final,
		identity: instancepresence.ProcessIdentity{
			PID: pid, StartedAt: startedAt(bootTime, final.StartTicks, clockTicks),
		},
		executableBase: executable, argvPrefix: arguments, ownerIdentity: owner,
	}, outcome
}

func optionalText(reader procReader, name string, limit int64, counts map[ReasonCode]uint64) string {
	return strings.TrimSpace(string(optionalBytes(reader, name, limit, counts)))
}

func optionalBytes(reader procReader, name string, limit int64, counts map[ReasonCode]uint64) []byte {
	data, err := reader.ReadFile(name, limit)
	if err != nil {
		if errors.Is(err, ErrReadLimit) && path.Base(name) == "cmdline" {
			counts[ReasonArgvPrefixTruncated]++
			return data
		}
		counts[classifyReadError(err)]++
		return nil
	}
	return data
}

func classifyReadError(err error) ReasonCode {
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ReasonProcessDisappeared
	case errors.Is(err, fs.ErrPermission):
		return ReasonPermissionDenied
	default:
		return ReasonInvalidProcData
	}
}

func readBootIdentity(reader procReader) (instancepresence.BootIdentity, error) {
	data, err := reader.ReadFile("sys/kernel/random/boot_id", bootIDReadLimit)
	if err != nil {
		return "", fmt.Errorf("read boot identity: %w", err)
	}
	identity := instancepresence.BootIdentity(strings.TrimSpace(string(data)))
	if err := identity.Validate(); err != nil {
		return "", fmt.Errorf("read boot identity: %w", err)
	}
	return identity, nil
}

func numericPID(name string) (uint64, bool) {
	pid, err := strconv.ParseUint(name, 10, 64)
	return pid, err == nil && pid > 0 && strconv.FormatUint(pid, 10) == name
}

// argvSignals immediately reduces the bounded local prefix to allowlisted
// classifier markers. Unknown argument values are discarded, not retained.
func argvSignals(data []byte) []string {
	const maxArguments = 4
	parts := strings.Split(string(data), "\x00")
	arguments := make([]string, 0, maxArguments)
	for index, part := range parts {
		if part == "" {
			continue
		}
		lower := strings.ToLower(part)
		base := normalizeName(filepath.Base(part))
		signal := ""
		switch {
		case index == 0:
			signal = sanitizeName(base)
		case strings.Contains(lower, "@anthropic-ai/claude-code"):
			signal = "@anthropic-ai/claude-code"
		case strings.Contains(lower, "@openai/codex"):
			signal = "@openai/codex"
		case isClaudeBinary(base) || isCodexBinary(base):
			signal = base
		}
		if signal != "" {
			arguments = append(arguments, signal)
		}
		if index+1 == maxArguments {
			break
		}
	}
	return arguments
}

func ownerFromStatus(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "Uid:" {
			if _, err := strconv.ParseUint(fields[1], 10, 64); err == nil {
				return "uid:" + fields[1]
			}
		}
	}
	return ""
}

func sanitizeName(value string) string {
	value = filepath.Base(strings.TrimSpace(value))
	var result strings.Builder
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("._-", character) {
			result.WriteRune(character)
		} else {
			result.WriteByte('_')
		}
		if result.Len() >= 64 {
			break
		}
	}
	if result.Len() == 0 {
		return "unknown"
	}
	return result.String()
}
