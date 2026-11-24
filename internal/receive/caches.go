package receive

import (
	"context"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// receiverTTL is how long a receiver's configuration read from the database
// is used before it is read again: a change made through the API reaches
// the receiving side within it.
const receiverTTL = time.Second

// receiverCache keeps each receiver's configuration for receiverTTL, so a
// file does not cost a read of its receiver.
type receiverCache struct {
	store tenant.Pipeline
	clock Clock

	mu      sync.Mutex
	entries map[string]receiverEntry
}

type receiverEntry struct {
	r    tenant.Receiver
	read time.Time
}

func newReceiverCache(store tenant.Pipeline, clock Clock) *receiverCache {
	return &receiverCache{store: store, clock: clock, entries: map[string]receiverEntry{}}
}

// get returns the receiver of that name, read again when older than
// receiverTTL.
func (c *receiverCache) get(ctx context.Context, name string) (tenant.Receiver, error) {
	now := c.clock.Now()
	c.mu.Lock()
	e, ok := c.entries[name]
	c.mu.Unlock()
	if ok && now.Sub(e.read) < receiverTTL && now.Sub(e.read) >= 0 {
		return e.r, nil
	}
	r, err := c.store.Receiver(ctx, name)
	if err != nil {
		return tenant.Receiver{}, err
	}
	c.mu.Lock()
	c.entries[name] = receiverEntry{r: r, read: now}
	c.mu.Unlock()
	return r, nil
}

// receptionKey is one study at one receiver.
type receptionKey struct{ receiver, study string }

// receptionCache keeps the reception each study at each receiver is
// receiving into, so a file does not cost a write to find it. An entry is
// dropped when the store says the reception closed.
type receptionCache struct {
	mu sync.Mutex
	m  map[receptionKey]string
}

func newReceptionCache() *receptionCache {
	return &receptionCache{m: map[receptionKey]string{}}
}

func (c *receptionCache) get(k receptionKey) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	id, ok := c.m[k]
	return id, ok
}

func (c *receptionCache) put(k receptionKey, id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k] = id
}

// drop forgets the reception id, wherever it is cached.
func (c *receptionCache) drop(id string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, v := range c.m {
		if v == id {
			delete(c.m, k)
		}
	}
}
