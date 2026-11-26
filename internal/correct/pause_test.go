package correct

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// refusingPause is a write gate that refuses while refuse is set and counts
// the removals it admits and releases.
type refusingPause struct {
	mu       sync.Mutex
	refuse   bool
	admitted int
	released int
}

// Enter refuses as a switching storage does, or admits and counts.
func (p *refusingPause) Enter(_ context.Context, storage string) (func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.refuse {
		return nil, fault.New(fault.Unavailable, "the storage "+storage+" is being switched to its replica")
	}
	p.admitted++
	return func() {
		p.mu.Lock()
		p.released++
		p.mu.Unlock()
	}, nil
}

// TestAPurgeAsksThePauseBeforeEachRemoval guards Config.Pause: a purge
// already past its fence asks the gate before it removes each object, so
// while the gate refuses (a switch of the storage runs) the purge stops with
// every object still on the storage and the correction still purging; once
// the gate admits, the purge ends purged, every removal admitted and each
// released.
func TestAPurgeAsksThePauseBeforeEachRemoval(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	pause := &refusingPause{}
	e := r.engineWith(func(c *Config) { c.Pause = pause })
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: r.scans()[0].SubjectLabel})
	if err != nil {
		t.Fatal(err)
	}
	deleted := r.objects()
	pause.refuse = true
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); fault.KindOf(err) != fault.Unavailable {
		t.Fatalf("a purge while the gate refuses: %v", err)
	}
	// The correction's record is rewritten as purging before any object
	// goes; every object under data/ stays as it was.
	after := r.objects()
	for k, sum := range deleted {
		if strings.HasPrefix(k, "data/") && after[k] != sum {
			t.Fatalf("%s went or changed while the gate refused", k)
		}
	}
	if got, err := r.p.Correction(r.ctx, c.ID); err != nil || got.State != record.StatePurging {
		t.Fatalf("after the refusal the correction is %+v (%v), want purging", got, err)
	}
	pause.refuse = false
	got, err := e.ContinuePurge(r.ctx, c.ID)
	if err != nil || got.State != record.StatePurged {
		t.Fatalf("the purge once the gate admits: %+v %v", got, err)
	}
	if pause.admitted == 0 || pause.admitted != pause.released {
		t.Fatalf("the gate admitted %d removals and saw %d released", pause.admitted, pause.released)
	}
}
