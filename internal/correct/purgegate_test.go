package correct

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// pausingGate is a write gate paused as a switch pauses one: while paused
// a writer waits, up to wait, then is refused; a pause holds until every
// writer admitted before it has released, then resumes.
type pausingGate struct {
	wait time.Duration

	mu      sync.Mutex
	paused  chan struct{}
	writers int
	idle    chan struct{}
	refused int
}

// Enter waits while the gate is paused, refusing after wait, and admits.
func (g *pausingGate) Enter(ctx context.Context, storage string) (func(), error) {
	timeout := time.After(g.wait)
	for {
		g.mu.Lock()
		ch := g.paused
		if ch == nil {
			g.writers++
			g.mu.Unlock()
			var once sync.Once
			return func() { once.Do(g.leave) }, nil
		}
		g.mu.Unlock()
		select {
		case <-ch:
		case <-timeout:
			g.mu.Lock()
			g.refused++
			g.mu.Unlock()
			return nil, fault.New(fault.Unavailable, "the storage "+storage+" is being switched to its replica")
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// leave ends one writer's admission.
func (g *pausingGate) leave() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.writers--; g.writers == 0 && g.idle != nil {
		close(g.idle)
		g.idle = nil
	}
}

// pause holds every new writer, and returns a channel closed once the
// writers admitted before have released and the gate has resumed.
func (g *pausingGate) pause() <-chan struct{} {
	g.mu.Lock()
	g.paused = make(chan struct{})
	idle := make(chan struct{})
	if g.writers == 0 {
		close(idle)
	} else {
		g.idle = idle
	}
	g.mu.Unlock()
	resumed := make(chan struct{})
	go func() {
		<-idle
		g.mu.Lock()
		close(g.paused)
		g.paused = nil
		g.mu.Unlock()
		close(resumed)
	}()
	return resumed
}

// TestAnInstancePurgeHoldsTheGateBeforeTheScanLock guards the order in
// which an instance delete's purge takes the storage's write gate and the
// scan's lock, the order a reception takes them in (the gate, then the
// lock): a switch pauses the gate while the purge holds the scan's lock and
// a reception admitted before the pause waits for that lock. The purge goes
// on without asking the paused gate again, ends purged, and the reception
// takes the lock once the purge lets it go; nothing waits until the gate
// refuses. A purge that took the lock first and asked the gate after would
// wait at the paused gate for the reception, which waits for the purge,
// until the gate refused the purge's removal.
func TestAnInstancePurgeHoldsTheGateBeforeTheScanLock(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	const wait = 3 * time.Second
	gate := &pausingGate{wait: wait}
	var locks keyLocks
	lockScan := func(storage, key string) func() { return locks.lock(storage + "\x00" + key) }
	e := r.engineWith(func(c *Config) {
		c.Pause = gate
		c.LockScan = lockScan
	})
	s := r.scanWith(3)
	in := r.held(pathOf(s))[0]
	d, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label, Instance: in.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	mk, err := layout.ManifestKey(pathOf(s))
	if err != nil {
		t.Fatal(err)
	}

	// Asked before the purge's first removal, under the scan's lock: a
	// reception is admitted by the gate, the switch pauses it, and the
	// reception then asks for the scan's lock.
	var once sync.Once
	var resumed <-chan struct{}
	received := make(chan time.Duration, 1)
	e.SetHeld(func(context.Context, tenant.Correction) bool {
		once.Do(func() {
			release, err := gate.Enter(r.ctx, "main")
			if err != nil {
				t.Error(err)
				return
			}
			resumed = gate.pause()
			go func() {
				defer release()
				start := time.Now()
				unlock := lockScan("main", mk)
				received <- time.Since(start)
				unlock()
			}()
		})
		return false
	})
	start := time.Now()
	got, err := e.PurgeNow(r.ctx, d.ID, r.audit(), nil)
	took := time.Since(start)
	if err != nil || got.State != record.StatePurged {
		t.Fatalf("the purge under a paused gate = %+v, %v (after %s); want it purged", got.State, err, took)
	}
	if took >= wait {
		t.Fatalf("the purge took %s, as long as the gate waits before it refuses", took)
	}
	select {
	case waited := <-received:
		if waited >= wait {
			t.Fatalf("the reception waited %s for the scan's lock", waited)
		}
	case <-time.After(wait):
		t.Fatal("the reception never took the scan's lock")
	}
	select {
	case <-resumed:
	case <-time.After(wait):
		t.Fatal("the switch's pause never saw the writers leave")
	}
	gate.mu.Lock()
	refused := gate.refused
	gate.mu.Unlock()
	if refused != 0 {
		t.Fatalf("the gate refused %d writes", refused)
	}
}
