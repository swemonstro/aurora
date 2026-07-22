package localhooktransport

import (
	"errors"
	"strings"
	"time"
)

type Config struct {
	SocketPath             string
	MaximumRequestBytes    uint32
	MaximumResponseBytes   uint32
	MaximumConcurrent      int
	ReadDeadline           time.Duration
	WriteDeadline          time.Duration
	MaximumHandlingTime    time.Duration
	MaximumRuntimes        int
	MaximumHooksPerRequest int
	MaximumRequestIDLength int
	MaximumOpaqueLength    int
	MaximumRevision        uint64
	MaximumObservationAge  time.Duration
	AllowedFutureSkew      time.Duration
	ReplayCapacity         int
	ReplayTTL              time.Duration
	Clock                  Clock
}

func DefaultConfig(clock Clock) Config {
	return Config{
		MaximumRequestBytes: 64 * 1024, MaximumResponseBytes: 64 * 1024,
		MaximumConcurrent: 8, ReadDeadline: 2 * time.Second, WriteDeadline: 2 * time.Second,
		MaximumHandlingTime: 5 * time.Second, MaximumRuntimes: 12, MaximumHooksPerRequest: 1,
		MaximumRequestIDLength: 64, MaximumOpaqueLength: 128, MaximumRevision: 1<<53 - 1,
		MaximumObservationAge: 2 * time.Minute, AllowedFutureSkew: 2 * time.Second,
		ReplayCapacity: 256, ReplayTTL: 5 * time.Minute, Clock: clock,
	}
}

func (config Config) Validate(requireSocket bool) error {
	if requireSocket && strings.TrimSpace(config.SocketPath) == "" {
		return errors.New("socket path must not be empty")
	}
	if config.MaximumRequestBytes == 0 || config.MaximumRequestBytes > 64*1024 {
		return errors.New("maximum request bytes must be between 1 and 65536")
	}
	if config.MaximumResponseBytes == 0 || config.MaximumResponseBytes > 64*1024 {
		return errors.New("maximum response bytes must be between 1 and 65536")
	}
	if config.MaximumConcurrent < 1 || config.MaximumConcurrent > 128 {
		return errors.New("maximum concurrent connections must be between 1 and 128")
	}
	if config.ReadDeadline <= 0 || config.WriteDeadline <= 0 || config.MaximumHandlingTime <= 0 {
		return errors.New("transport deadlines must be positive")
	}
	if config.MaximumRuntimes < 1 || config.MaximumRuntimes > 12 {
		return errors.New("maximum runtimes must be between 1 and 12")
	}
	if config.MaximumHooksPerRequest != 1 {
		return errors.New("exactly one hook observation per request is required")
	}
	if config.MaximumRequestIDLength < 8 || config.MaximumRequestIDLength > 128 ||
		config.MaximumOpaqueLength < 8 || config.MaximumOpaqueLength > 256 {
		return errors.New("identifier limits are outside supported bounds")
	}
	if config.MaximumRevision == 0 {
		return errors.New("maximum revision must be positive")
	}
	if config.MaximumObservationAge <= 0 || config.AllowedFutureSkew < 0 {
		return errors.New("observation time bounds are invalid")
	}
	if config.ReplayCapacity < 1 || config.ReplayCapacity > 4096 || config.ReplayTTL <= 0 {
		return errors.New("replay cache bounds are invalid")
	}
	if config.Clock == nil {
		return errors.New("clock must not be nil")
	}
	return nil
}
