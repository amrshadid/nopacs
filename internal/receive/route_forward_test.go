package receive

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRouteForwardRulesAllFire guards the forward phase over a real
// pipeline: every forward rule whose condition holds forwards the
// reception, not only the first; a rule makes one job per series the
// reception brought, or, with a series filter, per series whose first file
// it holds for, naming that series' scan, and none for the others; a rule whose condition reads an attribute the file
// does not hold is false and forwards nothing; a destination profile goes
// with its job; and the records name each rule with the series it sent.
func TestRouteForwardRulesAllFire(t *testing.T) {
	dest := tenant.ProfileRef{Name: "dest", Version: 1}
	e := newEnv(t, envOptions{routing: &tenant.RuleSet{Forward: []tenant.ForwardRule{
		{Name: "every", Senders: []string{"pacs"}, Profile: dest},
		{Name: "smart", When: `Modality == "CT"`, Series: `SeriesDescription contains "smartscore"`, Senders: []string{"ai"}},
		{Name: "absent", When: `(0019,"NOBODY",10) == "x" or ContrastBolusAgent present`, Senders: []string{"pacs"}},
	}}})
	e.sendAs("SCANNER", studyFiles...)
	r := e.settle(1)[0]
	if !done(r) {
		t.Fatalf("the study: %s", r.State)
	}
	if len(r.Route.Forwards) != 2 || r.Route.Forwards[0].Rule != "every" || r.Route.Forwards[1].Rule != "smart" ||
		len(r.Route.Forwards[1].Series) != 1 || r.Route.ForwardRuleSet != 1 {
		t.Fatalf("the reception's forwards: %+v", r.Route.Forwards)
	}
	jobs, _, err := e.store.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	scans, _, err := e.store.ReceptionScans(e.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	var smartScan string
	for _, sc := range scans {
		if sc.Instances == 5 {
			smartScan = sc.Label
		}
	}
	got := map[string]tenant.Job{}
	for _, j := range jobs {
		got[j.Sender+"/"+j.Target.ScanLabel] = j
	}
	if len(jobs) != 1+len(scans) {
		t.Errorf("%d forwards, want %d (every to pacs for each of %d scans, the SmartScore series to ai): %v", len(jobs), 1+len(scans), len(scans), got)
	}
	for _, sc := range scans {
		if j, ok := got["pacs/"+sc.Label]; !ok || j.Profile != dest {
			t.Errorf("the forward of every for scan %s: %+v, want that scan with profile %v", sc.Label, j, dest)
		}
	}
	if _, ok := got["ai/"+smartScan]; !ok || smartScan == "" {
		t.Errorf("no forward to ai of the SmartScore scan %q: %v", smartScan, got)
	}
	hits, err := e.store.RouteHits(e.ctx, "dimse", 1)
	if err != nil {
		t.Fatal(err)
	}
	// A version with no archive rule and no default of its own has no
	// archive phase to record.
	want := map[string][2]int64{"forward/every": {1, 0}, "forward/smart": {1, 1}}
	if len(hits) != len(want) {
		t.Errorf("the hits: %+v, want %v", hits, want)
	}
	for _, h := range hits {
		if w := want[string(h.Phase)+"/"+h.Rule]; w != [2]int64{h.Receptions, h.Series} {
			t.Errorf("hit %s/%s: %d receptions, %d series, want %v", h.Phase, h.Rule, h.Receptions, h.Series, w)
		}
	}
}

// TestRouteLateArrivalForwardsItsOwnSeries guards that forward rules are
// each reception's own: a later arrival of a session goes where the first
// did, and is forwarded by the rules the receiver runs when it is
// identified, over its own series, never the first arrival's.
func TestRouteLateArrivalForwardsItsOwnSeries(t *testing.T) {
	e := newEnv(t, envOptions{routing: &tenant.RuleSet{Forward: []tenant.ForwardRule{
		{Name: "scout", Series: `SeriesDescription = "scout"`, Senders: []string{"pacs"}},
	}}})
	e.sendAs("SCANNER", studyFiles[2:]...)
	first := e.settle(1)[0]
	if len(first.Route.Forwards) != 0 {
		t.Errorf("an arrival with no Scout series is forwarded: %+v", first.Route.Forwards)
	}
	e.sendAs("SCANNER", studyFiles[:2]...)
	late := receptionOf(t, e.settle(2), first.StudyUID, 2)
	if len(late.Route.Forwards) != 1 || len(late.Route.Forwards[0].Series) != 1 {
		t.Errorf("the late arrival, its Scout series: %+v", late.Route.Forwards)
	}
}

// TestRouteUnfilteredForwardSendsOnlyItsOwnSeries guards that a forward
// rule with no series filter, judged over one reception's first file, sends
// only the scans that reception brought: an earlier arrival's series, which
// the rule never held for, is not sent to the rule's senders.
func TestRouteUnfilteredForwardSendsOnlyItsOwnSeries(t *testing.T) {
	e := newEnv(t, envOptions{routing: &tenant.RuleSet{Forward: []tenant.ForwardRule{
		{Name: "scout", When: `SeriesDescription = "scout"`, Senders: []string{"pacs"}},
	}}})
	e.sendAs("SCANNER", studyFiles[2:]...)
	first := e.settle(1)[0]
	if len(first.Route.Forwards) != 0 {
		t.Fatalf("an arrival with no Scout series is forwarded: %+v", first.Route.Forwards)
	}
	e.sendAs("SCANNER", studyFiles[:2]...)
	late := receptionOf(t, e.settle(2), first.StudyUID, 2)
	if len(late.Route.Forwards) != 1 {
		t.Fatalf("the late arrival's forwards: %+v", late.Route.Forwards)
	}
	brought, _, err := e.store.ReceptionScans(e.ctx, late.ID)
	if err != nil {
		t.Fatal(err)
	}
	jobs, _, err := e.store.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	sent := map[string]bool{}
	for _, j := range jobs {
		if j.Sender == "pacs" {
			sent[j.Target.ScanLabel] = true
		}
	}
	if len(sent) != len(brought) || len(brought) == 0 {
		t.Errorf("the rule sent scans %v, want exactly the %d the late arrival brought", sent, len(brought))
	}
	for _, s := range brought {
		if !sent[s.Label] {
			t.Errorf("the scan %s the late arrival brought is not sent (sent: %v)", s.Label, sent)
		}
	}
}
