package localhooktransport

import (
	"testing"
	"time"
)

func TestDefaultConfigAndResourceBounds(t *testing.T) {
	clock := &testClock{now: testTime}
	config := DefaultConfig(clock)
	if err := config.Validate(false); err != nil {
		t.Fatalf("default config: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{name: "request bytes", mutate: func(value *Config) { value.MaximumRequestBytes = 0 }},
		{name: "response bytes", mutate: func(value *Config) { value.MaximumResponseBytes = 64*1024 + 1 }},
		{name: "concurrency", mutate: func(value *Config) { value.MaximumConcurrent = 0 }},
		{name: "read deadline", mutate: func(value *Config) { value.ReadDeadline = 0 }},
		{name: "write deadline", mutate: func(value *Config) { value.WriteDeadline = -time.Second }},
		{name: "handling deadline", mutate: func(value *Config) { value.MaximumHandlingTime = 0 }},
		{name: "runtime limit", mutate: func(value *Config) { value.MaximumRuntimes = 13 }},
		{name: "hook count", mutate: func(value *Config) { value.MaximumHooksPerRequest = 2 }},
		{name: "request ID limit", mutate: func(value *Config) { value.MaximumRequestIDLength = 7 }},
		{name: "opaque limit", mutate: func(value *Config) { value.MaximumOpaqueLength = 257 }},
		{name: "revision", mutate: func(value *Config) { value.MaximumRevision = 0 }},
		{name: "observation age", mutate: func(value *Config) { value.MaximumObservationAge = 0 }},
		{name: "future skew", mutate: func(value *Config) { value.AllowedFutureSkew = -1 }},
		{name: "replay capacity", mutate: func(value *Config) { value.ReplayCapacity = 0 }},
		{name: "replay TTL", mutate: func(value *Config) { value.ReplayTTL = 0 }},
		{name: "clock", mutate: func(value *Config) { value.Clock = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := config
			test.mutate(&invalid)
			if err := invalid.Validate(false); err == nil {
				t.Fatal("invalid config accepted")
			}
		})
	}
}
