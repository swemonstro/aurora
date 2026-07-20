package relay

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/presence"
	"github.com/swemonstro/aurora/internal/status"
)

func newTestHandler(t *testing.T) (*Store, http.Handler) {
	t.Helper()

	store := &Store{}
	handler, err := NewHandler(store)
	if err != nil {
		t.Fatalf("NewHandler returned error: %v", err)
	}

	return store, handler.Routes()
}

func TestGetPresenceReturnsNotFoundWhenStoreIsEmpty(t *testing.T) {
	_, handler := newTestHandler(t)

	request := httptest.NewRequest(http.MethodGet, "/presence", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func TestPostThenGetPresence(t *testing.T) {
	_, handler := newTestHandler(t)

	snapshot := presence.Snapshot{
		Version:   presence.ProtocolVersion,
		Source:    "claude",
		State:     status.Working,
		Timestamp: time.Date(2026, 7, 20, 8, 0, 0, 0, time.UTC),
	}

	body, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("json.Marshal returned error: %v", err)
	}

	postRequest := httptest.NewRequest(
		http.MethodPost,
		"/presence",
		bytes.NewReader(body),
	)
	postRequest.Header.Set("Content-Type", "application/json")
	postResponse := httptest.NewRecorder()

	handler.ServeHTTP(postResponse, postRequest)

	if postResponse.Code != http.StatusNoContent {
		t.Fatalf(
			"POST status = %d, want %d; body = %q",
			postResponse.Code,
			http.StatusNoContent,
			postResponse.Body.String(),
		)
	}

	getRequest := httptest.NewRequest(http.MethodGet, "/presence", nil)
	getResponse := httptest.NewRecorder()

	handler.ServeHTTP(getResponse, getRequest)

	if getResponse.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d", getResponse.Code, http.StatusOK)
	}

	var got presence.Snapshot
	if err := json.NewDecoder(getResponse.Body).Decode(&got); err != nil {
		t.Fatalf("decode GET response: %v", err)
	}

	if got != snapshot {
		t.Fatalf("GET snapshot = %#v, want %#v", got, snapshot)
	}
}

func TestPostPresenceRejectsInvalidSnapshots(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "unsupported protocol version",
			body: `{"version":2,"source":"claude","state":"working","timestamp":"2026-07-20T08:00:00Z"}`,
		},
		{
			name: "empty source",
			body: `{"version":1,"source":" ","state":"working","timestamp":"2026-07-20T08:00:00Z"}`,
		},
		{
			name: "unsupported state",
			body: `{"version":1,"source":"claude","state":"sleeping","timestamp":"2026-07-20T08:00:00Z"}`,
		},
		{
			name: "zero timestamp",
			body: `{"version":1,"source":"claude","state":"working","timestamp":"0001-01-01T00:00:00Z"}`,
		},
		{
			name: "unknown field",
			body: `{"version":1,"source":"claude","state":"working","timestamp":"2026-07-20T08:00:00Z","prompt":"secret"}`,
		},
		{
			name: "multiple values",
			body: `{"version":1,"source":"claude","state":"working","timestamp":"2026-07-20T08:00:00Z"} {}`,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, handler := newTestHandler(t)

			request := httptest.NewRequest(
				http.MethodPost,
				"/presence",
				strings.NewReader(test.body),
			)
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()

			handler.ServeHTTP(response, request)

			if response.Code != http.StatusBadRequest {
				t.Fatalf(
					"status = %d, want %d; body = %q",
					response.Code,
					http.StatusBadRequest,
					response.Body.String(),
				)
			}
		})
	}
}

func TestPostPresenceAcceptsJSONContentTypeWithParameters(t *testing.T) {
	_, handler := newTestHandler(t)

	request := httptest.NewRequest(
		http.MethodPost,
		"/presence",
		strings.NewReader(
			`{"version":1,"source":"claude","state":"working","timestamp":"2026-07-20T08:00:00Z"}`,
		),
	)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf(
			"status = %d, want %d; body = %q",
			response.Code,
			http.StatusNoContent,
			response.Body.String(),
		)
	}
}

func TestPostPresenceRejectsJSONPrefixContentType(t *testing.T) {
	_, handler := newTestHandler(t)

	request := httptest.NewRequest(
		http.MethodPost,
		"/presence",
		strings.NewReader(`{}`),
	)
	request.Header.Set("Content-Type", "application/json-malformed")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf(
			"status = %d, want %d",
			response.Code,
			http.StatusUnsupportedMediaType,
		)
	}
}

func TestPostPresenceRejectsOversizedBody(t *testing.T) {
	_, handler := newTestHandler(t)

	largeSource := strings.Repeat("a", maxSnapshotBodyBytes)
	body := `{"version":1,"source":"` + largeSource +
		`","state":"working","timestamp":"2026-07-20T08:00:00Z"}`

	request := httptest.NewRequest(
		http.MethodPost,
		"/presence",
		strings.NewReader(body),
	)
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf(
			"status = %d, want %d; body = %q",
			response.Code,
			http.StatusRequestEntityTooLarge,
			response.Body.String(),
		)
	}
}

func TestPostPresenceRejectsUnsupportedContentType(t *testing.T) {
	_, handler := newTestHandler(t)

	request := httptest.NewRequest(
		http.MethodPost,
		"/presence",
		strings.NewReader(`{}`),
	)
	request.Header.Set("Content-Type", "text/plain")
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusUnsupportedMediaType {
		t.Fatalf(
			"status = %d, want %d",
			response.Code,
			http.StatusUnsupportedMediaType,
		)
	}
}

func TestPresenceRejectsUnsupportedMethod(t *testing.T) {
	_, handler := newTestHandler(t)

	request := httptest.NewRequest(http.MethodDelete, "/presence", nil)
	response := httptest.NewRecorder()

	handler.ServeHTTP(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf(
			"status = %d, want %d",
			response.Code,
			http.StatusMethodNotAllowed,
		)
	}
	if got := response.Header().Get("Allow"); got != "GET, POST" {
		t.Fatalf("Allow = %q, want %q", got, "GET, POST")
	}
}

func TestNewHandlerRejectsNilStore(t *testing.T) {
	if _, err := NewHandler(nil); err == nil {
		t.Fatal("NewHandler(nil) returned no error")
	}
}
