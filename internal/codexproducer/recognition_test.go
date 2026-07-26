package codexproducer

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

func codexProcess(pid uint64, startedAt time.Time, envCodexHome string) runtimerecognition.ProcessObservation {
	return runtimerecognition.ProcessObservation{
		Process:            instancepresence.ProcessIdentity{PID: pid, StartedAt: startedAt},
		CommIdentity:       "exe:codex",
		ExecutableIdentity: "exe:codex",
		OwnerIdentity:      "uid:1000",
		EnvCodexHome:       envCodexHome,
	}
}

func testSnapshot(t *testing.T, observedAt time.Time, processes ...runtimerecognition.ProcessObservation) runtimerecognition.Snapshot {
	t.Helper()
	snapshot := runtimerecognition.Snapshot{ObservedAt: observedAt, BootID: "boot-1", Processes: processes}
	if err := snapshot.Validate(); err != nil {
		t.Fatalf("invalid test snapshot: %v", err)
	}
	return snapshot
}

func testSources(t *testing.T, defaultLabel SourceLabel, entries ...SourceEntry) *SourceSet {
	t.Helper()
	set, err := NewSourceSet(entries, defaultLabel)
	if err != nil {
		t.Fatalf("NewSourceSet: %v", err)
	}
	return set
}

// testCodexHomeDir creates and returns a fresh, real, hermetic directory to
// stand in for one configured CODEX_HOME source: NewSourceSet now requires
// every configured path to actually exist (see sources.go), so tests can no
// longer use arbitrary path strings like "/home/carl/.codex-business" that
// happen to exist (or not) on whichever machine runs them — every test
// fixture path must be one this test itself created and owns.
func testCodexHomeDir(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRecognizeFromSnapshot_AttributesConfiguredSource(t *testing.T) {
	business := testCodexHomeDir(t, "business")
	api := testCodexHomeDir(t, "api")
	sources := testSources(t, "", SourceEntry{Label: "business", Path: business}, SourceEntry{Label: "api", Path: api})
	now := time.Now().UTC()
	snapshot := testSnapshot(t, now, codexProcess(100, now.Add(-time.Minute), business))

	instances, err := RecognizeFromSnapshot(snapshot, sources)
	if err != nil {
		t.Fatalf("RecognizeFromSnapshot: %v", err)
	}
	if len(instances) != 1 {
		t.Fatalf("expected 1 recognized instance, got %d", len(instances))
	}
	if instances[0].Source != "business" {
		t.Fatalf("expected business source, got %q", instances[0].Source)
	}
}

func TestRecognizeFromSnapshot_IgnoresUnconfiguredCodexHome(t *testing.T) {
	business := testCodexHomeDir(t, "business")
	unconfigured := testCodexHomeDir(t, "unconfigured")
	sources := testSources(t, "", SourceEntry{Label: "business", Path: business})
	now := time.Now().UTC()
	snapshot := testSnapshot(t, now, codexProcess(100, now.Add(-time.Minute), unconfigured))

	instances, err := RecognizeFromSnapshot(snapshot, sources)
	if err != nil {
		t.Fatalf("RecognizeFromSnapshot: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("expected unconfigured CODEX_HOME to be ignored, got %d instances", len(instances))
	}
}

func TestRecognizeFromSnapshot_EmptyCodexHomeIgnoredWithoutDefault(t *testing.T) {
	api := testCodexHomeDir(t, "api")
	sources := testSources(t, "", SourceEntry{Label: "api", Path: api})
	now := time.Now().UTC()
	snapshot := testSnapshot(t, now, codexProcess(100, now.Add(-time.Minute), ""))

	instances, err := RecognizeFromSnapshot(snapshot, sources)
	if err != nil {
		t.Fatalf("RecognizeFromSnapshot: %v", err)
	}
	if len(instances) != 0 {
		t.Fatalf("expected empty CODEX_HOME with no -default-source to be ignored, got %d instances", len(instances))
	}
}

func TestRecognizeFromSnapshot_EmptyCodexHomeMatchesExplicitDefault(t *testing.T) {
	api := testCodexHomeDir(t, "api")
	sources := testSources(t, "api", SourceEntry{Label: "api", Path: api})
	now := time.Now().UTC()
	snapshot := testSnapshot(t, now, codexProcess(100, now.Add(-time.Minute), ""))

	instances, err := RecognizeFromSnapshot(snapshot, sources)
	if err != nil {
		t.Fatalf("RecognizeFromSnapshot: %v", err)
	}
	if len(instances) != 1 || instances[0].Source != "api" {
		t.Fatalf("expected empty CODEX_HOME to match explicit default source, got %+v", instances)
	}
}

func TestRecognizeFromSnapshot_BusinessAndAPINeverBlend(t *testing.T) {
	business := testCodexHomeDir(t, "business")
	api := testCodexHomeDir(t, "api")
	sources := testSources(t, "", SourceEntry{Label: "business", Path: business}, SourceEntry{Label: "api", Path: api})
	now := time.Now().UTC()
	snapshot := testSnapshot(t, now,
		codexProcess(100, now.Add(-time.Minute), business),
		codexProcess(200, now.Add(-time.Minute), api),
	)

	instances, err := RecognizeFromSnapshot(snapshot, sources)
	if err != nil {
		t.Fatalf("RecognizeFromSnapshot: %v", err)
	}
	if len(instances) != 2 {
		t.Fatalf("expected 2 recognized instances, got %d", len(instances))
	}
	bySource := map[SourceLabel]RecognizedInstance{}
	for _, instance := range instances {
		bySource[instance.Source] = instance
	}
	businessInstance, ok := bySource["business"]
	if !ok {
		t.Fatalf("missing business instance")
	}
	apiInstance, ok := bySource["api"]
	if !ok {
		t.Fatalf("missing api instance")
	}
	if businessInstance.InstanceID == apiInstance.InstanceID {
		t.Fatalf("business and api instance ids must never collide: %q", businessInstance.InstanceID)
	}
}

func TestRecognizeFromSnapshot_FiveParallelSessionsSameSource(t *testing.T) {
	business := testCodexHomeDir(t, "business")
	sources := testSources(t, "", SourceEntry{Label: "business", Path: business})
	now := time.Now().UTC()
	var processes []runtimerecognition.ProcessObservation
	for pid := uint64(100); pid < 105; pid++ {
		processes = append(processes, codexProcess(pid, now.Add(-time.Duration(pid)*time.Second), business))
	}
	snapshot := testSnapshot(t, now, processes...)

	instances, err := RecognizeFromSnapshot(snapshot, sources)
	if err != nil {
		t.Fatalf("RecognizeFromSnapshot: %v", err)
	}
	if len(instances) != 5 {
		t.Fatalf("expected 5 recognized instances, got %d", len(instances))
	}
	seen := map[string]struct{}{}
	for _, instance := range instances {
		if instance.Source != "business" {
			t.Fatalf("expected all instances attributed to business, got %q", instance.Source)
		}
		key := string(instance.InstanceID)
		if _, exists := seen[key]; exists {
			t.Fatalf("duplicate instance id %q among parallel sessions", key)
		}
		seen[key] = struct{}{}
	}
}

func TestRecognizeFromSnapshot_SameUnderlyingSessionLikeIdentifierAcrossSourcesDoesNotCollide(t *testing.T) {
	// Same PID and same start time is impossible on one real host for two
	// distinct processes, but DeriveInstanceID still mixes in the source
	// label as defense in depth (see identity.go) — verify it directly.
	startedAt := time.Now().UTC().Add(-time.Minute)
	business := DeriveInstanceID("business", 4242, startedAt)
	api := DeriveInstanceID("api", 4242, startedAt)
	if business == api {
		t.Fatalf("instance ids must differ across sources even for identical PID+StartedAt, got %q for both", business)
	}
}
