package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testReplaceProvenance guards that a replace's publish gives the
// instance's row its new revision's provenance, so a sender judges the
// bytes the row points to (tenant.SendRefusal): an image the replaced
// revision had filled, replaced by one a later version accepted at risk,
// is refused as pixel_risk_accepted; a de-identified instance replaced by
// bytes archived as received is refused as as_received. A publish that
// names no provenance is Invalid and changes nothing, one naming a version
// the tenant does not have is a precondition failure, and one of another
// kind naming it is Invalid. A replace abandoned before its publish, the
// way a replace is taken back, leaves the row's provenance as it was.
func testReplaceProvenance(e *cenv) {
	e.addReceiver("recv", "store", 11112)
	v1, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("research", "one"), at(0), e.audit(), nil)
	e.ok("CreateProfileVersion 1", err)
	v2, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("research", "two"), at(0), e.audit(), nil)
	e.ok("CreateProfileVersion 2", err)
	e.profile = v1.Ref()
	// One instance in each of four scans: a scan a replace published holds
	// until its purge, so each case has its own.
	a := e.archive("recv", 1, 4, 1, 0, at(0), "ses-1")
	e.finish(a)
	e.profile = tenant.ProfileRef{}
	scanOf := func(i int) label.Path {
		return label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100+i)}
	}
	index := map[string]int{}
	for i, f := range a.files {
		index[f.SOPInstanceUID] = i
	}
	row := func(uid string) tenant.ScanInstance {
		e.Helper()
		h, err := e.p.HeldInstance(e.ctx, scanOf(index[uid]), uid)
		e.ok("HeldInstance", err)
		return h
	}
	// prepare holds a different file of the instance and prepares its
	// replace at second s.
	prepare := func(i, s int) tenant.Correction {
		e.Helper()
		in := a.files[i]
		q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: "recv", ReceptionID: a.r.ID,
			Size: 777, SHA256: sum(4242 + i), KeptSHA256: sum(4300 + i), SOPClassUID: in.SOPClassUID, SOPInstanceUID: in.SOPInstanceUID,
			StudyUID: in.StudyUID, SeriesUID: in.SeriesUID, TransferSyntaxUID: in.TransferSyntaxUID, Incoming: "quarantine/q" + in.SOPInstanceUID,
			ContentKept: true, Quarantined: at(s - 10)}
		_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
		e.ok("HoldFiles", err)
		c, made, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindReplace, Quarantine: q.ID,
			NewSHA256: sum(5000 + i), NewSize: 780, RestoreLine: []byte(`{"line":1}`), Reason: "corrected re-send", Now: at(s)}, e.audit(), nil)
		e.ok("PrepareCorrection of a replace", err)
		if !made {
			e.Fatal("the replace was not made")
		}
		return c
	}
	publish := func(c tenant.Correction, s int, p *tenant.InstanceProvenance) error {
		series := a.files[index[c.Target.Instance]].SeriesUID
		_, err := e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: c.ID, Now: at(s), Grace: grace,
			Manifests: map[string]tenant.SHA256{series: sum(6000 + s)}, Replaced: p})
		return err
	}
	refusal := func(uid string) tenant.JobFailureCode {
		code, _ := tenant.SendRefusal(row(uid).Provenance(), false)
		return code
	}

	// Filled under version 1, replaced by bytes version 2 accepted at risk.
	risky := a.files[0].SOPInstanceUID
	if code := refusal(risky); code != "" {
		e.Fatalf("the first revision is refused as %s", code)
	}
	c := prepare(0, 500)
	e.wantKind("a replace published naming no provenance", publish(c, 510, nil), fault.Invalid)
	e.wantKind("a replace naming a version not stored",
		publish(c, 511, &tenant.InstanceProvenance{Profile: tenant.ProfileRef{Name: "research", Version: 9}}), fault.Precondition)
	e.wantState(c.ID, record.StatePrepared)
	if h := row(risky); h.Profile != v1.Ref() || h.PixelRiskAccepted {
		e.Fatalf("a refused publish changed the row to %v, risk %v", h.Profile, h.PixelRiskAccepted)
	}
	e.ok("the replace's publish", publish(c, 520, &tenant.InstanceProvenance{Profile: v2.Ref(), PixelRiskAccepted: true}))
	if h := row(risky); h.Profile != v2.Ref() || !h.PixelRiskAccepted {
		e.Errorf("after the replace the row names %v, risk %v; want %v, true", h.Profile, h.PixelRiskAccepted, v2.Ref())
	}
	if code := refusal(risky); code != tenant.FailurePixelRiskAccepted {
		e.Errorf("the replaced revision accepted at risk is refused as %q, want pixel_risk_accepted", code)
	}

	// De-identified, replaced by bytes archived as received.
	identified := a.files[1].SOPInstanceUID
	c = prepare(1, 600)
	e.ok("the replace's publish", publish(c, 610, &tenant.InstanceProvenance{}))
	if h := row(identified); !h.Profile.IsZero() || h.PixelRiskAccepted {
		e.Errorf("after a replace from as received the row names %v, risk %v", h.Profile, h.PixelRiskAccepted)
	}
	if code := refusal(identified); code != tenant.FailureAsReceived {
		e.Errorf("the replaced revision archived as received is refused as %q, want as_received", code)
	}

	// A replace taken back before its publish leaves the row as it was.
	kept := a.files[2].SOPInstanceUID
	c = prepare(2, 700)
	_, err = e.p.AbandonCorrection(e.ctx, c.ID, tenant.StateReasonStopped, at(710))
	e.ok("AbandonCorrection of a replace", err)
	if h := row(kept); h.Profile != v1.Ref() || h.PixelRiskAccepted {
		e.Errorf("after a replace abandoned the row names %v, risk %v; want %v, false", h.Profile, h.PixelRiskAccepted, v1.Ref())
	}

	// A provenance on another kind's publish.
	d := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: scanOf(3).Scan,
		Instance: a.files[3].SOPInstanceUID}, 800)
	_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: d.ID, Now: at(810), Grace: grace, Replaced: &tenant.InstanceProvenance{}})
	e.wantKind("a delete's publish naming a provenance", err, fault.Invalid)
}
