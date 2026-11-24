package auditseal

import (
	"sync"
	"sync/atomic"
	"time"
)

// Stats are what the passes have counted since the Sealer was made, for the
// server's metrics.
type Stats struct {
	// RolledBack counts the anchors a pass kept as they were because the log
	// they name no longer descends from them (Descends), or that the store
	// refused as going back.
	RolledBack uint64
	// Unencodable counts the passes that stopped at an unsealed entry that
	// cannot be encoded (UnencodableError).
	Unencodable uint64
	// Failed counts the other failures to seal, anchor or check a log: a
	// database that did not answer, mostly.
	Failed uint64
	// OldestUnsealed is the time of the oldest entry the last pass left
	// unsealed in any log, zero when it left none: an age that grows past
	// an interval or two is a log the passes cannot seal.
	OldestUnsealed time.Time
}

// stats is a Sealer's counts, safe for concurrent use.
type stats struct {
	rolledBack, unencodable, failed atomic.Uint64

	mu     sync.Mutex
	oldest time.Time
}

// setOldest records the oldest unsealed entry's time after a pass.
func (s *stats) setOldest(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.oldest = t
}

// Stats returns what the passes have counted.
func (s *Sealer) Stats() Stats {
	s.stats.mu.Lock()
	oldest := s.stats.oldest
	s.stats.mu.Unlock()
	return Stats{
		RolledBack:     s.stats.rolledBack.Load(),
		Unencodable:    s.stats.unencodable.Load(),
		Failed:         s.stats.failed.Load(),
		OldestUnsealed: oldest,
	}
}
