package tenanttest

import (
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// asReceivedFlags reads every scan and session of the catalog, fails the
// test unless each row's AsReceived is what its instances say (a scan: one
// of them records no profile version; a session: one of its scans is so),
// and returns the flags by scan label and by session label, so a caller can
// also name the answer it expects.
func (e *cenv) asReceivedFlags(when string) map[string]bool {
	e.Helper()
	out := map[string]bool{}
	sessions := map[string]bool{}
	scans := allPages(e.penv, 100, func(pg master.Page) ([]tenant.CatalogScan, string, error) {
		got, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, pg)
		return got.Items, got.Next, err
	})
	for _, sc := range scans {
		path := label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
		held := allPages(e.penv, 100, func(pg master.Page) ([]tenant.ScanInstance, string, error) {
			return e.p.ListHeldInstances(e.ctx, path, pg)
		})
		want := false
		for _, in := range held {
			want = want || in.Profile.IsZero()
		}
		if sc.AsReceived != want {
			e.Errorf("%s: scan %s says as received %v, its %d instances say %v", when, sc.Label, sc.AsReceived, len(held), want)
		}
		got, err := e.p.CatalogScan(e.ctx, path)
		e.ok("CatalogScan", err)
		if got.AsReceived != sc.AsReceived {
			e.Errorf("%s: scan %s read by its labels says %v, in a list %v", when, sc.Label, got.AsReceived, sc.AsReceived)
		}
		out[sc.Label] = want
		sessions[sc.SessionLabel] = sessions[sc.SessionLabel] || want
	}
	all := allPages(e.penv, 100, func(pg master.Page) ([]tenant.CatalogSession, string, error) {
		got, err := e.p.ListSessions(e.ctx, tenant.SessionFilter{Order: tenant.OrderLabel}, pg)
		return got.Items, got.Next, err
	})
	for _, se := range all {
		if se.AsReceived != sessions[se.Label] {
			e.Errorf("%s: session %s says as received %v, its scans say %v", when, se.Label, se.AsReceived, sessions[se.Label])
		}
		got, err := e.p.CatalogSession(e.ctx, se.SubjectLabel, se.Label)
		e.ok("CatalogSession", err)
		if got.AsReceived != se.AsReceived {
			e.Errorf("%s: session %s read by its labels says %v, in a list %v", when, se.Label, got.AsReceived, se.AsReceived)
		}
		out[se.Label] = se.AsReceived
	}
	return out
}

// wantFlags fails unless the flags by label are the ones given.
func (e *cenv) wantFlags(when string, got map[string]bool, want map[string]bool) {
	e.Helper()
	for k, w := range want {
		if got[k] != w {
			e.Errorf("%s: %s is as received %v, want %v", when, k, got[k], w)
		}
	}
}

