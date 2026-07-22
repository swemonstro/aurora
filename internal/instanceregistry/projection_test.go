package instanceregistry

import (
	"reflect"
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/presencev2"
)

func TestCanonicalSnapshotIsDeterministicAndSleepingWhenEmpty(t *testing.T) {
	registry, _ := newTestRegistry(t)
	empty, err := registry.CanonicalSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Presence != presencev2.PresenceSleeping || empty.Instances == nil || len(empty.Instances) != 0 {
		t.Fatalf("empty snapshot = %#v", empty)
	}
	if err := empty.Validate(); err != nil {
		t.Fatalf("empty snapshot invalid: %v", err)
	}

	for index, id := range []string{"instance-c", "instance-a", "instance-b"} {
		mustRegister(t, registry, registration(id, uint64(101+index)))
	}
	first, _ := registry.CanonicalSnapshot()
	second, _ := registry.CanonicalSnapshot()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("snapshot changed without mutation:\n%#v\n%#v", first, second)
	}
	want := []instancepresence.InstanceID{"instance-c", "instance-a", "instance-b"}
	got := make([]instancepresence.InstanceID, len(first.Instances))
	for index := range first.Instances {
		got[index] = first.Instances[index].InstanceID
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot order = %v, want %v", got, want)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("canonical snapshot invalid: %v", err)
	}

	first.Instances[0].Source.Provider = "mutated"
	again, _ := registry.CanonicalSnapshot()
	if again.Instances[0].Source.Provider != "codex-api" {
		t.Fatal("wire snapshot mutation leaked into registry")
	}
}

func TestPresentationOverflowAndZeroCapacityAreDeterministic(t *testing.T) {
	registry, _ := newTestRegistry(t)
	for index, id := range []string{"instance-a", "instance-b", "instance-c", "instance-d"} {
		mustRegister(t, registry, registration(id, uint64(101+index)))
	}
	if _, err := registry.EndRuntime("instance-b", runtimeMutation(2, instancepresence.RuntimeEnded, "end-b")); err != nil {
		t.Fatal(err)
	}

	presentation, err := registry.Presentation(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := presentation.Validate(); err != nil {
		t.Fatalf("presentation invalid: %v", err)
	}
	if len(presentation.Pixels) != 1 || presentation.Pixels[0].Pixel != 0 || presentation.Pixels[0].InstanceID != "instance-a" {
		t.Fatalf("visible pixels = %#v", presentation.Pixels)
	}
	wantOverflow := []instancepresence.InstanceID{"instance-c", "instance-d"}
	if !reflect.DeepEqual(presentation.OverflowInstanceIDs, wantOverflow) {
		t.Fatalf("overflow = %v, want %v", presentation.OverflowInstanceIDs, wantOverflow)
	}
	again, _ := registry.Presentation(2)
	if !reflect.DeepEqual(presentation, again) {
		t.Fatal("presentation changed without registry mutation")
	}

	zero, err := registry.Presentation(0)
	if err != nil {
		t.Fatal(err)
	}
	if err := zero.Validate(); err != nil {
		t.Fatalf("zero-capacity presentation invalid: %v", err)
	}
	if len(zero.Pixels) != 0 || zero.OverflowCount != 3 || zero.VisibleCount != 0 {
		t.Fatalf("zero-capacity presentation = %#v", zero)
	}

	zero.OverflowInstanceIDs[0] = "mutated"
	unchanged, _ := registry.Presentation(0)
	if unchanged.OverflowInstanceIDs[0] != "instance-a" {
		t.Fatal("presentation slice mutation leaked into registry")
	}
}
