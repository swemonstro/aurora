//go:build linux

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/swemonstro/aurora/internal/localhooktransport"
)

type fakeSender struct{}

func (fakeSender) Send(_ context.Context, request localhooktransport.Request) (localhooktransport.Response, error) {
	return localhooktransport.Response{
		ProtocolVersion: localhooktransport.CurrentProtocolVersion,
		RequestID:       request.RequestID, Status: localhooktransport.StatusOK,
		ErrorCodes: []localhooktransport.ErrorCode{}, Proposals: []localhooktransport.Proposal{},
		Ambiguous: []localhooktransport.Ambiguous{}, Rejected: []localhooktransport.Rejected{},
		UnmatchedHooks: []localhooktransport.Unmatched{}, UnmatchedRuntimes: []localhooktransport.Unmatched{},
		NoBindingPerformed: true,
	}, nil
}

func TestRunIsFiniteAndWritesSanitizedJSON(t *testing.T) {
	input := `{"protocol_version":1,"request_id":"request-01","operation":"correlate_observation","observation":{"tool":"claude","hook_session_ref":"session-fixture","producer_epoch":"epoch-fixture","revision":1,"observed_at":"2026-07-22T12:00:00Z","lifecycle":"active"}}`
	var stdout, stderr bytes.Buffer
	if err := run(nil, strings.NewReader(input), &stdout, &stderr, fakeSender{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), `"no_binding_performed":true`) {
		t.Fatalf("output = %s", stdout.String())
	}
	for _, forbidden := range []string{"pid", "host_id", "boot_id", "source", "provider", "profile"} {
		if strings.Contains(stdout.String(), `"`+forbidden+`"`) {
			t.Fatalf("output contains %q: %s", forbidden, stdout.String())
		}
	}
}

func TestRunRejectsMalformedOrForbiddenInput(t *testing.T) {
	for _, input := range []string{`{`, `{"uid":1000}`} {
		if err := run(nil, strings.NewReader(input), &bytes.Buffer{}, &bytes.Buffer{}, fakeSender{}); err == nil {
			t.Fatalf("input accepted: %s", input)
		}
	}
}