// testAsReceivedFlag guards the as-received flag of the catalog's scan and
// session rows (CatalogScan.AsReceived, CatalogSession.AsReceived), which a
// list for a key that may not learn original values reads in place of the
// instances: after each write that changes an instance row the flag is
// what the instances say, in the same transaction. The writes are an
// archive of a new scan and of an instance added to a scan, the delete of
// an instance, of a scan and of a session and their undo, a replace giving
// an instance the provenance of a de-identified revision and of an
// as-received one, and an edit that gives an instance its profile version.
func testAsReceivedFlag(e *cenv) {
	e.addReceiver("recv", "store", 11112)
	v1, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("research", "one"), at(0), e.audit(), nil)
	e.ok("CreateProfileVersion 1", err)
	v2, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("research", "two"), at(0), e.audit(), nil)
	e.ok("CreateProfileVersion 2", err)
	scanOf := func(study, i int) string { return "scan-" + uidOf(2, study*100+i) }

	// A new scan's flag is set by the commit that makes it: study 1 holds
	// two scans de-identified.
	e.profile = v1.Ref()
	e.finish(e.archive("recv", 1, 2, 2, 0, at(0), "ses-1"))
	flags := e.asReceivedFlags("two scans archived de-identified")
	e.wantFlags("two scans archived de-identified", flags, map[string]bool{scanOf(1, 0): false, scanOf(1, 1): false, "ses-1": false})

	// An instance archived as received, added to a scan that holds
	// de-identified ones, turns the scan's and its session's flags on.
	e.profile = tenant.ProfileRef{}
	plain := e.archive("recv", 1, 1, 1, 5000, at(10), "ses-1")
	e.finish(plain)
	flags = e.asReceivedFlags("an instance as received added")
	e.wantFlags("an instance as received added", flags, map[string]bool{scanOf(1, 0): true, scanOf(1, 1): false, "ses-1": true})

	// A second session archived as received entire, and a third de-identified.
	studyTwo := e.archive("recv", 2, 2, 1, 10000, at(100), "ses-2")
	e.finish(studyTwo)
	e.profile = v1.Ref()
	studyThree := e.archive("recv", 3, 2, 1, 20000, at(200), "ses-3")
	e.finish(studyThree)
	flags = e.asReceivedFlags("three sessions archived")
	e.wantFlags("three sessions archived", flags, map[string]bool{scanOf(2, 0): true, scanOf(2, 1): true, "ses-2": true,
		scanOf(3, 0): false, "ses-3": false})

	// The delete of the one instance archived as received turns the flags
	// of its scan and its session off, and its undo turns them on again.
	uid := plain.files[0].SOPInstanceUID
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: scanOf(1, 0), Instance: uid}, 300)
	c = e.commit(c.ID, 310, map[string]tenant.SHA256{plain.files[0].SeriesUID: sum(1)})
	flags = e.asReceivedFlags("the instance as received deleted")
	e.wantFlags("the instance as received deleted", flags, map[string]bool{scanOf(1, 0): false, "ses-1": false})
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "the wrong instance", at(320), e.audit(), nil)
	e.ok("UndoCorrection of an instance's delete", err)
	flags = e.asReceivedFlags("the delete of the instance undone")
	e.wantFlags("the delete of the instance undone", flags, map[string]bool{scanOf(1, 0): true, "ses-1": true})

	// The delete of a scan: the session keeps the flag of the scans that
	// stay, and the undo puts the scan back with its own flag.
	c = e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: scanOf(1, 0)}, 400)
	c = e.commit(c.ID, 410, nil)
	flags = e.asReceivedFlags("the scan as received deleted")
	e.wantFlags("the scan as received deleted", flags, map[string]bool{scanOf(1, 1): false, "ses-1": false})
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "the wrong scan", at(420), e.audit(), nil)
	e.ok("UndoCorrection of a scan's delete", err)
	flags = e.asReceivedFlags("the delete of the scan undone")
	e.wantFlags("the delete of the scan undone", flags, map[string]bool{scanOf(1, 0): true, scanOf(1, 1): false, "ses-1": true})
	c = e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: scanOf(1, 1)}, 500)
	c = e.commit(c.ID, 510, nil)
	flags = e.asReceivedFlags("the scan de-identified deleted")
	e.wantFlags("the scan de-identified deleted", flags, map[string]bool{scanOf(1, 0): true, "ses-1": true})
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "the wrong scan", at(520), e.audit(), nil)
	e.ok("UndoCorrection of a second scan's delete", err)
	e.asReceivedFlags("the second delete of a scan undone")

	// The delete of a whole session and its undo.
	c = e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 600)
	c = e.commit(c.ID, 610, nil)
	flags = e.asReceivedFlags("a session deleted")
	if _, there := flags["ses-2"]; there {
		e.Errorf("a deleted session is still listed")
	}
	_, err = e.p.UndoCorrection(e.ctx, c.ID, "the wrong session", at(620), e.audit(), nil)
	e.ok("UndoCorrection of a session's delete", err)
	flags = e.asReceivedFlags("the delete of the session undone")
	e.wantFlags("the delete of the session undone", flags, map[string]bool{scanOf(2, 0): true, "ses-2": true})

	// A replace gives the instance the provenance of its new revision: an
	// instance archived as received replaced by a de-identified revision
	// turns its scan's flag off (its session's stays while another scan of
	// it is as received), and a de-identified one replaced by a revision
	// archived as received turns it on.
	holds := 0
	replace := func(study, i, s int, a archived, p *tenant.InstanceProvenance) {
		e.Helper()
		in := a.files[i]
		holds++
		q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: "recv", ReceptionID: a.r.ID,
			Size: 777, SHA256: sum(4242 + holds), KeptSHA256: sum(4300 + holds), SOPClassUID: in.SOPClassUID, SOPInstanceUID: in.SOPInstanceUID,
			StudyUID: in.StudyUID, SeriesUID: in.SeriesUID, TransferSyntaxUID: in.TransferSyntaxUID, Incoming: "quarantine/q" + in.SOPInstanceUID,
			ContentKept: true, Quarantined: at(s - 10)}
		_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
		e.ok("HoldFiles", err)
		c, made, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(s), Kind: record.KindReplace, Quarantine: q.ID,
			NewSHA256: sum(5000 + holds), NewSize: 780, RestoreLine: []byte(`{"line":1}`), Reason: "corrected re-send", Now: at(s)}, e.audit(), nil)
		e.ok("PrepareCorrection of a replace", err)
		if !made {
			e.Fatal("the replace was not made")
		}
		_, err = e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: c.ID, Now: at(s + 5), Grace: grace,
			Manifests: map[string]tenant.SHA256{in.SeriesUID: sum(6000 + holds)}, Replaced: p})
		e.ok("CommitCorrection of a replace", err)
	}
	replace(2, 0, 700, studyTwo, &tenant.InstanceProvenance{Profile: v2.Ref()})
	flags = e.asReceivedFlags("an instance as received replaced by a de-identified revision")
	e.wantFlags("an instance as received replaced by a de-identified revision", flags, map[string]bool{scanOf(2, 0): false, scanOf(2, 1): true, "ses-2": true})
	replace(2, 1, 800, studyTwo, &tenant.InstanceProvenance{Profile: v2.Ref()})
	flags = e.asReceivedFlags("the other instance replaced too")
	e.wantFlags("the other instance replaced too", flags, map[string]bool{scanOf(2, 1): false, "ses-2": false})
	replace(3, 0, 900, studyThree, &tenant.InstanceProvenance{})
	flags = e.asReceivedFlags("a de-identified instance replaced by a revision as received")
	e.wantFlags("a de-identified instance replaced by a revision as received", flags, map[string]bool{scanOf(3, 0): true, scanOf(3, 1): false, "ses-3": true})

	// An edit that gives an instance its profile version (a profile re-run
	// is one) turns the flag off: study 4 is archived as received, one scan
	// of two instances, and the edit's new revisions name research@1.
	e.profile = tenant.ProfileRef{}
	e.finish(e.archive("recv", 4, 1, 2, 30000, at(1000), "ses-4"))
	flags = e.asReceivedFlags("a fourth session archived as received")
	e.wantFlags("a fourth session archived as received", flags, map[string]bool{scanOf(4, 0): true, "ses-4": true})
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-4", Scan: scanOf(4, 0)}
	ec, targets, err := e.prepareEdit(target, 1100)
	e.ok("PrepareCorrection of an edit", err)
	cc := tenant.CorrectionCommit{ID: ec.ID, Now: at(1110), Grace: grace, Manifests: map[string]tenant.SHA256{}}
	for i, t := range targets {
		cc.Edits = append(cc.Edits, tenant.EditWritten{UID: t.UID, SHA256: sum(8000 + i), Size: 900,
			Line: []byte(`{"profile":"` + v1.Ref().Name + "@" + strconv.Itoa(int(v1.Ref().Version)) + `"}`)})
		cc.Manifests[t.SeriesUID] = sum(9000)
	}
	_, err = e.p.CommitCorrection(e.ctx, cc)
	e.ok("CommitCorrection of an edit", err)
	flags = e.asReceivedFlags("an edit gave the instances a profile version")
	e.wantFlags("an edit gave the instances a profile version", flags, map[string]bool{scanOf(4, 0): false, "ses-4": false})

	// A relabel changes no instance and leaves the flags where they are.
	before := e.asReceivedFlags("before the relabel")
	rl, made, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "ses-1b", 1200)
	e.ok("PrepareCorrection of a relabel", err)
	if !made {
		e.Fatal("the relabel was not made")
	}
	scans, err := e.p.CorrectionScans(e.ctx, rl.ID)
	e.ok("CorrectionScans", err)
	manifests := map[string]tenant.SHA256{}
	for i, sc := range scans {
		manifests[sc.SeriesUID] = sum(900 + i)
	}
	e.commit(rl.ID, 1210, manifests)
	after := e.asReceivedFlags("after the relabel")
	for k, w := range before {
		if k == "ses-1" {
			k = "ses-1b"
		}
		if after[k] != w {
			e.Errorf("after a relabel %s is as received %v, was %v", k, after[k], w)
		}
	}
}
