package access

import (
	"context"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// DefaultExpiryInterval is how often the keys are read for keys about to
// expire: an hour, so a key is reported within an hour of entering the
// warning, at the cost of one paged read of the keys an hour.
const DefaultExpiryInterval = time.Hour

// expiryRepeat is how long after an expiring key's event the next one for
// the same key is raised: one a day while the key is about to expire.
const expiryRepeat = 24 * time.Hour

// maxKeyPages bounds the pages of keys ExpiringKeys reads: a thousand keys
// a page, so a hundred thousand keys, beyond any installation's. A store
// that keeps returning a cursor is a broken store, and the read fails
// rather than loop.
const maxKeyPages = 100

// expiryTimeout bounds one round of the expiry worker.
const expiryTimeout = time.Minute

// expiryWatch is the state of the goroutine that raises EventKeyExpiring.
// raised is when each key's last event was raised; it is held in memory, so
// a restart may raise a key's event a second time within the day, which
// costs a repeated notice and loses nothing. It holds only keys that were
// expiring at the last round, so it never outgrows the keys.
type expiryWatch struct {
	warning  func(ctx context.Context) (time.Duration, error)
	interval time.Duration
	done     chan struct{}
	raised   map[string]time.Time
	// rounds counts the rounds finished, read by a test that waits for
	// rounds rather than for time.
	rounds atomic.Int64
}

// ExpiringKeys returns the keys that work now and expire within within of
// the clock's now, in the order ListKeys answers them, oldest first.
// Revoked, expired and never-expiring keys are left out. It reads every key,
// page by page, and needs no Decision: a handler that answers them checks
// its caller first, and the expiry worker is the server's own.
func (a *Access) ExpiringKeys(ctx context.Context, within time.Duration) ([]master.Key, error) {
	now := a.clock()
	var out []master.Key
	cursor := ""
	for range maxKeyPages {
		keys, next, err := a.store.ListKeys(ctx, master.Page{Cursor: cursor, Limit: master.MaxPageLimit})
		if err != nil {
			return nil, err
		}
		for _, k := range keys {
			if k.Revoked.IsZero() && !k.Expires.IsZero() && k.Expires.After(now) && !k.Expires.After(now.Add(within)) {
				out = append(out, publicKey(k))
			}
		}
		if next == "" {
			return out, nil
		}
		cursor = next
	}
	return nil, fault.New(fault.Internal, "access: the keys did not end after "+strconv.Itoa(maxKeyPages)+" pages")
}

// expiryWorker raises EventKeyExpiring for each key about to expire, at
// once and then every interval, until Close.
func (a *Access) expiryWorker() {
	defer close(a.expiry.done)
	t := time.NewTicker(a.expiry.interval)
	defer t.Stop()
	for {
		a.expiryRound()
		select {
		case <-t.C:
		case <-a.stop:
			return
		}
	}
}

// expiryRound reads the warning and the keys once and raises the events
// due. A failure is logged and the round skipped: the next one tries again.
func (a *Access) expiryRound() {
	defer a.expiry.rounds.Add(1)
	ctx, cancel := context.WithTimeout(a.workCtx, expiryTimeout)
	defer cancel()
	within, err := a.expiry.warning(ctx)
	if err != nil {
		a.log.Warn("access: the keys about to expire were not read", "kind", fault.KindOf(err).String(), "error", err)
		return
	}
	keys, err := a.ExpiringKeys(ctx, within)
	if err != nil {
		a.log.Warn("access: the keys about to expire were not read", "kind", fault.KindOf(err).String(), "error", err)
		return
	}
	now := a.clock()
	still := make(map[string]time.Time, len(keys))
	for _, k := range keys {
		last, ok := a.expiry.raised[k.ID]
		if ok && now.Sub(last) < expiryRepeat {
			still[k.ID] = last
			continue
		}
		a.raise(Event{Kind: EventKeyExpiring, Time: now, KeyID: k.ID, Until: k.Expires})
		still[k.ID] = now
	}
	a.expiry.raised = still
}
