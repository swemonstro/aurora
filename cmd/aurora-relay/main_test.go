package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

func TestRunRejectsInvalidListenAddress(t *testing.T) {
	err := run(context.Background(), "127.0.0.1:not-a-port", log.New(io.Discard, "", 0))
	if err == nil {
		t.Fatal("run returned no error")
	}
}

func TestServerExposesPresenceRoutes(t *testing.T) {
	server, err := newServer()
	if err != nil {
		t.Fatalf("newServer returned error: %v", err)
	}

	testServer := httptest.NewServer(server.Handler)
	defer testServer.Close()

	want := presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "relay-cli-test",
		State:     status.Working,
		Timestamp: time.Date(2026, 7, 20, 10, 30, 0, 0, time.UTC),
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}

	response, err := http.Post(
		testServer.URL+"/presence",
		"application/json",
		bytes.NewReader(body),
	)
	if err != nil {
		t.Fatalf("POST /presence: %v", err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("POST status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}

	response, err = http.Get(testServer.URL + "/presence")
	if err != nil {
		t.Fatalf("GET /presence: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	var got presence.Snapshot
	if err := json.NewDecoder(response.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}
	if got != want {
		t.Fatalf("GET snapshot = %#v, want %#v", got, want)
	}
}

func TestServeShutsDownWhenContextIsCanceled(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	var logs strings.Builder
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- serve(ctx, listener, log.New(&logs, "", 0))
	}()

	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("serve returned error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not shut down")
	}

	if !strings.Contains(logs.String(), listener.Addr().String()) {
		t.Fatalf("startup log %q does not contain effective address", logs.String())
	}
}
