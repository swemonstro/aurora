package codexproducer

import (
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/producerprotocol"
)

type systemClockForTest struct{}

func (systemClockForTest) Now() time.Time { return time.Now().UTC() }

func TestDeriveInstanceID_Deterministic(t *testing.T) {
	startedAt := time.Now().UTC()
	first := DeriveInstanceID("business", 1234, startedAt)
	second := DeriveInstanceID("business", 1234, startedAt)
	if first != second {
		t.Fatalf("DeriveInstanceID must be deterministic: %q != %q", first, second)
	}
}

func TestDeriveInstanceID_DiffersByPID(t *testing.T) {
	startedAt := time.Now().UTC()
	if DeriveInstanceID("business", 1234, startedAt) == DeriveInstanceID("business", 5678, startedAt) {
		t.Fatalf("different PIDs must produce different instance ids")
	}
}

func TestDeriveInstanceID_DiffersByStartedAt(t *testing.T) {
	now := time.Now().UTC()
	if DeriveInstanceID("business", 1234, now) == DeriveInstanceID("business", 1234, now.Add(time.Second)) {
		t.Fatalf("PID reuse with a different start time must produce a different instance id")
	}
}

func TestDeriveInstanceID_DiffersBySource(t *testing.T) {
	startedAt := time.Now().UTC()
	if DeriveInstanceID("business", 1234, startedAt) == DeriveInstanceID("api", 1234, startedAt) {
		t.Fatalf("different sources must produce different instance ids")
	}
}

func TestDeriveInstanceID_IsOpaqueAndValid(t *testing.T) {
	id := DeriveInstanceID("business", 1234, time.Now().UTC())
	msg := producerprotocol.Message{
		ProtocolVersion: producerprotocol.CurrentProtocolVersion,
		Tool:            producerprotocol.ToolCodex,
		InstanceID:      id,
		ProducerEpoch:   "epoch-x",
		State:           producerprotocol.StateIdle,
		Revision:        1,
		ObservedAt:      time.Now().UTC(),
		LeaseExpiresAt:  time.Now().UTC().Add(time.Minute),
	}
	if err := producerprotocol.ValidateMessage(producerprotocol.DefaultConfig(systemClockForTest{}), producerprotocol.CanonicalMessage(msg)); err != nil {
		t.Fatalf("derived instance id must produce a valid wire message: %v", err)
	}
	// Canary: the derived id must never embed the raw PID or CODEX_HOME path
	// text, since instance ids may appear in logs.
	if strings.Contains(string(id), "1234") {
		t.Fatalf("instance id must not contain the raw PID: %q", id)
	}
	if strings.Contains(string(id), "home") || strings.Contains(string(id), "codex-business") {
		t.Fatalf("instance id must not leak CODEX_HOME path fragments: %q", id)
	}
}

func TestNewProducerEpoch_UniqueAndOpaque(t *testing.T) {
	first, err := NewProducerEpoch()
	if err != nil {
		t.Fatalf("NewProducerEpoch: %v", err)
	}
	second, err := NewProducerEpoch()
	if err != nil {
		t.Fatalf("NewProducerEpoch: %v", err)
	}
	if first == second {
		t.Fatalf("two producer epochs must not collide: %q", first)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("producer epoch must satisfy producerprotocol.ProducerEpoch.Validate: %v", err)
	}
	for _, forbidden := range []string{"/", "@", string([]rune{0})} {
		if strings.Contains(string(first), forbidden) {
			t.Fatalf("producer epoch must not contain %q: %q", forbidden, first)
		}
	}
}
