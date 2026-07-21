package publish

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

func TestHTTPPublisherPostsSnapshot(t *testing.T) {
	snapshot := presence.Snapshot{
		Version: presence.ProtocolVersion, Source: "claude", State: status.Working,
		Timestamp: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/presence" {
			t.Errorf("path = %q, want /presence", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var got presence.Snapshot
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request body: %v", err)
		}
		if got != snapshot {
			t.Errorf("snapshot = %#v, want %#v", got, snapshot)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	publisher, err := NewHTTPPublisher(server.URL+"/", server.Client())
	if err != nil {
		t.Fatalf("NewHTTPPublisher returned error: %v", err)
	}
	if err := publisher.Publish(context.Background(), snapshot); err != nil {
		t.Fatalf("Publish returned error: %v", err)
	}
}

func TestHTTPPublisherDeletesOnlyRequestedSource(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/presence" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("source"); got != "codex-business" {
			t.Errorf("source = %q", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	publisher, err := NewHTTPPublisher(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Remove(context.Background(), " codex-business "); err != nil {
		t.Fatal(err)
	}
	if err := publisher.Remove(context.Background(), " "); err == nil {
		t.Fatal("empty source was accepted")
	}
}

func TestHTTPPublisherReturnsErrorForNon2xx(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "relay unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	publisher, err := NewHTTPPublisher(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), presence.Snapshot{}); err == nil {
		t.Fatal("Publish returned no error")
	}
}

func TestHTTPPublisherReturnsNetworkError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	client, relayURL := server.Client(), server.URL
	server.Close()
	publisher, err := NewHTTPPublisher(relayURL, client)
	if err != nil {
		t.Fatal(err)
	}
	if err := publisher.Publish(context.Background(), presence.Snapshot{}); err == nil {
		t.Fatal("Publish returned no error")
	}
}

func TestNewHTTPPublisherValidatesDependencies(t *testing.T) {
	tests := []struct {
		name, relayURL string
		client         *http.Client
	}{
		{name: "empty relay URL", relayURL: " ", client: http.DefaultClient},
		{name: "nil client", relayURL: "http://127.0.0.1:8080"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewHTTPPublisher(test.relayURL, test.client); err == nil {
				t.Fatal("NewHTTPPublisher returned no error")
			}
		})
	}
}

func TestHTTPPublisherRespectsContextCancellation(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseHandler := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		close(requestStarted)
		<-releaseHandler
	}))
	defer server.Close()
	publisher, err := NewHTTPPublisher(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() { errCh <- publisher.Publish(ctx, presence.Snapshot{}) }()
	<-requestStarted
	cancel()
	err = <-errCh
	close(releaseHandler)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Publish error = %v, want context.Canceled", err)
	}
}
