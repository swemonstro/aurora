package runtimepresence

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
	"github.com/swemonstro/aurora/internal/status"
)

type action struct {
	kind   string
	source string
	state  status.State
}

type recordingPublisher struct {
	actions []action
	err     error
}

func (p *recordingPublisher) Publish(_ context.Context, snapshot presence.Snapshot) error {
	if p.err != nil {
		return p.err
	}
	p.actions = append(p.actions, action{kind: "publish", source: snapshot.Source, state: snapshot.State})
	return nil
}

func (p *recordingPublisher) Remove(_ context.Context, source string) error {
	if p.err != nil {
		return p.err
	}
	p.actions = append(p.actions, action{kind: "remove", source: source})
	return nil
}

func TestCountSecureFamiliesIgnoresUncertain(t *testing.T) {
	result := runtimerecognition.Result{
		Families: []runtimerecognition.Family{
			{Candidate: instancepresence.RuntimeCandidate{Tool: instancepresence.ToolClaude}},
			{Candidate: instancepresence.RuntimeCandidate{Tool: instancepresence.ToolClaude}},
			{Candidate: instancepresence.RuntimeCandidate{Tool: instancepresence.ToolCodex}},
		},
		UncertainFamilies: []runtimerecognition.UncertainFamily{
			{Tool: instancepresence.ToolClaude},
			{Tool: instancepresence.ToolCodex},
		},
	}
	counts := CountSecureFamilies(result)
	if counts.Claude != 2 || counts.Codex != 1 {
		t.Fatalf("counts = %#v, want claude=2 codex=1", counts)
	}
}

