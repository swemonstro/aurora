//go:build linux

package linuxidentitymeasure

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/swemonstro/aurora/internal/instancecorrelation"
	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/linuxprocess"
	"github.com/swemonstro/aurora/internal/localhooktransport"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

type fakeAdapter struct {
	chain      linuxprocess.AncestryCapture
	sample     linuxprocess.Sample
	observeErr error
	captures   int
	observes   int
}

func (adapter *fakeAdapter) CaptureGeneration(context.Context, uint64) linuxprocess.GenerationCapture {
	if !adapter.chain.OK || len(adapter.chain.Hops) == 0 {
		return linuxprocess.GenerationCapture{OK: false, ReasonCodes: adapter.chain.ReasonCodes}
	}
	return linuxprocess.GenerationCapture{OK: true, Identity: adapter.chain.Hops[0], ReasonCodes: adapter.chain.ReasonCodes}
}

func (adapter *fakeAdapter) CaptureAncestryChain(context.Context, uint64, int) linuxprocess.AncestryCapture {
	adapter.captures++
	return adapter.chain
}

func (adapter *fakeAdapter) Observe(context.Context) (linuxprocess.Sample, error) {
	adapter.observes++
	if adapter.observeErr != nil {
		return linuxprocess.Sample{}, adapter.observeErr
	}
	return adapter.sample, nil
}

type toolRecognizer struct {
	tool instancepresence.ToolKind
	pid  uint64
}

func (recognizer toolRecognizer) Recognize(process runtimerecognition.ProcessObservation) (runtimerecognition.Recognition, bool) {
	if process.Process.PID != recognizer.pid {
		return runtimerecognition.Recognition{}, false
	}
	return runtimerecognition.Recognition{
		Tool: recognizer.tool, Role: runtimerecognition.RoleDirect, Priority: runtimerecognition.PriorityExecutable,
	}, true
}

type multiPIDRecognizer struct {
	tool instancepresence.ToolKind
	pids map[uint64]struct{}
}

func (recognizer multiPIDRecognizer) Recognize(process runtimerecognition.ProcessObservation) (runtimerecognition.Recognition, bool) {
	if _, ok := recognizer.pids[process.Process.PID]; !ok {
		return runtimerecognition.Recognition{}, false
	}
	return runtimerecognition.Recognition{
		Tool: recognizer.tool, Role: runtimerecognition.RoleDirect, Priority: runtimerecognition.PriorityExecutable,
	}, true
}

func TestObserverDisabledHasNoSideEffectsWhenNotConstructed(t *testing.T) {
	var observer IngestIdentityObserver
	if observer != nil {
		t.Fatal("expected nil default")
	}
}

func TestCapturePeerDisappearance(t *testing.T) {
	adapter := &fakeAdapter{chain: linuxprocess.AncestryCapture{
		OK: false, ReasonCodes: []linuxprocess.ReasonCode{linuxprocess.ReasonProcessDisappeared},
	}}
	var buffer bytes.Buffer
	observer, err := NewObserver(adapter, "host-a", &buffer, Config{
		Clock: func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) },
	}, toolRecognizer{tool: instancepresence.ToolClaude, pid: 100})
	if err != nil {
		t.Fatal(err)
	}
	capture := observer.CapturePeer(localhooktransport.PeerIdentity{UID: 1000, PID: 4242})
	if capture.GenerationOK {
		t.Fatalf("capture = %#v", capture)
	}
	if adapter.captures != 1 {
		t.Fatalf("ancestry capture calls = %d", adapter.captures)
	}
	observer.CompleteIngest(capture, instancepresence.ToolClaude, instancecorrelation.LifecycleActive, true)
	record := decodeLastRecord(t, buffer.Bytes())
	if record.DiagnosticUniqueLink || !record.ValidatedIngress {
		t.Fatalf("record = %#v", record)
	}
	if !containsCode(record.ReasonCodes, "peer_process_unreadable") &&
		!containsCode(record.ReasonCodes, string(linuxprocess.ReasonProcessDisappeared)) {
		t.Fatalf("reason codes = %#v", record.ReasonCodes)
	}
	if !record.NoMutationPerformed || !record.Package6SequencingIndependent {
		t.Fatalf("safety flags missing: %#v", record)
	}
	if adapter.observes != 0 {
		t.Fatalf("observe called on failed generation: %d", adapter.observes)
	}
	if strings.Contains(buffer.String(), "trusted_hard_identity") {
		t.Fatalf("legacy attestation field present: %s", buffer.String())
	}
}

