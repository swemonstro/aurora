package presencebroker

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/swemonstro/aurora/internal/instanceregistry"
)

// DefaultExpiryInterval is how often RunExpiryLoop scans for lease
// transitions that are due.
const DefaultExpiryInterval = 5 * time.Second

// ExpirySource is the registry surface RunExpiryLoop drives. Nothing in
// this repository calls Registry.ExpireLeases on a timer on its own —
// without an active driver like this loop, no instance's lease ever
// actually expires, no matter how stale it becomes.
type ExpirySource interface {
	ExpireLeases() (instanceregistry.ExpiryResult, error)
}

// RunExpiryLoop calls source.ExpireLeases on interval until ctx is done.
// Failures are logged and never stop the loop; ExpireLeases itself only
// ever transitions the specific instances whose lease is actually due (see
// instanceregistry.Registry.ExpireLeases), so this loop cannot affect an
// instance before its own lease has elapsed.
func RunExpiryLoop(ctx context.Context, source ExpirySource, interval time.Duration, stderr io.Writer) {
	if interval <= 0 {
		interval = DefaultExpiryInterval
	}
	if stderr == nil {
		stderr = io.Discard
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := source.ExpireLeases(); err != nil {
			fmt.Fprintln(stderr, "lease expiry:", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
