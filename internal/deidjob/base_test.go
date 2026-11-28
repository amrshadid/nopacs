package deidjob

import (
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCheckJudgesByTheProfileStackedOverTheBase guards that a check judges a
// file by what a reception runs: a file archived before the tenant had a base
// passes the check; once a base that removes an element the file still holds
// is set, the same check refuses it (removed_element, the element's tag), so a
// file never cleaned to the base is found.
func TestCheckJudgesByTheProfileStackedOverTheBase(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	files := f.deidSession("ses-1", ref, map[string][]string{"ct": {"ct5n-2062.dcm"}}, nil)
	s := f.start(f.config())
	run := func() tenant.Job {
		j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "")
		s.Poke("lab")
		return f.waitJob(s, j.ID, 30*time.Second, ended)
	}
	if j := run(); j.State != tenant.JobDone || j.Counts.InstancesRefused != 0 {
		t.Fatalf("the check before a base ended %s with %+v, failures %+v", j.State, j.Counts, f.failures(j.ID))
	}
	doc := basicProfile()
	doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: "thickness", Selector: kw("SliceThickness"),
		Action: &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}})
	base := f.storeVersion("floor", doc)
	if err := f.p.SetSetting(f.ctx, settings.DeidBaseProfile, base.String(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	j := run()
	if j.State != tenant.JobPartial || j.Counts.InstancesRefused != 1 {
		t.Fatalf("the check after a base ended %s with %+v, failures %+v", j.State, j.Counts, f.failures(j.ID))
	}
	_, _, sop := uidsOf(t, files["ct5n-2062.dcm"])
	if x := f.failures(j.ID)[sop]; x.Code != tenant.FailureCheckFailed || x.Detail != "removed_element" {
		t.Errorf("failure %+v, want check_failed removed_element", x)
	}
	if n := f.tallies(j.ID)["check|removed_element|00180050"]; n != 1 {
		t.Errorf("tallies %v, want the base's element once", f.tallies(j.ID))
	}
}