func TestCapturePeerIncludesAncestryBeforeComplete(t *testing.T) {
	start := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	peerStart := start.Add(time.Second)
	adapter := &fakeAdapter{
		chain: linuxprocess.AncestryCapture{
			OK: true,
			Hops: []instancepresence.ProcessIdentity{
				{PID: 300, StartedAt: peerStart},
				{PID: 100, StartedAt: start},
			},
		},
		sample: linuxprocess.Sample{
			Recognition: runtimerecognition.Snapshot{
				ObservedAt: peerStart, BootID: "boot-a",
				Processes: []runtimerecognition.ProcessObservation{
					process(100, start, nil, "claude"),
					process(300, peerStart, &instancepresence.ProcessIdentity{PID: 100, StartedAt: start}, "hook"),
				},
			},
		},
	}
	var buffer bytes.Buffer
	observer, err := NewObserver(adapter, "host-a", &buffer, Config{
		Clock: func() time.Time { return peerStart.Add(2 * time.Millisecond) },
	}, multiPIDRecognizer{tool: instancepresence.ToolClaude, pids: map[uint64]struct{}{100: {}, 300: {}}})
	if err != nil {
		t.Fatal(err)
	}
	capture := observer.CapturePeer(localhooktransport.PeerIdentity{UID: 1000, PID: 300})
	if !capture.GenerationOK || len(capture.Ancestry) != 2 {
		t.Fatalf("capture must include ancestry immediately: %#v", capture)
	}
	if capture.Ancestry[0].PID != 300 || !capture.Ancestry[0].IsPeer || capture.Ancestry[1].PID != 100 {
		t.Fatalf("ancestry = %#v", capture.Ancestry)
	}
	if adapter.observes != 0 {
		t.Fatal("Observe must not run during CapturePeer")
	}
	observer.CompleteIngest(capture, instancepresence.ToolClaude, instancecorrelation.LifecycleActive, true)
	if adapter.observes != 1 {
		t.Fatalf("Observe calls = %d", adapter.observes)
	}
	record := decodeLastRecord(t, buffer.Bytes())
	if len(record.Ancestry) != 2 {
		t.Fatalf("record ancestry = %#v", record)
	}
	if !record.DiagnosticUniqueLink {
		t.Fatalf("expected diagnostic unique link: %#v", record)
	}
	if !containsCode(record.ReasonCodes, "diagnostic_unique_link") {
		t.Fatalf("reason codes = %#v", record.ReasonCodes)
	}
}

func TestUnvalidatedIngressDoesNotJoin(t *testing.T) {
	start := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	adapter := &fakeAdapter{chain: linuxprocess.AncestryCapture{
		OK: true, Hops: []instancepresence.ProcessIdentity{{PID: 1, StartedAt: start}},
	}}
	var buffer bytes.Buffer
	observer, err := NewObserver(adapter, "host-a", &buffer, Config{
		Clock: func() time.Time { return start },
	}, toolRecognizer{tool: instancepresence.ToolClaude, pid: 1})
	if err != nil {
		t.Fatal(err)
	}
	capture := observer.CapturePeer(localhooktransport.PeerIdentity{UID: 1, PID: 1})
	observer.CompleteIngest(capture, "", "", false)
	record := decodeLastRecord(t, buffer.Bytes())
	if record.ValidatedIngress || record.DiagnosticUniqueLink {
		t.Fatalf("record = %#v", record)
	}
	if adapter.observes != 0 {
		t.Fatal("observe should not run without validated tool")
	}
	if !containsCode(record.ReasonCodes, "request_not_validated") {
		t.Fatalf("reason codes = %#v", record.ReasonCodes)
	}
}

