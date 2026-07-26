// Package grokpresence contains the Grok-specific runtime adapter.
package grokpresence

import (
	"strings"

	"github.com/swemonstro/aurora/internal/instancepresence"
	"github.com/swemonstro/aurora/internal/runtimerecognition"
)

// RuntimeRecognizer recognizes the interactive Grok CLI process.
func RuntimeRecognizer() runtimerecognition.AgentRuntimeRecognizer {
	return runtimeRecognizer{}
}

type runtimeRecognizer struct{}

func (runtimeRecognizer) Recognize(
	process runtimerecognition.ProcessObservation,
) (runtimerecognition.Recognition, bool) {
	for _, identity := range []instancepresence.OpaqueIdentity{
		process.ExecutableIdentity,
		process.CommIdentity,
	} {
		name := strings.TrimPrefix(strings.ToLower(string(identity)), "exe:")
		if name == "grok" {
			return runtimerecognition.Recognition{
				Tool:     instancepresence.ToolGrok,
				Role:     runtimerecognition.RoleDirect,
				Priority: runtimerecognition.PriorityExecutable,
			}, true
		}
	}

	return runtimerecognition.Recognition{}, false
}
