package grokpresence

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
)

var (
	ErrProcessGenerationChanged = errors.New("grok process generation changed")
	ErrAmbiguousEventStream     = errors.New("multiple grok event streams")
	ErrEventFDChanged           = errors.New("grok event file descriptor changed")
)

// GenerationCapturer verifies one process generation using the Linux adapter's
// double-read /proc rules.
type GenerationCapturer interface {
	CaptureGeneration(context.Context, uint64) linuxprocess.GenerationCapture
}

// EventReader reads structural Grok state through a verified process's open
// events.jsonl descriptor. It never opens Grok history by session path.
type EventReader struct {
	ProcRoot string
	Capturer GenerationCapturer
}

// Observations returns every recognized structural state from the bounded
// event tail in file order. An empty result is valid for a pre-prompt process.
func (reader EventReader) Observations(
	ctx context.Context,
	process instancepresence.ProcessIdentity,
) ([]EventObservation, error) {
	if err := process.Validate(); err != nil {
		return nil, fmt.Errorf(
			"grok event process identity: %w",
			err,
		)
	}
	if strings.TrimSpace(reader.ProcRoot) == "" {
		return nil, errors.New(
			"grok event proc root must not be empty",
		)
	}
	if reader.Capturer == nil {
		return nil, errors.New(
			"grok event generation capturer must not be nil",
		)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if !captureMatches(reader.Capturer.CaptureGeneration(ctx, process.PID), process) {
		return nil, ErrProcessGenerationChanged
	}

	fdDirectory := filepath.Join(
		reader.ProcRoot,
		strconv.FormatUint(process.PID, 10),
		"fd",
	)
	entries, err := os.ReadDir(fdDirectory)
	if err != nil {
		return nil, fmt.Errorf(
			"list grok process descriptors: %w",
			err,
		)
	}

	// Deduplicate duplicated descriptors for the same underlying event path.
	candidates := make(map[string]string)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, err := strconv.ParseUint(entry.Name(), 10, 64); err != nil {
			continue
		}

		fdPath := filepath.Join(fdDirectory, entry.Name())
		target, err := os.Readlink(fdPath)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf(
				"inspect grok process descriptor: %w",
				err,
			)
		}

		normalized := normalizeEventTarget(target)
		if filepath.Base(normalized) != "events.jsonl" {
			continue
		}
		candidates[normalized] = fdPath
	}

	if len(candidates) > 1 {
		return nil, ErrAmbiguousEventStream
	}
	if len(candidates) == 0 {
		if !captureMatches(reader.Capturer.CaptureGeneration(ctx, process.PID), process) {
			return nil, ErrProcessGenerationChanged
		}
		return nil, nil
	}

	var fdPath, expectedTarget string
	for target, path := range candidates {
		expectedTarget = target
		fdPath = path
	}

	file, err := os.Open(fdPath)
	if errors.Is(err, os.ErrNotExist) {
		if !captureMatches(reader.Capturer.CaptureGeneration(ctx, process.PID), process) {
			return nil, ErrProcessGenerationChanged
		}
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf(
			"open grok event descriptor: %w",
			err,
		)
	}
	defer file.Close()

	openedTarget, err := os.Readlink(
		fmt.Sprintf("/proc/self/fd/%d", file.Fd()),
	)
	if err != nil {
		return nil, fmt.Errorf(
			"verify grok event descriptor: %w",
			err,
		)
	}
	if !sameEventTarget(expectedTarget, openedTarget) {
		return nil, ErrEventFDChanged
	}

	data, err := readStructuralTail(file)
	if err != nil {
		return nil, err
	}

	if !captureMatches(reader.Capturer.CaptureGeneration(ctx, process.PID), process) {
		return nil, ErrProcessGenerationChanged
	}

	return StructuralStates(data), nil
}

// Latest returns the final recognized structural state. found=false is valid
// for a pre-prompt Grok process or an event stream without a known safe state.
func (reader EventReader) Latest(
	ctx context.Context,
	process instancepresence.ProcessIdentity,
) (EventObservation, bool, error) {
	observations, err := reader.Observations(ctx, process)
	if err != nil {
		return EventObservation{}, false, err
	}
	if len(observations) == 0 {
		return EventObservation{}, false, nil
	}
	return observations[len(observations)-1], true, nil
}

func captureMatches(
	capture linuxprocess.GenerationCapture,
	expected instancepresence.ProcessIdentity,
) bool {
	return capture.OK &&
		capture.Identity.PID == expected.PID &&
		capture.Identity.StartedAt.Equal(expected.StartedAt)
}

func normalizeEventTarget(target string) string {
	return strings.TrimSuffix(target, " (deleted)")
}

func sameEventTarget(expected, opened string) bool {
	return normalizeEventTarget(expected) == normalizeEventTarget(opened)
}

func readStructuralTail(file *os.File) ([]byte, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat grok event descriptor: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("grok event descriptor is not a regular file")
	}

	start := int64(0)
	if info.Size() > MaxStructuralTailBytes {
		start = info.Size() - MaxStructuralTailBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return nil, fmt.Errorf("seek grok event descriptor: %w", err)
	}

	data, err := io.ReadAll(
		io.LimitReader(file, MaxStructuralTailBytes+1),
	)
	if err != nil {
		return nil, fmt.Errorf("read grok event descriptor: %w", err)
	}
	if len(data) > MaxStructuralTailBytes {
		data = data[len(data)-MaxStructuralTailBytes:]
	}
	return data, nil
}
