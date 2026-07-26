package codexproducer

import (
	"fmt"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/producerprotocol"
)

func TestCorrelator_SingleCandidateResolvesImmediately(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()

	if _, resolved := correlator.Deliver("session-1", "business", producerprotocol.StateIdle, HookDelivery{}, now); resolved {
		t.Fatalf("must not resolve before any candidate is known")
	}
	candidates := []RecognizedInstance{{InstanceID: "inst-1", Source: "business"}}
	resolvedSessions := correlator.Reconcile(candidates, now)
	if len(resolvedSessions) != 1 || resolvedSessions[0].InstanceID != "inst-1" {
		t.Fatalf("expected session-1 to resolve to inst-1, got %+v", resolvedSessions)
	}
	if len(resolvedSessions[0].States) != 1 || resolvedSessions[0].States[0] != producerprotocol.StateIdle {
		t.Fatalf("expected queued state replay [idle], got %v", resolvedSessions[0].States)
	}

	// Once resolved, later deliveries for the same session resolve immediately.
	id, resolved := correlator.Deliver("session-1", "business", producerprotocol.StateWorking, HookDelivery{}, now)
	if !resolved || id != "inst-1" {
		t.Fatalf("expected immediate resolution to inst-1, got id=%q resolved=%v", id, resolved)
	}
}

