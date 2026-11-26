package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testPreparedKeepsTheMetadataDigest guards the digest of a scan's
// metadata file while a relabel of the scan is prepared, whose publish
// keeps the digest of the file it copied: a pass's forget of the digest is
// refused (unavailable, the digest kept) so the pass puts no file the
// publish would name wrongly, and its record of a file records nothing;
// another scan's digest is forgotten and recorded as before; once the
// relabel is abandoned both work on the scan again.
func testPreparedKeepsTheMetadataDigest(e *renv) {
	e.replicaSetup()
	series, other := uidOf(2, 930), uidOf(2, 931)
	a := e.arrive(series, 20, 30)
	a.c.SidecarSHA256 = sum(500)
	e.ok("commit with a metadata digest", a.commit(31))
	o := e.arrive(other, 21, 32)
	o.c.SidecarSHA256 = sum(600)
	e.ok("commit of another scan with a metadata digest", o.commit(33))
	p := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	id, err := tenant.NewCorrectionID(at(40))
	e.ok("NewCorrectionID", err)
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindRelabel,
		Target: tenant.CorrectionTarget{Subject: p.Subject, Session: p.Session, Scan: p.Scan}, ToLabel: "renamed",
		Reason: "wrong series name", Now: at(40)}, e.audit(), nil)
	e.ok("PrepareCorrection of a relabel", err)

	_, err = e.p.ForgetSidecar(e.ctx, series, "store")
	e.wantKind("ForgetSidecar while a relabel of the scan is prepared", err, fault.Unavailable)
	if got := e.sidecarOfScan(p); got != sum(500) {
		e.Errorf("after a refused forget the digest reads %v; want it kept", got)
	}
	if recorded, err := e.p.RecordSidecarRewritten(e.ctx, series, "store", sum(501), at(41)); err != nil || recorded {
		e.Errorf("RecordSidecarRewritten while a relabel of the scan is prepared = %v, %v; want nothing recorded", recorded, err)
	}
	if got := e.sidecarOfScan(p); got != sum(500) {
		e.Errorf("after a record refused the digest reads %v; want it kept", got)
	}
	if recorded, err := e.p.RecordSidecarRewritten(e.ctx, other, "store", sum(601), at(42)); err != nil || !recorded {
		e.Errorf("RecordSidecarRewritten of a scan the relabel does not touch = %v, %v; want it recorded", recorded, err)
	}

	if _, err := e.p.AbandonCorrection(e.ctx, id, "asked by mistake", at(43)); err != nil {
		e.Fatalf("AbandonCorrection: %v", err)
	}
	if dropped, err := e.p.ForgetSidecar(e.ctx, series, "store"); err != nil || !dropped {
		e.Errorf("ForgetSidecar once the relabel is abandoned = %v, %v; want the digest dropped", dropped, err)
	}
	if recorded, err := e.p.RecordSidecarRewritten(e.ctx, series, "store", sum(502), at(44)); err != nil || !recorded {
		e.Errorf("RecordSidecarRewritten once the relabel is abandoned = %v, %v; want it recorded", recorded, err)
	}
	if got := e.sidecarOfScan(p); got != sum(502) {
		e.Errorf("once the relabel is abandoned the digest reads %v; want the one recorded", got)
	}
}