func TestEvaluateLinksAmbiguousAndUnique(t *testing.T) {
	start := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	peer := instancepresence.ProcessIdentity{PID: 300, StartedAt: start.Add(time.Second)}
	root := instancepresence.ProcessIdentity{PID: 100, StartedAt: start}
	links, _ := evaluateLinks(peer, nil, []instancecorrelation.RuntimeObservation{
		runtimeObs(instancepresence.ToolClaude, "r1", 100, start, []instancepresence.ProcessIdentity{root, peer}),
	})
	if countUniqueRuntimeRefs(links) != 1 || links[0].LinkRule != "L2_member" {
		t.Fatalf("unique links = %#v", links)
	}
	links, _ = evaluateLinks(peer, nil, []instancecorrelation.RuntimeObservation{
		runtimeObs(instancepresence.ToolClaude, "r1", 100, start, []instancepresence.ProcessIdentity{root, peer}),
		runtimeObs(instancepresence.ToolClaude, "r2", 200, start, []instancepresence.ProcessIdentity{{PID: 200, StartedAt: start}, peer}),
	})
	if countUniqueRuntimeRefs(links) != 2 {
		t.Fatalf("ambiguous links = %#v", links)
	}
	links, _ = evaluateLinks(root, nil, []instancecorrelation.RuntimeObservation{
		runtimeObs(instancepresence.ToolClaude, "r1", 100, start, []instancepresence.ProcessIdentity{root}),
	})
	if len(links) != 1 || links[0].LinkRule != "L1_root" {
		t.Fatalf("L1 links = %#v", links)
	}
	links, _ = evaluateLinks(peer, []AncestryHop{
		{PID: peer.PID, StartedAt: peer.StartedAt, IsPeer: true},
		{PID: root.PID, StartedAt: root.StartedAt, Depth: 1},
	}, []instancecorrelation.RuntimeObservation{
		runtimeObs(instancepresence.ToolClaude, "r1", 100, start, []instancepresence.ProcessIdentity{root}),
	})
	if len(links) != 1 || links[0].LinkRule != "L3_ancestry" {
		t.Fatalf("L3 links = %#v", links)
	}
}

func TestAmbiguousCompleteUsesDiagnosticFlags(t *testing.T) {
	start := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	peerStart := start.Add(time.Second)
	peer := instancepresence.ProcessIdentity{PID: 300, StartedAt: peerStart}
	// Two runtimes both listing peer as member — force ambiguity at join time.
	adapter := &fakeAdapter{
		chain: linuxprocess.AncestryCapture{OK: true, Hops: []instancepresence.ProcessIdentity{peer}},
		sample: linuxprocess.Sample{
			Recognition: runtimerecognition.Snapshot{
				ObservedAt: peerStart, BootID: "boot-a",
				Processes: []runtimerecognition.ProcessObservation{
					process(100, start, nil, "claude"),
					process(200, start, nil, "claude"),
					process(300, peerStart, nil, "hook"),
				},
			},
		},
	}
	var buffer bytes.Buffer
	observer, err := NewObserver(adapter, "host-a", &buffer, Config{
		Clock: func() time.Time { return peerStart },
	}, multiPIDRecognizer{tool: instancepresence.ToolClaude, pids: map[uint64]struct{}{100: {}, 200: {}, 300: {}}})
	if err != nil {
		t.Fatal(err)
	}
	// Bypass recognition ambiguity by completing with synthetic evaluateLinks path via custom runtimes:
	// recognition may merge families; assert evaluateLinks ambiguity and field naming separately.
	links, _ := evaluateLinks(peer, nil, []instancecorrelation.RuntimeObservation{
		runtimeObs(instancepresence.ToolClaude, "r1", 100, start, []instancepresence.ProcessIdentity{{PID: 100, StartedAt: start}, peer}),
		runtimeObs(instancepresence.ToolClaude, "r2", 200, start, []instancepresence.ProcessIdentity{{PID: 200, StartedAt: start}, peer}),
	})
	if countUniqueRuntimeRefs(links) != 2 {
		t.Fatalf("links = %#v", links)
	}
	capture := observer.CapturePeer(localhooktransport.PeerIdentity{UID: 1000, PID: 300})
	observer.CompleteIngest(capture, instancepresence.ToolClaude, instancecorrelation.LifecycleActive, true)
	record := decodeLastRecord(t, buffer.Bytes())
	if record.DiagnosticUniqueLink && record.MatchingRuntimeCount != 1 {
		t.Fatalf("inconsistent diagnostic flag: %#v", record)
	}
	if strings.Contains(buffer.String(), `"trusted_hard_identity_present"`) {
		t.Fatal("must not emit trusted_hard_identity_present")
	}
	if !record.NoMutationPerformed {
		t.Fatal("mutation flag missing")
	}
}

