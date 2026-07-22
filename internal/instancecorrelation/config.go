package instancecorrelation

import (
	"errors"
	"fmt"
	"time"
)

type Weights struct {
	ExactRuntimeIdentity  int
	RootProcessIdentity   int
	MemberProcessIdentity int
	HostMatch             int
	BootMatch             int
	ToolMatch             int
	ProcessGroupMatch     int
	OSSessionMatch        int
	TerminalMatch         int
	StartTimeClose        int
	ObservationTimeClose  int
	PIDOnlyHint           int
}

type Config struct {
	Weights               Weights
	MaximumStartTimeDelta time.Duration
	MaximumObservationAge time.Duration
	AllowedHookLead       time.Duration
	AmbiguousScoreDelta   int
	MinimumStrongScore    int
	MinimumWeakScore      int
	MaximumCandidateSize  int
}

func DefaultConfig() Config {
	return Config{
		Weights: Weights{
			ExactRuntimeIdentity:  1200,
			RootProcessIdentity:   1000,
			MemberProcessIdentity: 700,
			HostMatch:             20,
			BootMatch:             20,
			ToolMatch:             20,
			ProcessGroupMatch:     45,
			OSSessionMatch:        45,
			TerminalMatch:         15,
			StartTimeClose:        25,
			ObservationTimeClose:  15,
			PIDOnlyHint:           5,
		},
		MaximumStartTimeDelta: 30 * time.Second,
		MaximumObservationAge: 2 * time.Minute,
		AllowedHookLead:       2 * time.Second,
		AmbiguousScoreDelta:   10,
		MinimumStrongScore:    500,
		MinimumWeakScore:      70,
		MaximumCandidateSize:  12,
	}
}

func (config Config) Validate() error {
	if config.MaximumStartTimeDelta < 0 || config.MaximumObservationAge <= 0 || config.AllowedHookLead < 0 {
		return errors.New("correlation time bounds must be non-negative and observation age must be positive")
	}
	if config.AmbiguousScoreDelta < 0 {
		return errors.New("ambiguous score delta must not be negative")
	}
	if config.MinimumWeakScore <= 0 || config.MinimumStrongScore <= config.MinimumWeakScore {
		return errors.New("strong score must be greater than positive weak score")
	}
	if config.MaximumCandidateSize < 1 || config.MaximumCandidateSize > 12 {
		return errors.New("maximum candidate size must be between 1 and 12")
	}
	weights := []struct {
		name  string
		value int
	}{
		{"exact runtime identity", config.Weights.ExactRuntimeIdentity},
		{"root process identity", config.Weights.RootProcessIdentity},
		{"member process identity", config.Weights.MemberProcessIdentity},
		{"host match", config.Weights.HostMatch},
		{"boot match", config.Weights.BootMatch},
		{"tool match", config.Weights.ToolMatch},
		{"process group match", config.Weights.ProcessGroupMatch},
		{"OS session match", config.Weights.OSSessionMatch},
		{"terminal match", config.Weights.TerminalMatch},
		{"start time close", config.Weights.StartTimeClose},
		{"observation time close", config.Weights.ObservationTimeClose},
		{"PID-only hint", config.Weights.PIDOnlyHint},
	}
	for _, weight := range weights {
		if weight.value < 0 {
			return fmt.Errorf("%s weight must not be negative", weight.name)
		}
	}

	standaloneInsufficient := []struct {
		name  string
		value int
	}{
		{"tool match", config.Weights.ToolMatch},
		{"terminal match", config.Weights.TerminalMatch},
		{"start time close", config.Weights.StartTimeClose},
		{"observation time close", config.Weights.ObservationTimeClose},
		{"PID-only hint", config.Weights.PIDOnlyHint},
	}
	for _, weight := range standaloneInsufficient {
		if weight.value >= config.MinimumWeakScore {
			return fmt.Errorf("%s weight must remain below the weak threshold", weight.name)
		}
	}

	hardPositive := []struct {
		name  string
		value int
	}{
		{"exact runtime identity", config.Weights.ExactRuntimeIdentity},
		{"root process identity", config.Weights.RootProcessIdentity},
		{"member process identity", config.Weights.MemberProcessIdentity},
	}
	for _, weight := range hardPositive {
		if weight.value < config.MinimumStrongScore {
			return fmt.Errorf("hard-positive %s weight must reach the strong threshold", weight.name)
		}
	}

	softWeights := []int{
		config.Weights.HostMatch,
		config.Weights.BootMatch,
		config.Weights.ToolMatch,
		config.Weights.ProcessGroupMatch,
		config.Weights.OSSessionMatch,
		config.Weights.TerminalMatch,
		config.Weights.StartTimeClose,
		config.Weights.ObservationTimeClose,
		config.Weights.PIDOnlyHint,
	}
	remainingBeforeStrong := config.MinimumStrongScore
	for _, weight := range softWeights {
		// Comparing against the remaining budget avoids integer overflow while
		// enforcing the strict softMaximum < MinimumStrongScore invariant.
		if weight >= remainingBeforeStrong {
			return errors.New("maximum soft-signal score must remain below the strong threshold")
		}
		remainingBeforeStrong -= weight
	}
	return nil
}
