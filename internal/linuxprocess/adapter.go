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
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

const (
	defaultClockTicks  = 100
	statReadLimit      = 16 * 1024
	commReadLimit      = 256
	cmdlineReadLimit   = 1024
	statusReadLimit    = 16 * 1024
	environReadLimit   = 64 * 1024
	bootIDReadLimit    = 256
	maxRecognitionArgv = 16
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
		record, outcome := readProcess(reader, pid, bootTime, adapter.config.ClockTicks, adapter.config.LaunchIdentityRules)
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

	byPID := make(map[uint64][]*rawProcess, len(records))
	for index := range records {
		pid := records[index].identity.PID
		byPID[pid] = append(byPID[pid], &records[index])
	}
	observations := make([]instancepresence.ProcessObservation, 0, len(records))
	recognitionProcesses := make([]runtimerecognition.ProcessObservation, 0, len(records))
	for index := range records {
		record := &records[index]
		parent := verifiedParent(reader, record, byPID, bootTime, adapter.config.ClockTicks)
		publicExecutable := record.commBase
		if record.argvBase != "" {
			publicExecutable = record.argvBase
		}
		observation := instancepresence.ProcessObservation{
			Process: record.identity, Parent: parent,
			ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + sanitizeName(publicExecutable)),
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
		recognitionProcesses = append(recognitionProcesses, runtimerecognition.ProcessObservation{
			Process: record.identity, Parent: parent, ParentPIDHint: record.stat.ParentPID,
			CommIdentity:       instancepresence.OpaqueIdentity("exe:" + sanitizeName(record.commBase)),
			ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + sanitizeName(record.argvBase)),
			LaunchIdentities:   append([]instancepresence.OpaqueIdentity{}, record.launchIdentities...),
			ProcessGroupOrJob:  observation.ProcessGroupOrJob, OSSession: observation.OSSession,
			TerminalFingerprint: observation.TerminalFingerprint, OwnerIdentity: observation.OwnerIdentity,
			// Suspended stays recognition-local; public ProcessObservation omits it.
			Suspended:        processStateStopped(record.stat.State),
			WorkingDirectory: record.workingDirectory,
			EnvCodexHome:     record.envCodexHome,
			Argv:             append([]string{}, record.argv...),
		})
	}
	snapshot := instancepresence.ProcessSnapshot{ObservedAt: observedAt, Processes: observations}
	if err := snapshot.Validate(); err != nil {
		return Sample{}, fmt.Errorf("linux process snapshot invariant: %w", err)
	}

	recognition := runtimerecognition.Snapshot{ObservedAt: observedAt, BootID: bootID, Processes: recognitionProcesses}
	if err := recognition.Validate(); err != nil {
		return Sample{}, fmt.Errorf("linux recognition snapshot invariant: %w", err)
	}
	return Sample{Snapshot: snapshot, Recognition: recognition, Diagnostics: diagnosticsFromCounts(counts), uncertainPIDs: uncertainPIDs}, nil
}

// verifiedParent establishes an exact parent generation only after observing
// both records again. A PPID by itself is deliberately retained only as a
// conservative hint because a reused PID cannot identify a process generation.
func verifiedParent(reader procReader, child *rawProcess, byPID map[uint64][]*rawProcess, bootTime time.Time, clockTicks uint64) *instancepresence.ProcessIdentity {
	candidates := byPID[child.stat.ParentPID]
	if len(candidates) != 1 || child.stat.ParentPID == 0 {
		return nil
	}
	parent := candidates[0]
	parentData, err := reader.ReadFile(path.Join(strconv.FormatUint(parent.identity.PID, 10), "stat"), statReadLimit)
	if err != nil {
		return nil
	}
	currentParent, err := parseProcStat(parentData)
	if err != nil || currentParent.PID != parent.identity.PID || !startedAt(bootTime, currentParent.StartTicks, clockTicks).Equal(parent.identity.StartedAt) {
		return nil
	}
	childData, err := reader.ReadFile(path.Join(strconv.FormatUint(child.identity.PID, 10), "stat"), statReadLimit)
	if err != nil {
		return nil
	}
	currentChild, err := parseProcStat(childData)
	if err != nil || currentChild.PID != child.identity.PID || currentChild.ParentPID != parent.identity.PID || !startedAt(bootTime, currentChild.StartTicks, clockTicks).Equal(child.identity.StartedAt) {
		return nil
	}
	identity := parent.identity
	return &identity
}

