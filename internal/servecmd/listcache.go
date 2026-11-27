package servecmd

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/fault"
)

// listCacheInterval is how often the bound of the text lists' cache is
// read again (deid.list-cache-bytes): a change takes effect within it.
const listCacheInterval = 10 * time.Second

// listCacheBound keeps the bound of the cache stored profile versions'
// text lists share (profile.SetListCacheBytes) at the installation's
// deid.list-cache-bytes, read when the server starts and every
// listCacheInterval after, until it stops. A value that cannot be read
// leaves the bound as it was.
type listCacheBound struct {
	read func(ctx context.Context) (int64, error)
	log  *slog.Logger

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

// Start reads the bound once and starts reading it again on a timer.
func (b *listCacheBound) Start(ctx context.Context) error {
	b.apply(ctx)
	ctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b.mu.Lock()
	b.cancel, b.done = cancel, make(chan struct{})
	done := b.done
	b.mu.Unlock()
	go func() {
		defer close(done)
		t := time.NewTicker(listCacheInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				b.apply(ctx)
			}
		}
	}()
	return nil
}

// apply reads the setting and sets the bound.
func (b *listCacheBound) apply(ctx context.Context) {
	n, err := b.read(ctx)
	if err != nil {
		if ctx.Err() == nil {
			b.log.Warn("deid: the text lists' cache bound could not be read; kept as it was", "kind", fault.KindOf(err).String(), "reason", fault.Message(err))
		}
		return
	}
	profile.SetListCacheBytes(n)
}

// Stop stops the timer and waits for it within ctx.
func (b *listCacheBound) Stop(ctx context.Context) error {
	b.mu.Lock()
	cancel, done := b.cancel, b.done
	b.mu.Unlock()
	if cancel == nil {
		return nil
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
