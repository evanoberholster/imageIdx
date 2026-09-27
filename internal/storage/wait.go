package storage

import (
	"context"
	"time"
)

// idlePollInterval is the shared polling period for index WaitIdle methods,
// which block until background work drains or the context is cancelled.
const idlePollInterval = 100 * time.Microsecond

// WaitForCondition polls done every 100 microseconds until it reports true
// or ctx is cancelled. It backs the WaitIdle methods of the index packages
// so their cancellation and polling behavior stays identical.
func WaitForCondition(ctx context.Context, done func() bool) error {
	ticker := time.NewTicker(idlePollInterval)
	defer ticker.Stop()
	for !done() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}
