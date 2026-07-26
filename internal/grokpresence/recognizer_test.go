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

func TestRuntimeRecognizerRejectsLookalikes(t *testing.T) {
	for _, name := range []string{
		"grok-helper",
		"my-grok",
		"grok-server",
		"not-grok",
	} {
		process := runtimerecognition.ProcessObservation{
			ExecutableIdentity: instancepresence.OpaqueIdentity("exe:" + name),
		}
		if recognition, recognized := RuntimeRecognizer().Recognize(process); recognized {
			t.Fatalf("%q recognized as %#v", name, recognition)
		}
	}
}