func TestCorrelator_QueuesFullSequenceUntilResolved(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()

	correlator.Deliver("session-1", "business", producerprotocol.StateIdle, HookDelivery{}, now)
	correlator.Deliver("session-1", "business", producerprotocol.StateWorking, HookDelivery{}, now.Add(time.Second))
	correlator.Deliver("session-1", "business", producerprotocol.StateAttention, HookDelivery{}, now.Add(2*time.Second))

	candidates := []RecognizedInstance{{InstanceID: "inst-1", Source: "business"}}
	resolvedSessions := correlator.Reconcile(candidates, now.Add(3*time.Second))
	if len(resolvedSessions) != 1 {
		t.Fatalf("expected exactly one resolved session, got %d", len(resolvedSessions))
	}
	want := []producerprotocol.State{producerprotocol.StateIdle, producerprotocol.StateWorking, producerprotocol.StateAttention}
	got := resolvedSessions[0].States
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestCorrelator_AmbiguousMultipleCandidatesStayPending(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()

	correlator.Deliver("session-1", "business", producerprotocol.StateIdle, HookDelivery{}, now)
	// No process-group/session hint at all: two same-source candidates with
	// nothing to disambiguate them must remain pending, never guessed.
	candidates := []RecognizedInstance{
		{InstanceID: "inst-1", Source: "business"},
		{InstanceID: "inst-2", Source: "business"},
	}
	resolvedSessions := correlator.Reconcile(candidates, now)
	if len(resolvedSessions) != 0 {
		t.Fatalf("expected no resolution under real ambiguity, got %+v", resolvedSessions)
	}
	if correlator.PendingCount() != 1 {
		t.Fatalf("expected session to remain pending, got pending count %d", correlator.PendingCount())
	}
}

func TestCorrelator_ProcessGroupDisambiguatesConcurrentSessions(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()

	correlator.Deliver("session-1", "business", producerprotocol.StateIdle, HookDelivery{ProcessGroupOrJob: "pgrp:100"}, now)
	candidates := []RecognizedInstance{
		{InstanceID: "inst-1", Source: "business", ProcessGroupOrJob: "pgrp:100"},
		{InstanceID: "inst-2", Source: "business", ProcessGroupOrJob: "pgrp:200"},
	}
	resolvedSessions := correlator.Reconcile(candidates, now)
	if len(resolvedSessions) != 1 || resolvedSessions[0].InstanceID != "inst-1" {
		t.Fatalf("expected process-group match to resolve to inst-1, got %+v", resolvedSessions)
	}
}

func TestCorrelator_IdenticalSessionIDAcrossSourcesNeverCollide(t *testing.T) {
	// internal/sessionbinding.Key is (Tool, Session) only, and Tool is fixed
	// to Codex for this whole package — so without scoping the pending/bound
	// key by source too, an identical underlying session_id string arriving
	// from two different CODEX_HOME sources would merge into a single
	// binding (see Correlator's doc comment on compositeSession). This test
	// is the direct regression check for
	// "samma underliggande sessionsidentifierare i två olika källor får inte
	// kollidera".
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()

	correlator.Deliver("shared-session-id", "business", producerprotocol.StateIdle, HookDelivery{}, now)
	correlator.Deliver("shared-session-id", "api", producerprotocol.StateIdle, HookDelivery{}, now)
	if correlator.PendingCount() != 2 {
		t.Fatalf("expected two independent pending sessions (one per source), got %d", correlator.PendingCount())
	}

	candidates := []RecognizedInstance{
		{InstanceID: "inst-business", Source: "business"},
		{InstanceID: "inst-api", Source: "api"},
	}
	resolvedSessions := correlator.Reconcile(candidates, now)
	if len(resolvedSessions) != 2 {
		t.Fatalf("expected both same-string sessions to resolve independently, got %+v", resolvedSessions)
	}
	bySource := map[SourceLabel]producerprotocol.InstanceID{}
	for _, resolvedSession := range resolvedSessions {
		if resolvedSession.Session != "shared-session-id" {
			t.Fatalf("expected the raw session id to be reported back unmodified, got %q", resolvedSession.Session)
		}
		bySource[resolvedSession.Source] = resolvedSession.InstanceID
	}
	if bySource["business"] != "inst-business" || bySource["api"] != "inst-api" {
		t.Fatalf("expected business/api to bind independently, got %+v", bySource)
	}
}

func TestCorrelator_ForgetInstanceReleasesBinding(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()
	correlator.Deliver("session-1", "business", producerprotocol.StateIdle, HookDelivery{}, now)
	candidates := []RecognizedInstance{{InstanceID: "inst-1", Source: "business"}}
	correlator.Reconcile(candidates, now)

	correlator.ForgetInstance("inst-1")
	// After forgetting, a fresh delivery for the same session id must be
	// treated as unresolved again (not silently still bound to a gone
	// instance), and can be re-resolved to a new instance id.
	if _, resolved := correlator.Deliver("session-1", "business", producerprotocol.StateIdle, HookDelivery{}, now); resolved {
		t.Fatalf("expected session to require re-resolution after ForgetInstance")
	}
	resolvedSessions := correlator.Reconcile([]RecognizedInstance{{InstanceID: "inst-2", Source: "business"}}, now)
	if len(resolvedSessions) != 1 || resolvedSessions[0].InstanceID != "inst-2" {
		t.Fatalf("expected re-resolution to inst-2, got %+v", resolvedSessions)
	}
}

func TestCorrelator_ExpiresStalePendingSessions(t *testing.T) {
	correlator := NewCorrelator(time.Second)
	now := time.Now().UTC()
	correlator.Deliver("session-1", "business", producerprotocol.StateIdle, HookDelivery{}, now)
	// No candidates ever appear; after the TTL, Reconcile must drop it.
	correlator.Reconcile(nil, now.Add(2*time.Second))
	if correlator.PendingCount() != 0 {
		t.Fatalf("expected stale pending session to expire, got pending count %d", correlator.PendingCount())
	}
}

func TestCorrelator_FiveParallelSessionsSameSourceResolveDistinctly(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()
	var candidates []RecognizedInstance
	for index := 0; index < 5; index++ {
		session := SessionID("session-" + string(rune('1'+index)))
		group := "pgrp:" + string(rune('1'+index))
		correlator.Deliver(session, "business", producerprotocol.StateIdle, HookDelivery{ProcessGroupOrJob: group}, now)
		candidates = append(candidates, RecognizedInstance{
			InstanceID:        producerprotocol.InstanceID("inst-" + string(rune('1'+index))),
			Source:            "business",
			ProcessGroupOrJob: instancepresence.OpaqueIdentity(group),
		})
	}
	resolvedSessions := correlator.Reconcile(candidates, now)
	if len(resolvedSessions) != 5 {
		t.Fatalf("expected all 5 parallel sessions to resolve, got %d", len(resolvedSessions))
	}
	seenInstances := map[producerprotocol.InstanceID]struct{}{}
	for _, resolvedSession := range resolvedSessions {
		if _, exists := seenInstances[resolvedSession.InstanceID]; exists {
			t.Fatalf("two sessions bound to the same instance: %+v", resolvedSessions)
		}
		seenInstances[resolvedSession.InstanceID] = struct{}{}
	}
}

// TestCorrelator_PendingSessionsAreBounded is the regression test for the
// G.4 ultrareview hardening finding: an unbounded or misbehaving hook
// stream sending many distinct session ids that never resolve (e.g. all
// under a source with no matching recognized process yet) must not grow
// Correlator's pending set without limit.
func TestCorrelator_PendingSessionsAreBounded(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()
	for index := 0; index < maxPendingSessions+50; index++ {
		session := SessionID(fmt.Sprintf("session-%d", index))
		correlator.Deliver(session, "business", producerprotocol.StateIdle, HookDelivery{}, now)
	}
	if got := correlator.PendingCount(); got != maxPendingSessions {
		t.Fatalf("PendingCount = %d, want capped at %d", got, maxPendingSessions)
	}
}

// TestCorrelator_QueuedEventsPerSessionAreBoundedAndKeepMostRecent is the
// regression test for the per-session half of the same hardening finding:
// a single session that never resolves but receives many hook events (a
// buggy hook stream) must not grow that session's queue without limit, and
// the retained window must be the most recent events (so an eventual
// resolve still reflects current reality) rather than an arbitrary stale
// prefix.
func TestCorrelator_QueuedEventsPerSessionAreBoundedAndKeepMostRecent(t *testing.T) {
	correlator := NewCorrelator(time.Minute)
	now := time.Now().UTC()
	totalEvents := maxQueuedEventsPerSession + 10
	for index := 0; index < totalEvents; index++ {
		mapped := producerprotocol.StateWorking
		if index == totalEvents-1 {
			mapped = producerprotocol.StateAttention // the most recent event.
		}
		correlator.Deliver("session-1", "business", mapped, HookDelivery{}, now.Add(time.Duration(index)*time.Millisecond))
	}
	candidates := []RecognizedInstance{{InstanceID: "inst-1", Source: "business"}}
	resolvedSessions := correlator.Reconcile(candidates, now)
	if len(resolvedSessions) != 1 {
		t.Fatalf("expected the session to resolve, got %+v", resolvedSessions)
	}
	states := resolvedSessions[0].States
	if len(states) != maxQueuedEventsPerSession {
		t.Fatalf("queued states length = %d, want capped at %d", len(states), maxQueuedEventsPerSession)
	}
	if states[len(states)-1] != producerprotocol.StateAttention {
		t.Fatalf("expected the retained window to keep the most recent event (attention), got %v", states[len(states)-1])
	}
}
