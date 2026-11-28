package transfer

import (
	"sync"
	"time"
)

// Waiting remembers since when each transfer job has been waiting on
// refusals that pass by themselves, so a job that waits past transfer.wait
// ends failed instead of retrying for ever. A job that makes progress is
// forgotten. It is kept in memory: a restart of the server starts the wait
// again, which is why a job is also bounded by the pass that meets it.
type Waiting struct {
	mu    sync.Mutex
	since map[string]time.Time
}

// NewWaiting returns an empty record.
func NewWaiting() *Waiting { return &Waiting{since: map[string]time.Time{}} }

// mark records that job waits at now, and answers since when it has.
func (w *Waiting) mark(job string, now time.Time) time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.since == nil {
		w.since = map[string]time.Time{}
	}
	t, ok := w.since[job]
	if !ok {
		t = now
		w.since[job] = t
	}
	return t
}

// clear forgets job: it made progress or ended.
func (w *Waiting) clear(job string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.since, job)
}
