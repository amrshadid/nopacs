package scrub

import (
	"context"
	"time"
)

// withRenew sets the interval of lease renewal.
func withRenew(c Config, d time.Duration) Config { c.seams.renew = d; return c }

// withSettle sets the wait before a second look at a scan's files.
func withSettle(c Config, d time.Duration) Config { c.seams.settle = d; return c }

// withSleep sets how a unit waits.
func withSleep(c Config, f func(ctx context.Context, d time.Duration) error) Config {
	c.seams.sleep = f
	return c
}

// withRead sets the seam called before each read of an object.
func withRead(c Config, f func(key string, attempt int)) Config { c.seams.read = f; return c }