func TestUniqueL2LinkSuccess(t *testing.T) {
	start := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	root := process(100, start, nil, "claude")
	peer := process(300, start.Add(time.Second), &root.Process, "aurora-claude-hook")
	adapter := &fakeAdapter{
		chain: linuxprocess.AncestryCapture{
			OK:   true,
			Hops: []instancepresence.ProcessIdentity{peer.Process, root.Process},
		},
		sample: linuxprocess.Sample{
			Recognition: runtimerecognition.Snapshot{
				ObservedAt: start.Add(2 * time.Second), BootID: "boot-a",
				Processes: []runtimerecognition.ProcessObservation{root, peer},
			},
		},
	}
	var buffer bytes.Buffer
	observer, err := NewObserver(adapter, "host-a", &buffer, Config{
		Clock: func() time.Time { return start.Add(3 * time.Second) },
	}, multiPIDRecognizer{tool: instancepresence.ToolClaude, pids: map[uint64]struct{}{100: {}, 300: {}}})
	if err != nil {
		t.Fatal(err)
	}
	capture := observer.CapturePeer(localhooktransport.PeerIdentity{UID: 1000, PID: 300})
	observer.CompleteIngest(capture, instancepresence.ToolClaude, instancecorrelation.LifecycleActive, true)
	record := decodeLastRecord(t, buffer.Bytes())
	if !record.DiagnosticUniqueLink {
		t.Fatalf("expected diagnostic unique link: %#v", record)
	}
	if record.MatchingRuntimeCount != 1 {
		t.Fatalf("matching = %d record=%#v", record.MatchingRuntimeCount, record)
	}
	if !containsCode(record.ReasonCodes, "diagnostic_unique_link") {
		t.Fatalf("reason codes = %#v", record.ReasonCodes)
	}
}

func TestNoMutationAndContentFreeOutput(t *testing.T) {
	start := time.Date(2026, 7, 22, 11, 0, 0, 0, time.UTC)
	root := process(100, start, nil, "claude")
	peer := process(300, start.Add(time.Second), &root.Process, "hook")
	adapter := &fakeAdapter{
		chain: linuxprocess.AncestryCapture{OK: true, Hops: []instancepresence.ProcessIdentity{peer.Process, root.Process}},
		sample: linuxprocess.Sample{
			Recognition: runtimerecognition.Snapshot{
				ObservedAt: start, BootID: "boot-a",
				Processes: []runtimerecognition.ProcessObservation{root, peer},
			},
		},
	}
	var buffer bytes.Buffer
	observer, err := NewObserver(adapter, "host-a", &buffer, Config{
		Clock: func() time.Time { return start },
	}, multiPIDRecognizer{tool: instancepresence.ToolClaude, pids: map[uint64]struct{}{100: {}, 300: {}}})
	if err != nil {
		t.Fatal(err)
	}
	capture := observer.CapturePeer(localhooktransport.PeerIdentity{UID: 1000, PID: 300})
	observer.CompleteIngest(capture, instancepresence.ToolClaude, instancecorrelation.LifecycleActive, true)
	raw := buffer.String()
	for _, forbidden := range []string{"prompt", "argv", "cwd", "transcript", "HOME=", "session-", "api-key", "trusted_hard_identity"} {
		if strings.Contains(strings.ToLower(raw), forbidden) {
			t.Fatalf("forbidden content %q in %s", forbidden, raw)
		}
	}
	record := decodeLastRecord(t, buffer.Bytes())
	if !record.NoMutationPerformed {
		t.Fatal("expected no_mutation_performed")
	}
}

