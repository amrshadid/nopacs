package servecmd

import (
	"crypto/sha256"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestQuarantineWarningIsReportedAndMeasured guards the quarantine watch as
// the server wires it per tenant: a file held for sixty days is past the
// default quarantine.warn-age, so the tenant's feed gets one QUARANTINE_AGING
// event however many looks follow, its watchers are woken, and the tenant's
// held bytes and oldest age show in the metrics.
func TestQuarantineWarningIsReportedAndMeasured(t *testing.T) {
	r := newWorkRig(t)
	r.w.quarantine.interval = 50 * time.Millisecond
	woken := 0
	r.w.wakeFeed = func(string) { woken++ }
	q := tenant.QuarantinedFile{
		ID: "id-held", Reason: tenant.QuarantineBadLabel, Receiver: "dimse",
		Details: map[string]string{"label": "subject", "attribute": "PatientID", "fault": "missing"},
		Caller:  tenant.Caller{CallingAETitle: "PRISMA1", CalledAETitle: "NOPACS", Address: "10.20.0.31:50288"},
		Size:    4096, SHA256: sha256.Sum256([]byte("held")), SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", SOPInstanceUID: "2.25.91",
		StudyUID: "2.25.1234567890", SeriesUID: "2.25.222", TransferSyntaxUID: "1.2.840.10008.1.2.1",
		Incoming: "q/id-held", ContentKept: true, Quarantined: time.Now().Add(-60 * 24 * time.Hour),
	}
	if _, err := r.db.HoldFiles(r.ctx, []tenant.QuarantinedFile{q}, nil); err != nil {
		t.Fatal(err)
	}
	r.start()
	read := func() []tenant.Event {
		page, err := r.db.(tenant.Events).ReadEvents(r.ctx, "", []tenant.EventType{tenant.EventQuarantineAging}, 100)
		if err != nil {
			t.Fatal(err)
		}
		return page.Events
	}
	r.waitFor("a quarantine_aging event", time.Minute, func() bool { return len(read()) > 0 })
	time.Sleep(300 * time.Millisecond) // several more looks: still one event
	evs := read()
	p, ok := evs[0].Payload.(*tenant.QuarantineAgingChanged)
	if len(evs) != 1 || !ok || !p.AgePassed || p.BytesPassed || p.Recovered || p.HeldBytes != 4096 {
		t.Fatalf("the feed holds %d quarantine_aging events, the first %+v; want one with the age warning", len(evs), evs[0].Payload)
	}
	if woken == 0 {
		t.Error("the feed's watchers were not woken after the event")
	}
	got := map[string]float64{}
	for _, g := range r.w.quarantine.gauges() {
		if !g.Empty && len(g.Labels) == 2 && g.Labels[1] == "lab" {
			got[g.Name] = g.Value
		}
	}
	if got[metricQuarantineBytes] != 4096 || got[metricQuarantineOldest] < 59*24*3600 {
		t.Errorf("the metrics read %v; want 4096 bytes and about sixty days", got)
	}
}