func TestApplyClaudeTransitions0To1To2To1To0(t *testing.T) {
	publisher := &recordingPublisher{}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	agent, err := New(publisher, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// 0 → 1: publish idle
	if err := agent.Apply(ctx, FamilyCounts{Claude: 1}); err != nil {
		t.Fatal(err)
	}
	// 1 → 2: no change
	if err := agent.Apply(ctx, FamilyCounts{Claude: 2}); err != nil {
		t.Fatal(err)
	}
	// 2 → 1: no change (one family remains)
	if err := agent.Apply(ctx, FamilyCounts{Claude: 1}); err != nil {
		t.Fatal(err)
	}
	// 1 → 0: delete
	if err := agent.Apply(ctx, FamilyCounts{Claude: 0}); err != nil {
		t.Fatal(err)
	}

	want := []action{
		{kind: "publish", source: SourceClaudeRuntime, state: status.Idle},
		{kind: "remove", source: SourceClaudeRuntime},
	}
	if len(publisher.actions) != len(want) {
		t.Fatalf("actions = %#v, want %#v", publisher.actions, want)
	}
	for index, got := range publisher.actions {
		if got != want[index] {
			t.Fatalf("action[%d] = %#v, want %#v", index, got, want[index])
		}
	}
	if agent.ClaudePublished() {
		t.Fatal("claude still marked published after 1→0")
	}
}

func TestApplyCodexTransitions0To1To2To1To0(t *testing.T) {
	publisher := &recordingPublisher{}
	agent, err := New(publisher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	if err := agent.Apply(ctx, FamilyCounts{Codex: 1}); err != nil {
		t.Fatal(err)
	}
	if err := agent.Apply(ctx, FamilyCounts{Codex: 2}); err != nil {
		t.Fatal(err)
	}
	if err := agent.Apply(ctx, FamilyCounts{Codex: 1}); err != nil {
		t.Fatal(err)
	}
	if err := agent.Apply(ctx, FamilyCounts{Codex: 0}); err != nil {
		t.Fatal(err)
	}

	want := []action{
		{kind: "publish", source: SourceCodexRuntime, state: status.Idle},
		{kind: "remove", source: SourceCodexRuntime},
	}
	if len(publisher.actions) != len(want) {
		t.Fatalf("actions = %#v, want %#v", publisher.actions, want)
	}
	for index, got := range publisher.actions {
		if got != want[index] {
			t.Fatalf("action[%d] = %#v, want %#v", index, got, want[index])
		}
	}
}

func TestApplyClaudeAndCodexInParallel(t *testing.T) {
	publisher := &recordingPublisher{}
	agent, err := New(publisher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Both appear together.
	if err := agent.Apply(ctx, FamilyCounts{Claude: 1, Codex: 1}); err != nil {
		t.Fatal(err)
	}
	// Parallel family counts grow independently without re-publish.
	if err := agent.Apply(ctx, FamilyCounts{Claude: 2, Codex: 3}); err != nil {
		t.Fatal(err)
	}
	// Claude drops to zero; Codex remains.
	if err := agent.Apply(ctx, FamilyCounts{Claude: 0, Codex: 2}); err != nil {
		t.Fatal(err)
	}
	// Codex drops to zero.
	if err := agent.Apply(ctx, FamilyCounts{Claude: 0, Codex: 0}); err != nil {
		t.Fatal(err)
	}

	want := []action{
		{kind: "publish", source: SourceClaudeRuntime, state: status.Idle},
		{kind: "publish", source: SourceCodexRuntime, state: status.Idle},
		{kind: "remove", source: SourceClaudeRuntime},
		{kind: "remove", source: SourceCodexRuntime},
	}
	if len(publisher.actions) != len(want) {
		t.Fatalf("actions = %#v, want %#v", publisher.actions, want)
	}
	for index, got := range publisher.actions {
		if got != want[index] {
			t.Fatalf("action[%d] = %#v, want %#v", index, got, want[index])
		}
	}
	if agent.ClaudePublished() || agent.CodexPublished() {
		t.Fatalf("published flags = claude=%v codex=%v", agent.ClaudePublished(), agent.CodexPublished())
	}
}

func TestApplyRecognitionUsesOnlySecureFamilies(t *testing.T) {
	publisher := &recordingPublisher{}
	agent, err := New(publisher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	// Only uncertain Claude/Codex families → no presence.
	if err := agent.ApplyRecognition(context.Background(), runtimerecognition.Result{
		UncertainFamilies: []runtimerecognition.UncertainFamily{
			{Tool: instancepresence.ToolClaude},
			{Tool: instancepresence.ToolCodex},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(publisher.actions) != 0 {
		t.Fatalf("uncertain families published: %#v", publisher.actions)
	}

	// One secure Claude family appears.
	if err := agent.ApplyRecognition(context.Background(), runtimerecognition.Result{
		Families: []runtimerecognition.Family{
			{Candidate: instancepresence.RuntimeCandidate{Tool: instancepresence.ToolClaude}},
		},
		UncertainFamilies: []runtimerecognition.UncertainFamily{
			{Tool: instancepresence.ToolCodex},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if len(publisher.actions) != 1 || publisher.actions[0].source != SourceClaudeRuntime {
		t.Fatalf("actions = %#v", publisher.actions)
	}
}

func TestShutdownRemovesPublishedSources(t *testing.T) {
	publisher := &recordingPublisher{}
	agent, err := New(publisher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Apply(context.Background(), FamilyCounts{Claude: 1, Codex: 1}); err != nil {
		t.Fatal(err)
	}
	publisher.actions = nil
	if err := agent.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []action{
		{kind: "remove", source: SourceClaudeRuntime},
		{kind: "remove", source: SourceCodexRuntime},
	}
	if len(publisher.actions) != len(want) {
		t.Fatalf("actions = %#v, want %#v", publisher.actions, want)
	}
	for index, got := range publisher.actions {
		if got != want[index] {
			t.Fatalf("action[%d] = %#v, want %#v", index, got, want[index])
		}
	}
	if agent.ClaudePublished() || agent.CodexPublished() {
		t.Fatal("sources still marked published after shutdown")
	}
	// Second shutdown is a no-op.
	if err := agent.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(publisher.actions) != len(want) {
		t.Fatalf("second shutdown produced actions: %#v", publisher.actions)
	}
}

func TestApplyDoesNotMarkActiveOnPublishFailure(t *testing.T) {
	publisher := &recordingPublisher{err: errors.New("relay down")}
	agent, err := New(publisher, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Apply(context.Background(), FamilyCounts{Claude: 1}); err == nil {
		t.Fatal("expected publish error")
	}
	if agent.ClaudePublished() {
		t.Fatal("failed publish must not mark source active")
	}
	publisher.err = nil
	if err := agent.Apply(context.Background(), FamilyCounts{Claude: 1}); err != nil {
		t.Fatal(err)
	}
	if len(publisher.actions) != 1 || publisher.actions[0].kind != "publish" {
		t.Fatalf("actions = %#v", publisher.actions)
	}
}

func TestNewValidatesDependencies(t *testing.T) {
	if _, err := New(nil, time.Now); err == nil {
		t.Fatal("nil publisher accepted")
	}
	if _, err := New(&recordingPublisher{}, nil); err == nil {
		t.Fatal("nil clock accepted")
	}
}

func TestPublishSnapshotFields(t *testing.T) {
	publisher := &recordingPublisher{}
	now := time.Date(2026, 7, 22, 15, 30, 0, 0, time.FixedZone("CEST", 2*60*60))
	agent, err := New(publisher, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	// Capture via a richer publisher to inspect full snapshot.
	rich := &snapshotPublisher{}
	agent.publisher = rich
	if err := agent.Apply(context.Background(), FamilyCounts{Claude: 1}); err != nil {
		t.Fatal(err)
	}
	if rich.snapshot.Version != presence.ProtocolVersion {
		t.Fatalf("version = %d", rich.snapshot.Version)
	}
	if rich.snapshot.Source != SourceClaudeRuntime || rich.snapshot.State != status.Idle {
		t.Fatalf("snapshot = %#v", rich.snapshot)
	}
	want := time.Date(2026, 7, 22, 13, 30, 0, 0, time.UTC)
	if !rich.snapshot.Timestamp.Equal(want) {
		t.Fatalf("timestamp = %s, want %s", rich.snapshot.Timestamp, want)
	}
}

type snapshotPublisher struct {
	snapshot presence.Snapshot
}

func (p *snapshotPublisher) Publish(_ context.Context, snapshot presence.Snapshot) error {
	p.snapshot = snapshot
	return nil
}

func (p *snapshotPublisher) Remove(context.Context, string) error { return nil }
