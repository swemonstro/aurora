package localhooktransport

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestIngestRequestSerializationAllowlist(t *testing.T) {
	ingress := hookadapter.IngressObservation{
		Tool:           instancepresence.ToolClaude,
		HookSessionRef: "session-a",
		Lifecycle:      instancecorrelation.LifecycleActive,
	}
	request, err := NewIngestRequest(ingress)
	if err != nil {
		t.Fatal(err)
	}
	if request.ProtocolVersion != IngestProtocolVersion || request.Operation != OperationIngestHookEvent {
		t.Fatalf("request = %#v", request)
	}
	if len(request.RequestID) != 32 {
		t.Fatalf("request ID length = %d", len(request.RequestID))
	}
	data, err := EncodeIngestRequestJSON(request)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(data)) > DefaultIngestMaximumRequestBytes {
		t.Fatalf("request too large: %d", len(data))
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["payload"]; !ok {
		t.Fatalf("missing payload: %s", data)
	}
	if _, ok := raw["observation"]; ok {
		t.Fatalf("legacy observation field present: %s", data)
	}
	for _, forbidden := range []string{
		"producer_epoch", "revision", "observed_at", "idempotency_key",
		"process_hint", "runtime_hint", "parent_or_root_pid_hint", "host_id",
		"boot_id", "cwd", "argv", "transcript", "metadata", "event_kind", "event_fingerprint",
	} {
		if strings.Contains(string(data), `"`+forbidden+`"`) {
			t.Fatalf("forbidden field %q in %s", forbidden, data)
		}
	}
	decoded, err := DecodeIngestRequestJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Payload != request.Payload || decoded.RequestID != request.RequestID {
		t.Fatalf("decoded = %#v", decoded)
	}
}

func TestIngestRequestValidation(t *testing.T) {
	config := DefaultIngestClientConfig()
	config.SocketPath = "/run/user/1000/aurora/presence-hook.sock"
	valid := IngestRequest{
		ProtocolVersion: IngestProtocolVersion,
		Operation:       OperationIngestHookEvent,
		RequestID:       "0123456789abcdef0123456789abcdef",
		Payload: IngressPayload{
			Tool:           instancepresence.ToolCodex,
			HookSessionRef: "session-b",
			Lifecycle:      instancecorrelation.LifecycleIdle,
		},
	}
	if err := ValidateIngestRequest(config, valid); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		mut  func(*IngestRequest)
		code ErrorCode
	}{
		{name: "version", mut: func(r *IngestRequest) { r.ProtocolVersion = 1 }, code: CodeUnsupportedProtocolVersion},
		{name: "operation", mut: func(r *IngestRequest) { r.Operation = OperationCorrelateObservation }, code: CodeUnsupportedOperation},
		{name: "request id", mut: func(r *IngestRequest) { r.RequestID = "bad/id" }, code: CodeInvalidRequestID},
		{name: "empty session", mut: func(r *IngestRequest) { r.Payload.HookSessionRef = "" }, code: CodeInvalidIngress},
		{name: "tool", mut: func(r *IngestRequest) { r.Payload.Tool = "hermes" }, code: CodeInvalidIngress},
		{name: "lifecycle", mut: func(r *IngestRequest) { r.Payload.Lifecycle = "paused" }, code: CodeInvalidIngress},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := valid
			test.mut(&request)
			if code := errorCode(ValidateIngestRequest(config, request)); code != test.code {
				t.Fatalf("code = %q want %q", code, test.code)
			}
		})
	}
}

func TestIngestRequestStrictDecode(t *testing.T) {
	tests := []struct {
		name string
		data string
		code ErrorCode
	}{
		{name: "empty", data: "", code: CodeMalformedRequest},
		{name: "unknown field", data: `{"protocol_version":2,"operation":"ingest_hook_event","request_id":"0123456789abcdef0123456789abcdef","payload":{"tool":"claude","hook_session_ref":"s","lifecycle":"active"},"extra":true}`, code: CodeUnknownField},
		{name: "unknown payload field", data: `{"protocol_version":2,"operation":"ingest_hook_event","request_id":"0123456789abcdef0123456789abcdef","payload":{"tool":"claude","hook_session_ref":"s","lifecycle":"active","producer_epoch":"x"}}`, code: CodeUnknownField},
		{name: "trailing", data: `{"protocol_version":2,"operation":"ingest_hook_event","request_id":"0123456789abcdef0123456789abcdef","payload":{"tool":"claude","hook_session_ref":"s","lifecycle":"active"}}{}`, code: CodeMalformedRequest},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := DecodeIngestRequestJSON([]byte(test.data))
			if errorCode(err) != test.code {
				t.Fatalf("err=%v code=%q", err, errorCode(err))
			}
		})
	}
}