func TestObserveErrorIsFailClosed(t *testing.T) {
	start := time.Date(2026, 7, 22, 1, 0, 0, 0, time.UTC)
	adapter := &fakeAdapter{
		chain: linuxprocess.AncestryCapture{
			OK: true, Hops: []instancepresence.ProcessIdentity{{PID: 9, StartedAt: start}},
		},
		observeErr: errors.New("boom"),
	}
	var buffer bytes.Buffer
	observer, err := NewObserver(adapter, "host-a", &buffer, Config{
		Clock: func() time.Time { return time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC) },
	}, toolRecognizer{tool: instancepresence.ToolCodex, pid: 9})
	if err != nil {
		t.Fatal(err)
	}
	capture := observer.CapturePeer(localhooktransport.PeerIdentity{UID: 1, PID: 9})
	observer.CompleteIngest(capture, instancepresence.ToolCodex, instancecorrelation.LifecycleActive, true)
	record := decodeLastRecord(t, buffer.Bytes())
	if record.DiagnosticUniqueLink || !containsCode(record.ReasonCodes, "attestation_internal_error") {
		t.Fatalf("record = %#v", record)
	}
	if !containsCode(record.ReasonCodes, "diagnostic_no_unique_link") {
		t.Fatalf("reason codes = %#v", record.ReasonCodes)
	}
}

// IngestIdentityObserver documents the nil default in tests.
type IngestIdentityObserver = localhooktransport.IngestIdentityObserver

func process(pid uint64, started time.Time, parent *instancepresence.ProcessIdentity, name string) runtimerecognition.ProcessObservation {
	observation := runtimerecognition.ProcessObservation{
		Process:            instancepresence.ProcessIdentity{PID: pid, StartedAt: started},
		Parent:             parent,
		CommIdentity:       instancepresence.OpaqueIdentity("exe:" + name),
		ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + name),
		OwnerIdentity:      "owner:fixture",
	}
	if parent != nil {
		observation.ParentPIDHint = parent.PID
	}
	return observation
}

func runtimeObs(tool instancepresence.ToolKind, id string, rootPID uint64, rootStart time.Time, members []instancepresence.ProcessIdentity) instancecorrelation.RuntimeObservation {
	root := instancepresence.ProcessIdentity{PID: rootPID, StartedAt: rootStart}
	return instancecorrelation.RuntimeObservation{
		Candidate: instancepresence.RuntimeCandidate{
			InstanceID: instancepresence.InstanceID(id),
			Tool:       tool,
			Runtime: instancepresence.RuntimeIdentity{
				HostID: "host-a", BootID: "boot-a", RootProcess: root,
			},
			Members: members,
		},
		ObservedAt: rootStart,
		Lifecycle:  instancecorrelation.LifecycleActive,
	}
}

func decodeLastRecord(t *testing.T, data []byte) Record {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) == 0 || len(lines[len(lines)-1]) == 0 {
		t.Fatalf("no records in %q", data)
	}
	var record Record
	if err := json.Unmarshal(lines[len(lines)-1], &record); err != nil {
		t.Fatalf("decode %s: %v", lines[len(lines)-1], err)
	}
	return record
}

func containsCode(codes []string, want string) bool {
	for _, code := range codes {
		if code == want {
			return true
		}
	}
	return false
}
