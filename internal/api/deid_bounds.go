package api

import (
	"sync"
	"time"
)

// maxBuckets is how many buckets are kept before the full ones are let go.
const maxBuckets = 4096

// bucketKind is one bound: size calls, filling again evenly over period.
type bucketKind struct {
	// name says what is bounded, in a refusal's message ("code key").
	name   string
	size   float64
	period time.Duration
}

// bucket is one bucket of a kind: the kind and whose it is (a key, a
// tenant).
type bucket struct {
	kind  bucketKind
	owner string
}

// rateBuckets bounds the forward lookups and the imports of lookup codes,
// in memory: one process serves an installation, and a restart giving
// every bucket its size again costs at most one more period's worth.
type rateBuckets struct {
	mu    sync.Mutex
	state map[bucket]*bucketState
}

// bucketState is how many calls a bucket allows, as of at.
type bucketState struct {
	tokens float64
	at     time.Time
}

// take spends one call of every bucket given at now, and returns "", or
// spends none and returns what is bounded ("this key", "this tenant") when
// any of them is empty: a call one bound refuses does not use up another.
func (b *rateBuckets) take(now time.Time, buckets ...bucket) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == nil {
		b.state = map[bucket]*bucketState{}
	}
	if len(b.state) >= maxBuckets {
		for k, s := range b.state {
			if now.Sub(s.at) >= k.kind.period {
				delete(b.state, k)
			}
		}
	}
	states := make([]*bucketState, len(buckets))
	for i, k := range buckets {
		s, ok := b.state[k]
		if !ok {
			s = &bucketState{tokens: k.kind.size, at: now}
			b.state[k] = s
		}
		if d := now.Sub(s.at); d > 0 {
			s.tokens = min(k.kind.size, s.tokens+k.kind.size*d.Seconds()/k.kind.period.Seconds())
			s.at = now
		}
		states[i] = s
	}
	for i, k := range buckets {
		if states[i].tokens < 1 {
			if k.kind == codeTenantBucket {
				return "this tenant"
			}
			return "this key"
		}
	}
	for _, s := range states {
		s.tokens--
	}
	return ""
}
