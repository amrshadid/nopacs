package servecmd

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAReplicaBehindIsReportedAndMeasured guards the lag watch as the
// server wires it per tenant: with the replica's place gone, so its scans
// stay pending, and the server's clock two hours past their commit, the
// tenant's feed gets one REPLICA_BEHIND event naming the replica and its
// primary with the scans behind; the watchers of the feed are woken; and
// the replica's lag shows in the metrics by tenant and replica.
func TestAReplicaBehindIsReportedAndMeasured(t *testing.T) {
	r := newWorkRig(t)
	if err := os.RemoveAll(filepath.Join(r.root, "lab", "copy")); err != nil {
		t.Fatal(err)
	}
	r.w.lagInterval = 50 * time.Millisecond
	r.w.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	var woken atomic.Int64
	r.w.wakeFeed = func(name string) {
		if name == "lab" {
			woken.Add(1)
		}
	}
	r.start()
	p := r.pipeline()
	r.accept(p, workStudy...)
	r.waitFor("the reception to be archived", time.Minute, func() bool { return r.archived(1) })
	var behind []*tenant.ReplicaBehind
	r.waitFor("a replica_behind event", time.Minute, func() bool {
		page, err := r.db.(tenant.Events).ReadEvents(r.ctx, "", []tenant.EventType{tenant.EventReplicaBehind}, 100)
		if err != nil {
			t.Fatal(err)
		}
		behind = behind[:0]
		for _, e := range page.Events {
			if b, ok := e.Payload.(*tenant.ReplicaBehind); ok {
				behind = append(behind, b)
			}
		}
		return len(behind) > 0
	})
	time.Sleep(200 * time.Millisecond) // several more looks: still one event
	page, err := r.db.(tenant.Events).ReadEvents(r.ctx, "", []tenant.EventType{tenant.EventReplicaBehind}, 100)
	if err != nil {
		t.Fatal(err)
	}
	b := behind[0]
	if len(page.Events) != 1 || b.Replica != "copy" || b.Primary != "main" || b.Recovered || b.ScansBehind < 1 ||
		time.Duration(b.LagMicros)*time.Microsecond < time.Hour {
		t.Fatalf("the feed holds %d replica_behind events, the first %+v; want one, copy of main, behind past an hour", len(page.Events), b)
	}
	if woken.Load() == 0 {
		t.Error("the feed's watchers were not woken after the event")
	}
	var lag float64
	found := false
	for _, g := range r.w.replicaGauges() {
		if g.Name == metricReplicaLag && len(g.Labels) == 4 && g.Labels[1] == "lab" && g.Labels[3] == "copy" && !g.Empty {
			lag, found = g.Value, true
		}
	}
	if !found || lag < 3600 {
		t.Errorf("the replica's lag in the metrics is %v (found %v); want at least an hour", lag, found)
	}
}

// TestScrubCoverageGauges guards the metrics of scrubs' coverage: with no
// scrub ended, each is described with no sample; after, one sample per
// tenant and storage of the share, the oldest unverified age in seconds and
// the overdue count.
func TestScrubCoverageGauges(t *testing.T) {
	empty := scrubGaugesOf(nil)
	if len(empty) != 3 || !empty[0].Empty || !empty[1].Empty || !empty[2].Empty {
		t.Fatalf("with no scrub ended the gauges are %+v; want three described, empty", empty)
	}
	got := scrubGauges([]scrub.TenantCoverage{
		{Tenant: "lab", StorageCoverage: scrub.StorageCoverage{Storage: "main", Objects: 10, Verified: 4, Share: 0.4, OldestUnverified: 90 * time.Second, Overdue: 6}},
	})
	want := map[string]float64{metricVerifiedShare: 0.4, metricOldestUnchecked: 90, metricOverdue: 6}
	if len(got) != 3 {
		t.Fatalf("the gauges are %+v; want one per metric", got)
	}
	for _, g := range got {
		if g.Empty || len(g.Labels) != 4 || g.Labels[0] != "tenant" || g.Labels[1] != "lab" || g.Labels[2] != "storage" || g.Labels[3] != "main" ||
			g.Value != want[g.Name] || g.Help == "" {
			t.Errorf("the gauge %+v; want %v with lab and main", g, want[g.Name])
		}
	}
}
