package instancecorrelation

import (
	"strings"
	"testing"
)

func TestConfigRejectsStandaloneSignalAtWeakThreshold(t *testing.T) {
	tests := []struct {
		name   string
		set    func(*Weights, int)
		reason string
	}{
		{name: "tool", set: func(weights *Weights, value int) { weights.ToolMatch = value }, reason: "tool match"},
		{name: "terminal", set: func(weights *Weights, value int) { weights.TerminalMatch = value }, reason: "terminal match"},
		{name: "start time", set: func(weights *Weights, value int) { weights.StartTimeClose = value }, reason: "start time close"},
		{name: "observation time", set: func(weights *Weights, value int) { weights.ObservationTimeClose = value }, reason: "observation time close"},
		{name: "PID only", set: func(weights *Weights, value int) { weights.PIDOnlyHint = value }, reason: "PID-only hint"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			config := DefaultConfig()
			test.set(&config.Weights, config.MinimumWeakScore)
			err := config.Validate()
			if err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("Validate() error = %v, want standalone %s rejection", err, test.reason)
			}
		})
	}
}

func TestConfigRejectsAllSoftSignalsReachingStrongThreshold(t *testing.T) {
	config := DefaultConfig()
	config.MinimumStrongScore = softMaximum(config.Weights)
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "soft-signal") {
		t.Fatalf("Validate() error = %v, want soft maximum rejection", err)
	}
}

func TestConfigAcceptsMultipleSoftSignalsReachingWeak(t *testing.T) {
	config := DefaultConfig()
	combined := config.Weights.ProcessGroupMatch + config.Weights.OSSessionMatch
	if combined < config.MinimumWeakScore {
		t.Fatalf("fixture soft score = %d, want at least weak %d", combined, config.MinimumWeakScore)
	}
	if err := config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestDefaultConfigIsValidAndHardIdentitiesReachStrong(t *testing.T) {
	config := DefaultConfig()
	if err := config.Validate(); err != nil {
		t.Fatalf("DefaultConfig().Validate() error = %v", err)
	}
	hardPositive := map[string]int{
		"exact runtime":  config.Weights.ExactRuntimeIdentity,
		"root process":   config.Weights.RootProcessIdentity,
		"member process": config.Weights.MemberProcessIdentity,
	}
	for name, weight := range hardPositive {
		if weight < config.MinimumStrongScore {
			t.Errorf("%s weight = %d, want at least strong %d", name, weight, config.MinimumStrongScore)
		}
	}
}

func softMaximum(weights Weights) int {
	return weights.HostMatch + weights.BootMatch + weights.ToolMatch +
		weights.ProcessGroupMatch + weights.OSSessionMatch + weights.TerminalMatch +
		weights.StartTimeClose + weights.ObservationTimeClose + weights.PIDOnlyHint
}
