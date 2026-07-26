package grokpresence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
)

type fakeGenerationCapturer struct {
	capture linuxprocess.GenerationCapture
}

func (capturer fakeGenerationCapturer) CaptureGeneration(
	context.Context,
	uint64,
) linuxprocess.GenerationCapture {
	return capturer.capture
}

func TestEventReaderLatestCompletedIsIdle(t *testing.T) {
	process := testGrokProcess()
	root := t.TempDir()

	writeEventFD(t, root, process.PID, "29", []byte(
		"{\"type\":\"phase_changed\",\"phase\":\"streaming_text\","+
			"\"ts\":\"2026-07-25T14:40:22Z\"}\n"+
			"{\"type\":\"turn_ended\",\"outcome\":\"completed\","+
			"\"ts\":\"2026-07-25T14:40:23Z\"}\n",
	))

	reader := testEventReader(root, process)
	observation, found, err := reader.Latest(context.Background(), process)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if !found || observation.State != instancepresence.StateIdle {
		t.Fatalf("observation = %#v found=%t", observation, found)
	}
}

func TestEventReaderObservationsPreserveTransitions(t *testing.T) {
	process := testGrokProcess()
	root := t.TempDir()

	writeEventFD(t, root, process.PID, "29", []byte(
		"{\"type\":\"phase_changed\",\"phase\":\"streaming_text\","+
			"\"ts\":\"2026-07-25T14:40:22Z\"}\n"+
			"{\"type\":\"phase_changed\",\"phase\":\"permission_prompt\","+
			"\"ts\":\"2026-07-25T14:40:23Z\"}\n"+
			"{\"type\":\"turn_ended\",\"outcome\":\"completed\","+
			"\"ts\":\"2026-07-25T14:40:24Z\"}\n",
	))

	reader := testEventReader(root, process)
	observations, err := reader.Observations(context.Background(), process)
	if err != nil {
		t.Fatalf("Observations() error = %v", err)
	}

	want := []instancepresence.EffectiveState{
		instancepresence.StateWorking,
		instancepresence.StateAttention,
		instancepresence.StateIdle,
	}
	if len(observations) != len(want) {
		t.Fatalf("len(Observations()) = %d want %d", len(observations), len(want))
	}
	for index := range want {
		if observations[index].State != want[index] {
			t.Fatalf(
				"observation[%d].State = %q want %q",
				index,
				observations[index].State,
				want[index],
			)
		}
	}
}

func TestSameEventTargetRejectsAnotherSession(t *testing.T) {
	expected := "/home/carl/.grok/sessions/project/session-a/events.jsonl"

	if !sameEventTarget(expected, expected+" (deleted)") {
		t.Fatal("deleted suffix should retain the same event target")
	}
	if sameEventTarget(
		expected,
		"/home/carl/.grok/sessions/project/session-b/events.jsonl",
	) {
		t.Fatal("another session must not match the selected event target")
	}
}

func TestEventReaderPrePromptHasNoState(t *testing.T) {
	process := testGrokProcess()
	root := t.TempDir()
	makeFDDirectory(t, root, process.PID)

	reader := testEventReader(root, process)
	observation, found, err := reader.Latest(context.Background(), process)
	if err != nil {
		t.Fatalf("Latest() error = %v", err)
	}
	if found || observation != (EventObservation{}) {
		t.Fatalf("observation = %#v found=%t", observation, found)
	}
}

func TestEventReaderRejectsGenerationChange(t *testing.T) {
	process := testGrokProcess()
	root := t.TempDir()
	makeFDDirectory(t, root, process.PID)

	replacement := process
	replacement.StartedAt = replacement.StartedAt.Add(time.Second)

	reader := EventReader{
		ProcRoot: root,
		Capturer: fakeGenerationCapturer{
			capture: linuxprocess.GenerationCapture{
				Identity: replacement,
				OK:       true,
			},
		},
	}

	_, _, err := reader.Latest(context.Background(), process)
	if !errors.Is(err, ErrProcessGenerationChanged) {
		t.Fatalf("Latest() error = %v", err)
	}
}

func TestEventReaderRejectsMultipleEventStreams(t *testing.T) {
	process := testGrokProcess()
	root := t.TempDir()

	writeEventFD(t, root, process.PID, "29", []byte(
		"{\"type\":\"turn_ended\",\"outcome\":\"completed\","+
			"\"ts\":\"2026-07-25T14:40:23Z\"}\n",
	))
	writeNamedEventFD(t, root, process.PID, "30", "second-session")

	reader := testEventReader(root, process)
	_, _, err := reader.Latest(context.Background(), process)
	if !errors.Is(err, ErrAmbiguousEventStream) {
		t.Fatalf("Latest() error = %v", err)
	}
}

func testGrokProcess() instancepresence.ProcessIdentity {
	return instancepresence.ProcessIdentity{
		PID:       4242,
		StartedAt: time.Date(2026, 7, 25, 14, 0, 0, 0, time.UTC),
	}
}

func testEventReader(
	root string,
	process instancepresence.ProcessIdentity,
) EventReader {
	return EventReader{
		ProcRoot: root,
		Capturer: fakeGenerationCapturer{
			capture: linuxprocess.GenerationCapture{
				Identity: process,
				OK:       true,
			},
		},
	}
}

func makeFDDirectory(t *testing.T, root string, pid uint64) string {
	t.Helper()
	directory := filepath.Join(root, strconv.FormatUint(pid, 10), "fd")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return directory
}

func writeEventFD(
	t *testing.T,
	root string,
	pid uint64,
	fd string,
	data []byte,
) {
	t.Helper()
	directory := filepath.Join(root, "session-"+fd)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(directory, "events.jsonl")
	if err := os.WriteFile(eventPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	fdDirectory := makeFDDirectory(t, root, pid)
	if err := os.Symlink(eventPath, filepath.Join(fdDirectory, fd)); err != nil {
		t.Fatal(err)
	}
}

func writeNamedEventFD(
	t *testing.T,
	root string,
	pid uint64,
	fd string,
	session string,
) {
	t.Helper()
	directory := filepath.Join(root, session)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	eventPath := filepath.Join(directory, "events.jsonl")
	if err := os.WriteFile(eventPath, []byte(
		"{\"type\":\"phase_changed\",\"phase\":\"permission_prompt\","+
			"\"ts\":\"2026-07-25T14:40:24Z\"}\n",
	), 0o600); err != nil {
		t.Fatal(err)
	}
	fdDirectory := makeFDDirectory(t, root, pid)
	if err := os.Symlink(eventPath, filepath.Join(fdDirectory, fd)); err != nil {
		t.Fatal(err)
	}
}
