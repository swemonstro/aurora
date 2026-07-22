package localhooktransport

import (
	"fmt"
	"time"
)

func ValidateRequest(config Config, request Request) error {
	if request.ProtocolVersion != CurrentProtocolVersion {
		return protocolError(CodeUnsupportedProtocolVersion, fmt.Errorf("unsupported version"))
	}
	if request.Operation != OperationCorrelateObservation {
		return protocolError(CodeMalformedRequest, fmt.Errorf("unsupported operation"))
	}
	if err := validateOpaque("request ID", request.RequestID, config.MaximumRequestIDLength, true); err != nil {
		return protocolError(CodeInvalidRequestID, err)
	}
	observation := request.Observation
	values := []struct {
		name     string
		value    string
		required bool
	}{
		{"hook session reference", string(observation.HookSessionRef), true},
		{"producer epoch", string(observation.ProducerEpoch), true},
		{"idempotency key", observation.IdempotencyKey, false},
		{"process group", string(observation.ProcessGroupOrJob), false},
		{"OS session", string(observation.OSSession), false},
		{"terminal fingerprint", string(observation.TerminalFingerprint), false},
		{"host ID", observation.HostID, false},
		{"boot ID", string(observation.BootID), false},
	}
	for _, value := range values {
		if err := validateOpaque(value.name, value.value, config.MaximumOpaqueLength, value.required); err != nil {
			return protocolError(CodeInvalidHookObservation, err)
		}
	}
	if observation.RuntimeHint != nil {
		if err := validateOpaque("runtime host ID", observation.RuntimeHint.HostID, config.MaximumOpaqueLength, true); err != nil {
			return protocolError(CodeInvalidHookObservation, err)
		}
		if err := validateOpaque("runtime boot ID", string(observation.RuntimeHint.BootID), config.MaximumOpaqueLength, true); err != nil {
			return protocolError(CodeInvalidHookObservation, err)
		}
	}
	domain := observation.domain()
	if err := domain.Validate(); err != nil {
		return protocolError(CodeInvalidHookObservation, err)
	}
	now := canonicalTime(config.Clock.Now())
	observedAt := canonicalTime(observation.ObservedAt)
	if now.Sub(observedAt) > config.MaximumObservationAge {
		return protocolError(CodeStaleObservation, fmt.Errorf("stale observation"))
	}
	if observedAt.After(now.Add(config.AllowedFutureSkew)) {
		return protocolError(CodeInvalidHookObservation, fmt.Errorf("observation is too far in future"))
	}
	if observation.Revision > config.MaximumRevision {
		return protocolError(CodeInvalidHookObservation, fmt.Errorf("revision is outside supported range"))
	}
	return nil
}

func canonicalRequest(request Request) Request {
	request.Observation.ObservedAt = canonicalTime(request.Observation.ObservedAt)
	if request.Observation.ProcessHint != nil {
		request.Observation.ProcessHint.StartedAt = canonicalTime(request.Observation.ProcessHint.StartedAt)
	}
	if request.Observation.RuntimeHint != nil {
		request.Observation.RuntimeHint.RootProcess.StartedAt = canonicalTime(request.Observation.RuntimeHint.RootProcess.StartedAt)
	}
	return request
}

func durationBucket(duration time.Duration) string {
	switch {
	case duration < 10*time.Millisecond:
		return "lt_10ms"
	case duration < 100*time.Millisecond:
		return "lt_100ms"
	case duration < time.Second:
		return "lt_1s"
	default:
		return "gte_1s"
	}
}
