package localhooktransport

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "fixture timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

type timeoutWriter struct{}

func (timeoutWriter) Write([]byte) (int, error) { return 0, timeoutError{} }

func TestJSONFixturesAreStrictAndValid(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	for _, path := range []string{"testdata/claude-observation.json", "testdata/codex-observation.json"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		request, err := DecodeRequestJSON(data)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := ValidateRequest(config, request); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
}

func TestDecodeRequestStrictFailures(t *testing.T) {
	tests := []struct {
		name string
		data string
		code ErrorCode
	}{
		{name: "empty", data: "", code: CodeMalformedRequest},
		{name: "malformed", data: "{", code: CodeMalformedRequest},
		{name: "unknown field", data: `{"protocol_version":1,"unknown":true}`, code: CodeUnknownField},
		{name: "trailing value", data: `{}` + `{}`, code: CodeMalformedRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeRequestJSON([]byte(test.data))
			if errorCode(err) != test.code {
				t.Fatalf("error = %v, code = %q", err, errorCode(err))
			}
		})
	}
}

func TestRequestValidationVersionIDTimeAndUnknownPayloadUID(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultConfig(clock)
	request := testRequest("request-01", "claude")
	request.ProtocolVersion = 99
	if code := errorCode(ValidateRequest(config, request)); code != CodeUnsupportedProtocolVersion {
		t.Fatalf("version code = %q", code)
	}
	request = testRequest("bad/id", "claude")
	if code := errorCode(ValidateRequest(config, request)); code != CodeInvalidRequestID {
		t.Fatalf("request ID code = %q", code)
	}
	request = testRequest("request-01", "claude")
	request.Observation.ObservedAt = testTime.Add(-config.MaximumObservationAge - time.Nanosecond)
	if code := errorCode(ValidateRequest(config, request)); code != CodeStaleObservation {
		t.Fatalf("stale code = %q", code)
	}
	data := []byte(`{"protocol_version":1,"request_id":"request-01","operation":"correlate_observation","observation":{"tool":"claude","hook_session_ref":"hook-fixture","producer_epoch":"epoch-fixture","revision":1,"observed_at":"2026-07-22T12:00:00Z","lifecycle":"active","uid":1000}}`)
	if _, err := DecodeRequestJSON(data); errorCode(err) != CodeUnknownField {
		t.Fatalf("payload UID error = %v", err)
	}
}

func TestFrameBoundsDisconnectAndTimeout(t *testing.T) {
	var oversized bytes.Buffer
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], 11)
	oversized.Write(header[:])
	if _, err := readFrame(&oversized, 10); !errors.Is(err, ErrRequestTooLarge) {
		t.Fatalf("oversized error = %v", err)
	}
	var partial bytes.Buffer
	binary.BigEndian.PutUint32(header[:], 5)
	partial.Write(header[:])
	partial.WriteString("ab")
	if _, err := readFrame(&partial, 10); !errors.Is(err, ErrPeerDisconnected) {
		t.Fatalf("partial error = %v", err)
	}
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()
	if err := server.SetReadDeadline(time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := readFrame(server, 10); !errors.Is(err, ErrReadTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	if err := writeFrame(timeoutWriter{}, []byte("fixture"), 10); !errors.Is(err, ErrWriteTimeout) {
		t.Fatalf("write timeout error = %v", err)
	}
}

func TestResponseLimitAndDeterministicJSON(t *testing.T) {
	response := emptyResponse(StatusOK, "request-01")
	first, err := EncodeResponseJSON(response, 4096)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := EncodeResponseJSON(response, 4096)
	if !bytes.Equal(first, second) {
		t.Fatalf("response JSON differs: %s / %s", first, second)
	}
	if _, err := EncodeResponseJSON(response, 1); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("response limit error = %v", err)
	}
	var value map[string]any
	if err := json.Unmarshal(first, &value); err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"pid", "started_at", "host_id", "boot_id", "process_group", "os_session", "terminal_fingerprint", "provider", "profile"} {
		if strings.Contains(string(first), `"`+forbidden+`"`) {
			t.Fatalf("response contains forbidden field %q: %s", forbidden, first)
		}
	}
}
