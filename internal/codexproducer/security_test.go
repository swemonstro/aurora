package codexproducer

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

// These are canary tests for the G.4 secrecy requirements: CODEX_HOME paths,
// cwd, argv, prompts, hook payload, session ids, and auth content must never
// appear in anything this package logs, returns as an error string, or puts
// on the producerprotocol wire (which carries only instance_id,
// producer_epoch, state, revision, and timestamps — see protocol.go).

const canarySessionID = "session-should-never-leak-0f3a9c"

// canaryCodexHomePath is a distinctive, content-suggestive substring used
// only to check that path-shaped values never leak into unrelated output
// (instance ids, wire messages, error classifications) — it is never passed
// to NewSourceSet, so it never needs to exist on disk.
const canaryCodexHomePath = "/home/carl/.codex-super-secret-business-path"

func TestSecurity_DuplicateSourcePathErrorNeverLeaksPath(t *testing.T) {
	// NewSourceSet requires every configured path to actually exist (see
	// sources.go), so the canary must be a real directory, not just a
	// content-suggestive nonexistent string — otherwise the "does not
	// exist" error (also content-free, but a different code path) would
	// fire first and this test would stop exercising duplicate detection.
	canaryPath := filepath.Join(t.TempDir(), "super-secret-business-path")
	if err := os.MkdirAll(canaryPath, 0o700); err != nil {
		t.Fatal(err)
	}
	_, err := NewSourceSet([]SourceEntry{
		{Label: "business", Path: canaryPath},
		{Label: "api", Path: canaryPath},
	}, "")
	if err == nil {
		t.Fatal("expected an error for a shared source path")
	}
	if strings.Contains(err.Error(), canaryPath) {
		t.Fatalf("duplicate-path error must never echo the CODEX_HOME path: %v", err)
	}
}

func TestSecurity_InstanceIDNeverEmbedsSourcePathOrPID(t *testing.T) {
	id := DeriveInstanceID(SourceLabel("business"), 987654, time.Now().UTC())
	if strings.Contains(string(id), canaryCodexHomePath) {
		t.Fatalf("instance id must never embed a CODEX_HOME path: %q", id)
	}
	if strings.Contains(string(id), "987654") {
		t.Fatalf("instance id must never embed the raw PID: %q", id)
	}
}

func TestSecurity_WireMessageNeverContainsSourceOrSessionContent(t *testing.T) {
	epoch, err := NewProducerEpoch()
	if err != nil {
		t.Fatal(err)
	}
	id := DeriveInstanceID(SourceLabel("business"), 42, time.Now().UTC())
	now := time.Now().UTC()
	msg := producerprotocol.Message{
		ProtocolVersion: producerprotocol.CurrentProtocolVersion,
		Tool:            producerprotocol.ToolCodex,
		InstanceID:      id,
		ProducerEpoch:   epoch,
		State:           producerprotocol.StateAttention,
		Revision:        1,
		ObservedAt:      now,
		LeaseExpiresAt:  now.Add(time.Minute),
	}
	encoded, err := producerprotocol.EncodeMessageJSON(msg, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte(canaryCodexHomePath)) {
		t.Fatalf("wire message must never contain a CODEX_HOME path: %s", encoded)
	}
	if bytes.Contains(encoded, []byte(canarySessionID)) {
		t.Fatalf("wire message must never contain a session id: %s", encoded)
	}
}

func TestSecurity_ObserveErrorClassificationNeverEchoesUnderlyingError(t *testing.T) {
	// classifyObserveError must return a fixed, content-free string
	// regardless of what the underlying error says, since a real /proc
	// error could otherwise embed a path or other detail.
	sensitive := errorContaining(canaryCodexHomePath)
	got := classifyObserveError(sensitive)
	if strings.Contains(got, canaryCodexHomePath) {
		t.Fatalf("observe error classification leaked sensitive content: %q", got)
	}
	if got != "process observation failed" {
		t.Fatalf("expected a fixed classification string, got %q", got)
	}
}

type sensitiveError struct{ message string }

func (err sensitiveError) Error() string { return err.message }

func errorContaining(message string) error { return sensitiveError{message: message} }

func TestSecurity_HookIngressWireEnvelopeHasNoUnexpectedFields(t *testing.T) {
	// hookadapter.IngressObservation is the exact wire envelope this
	// package's ingress decodes (see ingress_linux.go). It must carry only
	// Tool, HookSessionRef, Lifecycle, and EffectiveState — never
	// CODEX_HOME, cwd, argv, prompt, or transcript path. This canary uses
	// reflection so that adding a field to that struct fails this test
	// instead of silently starting to flow through this producer's ingress
	// unreviewed.
	fields := reflect.VisibleFields(reflect.TypeOf(hookadapter.IngressObservation{}))
	got := make([]string, 0, len(fields))
	for _, field := range fields {
		got = append(got, field.Name)
	}
	sort.Strings(got)
	want := []string{"EffectiveState", "HookSessionRef", "Lifecycle", "Tool"}
	if len(got) != len(want) {
		t.Fatalf("hookadapter.IngressObservation fields changed: got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("hookadapter.IngressObservation fields changed: got %v, want %v", got, want)
		}
	}
}
