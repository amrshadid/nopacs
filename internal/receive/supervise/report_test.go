package supervise_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// failed returns the tenant's association failure events.
func (r *rig) failed() []*tenant.AssociationFailed {
	var out []*tenant.AssociationFailed
	for _, e := range r.events(tenant.EventAssociationFailed) {
		out = append(out, e.Payload.(*tenant.AssociationFailed))
	}
	return out
}

// TestRefusedCallersAreGroupedByHost guards the grouping of refused callers
// (monitor.proto, AssociationFailed): the first failure of a receiver,
// failure and host makes an event at once; the next ones in its minute make
// one event at its end with their count; another host has its own group.
func TestRefusedCallersAreGroupedByHost(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	report := r.running("ct")[0].spec.Report
	stranger := tenant.GroupCaller{CallingAETitle: "STRANGER", CalledAETitle: "ARCHIVE", Address: "192.0.2.7"}
	for range 5 {
		report(tenant.FailureCallerNotAllowed, stranger)
	}
	report(tenant.FailureCallerNotAllowed, tenant.GroupCaller{Address: "192.0.2.8"})
	r.waitFor("the first event of each host", func() bool { return len(r.failed()) == 2 })
	for _, e := range r.failed() {
		if e.Count != 1 || e.Hosts != 1 || e.Caller == nil || e.Failure != tenant.FailureCallerNotAllowed {
			t.Fatalf("first event %+v", e)
		}
	}
	r.clock.advance(time.Minute + time.Second)
	r.waitFor("the end-of-minute event", func() bool { return len(r.failed()) == 3 })
	last := r.failed()[2]
	if last.Count != 4 || last.Caller == nil || last.Caller.Address != "192.0.2.7" || last.Caller.CallingAETitle != "STRANGER" {
		t.Fatalf("end-of-minute event %+v, want 4 more from 192.0.2.7", last)
	}
	report(tenant.FailureCallerNotAllowed, stranger)
	r.waitFor("a new group after the minute", func() bool { return len(r.failed()) == 4 })
}

// TestManyHostsAreFoldedTogether guards the flood bound: past 64 distinct
// hosts failing at one receiver in a minute, the rest are folded into one
// event at the minute's end with no caller and the count of their hosts.
func TestManyHostsAreFoldedTogether(t *testing.T) {
	r := newRig(t)
	r.add("ct", 11112, true)
	r.start()
	r.waitFor("ct up", r.isUp("ct"))
	report := r.running("ct")[0].spec.Report
	for i := range 70 {
		report(tenant.FailureCallerNotAllowed, tenant.GroupCaller{Address: fmt.Sprintf("192.0.2.%d", i+1)})
	}
	r.waitFor("64 events of their own", func() bool { return len(r.failed()) == 64 })
	r.clock.advance(time.Minute + time.Second)
	r.waitFor("the folded event", func() bool { return len(r.failed()) == 65 })
	folded := r.failed()[64]
	if folded.Caller != nil || folded.Count != 6 || folded.Hosts != 6 {
		t.Fatalf("folded event %+v, want 6 failures of 6 hosts and no caller", folded)
	}
}