// RuntimeSnapshot returns a snapshot and its boot identity from one bounded
// Linux observation. It exposes no agent classification.
func (adapter *Adapter) RuntimeSnapshot(ctx context.Context) (runtimerecognition.Snapshot, error) {
	sample, err := adapter.Observe(ctx)
	if err != nil {
		return runtimerecognition.Snapshot{}, err
	}
	return sample.Recognition, nil
}

type rawProcess struct {
	stat             procStat
	identity         instancepresence.ProcessIdentity
	commBase         string
	argvBase         string
	launchIdentities []instancepresence.OpaqueIdentity
	ownerIdentity    string
	workingDirectory string
	envCodexHome     string
	argv             []string
}

type readOutcome struct {
	accepted  bool
	uncertain bool
	counts    map[ReasonCode]uint64
}

func readProcess(reader procReader, pid uint64, bootTime time.Time, clockTicks uint64, rules []runtimerecognition.LaunchIdentityRule) (rawProcess, readOutcome) {
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
	launchIdentities := launchIdentities(argvData, rules)
	argv := parseCmdlineArgv(argvData, maxRecognitionArgv)
	cwd := optionalReadLink(reader, path.Join(directory, "cwd"), outcome.counts)
	codexHome := codexHomeFromEnviron(optionalBytes(reader, path.Join(directory, "environ"), environReadLimit, outcome.counts))
	outcome.accepted = true
	return rawProcess{
		stat: final,
		identity: instancepresence.ProcessIdentity{
			PID: pid, StartedAt: startedAt(bootTime, final.StartTicks, clockTicks),
		},
		commBase: comm, argvBase: safeExecutableName(launchExecutable(argvData)), launchIdentities: launchIdentities, ownerIdentity: owner,
		workingDirectory: cwd, envCodexHome: codexHome, argv: argv,
	}, outcome
}

func optionalReadLink(reader procReader, name string, counts map[ReasonCode]uint64) string {
	target, err := reader.ReadLink(name)
	if err != nil {
		counts[classifyReadError(err)]++
		return ""
	}
	target = strings.TrimSpace(target)
	if target == "" || !filepath.IsAbs(target) {
		return ""
	}
	return filepath.Clean(target)
}

// parseCmdlineArgv retains only structural classification tokens for Codex
// interactive detection: a synthetic "codex" marker (never absolute paths) plus
// at most one allowlisted command token. Safe help/version flags become
// "help"/"version". Flag values, launcher script paths, and free-form prompts
// are discarded.
func parseCmdlineArgv(data []byte, max int) []string {
	if len(data) == 0 || max <= 0 {
		return nil
	}
	parts := strings.Split(string(data), "\x00")
	out := make([]string, 0, 2)
	sawCodex := false
	for index := 0; index < len(parts); index++ {
		part := parts[index]
		if part == "" {
			continue
		}
		if part == "--" {
			break
		}
		if mapped := structuralFlagToken(part); mapped != "" {
			if !sawCodex {
				out = append(out, "codex")
				sawCodex = true
			}
			out = append(out, mapped)
			break
		}
		if strings.HasPrefix(part, "-") {
			if strings.Contains(part, "=") {
				continue
			}
			if flagTakesValue(part) && index+1 < len(parts) && !strings.HasPrefix(parts[index+1], "-") {
				index++ // skip value without retaining it
			}
			continue
		}
		// node / npx / package entrypoint: never store absolute paths.
		if isCodexLauncherToken(part) {
			continue
		}
		if isCodexEntrypointToken(part) {
			if !sawCodex {
				out = append(out, "codex")
				sawCodex = true
			}
			continue
		}
		if token := structuralBareToken(part); token != "" {
			if !sawCodex {
				// Bare allowlisted command without an explicit codex marker is
				// not classified (avoid false positives on unrelated tools).
				if index == 0 {
					break
				}
				out = append(out, "codex")
				sawCodex = true
			}
			out = append(out, token)
			break
		}
		// Free-form prompt or unrelated path: stop without storing it.
		break
	}
	if len(out) > max {
		return out[:max]
	}
	return out
}