func TestIngestResponseValidationAndSerialization(t *testing.T) {
	response := emptyIngestResponse(StatusOK, "0123456789abcdef0123456789abcdef")
	data, err := EncodeIngestResponseJSON(response, DefaultIngestMaximumResponseBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"proposals", "summary", "hook_session_ref", "producer_epoch", "pid"} {
		if strings.Contains(string(data), `"`+forbidden+`"`) {
			t.Fatalf("forbidden field %q in %s", forbidden, data)
		}
	}
	decoded, err := DecodeIngestResponseJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateIngestResponse(decoded, response.RequestID); err != nil {
		t.Fatal(err)
	}
	bad := response
	bad.NoBindingPerformed = false
	if err := ValidateIngestResponse(bad, response.RequestID); err == nil {
		t.Fatal("binding response accepted")
	}
	bad = response
	bad.RequestID = "other"
	if err := ValidateIngestResponse(bad, response.RequestID); err == nil {
		t.Fatal("mismatched request ID accepted")
	}
	if _, err := EncodeIngestResponseJSON(response, 1); !errors.Is(err, ErrResponseTooLarge) {
		t.Fatalf("size limit error = %v", err)
	}
}

func TestLocalHookEnabledAndSocketResolution(t *testing.T) {
	for _, value := range []string{"", "0", "false", "yes", "TRUE ", " true"} {
		if value == "TRUE " || value == " true" {
			if !LocalHookEnabled(value) {
				t.Fatalf("enabled value %q rejected", value)
			}
			continue
		}
		if LocalHookEnabled(value) {
			t.Fatalf("disabled value %q accepted", value)
		}
	}
	if !LocalHookEnabled("1") || !LocalHookEnabled("true") || !LocalHookEnabled("TRUE") {
		t.Fatal("expected enable values")
	}

	path, err := ResolveLocalHookSocket(func(key string) string {
		if key == EnvLocalHookSocket {
			return "/run/user/1000/aurora/presence-hook.sock"
		}
		return ""
	})
	if err != nil || path != "/run/user/1000/aurora/presence-hook.sock" {
		t.Fatalf("explicit path = %q err=%v", path, err)
	}
	path, err = ResolveLocalHookSocket(func(key string) string {
		if key == EnvXDGRuntimeDir {
			return "/run/user/1000"
		}
		return ""
	})
	if err != nil || path != "/run/user/1000/aurora/presence-hook.sock" {
		t.Fatalf("xdg path = %q err=%v", path, err)
	}
	if _, err := ResolveLocalHookSocket(func(string) string { return "" }); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("missing path error = %v", err)
	}
	if _, err := ResolveLocalHookSocket(func(key string) string {
		if key == EnvLocalHookSocket {
			return "/tmp/aurora.sock"
		}
		return ""
	}); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("tmp socket error = %v", err)
	}
	if _, err := ResolveLocalHookSocket(func(key string) string {
		if key == EnvLocalHookSocket {
			return "relative/path.sock"
		}
		return ""
	}); !errors.Is(err, ErrInsecureSocketPath) {
		t.Fatalf("relative socket error = %v", err)
	}
}

func TestIngestClientConfigBounds(t *testing.T) {
	config := DefaultIngestClientConfig()
	config.SocketPath = "/run/user/1000/aurora/presence-hook.sock"
	if err := config.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := config
	invalid.TotalBudget = 200 * time.Millisecond
	if err := invalid.Validate(); err == nil {
		t.Fatal("oversized total budget accepted")
	}
	invalid = config
	invalid.ConnectDeadline = 50 * time.Millisecond
	if err := invalid.Validate(); err == nil {
		t.Fatal("oversized connect deadline accepted")
	}
	invalid = config
	invalid.MaximumRequestBytes = 16 * 1024
	if err := invalid.Validate(); err == nil {
		t.Fatal("oversized request limit accepted")
	}
}

func TestClientLatencyBuckets(t *testing.T) {
	if clientLatencyBucket(5*time.Millisecond, false) != "lt_10ms" {
		t.Fatal("lt_10ms")
	}
	if clientLatencyBucket(20*time.Millisecond, false) != "lt_50ms" {
		t.Fatal("lt_50ms")
	}
	if clientLatencyBucket(80*time.Millisecond, false) != "lt_100ms" {
		t.Fatal("lt_100ms")
	}
	if clientLatencyBucket(5*time.Millisecond, true) != "timeout" {
		t.Fatal("timeout")
	}
}

func TestV1CorrelateStillDistinctFromIngest(t *testing.T) {
	config := DefaultConfig(&testClock{now: testTime})
	request := testRequest("request-01", instancepresence.ToolClaude)
	if err := ValidateRequest(config, request); err != nil {
		t.Fatal(err)
	}
	request.Operation = OperationIngestHookEvent
	if code := errorCode(ValidateRequest(config, request)); code != CodeMalformedRequest {
		t.Fatalf("v1 validator accepted ingest operation: %q", code)
	}
}
