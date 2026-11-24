package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testReleasedProfile guards a release that names a profile version
// (tenant.Release.Profile): through a receiver that de-identifies, the
// record keeps it and the reception it joins keeps it as its released
// profile; a second release into that reception naming another version is
// a precondition failure, the record still held, and one naming the same
// version joins; a release naming a version through a receiver that
// archives as received is a precondition failure; and a released file that
// lands again naming a version gives it to the reception it joins.
func testReleasedProfile(e *penv) {
	e.addStorage("store")
	e.addReceiver("plain", "store", 11112)
	deid := receiverOf("deid", "store", 11113)
	deid.Profile = tenant.ProfileRef{Name: "research", Version: 1}
	_, err := e.p.AddReceiver(e.ctx, deid, at(1), e.audit(), nil)
	e.ok("AddReceiver de-identifying", err)
	fixed, other := tenant.ProfileRef{Name: "fixed", Version: 2}, tenant.ProfileRef{Name: "other", Version: 1}

	a, b, plain := e.differentFile("deid", "", 21), e.differentFile("deid", "", 22), e.differentFile("plain", "", 23)
	a.Deidentified, b.Deidentified = true, true
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{a, b, plain})
	e.ok("HoldFiles", err)
	pp := e.parts()
	got, err := pp.ReleaseFile(e.ctx, tenant.Release{ID: a.ID, Mode: tenant.ReleaseAsIs, ReceptionID: e.id("rc"), Profile: fixed, Now: at(10)}, e.audit(), nil)
	e.ok("ReleaseFile naming a version", err)
	if got.ReleaseReception == "" || got.ReleaseProfile != fixed {
		e.Fatalf("the release joined %q keeping %v", got.ReleaseReception, got.ReleaseProfile)
	}
	if read, _ := e.p.QuarantinedFile(e.ctx, a.ID); read.ReleaseProfile != fixed {
		e.Errorf("the released record reads back %v, want %v", read.ReleaseProfile, fixed)
	}
	joined, _, err := e.p.Reception(e.ctx, got.ReleaseReception)
	e.ok("Reception", err)
	if joined.Route.ReleasedProfile != fixed {
		e.Errorf("the reception joined keeps %+v, want the version named", joined.Route)
	}
	_, err = pp.ReleaseFile(e.ctx, tenant.Release{ID: b.ID, Mode: tenant.ReleaseAsIs, ReceptionID: e.id("rc"), Profile: other, Now: at(11)}, e.audit(), nil)
	e.wantKind("ReleaseFile naming another version into the same reception", err, fault.Precondition)
	if still, _ := e.p.QuarantinedFile(e.ctx, b.ID); still.State != tenant.QuarantineHeld {
		e.Errorf("after a refused release the record is %s", still.State)
	}
	got, err = pp.ReleaseFile(e.ctx, tenant.Release{ID: b.ID, Mode: tenant.ReleaseAsIs, ReceptionID: e.id("rc"), Profile: fixed, Now: at(12)}, e.audit(), nil)
	e.ok("ReleaseFile naming the same version into the same reception", err)
	if got.ReleaseReception != joined.ID {
		e.Errorf("the second release joined %s, want %s", got.ReleaseReception, joined.ID)
	}
	_, err = pp.ReleaseFile(e.ctx, tenant.Release{ID: plain.ID, Mode: tenant.ReleaseAsIs, ReceptionID: e.id("rc"), Profile: fixed, Now: at(13)}, e.audit(), nil)
	e.wantKind("ReleaseFile naming a version through a receiver that archives as received", err, fault.Precondition)

	r := e.openReception("deid", 2, at(14))
	f := file(r, 2, 31)
	f.Released, f.ReleasedProfile, f.Deidentified = true, other, true
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(15))
	e.ok("AddReceivedFiles of a landed release naming a version", err)
	landed, _, err := e.p.Reception(e.ctx, r.ID)
	e.ok("Reception", err)
	if landed.Route.ReleasedProfile != other {
		e.Errorf("the reception a landed release joined keeps %+v, want the version named", landed.Route)
	}
}
