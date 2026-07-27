package grokpresence

import (
	"testing"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

func TestToolGrokValidates(t *testing.T) {
	if err := instancepresence.ToolGrok.Validate(); err != nil {
		t.Fatalf("ToolGrok.Validate() error = %v", err)
	}
}

func TestRuntimeRecognizerRecognizesGrok(t *testing.T) {
	tests := []runtimerecognition.ProcessObservation{
		{ExecutableIdentity: "exe:grok"},
		{CommIdentity: "exe:grok"},
		{ExecutableIdentity: "exe:GROK"},
		// Official versioned download basename (executable identity).
		{ExecutableIdentity: "exe:grok-0.2.112-linux-x86_64"},
		// Real Linux comm truncation of grok-0.2.112-linux-x86_64 (15 chars).
		{CommIdentity: "exe:grok-0.2.112-li"},
		// Mixed case via existing ToLower normalization.
		{ExecutableIdentity: "exe:Grok-0.2.112-Linux-x86_64"},
		{CommIdentity: "exe:Grok-0.2.112-Li"},
		// Official unversioned local download basename.
		{ExecutableIdentity: "exe:grok-linux-x86_64"},
		// Linux comm truncation of grok-linux-x86_64.
		{CommIdentity: "exe:grok-linux-x86_"},
	}

	for _, process := range tests {
		recognition, recognized := RuntimeRecognizer().Recognize(process)
		if !recognized {
			t.Fatalf("process %#v was not recognized", process)
		}
		if recognition.Tool != instancepresence.ToolGrok ||
			recognition.Role != runtimerecognition.RoleDirect ||
			recognition.Priority != runtimerecognition.PriorityExecutable {
			t.Fatalf("recognition = %#v", recognition)
		}
	}
}

func TestRuntimeRecognizerRejectsTruncationAsExecutableIdentity(t *testing.T) {
	// Truncated / mid-version forms may be valid Linux comm only.
	// They must never be accepted as full executable basenames.
	for _, name := range []string{
		"grok-0.2.112-li",
		"grok-linux-x86_",
		"grok-1234567890",
	} {
		process := runtimerecognition.ProcessObservation{
			ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + name),
		}
		if recognition, recognized := RuntimeRecognizer().Recognize(process); recognized {
			t.Fatalf("executable %q recognized as %#v", name, recognition)
		}
	}
}

func TestRuntimeRecognizerAcceptsTruncationAsCommIdentity(t *testing.T) {
	for _, name := range []string{
		"grok-0.2.112-li",
		"grok-linux-x86_",
	} {
		process := runtimerecognition.ProcessObservation{
			CommIdentity: instancepresence.OpaqueIdentity("exe:" + name),
		}
		recognition, recognized := RuntimeRecognizer().Recognize(process)
		if !recognized {
			t.Fatalf("comm %q was not recognized", name)
		}
		if recognition.Tool != instancepresence.ToolGrok ||
			recognition.Role != runtimerecognition.RoleDirect ||
			recognition.Priority != runtimerecognition.PriorityExecutable {
			t.Fatalf("comm %q recognition = %#v", name, recognition)
		}
	}
}

func TestRuntimeRecognizerRejectsIncompleteVersionComm(t *testing.T) {
	// Names that end before a complete version+platform-prefix form.
	// These must never match the truncated versioned comm rule.
	for _, name := range []string{
		"grok-1234567890",  // 15 chars, one version component only
		"grok-0.123456789", // 16 chars, two version components only
		"grok-0.2.123456",  // 15 chars, three components but no platform prefix
	} {
		process := runtimerecognition.ProcessObservation{
			CommIdentity: instancepresence.OpaqueIdentity("exe:" + name),
		}
		if recognition, recognized := RuntimeRecognizer().Recognize(process); recognized {
			t.Fatalf("comm %q recognized as %#v", name, recognition)
		}
	}
}

func TestRuntimeRecognizerRejectsLookalikes(t *testing.T) {
	for _, name := range []string{
		"grok-helper",
		"my-grok",
		"grok-server",
		"not-grok",
		"grok-foo-linux-x86_64",
		"grok-0.2-linux-x86_64",
		"grok-0.2.112-linux-x86_64-helper",
		"grok-",
		"grok-0.2.112",
		"grok-0.2.112-linux",
		"grok-0.2.112-linux-x86",
		"grok-0.2.112-linux-arm64",
		"grok-0.2.112-darwin-x86_64",
		"grok-01.02.03-linux-x86_64-extra",
		"notgrok-0.2.112-linux-x86_64",
		// 15-char lookalikes that must not pass the truncated-comm path.
		"grok-0.2.112-lx", // wrong platform prefix
		"grok-helper-xxxx",
		"grok-server-xxxx",
		"grok-foo-bar-baz",
		// Incomplete version tails (must not be treated as truncated official).
		"grok-1234567890",
		"grok-0.123456789",
		"grok-0.2.123456",
	} {
		process := runtimerecognition.ProcessObservation{
			ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + name),
		}
		if recognition, recognized := RuntimeRecognizer().Recognize(process); recognized {
			t.Fatalf("%q recognized as %#v", name, recognition)
		}
		process = runtimerecognition.ProcessObservation{
			CommIdentity: instancepresence.OpaqueIdentity("exe:" + name),
		}
		if recognition, recognized := RuntimeRecognizer().Recognize(process); recognized {
			t.Fatalf("comm %q recognized as %#v", name, recognition)
		}
	}
}

