package correct

import (
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestFenceStopsEveryStorageWrite guards Config.Fence: while it refuses, a
// delete, a relabel and a purge are each answered its refusal and write
// nothing to the storage (every object's bytes as before, no correction
// made); once it lets the tenant write, the same delete goes through.
func TestFenceStopsEveryStorageWrite(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	fenced := master.ReadOnlyError("lab", master.StateFenced, "the storage main's owner record names installation B")
	refuse := true
	e := r.engineWith(func(c *Config) {
		c.Fence = func(context.Context) error {
			if refuse {
				return fenced
			}
			return nil
		}
	})
	subject := r.scans()[0].SubjectLabel
	before := r.objects()
	target := tenant.CorrectionTarget{Subject: subject}
	if _, err := r.deleteNow(e, target); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a delete while fenced: %v", err)
	}
	if _, err := e.Relabel(r.ctx, RelabelRequest{Target: target, ToLabel: "renamed-1", Reason: "x", Audit: r.audit()}); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a relabel while fenced: %v", err)
	}
	r.wantUnchanged(before, r.objects())
	if cs, _, err := r.p.ListCorrections(r.ctx, tenant.CorrectionFilter{}, master.Page{Limit: 10}); err != nil || len(cs) != 0 {
		t.Fatalf("corrections made while fenced: %v %v", cs, err)
	}
	refuse = false
	c, err := r.deleteNow(e, target)
	if err != nil {
		t.Fatalf("a delete once the fence lets it: %v", err)
	}
	refuse = true
	deleted := r.objects()
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a purge while fenced: %v", err)
	}
	r.wantUnchanged(deleted, r.objects())
}
