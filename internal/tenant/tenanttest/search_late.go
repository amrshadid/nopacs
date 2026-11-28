package tenanttest

import (
	"slices"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// cenvOf returns the corrections' test helpers over e.
func cenvOf(e *penv) *cenv {
	ev, ok := e.p.(EventStore)
	if !ok {
		e.Fatal("the store is not an event store")
	}
	return &cenv{senv: &senv{eenv: &eenv{penv: e, ev: ev}}}
}

// scanIDOf returns the catalog's id of the scan at path.
func (e *penv) scanIDOf(path label.Path) int64 {
	e.Helper()
	scans, err := e.p.SearchBackfillScans(e.ctx, 0, tenant.MaxSearchBackfillPage)
	e.ok("SearchBackfillScans", err)
	for _, s := range scans {
		if s.Path == path && s.Correction == "" {
			return s.ID
		}
	}
	e.Fatalf("the catalog holds no scan %v", path)
	return 0
}

// testSearchBackfillRacesADelete guards a scan a delete hides between the
// listing of a page and its write: the page is refused as stale, whole,
// where the job would have gone past a scan it never filled; listed again,
// the scan comes from the hidden tables, is filled there, and once the
// delete is undone it is found by the new attribute.
func testSearchBackfillRacesADelete(e *penv, h SearchHooks) {
	c := cenvOf(e)
	c.setSearchRows()
	c.setupTwo()
	s1 := "sub-01/ses-1/scan-" + uidOf(2, 100)
	h.Building(e.T, e.p, backfillField)
	j := e.queueBackfill("sb-1")
	listed, err := e.p.SearchBackfillScans(e.ctx, 0, 100)
	e.ok("SearchBackfillScans", err)
	rowsOf := func(scans []tenant.SearchBackfillScan) []tenant.SearchBackfillRow {
		var rows []tenant.SearchBackfillRow
		for _, s := range scans {
			echo := "20"
			if s.Path.Scan == "scan-"+uidOf(2, 100) {
				echo = "21"
			}
			rows = append(rows, tenant.SearchBackfillRow{ID: s.ID, Correction: s.Correction, Manifest: s.Manifest,
				Row: search.Extract(dataSetOf(e, map[string]string{backfillField: echo, "Modality": "MR"}), "2.25.0")})
		}
		return rows
	}
	stale := rowsOf(listed)
	del := c.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}, 500)
	c.commit(del.ID, 600, nil)
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: stale, Now: at(610)})
	e.wantIs("a page with a scan deleted after it was listed", err, tenant.ErrSearchBackfillStale, fault.Precondition)
	if bj, err := e.p.SearchBackfillJob(e.ctx, j.ID); err != nil || bj.After != 0 {
		e.Fatalf("a refused page moved the job to %+v (%v)", bj, err)
	}
	again, err := e.p.SearchBackfillScans(e.ctx, 0, 100)
	e.ok("SearchBackfillScans", err)
	if !slices.ContainsFunc(again, func(s tenant.SearchBackfillScan) bool { return s.Correction == del.ID }) {
		e.Fatalf("the scan the delete hid is not listed again from the hidden tables: %+v", again)
	}
	_, err = e.p.ApplySearchBackfill(e.ctx, tenant.SearchBackfillApply{JobID: j.ID, After: 0, Rows: rowsOf(again), Now: at(620)})
	e.ok("ApplySearchBackfill", err)
	_, err = e.p.EndSearchBackfill(e.ctx, j.ID, at(630))
	e.ok("EndSearchBackfill", err)
	_, err = e.p.UndoCorrection(e.ctx, del.ID, "the wrong scan", at(700), e.audit(), nil)
	e.ok("UndoCorrection", err)
	_, paths := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition("EchoTime == 21"), Now: T0}, 10)
	if !slices.Equal(paths, []string{s1}) {
		e.Errorf("after the race and the undo EchoTime == 21 finds %v, want %s", paths, s1)
	}
}

// testSearchPurgeLeavesNoOldValue guards the purge of an edit: the values
// the files held before it go with it, and no table of the database, the
// search rows kept for its undo included, holds one afterwards.
func testSearchPurgeLeavesNoOldValue(e *penv, h SearchHooks) {
	c := cenvOf(e)
	c.setSearchRows()
	c.setupTwo()
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}
	cor, targets, err := c.prepareEdit(target, 500)
	e.ok("prepare", err)
	cc := tenant.CorrectionCommit{ID: cor.ID, Now: at(600), Grace: grace, Manifests: map[string]tenant.SHA256{}, Search: map[string]search.Row{}}
	for i, tg := range targets {
		cc.Edits = append(cc.Edits, tenant.EditWritten{UID: tg.UID, SHA256: sum(8000 + i), Size: 900, Line: []byte(`{"new":"` + tg.UID + `"}`)})
		cc.Manifests[tg.SeriesUID] = sum(9000)
	}
	cc.Search[targets[0].SeriesUID] = search.Extract(dataSetOf(e, map[string]string{"Modality": "MR", "SeriesDescription": "Scrubbed"}), targets[0].UID)
	_, err = e.p.CommitCorrection(e.ctx, cc)
	e.ok("commit", err)
	const old = "T1 axial"
	if n := h.CountText(e.T, e.p, old); n == 0 {
		e.Fatalf("before the purge no row holds the value the edit replaced (%q): the test proves nothing", old)
	}
	c.purgeNow(cor.ID, 700)
	if n := h.CountText(e.T, e.p, old); n != 0 {
		e.Errorf("after the edit is purged %d row(s) still hold the pre-edit value %q", n, old)
	}
}