func TestIsAllowedGrokExecutableName(t *testing.T) {
	allowed := []string{
		"grok",
		"grok-0.2.112-linux-x86_64",
		"grok-0.2.111-linux-x86_64",
		"grok-linux-x86_64",
	}
	for _, name := range allowed {
		if !isAllowedGrokExecutableName(name) {
			t.Fatalf("%q should be allowed as executable", name)
		}
	}

	// Truncated / incomplete forms are never full executable basenames.
	rejected := []string{
		"grok-helper",
		"my-grok",
		"grok-server",
		"not-grok",
		"grok-foo-linux-x86_64",
		"grok-0.2-linux-x86_64",
		"grok-0.2.112-linux-x86_64-helper",
		"grok-0.2.112-li",
		"grok-linux-x86_",
		"grok-1234567890",
		"grok-0.2.112-liX",
		"grok-0.2.112-lx",
	}
	for _, name := range rejected {
		if isAllowedGrokExecutableName(name) {
			t.Fatalf("%q should be rejected as executable", name)
		}
	}
}

func TestIsAllowedGrokCommName(t *testing.T) {
	allowed := []string{
		"grok",
		"grok-0.2.112-linux-x86_64",
		"grok-0.2.111-linux-x86_64",
		"grok-linux-x86_64",
		// 15-char Linux truncations of official long forms only.
		"grok-0.2.112-li",
		"grok-linux-x86_",
	}
	for _, name := range allowed {
		if !isAllowedGrokCommName(name) {
			t.Fatalf("%q should be allowed as comm", name)
		}
	}

	rejected := []string{
		"grok-helper",
		"my-grok",
		"grok-server",
		"not-grok",
		"grok-foo-linux-x86_64",
		"grok-0.2-linux-x86_64",
		"grok-0.2.112-linux-x86_64-helper",
		"grok-0.2.112-liX", // 16 chars: not full, not comm-length
		"grok-0.2.112-lx",
		"grok-helper-xxxx",
		"grok-server-xxxx",
		"grok-foo-bar-baz",
		// Incomplete version tails (15 chars) must not match truncated form.
		"grok-1234567890",
		"grok-0.123456789",
		"grok-0.2.123456",
	}
	for _, name := range rejected {
		if isAllowedGrokCommName(name) {
			t.Fatalf("%q should be rejected as comm", name)
		}
	}
}

func TestIsOfficialVersionedGrokLinuxAMD64(t *testing.T) {
	if !isOfficialVersionedGrokLinuxAMD64("grok-0.2.112-linux-x86_64") {
		t.Fatal("expected versioned official name to match")
	}
	for _, name := range []string{
		"grok",
		"grok-linux-x86_64",
		"grok-0.2-linux-x86_64",
		"grok-0.2.112-li",
		"grok-foo-linux-x86_64",
		"grok-0.2.112-linux-x86_64-helper",
	} {
		if isOfficialVersionedGrokLinuxAMD64(name) {
			t.Fatalf("%q should not be a full versioned official name", name)
		}
	}
}

func TestIsTruncatedVersionedGrokLinuxAMD64Comm(t *testing.T) {
	// Observed real truncation of grok-0.2.112-linux-x86_64.
	if !isTruncatedVersionedGrokLinuxAMD64Comm("grok-0.2.112-li") {
		t.Fatal("expected truncated versioned comm to match")
	}
	// Complete three-part version plus partial platform within 15 chars.
	if !isTruncatedVersionedGrokLinuxAMD64Comm("grok-1.0.0-linu") {
		t.Fatal("expected mid-platform truncation to match")
	}
	for _, name := range []string{
		"grok-helper",
		"grok-0.2.112-lx",
		"grok-foo-linux-x",
		"grok-0.2-linux-x8",
		"not-grok-0.2.112",
		// Incomplete version: ends before all three components + platform prefix.
		"grok-1234567890",
		"grok-0.123456789",
		"grok-0.2.123456",
		"grok-0.12345678", // 15 chars, only two version components
		// Wrong length / full official name handled elsewhere.
		"grok-0.2.112-linux-x86_64",
		"grok-linux-x86_",
	} {
		if isTruncatedVersionedGrokLinuxAMD64Comm(name) {
			t.Fatalf("%q should not be a truncated versioned comm", name)
		}
	}
}