func isCodexLauncherToken(part string) bool {
	base := strings.ToLower(filepath.Base(part))
	switch base {
	case "node", "nodejs", "npx":
		return true
	default:
		return false
	}
}

func isCodexEntrypointToken(part string) bool {
	base := strings.ToLower(filepath.Base(part))
	switch base {
	case "codex", "aurora-codex", "codex.js":
		return true
	default:
		// package path .../@openai/codex/bin/codex.js already handled by Base.
		return strings.HasPrefix(base, "codex.") && strings.HasSuffix(base, ".js")
	}
}

// structuralFlagToken maps safe help/version flags to allowlisted tokens.
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

// structuralBareToken returns an allowlisted Codex top-level command name.
// Free-form prompt text and arbitrary words are rejected.
func structuralBareToken(value string) string {
	if value == "" || len(value) > 24 || strings.ContainsAny(value, "/\\=:.") {
		return ""
	}
	lower := strings.ToLower(value)
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
		"--local-provider":
		return true
	default:
		return false
	}
}

func codexHomeFromEnviron(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	for _, entry := range strings.Split(string(data), "\x00") {
		if entry == "" {
			continue
		}
		const prefix = "CODEX_HOME="
		if strings.HasPrefix(entry, prefix) {
			value := strings.TrimSpace(strings.TrimPrefix(entry, prefix))
			if value != "" && filepath.IsAbs(value) {
				return filepath.Clean(value)
			}
			return ""
		}
	}
	return ""
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

// launchIdentities immediately reduces a bounded local argv prefix to the
// configured opaque identities. Unknown argument values are discarded and no
// agent-specific rule is embedded in the Linux backend.
func launchIdentities(data []byte, rules []runtimerecognition.LaunchIdentityRule) []instancepresence.OpaqueIdentity {
	const maxArguments = 2
	parts := strings.Split(string(data), "\x00")
	identities := make([]instancepresence.OpaqueIdentity, 0, len(rules))
	seen := make(map[instancepresence.OpaqueIdentity]struct{}, len(rules))
	for index := 0; index < len(parts) && index < maxArguments; index++ {
		for _, rule := range rules {
			if rule.Matches(parts, index) {
				if _, exists := seen[rule.Identity]; !exists {
					identities = append(identities, rule.Identity)
					seen[rule.Identity] = struct{}{}
				}
			}
		}
	}
	return identities
}

func launchExecutable(data []byte) string {
	parts := strings.Split(string(data), "\x00")
	if len(parts) == 0 || parts[0] == "" {
		return ""
	}
	base := filepath.Base(parts[0])
	if base == "." || base == string(filepath.Separator) {
		return ""
	}
	return base
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
	value = strings.TrimSpace(value)
	if value == "" {
		return "unknown"
	}
	value = filepath.Base(value)
	if value == "." || value == string(filepath.Separator) {
		return "unknown"
	}
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

// safeExecutableName retains only a sanitized executable basename. argv[0] is
// process-controlled, so empty and option-like values become unknown rather
// than entering a serializable observation.
func safeExecutableName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "-") || strings.Contains(value, "=") {
		return "unknown"
	}
	return sanitizeName(value)
}
