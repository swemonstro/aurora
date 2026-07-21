package presence

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/status"
)

func TestV1SnapshotWireContractIsFrozen(t *testing.T) {
	if ProtocolVersion != 1 {
		t.Fatalf("v1 protocol version = %d, want 1", ProtocolVersion)
	}
	snapshotType := reflect.TypeOf(Snapshot{})
	wantFields := []string{"Version", "Source", "State", "Timestamp"}
	if snapshotType.NumField() != len(wantFields) {
		t.Fatalf("v1 Snapshot has %d fields, want %d", snapshotType.NumField(), len(wantFields))
	}
	for index, want := range wantFields {
		if got := snapshotType.Field(index).Name; got != want {
			t.Fatalf("v1 field %d = %q, want %q", index, got, want)
		}
	}

	snapshot := Snapshot{Version: 1, Source: "claude", State: status.Working, Timestamp: time.Date(2026, 7, 21, 10, 0, 0, 0, time.UTC)}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"source":"claude","state":"working","timestamp":"2026-07-21T10:00:00Z"}`
	if string(encoded) != want {
		t.Fatalf("v1 JSON = %s, want %s", encoded, want)
	}
}
