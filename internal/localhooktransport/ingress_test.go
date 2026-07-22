package localhooktransport

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/hookadapter"
	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
)

func TestHookObservationFromIngressProjectsOnlyAllowlistedFields(t *testing.T) {
	ingress := hookadapter.Observation{
		Tool: instancepresence.ToolClaude, HookSessionRef: "session-a", ProducerEpoch: "epoch-a", Revision: 2,
		IdempotencyKey: "key-a", ObservedAt: time.Date(2026, 7, 22, 12, 0, 0, 0, time.FixedZone("local", 3600)), Lifecycle: instancecorrelation.LifecycleIdle,
	}
	observation, err := HookObservationFromIngress(ingress)
	if err != nil {
		t.Fatal(err)
	}
	if observation.ProcessHint != nil || observation.RuntimeHint != nil || observation.ParentOrRootPIDHint != nil || observation.HostID != "" || observation.BootID != "" || observation.ProcessGroupOrJob != "" || observation.OSSession != "" || observation.TerminalFingerprint != "" || observation.ObservedAt != ingress.ObservedAt.Round(0).UTC() {
		t.Fatalf("observation = %#v", observation)
	}
	data, err := json.Marshal(observation)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"cwd", "argv", "transcript", "metadata", "process_hint", "runtime_hint", "parent_or_root_pid_hint", "host_id", "boot_id", "process_group_or_job", "os_session", "terminal_fingerprint"} {
		if strings.Contains(string(data), forbidden) {
			t.Fatalf("sensitive field %q in %s", forbidden, data)
		}
	}
}