// testSearchUndoAfterLowerArrival guards the undo of a delete of the instance
// a row was read from, when a file with a still lower SOP Instance UID has
// arrived since and given the scan its row: the undo keeps that row, as the
// lowest-instance rule does at archive, where it wrote the row it had kept.
func testSearchUndoAfterLowerArrival(e *penv, h SearchHooks) {
	c := cenvOf(e)
	c.setSearchRows()
	a, _ := c.setupTwo()
	series := uidOf(2, 100)
	scan := "sub-01/ses-1/scan-" + series
	first, second := a.files[0].SOPInstanceUID, a.files[1].SOPInstanceUID
	d := c.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series, Instance: first}, 500)
	next := search.Extract(dataSetOf(e, map[string]string{"Modality": "MR", "Manufacturer": "NEXTCO"}), second)
	_, err := e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: d.ID, Now: at(510), Grace: grace, Search: map[string]search.Row{series: next}})
	e.ok("commit", err)
	c.arrive(h, series, "LOWCO")
	c.wantFound("arrival", `Manufacturer == "LOWCO"`, scan)
	_, err = e.p.UndoCorrection(e.ctx, d.ID, "wrong", at(600), e.audit(), nil)
	e.ok("undo", err)
	c.wantFound("the delete undone", `Manufacturer == "LOWCO"`, scan)
	c.wantFound("the delete undone", `Manufacturer == "ACME"`, "sub-01/ses-1/scan-"+uidOf(2, 101))
}

// testSearchEditUndoAfterLowerArrival is the same for an edit: the row an
// undo would give back was read from a file a lower one has since replaced.
func testSearchEditUndoAfterLowerArrival(e *penv, h SearchHooks) {
	c := cenvOf(e)
	c.setSearchRows()
	c.setupTwo()
	series := uidOf(2, 100)
	scan := "sub-01/ses-1/scan-" + series
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	cor, targets, err := c.prepareEdit(target, 500)
	e.ok("prepare", err)
	cc := tenant.CorrectionCommit{ID: cor.ID, Now: at(600), Grace: grace, Manifests: map[string]tenant.SHA256{}, Search: map[string]search.Row{}}
	for i, tg := range targets {
		cc.Edits = append(cc.Edits, tenant.EditWritten{UID: tg.UID, SHA256: sum(8000 + i), Size: 900, Line: []byte(`{"new":"` + tg.UID + `"}`)})
		cc.Manifests[tg.SeriesUID] = sum(9000)
	}
	cc.Search[series] = search.Extract(dataSetOf(e, map[string]string{"Modality": "MR", "Manufacturer": "NEWCO"}), targets[0].UID)
	_, err = e.p.CommitCorrection(e.ctx, cc)
	e.ok("commit", err)
	c.arrive(h, series, "LOWCO")
	ed := c.editsOf()
	_, err = ed.BeginEditUndo(e.ctx, cor.ID, "the values were right", at(700), e.audit(), nil)
	e.ok("BeginEditUndo", err)
	manifests := map[string]tenant.SHA256{}
	for _, t := range targets {
		manifests[t.SeriesUID] = sum(9710)
	}
	_, err = ed.UndoEdit(e.ctx, tenant.EditUndo{ID: cor.ID, Now: at(710), Manifests: manifests})
	e.ok("UndoEdit", err)
	c.wantFound("the edit undone", `Manufacturer == "LOWCO"`, scan)
	c.wantFound("the edit undone", `Manufacturer == "ACME"`, "sub-01/ses-1/scan-"+uidOf(2, 101))
}

// arrive writes the row a file with the lowest SOP Instance UID of all gives
// the scan of series, with the manufacturer, as the archive step writes it.
func (e *cenv) arrive(h SearchHooks, series, manufacturer string) {
	e.Helper()
	path := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series}
	id := e.scanIDOf(path)
	low := search.Extract(dataSetOf(e.penv, map[string]string{"Modality": "MR", "Manufacturer": manufacturer}), "1.0.0.1")
	h.Exec(e.T, e.p, search.UpsertSQL(search.Lowest), search.Args(id, low)...)
}
