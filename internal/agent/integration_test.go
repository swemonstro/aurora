package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/publish"
	"github.com/swemonstro/aurora/internal/relay"
	"github.com/swemonstro/aurora/internal/status"
)

func TestAgentPublishesPresenceThroughRelay(t *testing.T) {
	tests := []struct {
		name  string
		event string
		state status.State
	}{
		{name: "working", event: "working", state: status.Working},
		{name: "attention", event: "attention", state: status.Attention},
		{name: "error", event: "error", state: status.Error},
		{name: "idle", event: "idle", state: status.Idle},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := &relay.Store{}
			handler, err := relay.NewHandler(store)
			if err != nil {
				t.Fatalf("NewHandler returned error: %v", err)
			}

			server := httptest.NewServer(handler.Routes())
			defer server.Close()

			publisher, err := publish.NewHTTPPublisher(server.URL, server.Client())
			if err != nil {
				t.Fatalf("NewHTTPPublisher returned error: %v", err)
			}

			const source = "integration-test"
			timestamp := time.Date(2026, 7, 20, 10, 30, 0, 0, time.UTC)
			instance, err := New(source, publisher, func() time.Time { return timestamp })
			if err != nil {
				t.Fatalf("New returned error: %v", err)
			}

			if err := instance.Handle(context.Background(), test.event); err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}

			response, err := server.Client().Get(server.URL + "/presence")
			if err != nil {
				t.Fatalf("GET /presence returned error: %v", err)
			}
			defer response.Body.Close()

			if response.StatusCode != http.StatusOK {
				t.Fatalf("GET /presence status = %d, want %d", response.StatusCode, http.StatusOK)
			}

			var snapshot presence.Snapshot
			if err := json.NewDecoder(response.Body).Decode(&snapshot); err != nil {
				t.Fatalf("decode GET /presence response: %v", err)
			}

			if snapshot.State != test.state {
				t.Errorf("state = %q, want %q", snapshot.State, test.state)
			}
			if snapshot.Source != source {
				t.Errorf("source = %q, want %q", snapshot.Source, source)
			}
			if snapshot.Timestamp.IsZero() {
				t.Error("timestamp is zero")
			}
			if !snapshot.Timestamp.Equal(timestamp) {
				t.Errorf("timestamp = %s, want %s", snapshot.Timestamp, timestamp)
			}
			if snapshot.Version != presence.ProtocolVersion {
				t.Errorf("version = %d, want %d", snapshot.Version, presence.ProtocolVersion)
			}
		})
	}
}
