package presencev2

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestContractExamples(t *testing.T) {
	tests := []struct {
		filename string
		value    interface{ Validate() error }
	}{
		{"no-instances.json", &CanonicalSnapshot{}},
		{"one-claude-instance.json", &CanonicalSnapshot{}},
		{"multiple-instances.json", &CanonicalSnapshot{}},
		{"four-pixel-presentation.json", &Presentation{}},
		{"runtime-mutation.json", &RuntimeMutation{}},
		{"hook-state-mutation.json", &HookStateMutation{}},
		{"mutation-response.json", &MutationResponse{}},
		{"revision-conflict-error.json", &Error{}},
	}
	for _, test := range tests {
		t.Run(test.filename, func(t *testing.T) {
			decodeExample(t, test.filename, test.value)
			if err := test.value.Validate(); err != nil {
				t.Fatalf("validate example: %v", err)
			}
		})
	}
}

func TestCanonicalAndPresentationContractsStaySeparate(t *testing.T) {
	var canonical CanonicalSnapshot
	err := decodeStrict([]byte(`{
		"api_version":2,"generated_at":"2026-07-21T10:00:00Z",
		"presence":"sleeping","instances":[],
		"slots":{"namespace":"default","active_count":0},"pixel_capacity":4
	}`), &canonical)
	if err == nil {
		t.Fatal("canonical snapshot accepted presentation pixel_capacity")
	}

	var mutation RuntimeMutation
	err = decodeStrict([]byte(`{
		"producer_epoch":"epoch-a","runtime_revision":1,"status":"alive",
		"observed_at":"2026-07-21T10:00:00Z","idempotency_key":"event-a",
		"source":{"provider":"claude","profile":"default","collector_id":"collector-a"}
	}`), &mutation)
	if err == nil {
		t.Fatal("runtime mutation accepted source metadata as a key")
	}
}

func TestSleepingIsNotAnInstanceState(t *testing.T) {
	instance := validAPIInstance()
	instance.State = "sleeping"
	if err := instance.Validate(); err == nil {
		t.Fatal("sleeping was accepted as an instance state")
	}
}

func TestHookIdleMeansClaimRemoval(t *testing.T) {
	mutation := HookStateMutation{
		ProducerEpoch: "epoch-alpha", HookRevision: 1, State: instancepresence.StateIdle,
		ObservedAt: fixedAPITestTime(), IdempotencyKey: "event-alpha",
	}
	if err := mutation.Validate(); err != nil {
		t.Fatalf("idle hook mutation rejected: %v", err)
	}
	claim, err := instancepresence.ApplyHookState(mutation.State)
	if err != nil || claim != instancepresence.NoHookClaim {
		t.Fatalf("idle mutation produced claim %q, %v", claim, err)
	}
}

func decodeExample(t *testing.T, filename string, target any) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), "api", "v2", "examples", filename))
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	if err := decodeStrict(data, target); err != nil {
		t.Fatalf("decode example: %v", err)
	}
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("more than one JSON value")
		}
		return err
	}
	return nil
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate contract test")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(filename), "..", ".."))
}
