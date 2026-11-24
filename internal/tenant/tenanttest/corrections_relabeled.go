package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wantRelabeled fails unless RelabeledTo answers p moved to want by the
// relabel id, or, with id empty, not moved.
func (e *cenv) wantRelabeled(what string, p label.Path, id string, want label.Path) {
	e.Helper()
	got, ok, err := e.p.RelabeledTo(e.ctx, p)
	e.ok("RelabeledTo "+what, err)
	switch {
	case id == "" && ok:
		e.Fatalf("%s: RelabeledTo answers %+v, want nothing moved", what, got)
	case id != "" && (!ok || got.Correction != id || got.To != want):
		e.Fatalf("%s: RelabeledTo answers %+v (%v), want %+v by %s", what, got, ok, want, id)
	}
}

// purgeNow purges a committed correction at second s, so the next one on
// its target may be prepared.
func (e *cenv) purgeNow(id string, s int) {
	e.Helper()
	_, err := e.p.StartPurge(e.ctx, id, at(s), &master.AuditEntry{ID: e.id("a"), Time: at(s), Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil)
	e.ok("StartPurge", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: id, Now: at(s + 1)})
	e.ok("FinishPurge", err)
}

// testRelabeledToChains guards the chains an older relabel starts: a
// session relabeled, then its subject, is found by its first labels under
// both new ones (the subject's relabel, the newest that moved them, leads
// nowhere alone); and a subject label given up twice (relabeled, new data
// arrived under it, relabeled again) points a session of the first data to
// the first relabel's subject and one of the second data, like the subject
// label itself, to the second's.
func testRelabeledToChains(e *cenv) {
	e.setupTwo()
	s, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "ses-9", 500)
	e.ok("PrepareCorrection of a session's relabel", err)
	e.commit(s.ID, 501, nil)
	e.purgeNow(s.ID, 502)
	c, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-02", 600)
	e.ok("PrepareCorrection of the subject's relabel", err)
	e.commit(c.ID, 601, nil)
	e.wantRelabeled("a session relabeled before its subject", label.Path{Subject: "sub-01", Session: "ses-1"}, c.ID,
		label.Path{Subject: "sub-02", Session: "ses-9"})
	e.wantRelabeled("the subject's other session", label.Path{Subject: "sub-01", Session: "ses-2"}, c.ID,
		label.Path{Subject: "sub-02", Session: "ses-2"})
	e.purgeNow(c.ID, 602)

	// New data under the label given up, relabeled in turn.
	e.finish(e.archive("recv", 3, 1, 1, 20000, at(700), "ses-3"))
	again, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-03", 800)
	e.ok("PrepareCorrection of the label's second relabel", err)
	e.commit(again.ID, 801, nil)
	e.wantRelabeled("the subject label given up twice", label.Path{Subject: "sub-01"}, again.ID, label.Path{Subject: "sub-03"})
	e.wantRelabeled("a session of the second data", label.Path{Subject: "sub-01", Session: "ses-3"}, again.ID,
		label.Path{Subject: "sub-03", Session: "ses-3"})
	e.wantRelabeled("a session of the first data", label.Path{Subject: "sub-01", Session: "ses-2"}, c.ID,
		label.Path{Subject: "sub-02", Session: "ses-2"})
	got, ok, err := e.p.RelabeledTo(e.ctx, label.Path{Subject: "sub-01", Session: "ses-404"})
	e.ok("RelabeledTo of a session no chain holds", err)
	if ok || got != (tenant.Relabeled{}) {
		e.Fatalf("a session no chain holds answers %+v (%v), want the zero answer", got, ok)
	}
}

// testRelabeledTo guards where old labels point: nothing while a relabel
// is only prepared; once committed, a subject, a session and a scan under
// the old subject label point to the new labels, at the depth asked, while
// the old label in another case, a label never relabeled and a malformed
// one do not; a session relabeled after its subject is followed through
// both relabels, named by the last; and once a delete hides the data, the
// old label points nowhere.
func testRelabeledTo(e *cenv) {
	e.setupTwo()
	old := label.Path{Subject: "sub-01"}
	e.wantRelabeled("before any relabel", old, "", label.Path{})
	c, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-02", 500)
	e.ok("PrepareCorrection of a relabel", err)
	e.wantRelabeled("while the relabel is prepared", old, "", label.Path{})
	scans, err := e.p.CorrectionScans(e.ctx, c.ID)
	e.ok("CorrectionScans", err)
	e.commit(c.ID, 600, nil)
	e.wantRelabeled("a subject", old, c.ID, label.Path{Subject: "sub-02"})
	e.wantRelabeled("a session", label.Path{Subject: "sub-01", Session: "ses-1"}, c.ID, label.Path{Subject: "sub-02", Session: "ses-1"})
	e.wantRelabeled("a scan", scans[0].Path, c.ID, scans[0].NewPath)
	e.wantRelabeled("the old label in another case", label.Path{Subject: "SUB-01"}, "", label.Path{})
	e.wantRelabeled("a label never relabeled", label.Path{Subject: "sub-77"}, "", label.Path{})
	e.wantRelabeled("a session the subject never had", label.Path{Subject: "sub-01", Session: "ses-404"}, "", label.Path{})
	_, _, err = e.p.RelabeledTo(e.ctx, label.Path{Subject: "sub-01", Scan: "x"})
	e.wantKind("RelabeledTo of a scan with no session", err, fault.Invalid)
	_, _, err = e.p.RelabeledTo(e.ctx, label.Path{Subject: "../x"})
	e.wantKind("RelabeledTo of a malformed label", err, fault.Invalid)

	e.purgeNow(c.ID, 700)
	s, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-02", Session: "ses-1"}, "ses-9", 800)
	e.ok("PrepareCorrection of a session's relabel", err)
	e.commit(s.ID, 801, nil)
	e.wantRelabeled("a session through two relabels", label.Path{Subject: "sub-01", Session: "ses-1"}, s.ID,
		label.Path{Subject: "sub-02", Session: "ses-9"})
	e.wantRelabeled("the session's own old labels", label.Path{Subject: "sub-02", Session: "ses-1"}, s.ID,
		label.Path{Subject: "sub-02", Session: "ses-9"})
	e.wantRelabeled("the subject after its session's relabel", old, c.ID, label.Path{Subject: "sub-02"})

	e.purgeNow(s.ID, 900)
	d := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-02"}, 1000)
	e.commit(d.ID, 1002, nil)
	e.wantRelabeled("a subject deleted since", old, "", label.Path{})
}
