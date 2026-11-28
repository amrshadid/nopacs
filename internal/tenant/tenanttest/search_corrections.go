package tenanttest

import (
	"slices"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// searchRowsOfTwo are the headers of setupTwo's three scans, which
// setSearchRows commits them with: study 1's two scans of sub-01/ses-1 and
// study 2's one of sub-01/ses-2.
var searchRowsOfTwo = map[[2]int]map[string]string{
	{1, 0}: {"Modality": "MR", "SeriesDescription": "T1 axial", "Manufacturer": "ACME", "SliceThickness": "1", "ImageType": `ORIGINAL\PRIMARY`},
	{1, 1}: {"Modality": "MR", "SeriesDescription": "diffusion", "Manufacturer": "ACME"},
	{2, 0}: {"Modality": "CT", "SeriesDescription": "head", "Manufacturer": "OTHERCO"},
}

// setSearchRows makes archive commit each scan of setupTwo with the search
// row of its header in searchRowsOfTwo; the row's source is the scan's
// first instance, the lowest UID archive gives it.
func (e *cenv) setSearchRows() {
	bases := map[int]int{1: 0, 2: 10000}
	e.rows = func(study, scan int) search.Row {
		vals, ok := searchRowsOfTwo[[2]int{study, scan}]
		if !ok {
			return search.Row{}
		}
		return search.Extract(dataSetOf(e.penv, vals), uidOf(3, bases[study]+1000*scan))
	}
}

// found runs a search over scans and returns the paths it finds, sorted.
func (e *cenv) found(cond string) []string {
	e.Helper()
	_, got := e.searchAll(tenant.SearchQuery{Level: query.LevelScan, Condition: e.searchCondition(cond), Now: T0}, 100)
	slices.Sort(got)
	return got
}

// wantFound fails the test unless the condition finds exactly the scans.
func (e *cenv) wantFound(when, cond string, want ...string) {
	e.Helper()
	slices.Sort(want)
	if got := e.found(cond); !slices.Equal(got, want) {
		e.Errorf("%s: %q finds %v, want %v", when, cond, got, want)
	}
}

// testSearchFollowsCorrections guards the search table through every
// correction: a delete takes a scan out of every search and its undo puts
// it back (with its values), a purge keeps it out; a relabel is followed by
// what a search answers (the labels are the catalog's); an edit changes
// the values a search finds, and its undo gives the old ones back; a move
// of a session carries its scans' rows with it, found by what is true of
// the session's new subject; a delete of one instance leaves the row.
func testSearchFollowsCorrections(e *cenv) {
	e.setSearchRows()
	e.setupTwo()
	s1, s2, s3 := "sub-01/ses-1/scan-"+uidOf(2, 100), "sub-01/ses-1/scan-"+uidOf(2, 101), "sub-01/ses-2/scan-"+uidOf(2, 200)
	e.wantFound("archived", `Manufacturer == "ACME"`, s1, s2)
	e.wantFound("archived", `SeriesDescription contains "head"`, s3)

	// A delete of one scan, undone, then a delete of its session, purged.
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}, 500)
	c = e.commit(c.ID, 600, nil)
	e.wantFound("a scan deleted", `Manufacturer == "ACME"`, s2)
	e.wantFound("a scan deleted", `ImageType == "PRIMARY"`)
	_, err := e.p.UndoCorrection(e.ctx, c.ID, "the wrong scan", at(700), e.audit(), nil)
	e.ok("UndoCorrection", err)
	e.wantFound("the delete undone", `Manufacturer == "ACME"`, s1, s2)
	e.wantFound("the delete undone", `ImageType == "PRIMARY" and SliceThickness = 1`, s1)
	c = e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 800)
	c = e.commit(c.ID, 900, nil)
	e.wantFound("a session deleted", `Manufacturer == "ACME"`)
	e.wantFound("a session deleted", `Modality == "CT"`, s3)
	if _, err := e.p.StartPurge(e.ctx, c.ID, c.PurgeAfter, nil, nil); err != nil {
		e.Fatalf("StartPurge: %v", err)
	}
	if _, err := e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: c.ID, Now: c.PurgeAfter.Add(1)}); err != nil {
		e.Fatalf("FinishPurge: %v", err)
	}
	e.wantFound("a session purged", `Manufacturer == "ACME"`)
	e.wantFound("a session purged", `has session (session.date absent) or Modality == "CT"`, s3)
}

// testSearchFollowsEdits guards an edit: the row a search finds changes with
// the new header of the instance it was read from, an edit of an instance
// the row was not read from leaves it, and the undo gives the old row back.
func testSearchFollowsEdits(e *cenv) {
	e.setSearchRows()
	e.setupTwo()
	s1 := "sub-01/ses-1/scan-" + uidOf(2, 100)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)}
	c, targets, err := e.prepareEdit(target, 500)
	e.ok("PrepareCorrection of an edit", err)
	cc := tenant.CorrectionCommit{ID: c.ID, Now: at(600), Grace: grace, Manifests: map[string]tenant.SHA256{}, Search: map[string]search.Row{}}
	for i, t := range targets {
		cc.Edits = append(cc.Edits, tenant.EditWritten{UID: t.UID, SHA256: sum(8000 + i), Size: 900, Line: []byte(`{"new":"` + t.UID + `"}`)})
		cc.Manifests[t.SeriesUID] = sum(9000)
	}
	// The row of the edited file with the lowest UID, as an edit's engine reads it.
	cc.Search[targets[0].SeriesUID] = search.Extract(dataSetOf(e.penv, map[string]string{"Modality": "MR", "SeriesDescription": "Scout view",
		"Manufacturer": "NEWCO", "ImageType": `DERIVED\SECONDARY`}), targets[0].UID)
	got, err := e.p.CommitCorrection(e.ctx, cc)
	e.ok("CommitCorrection of an edit", err)
	if got.State != record.StateCommitted {
		e.Fatalf("a published edit is %s", got.State)
	}
	e.wantFound("edited", `Manufacturer == "NEWCO"`, s1)
	e.wantFound("edited", `Manufacturer == "ACME"`, "sub-01/ses-1/scan-"+uidOf(2, 101))
	e.wantFound("edited", `ImageType == "SECONDARY"`, s1)
	e.wantFound("edited", `ImageType == "PRIMARY"`)
	e.wantFound("edited", `SeriesDescription contains "scout"`, s1)
	e.wantFound("edited", `SliceThickness present and Modality == "MR"`)
	ed := e.editsOf()
	_, err = ed.BeginEditUndo(e.ctx, c.ID, "the values were right", at(700), e.audit(), nil)
	e.ok("BeginEditUndo", err)
	manifests := map[string]tenant.SHA256{}
	for _, t := range targets {
		manifests[t.SeriesUID] = sum(9710)
	}
	_, err = ed.UndoEdit(e.ctx, tenant.EditUndo{ID: c.ID, Now: at(710), Manifests: manifests})
	e.ok("UndoEdit", err)
	e.wantFound("undone", `Manufacturer == "ACME"`, s1, "sub-01/ses-1/scan-"+uidOf(2, 101))
	e.wantFound("undone", `ImageType == "PRIMARY"`, s1)
	e.wantFound("undone", `SeriesDescription contains "scout"`)
	e.wantFound("undone", `SliceThickness = 1`, s1)
}

// testSearchFollowsRelabelsAndMoves guards what a search answers after a
// relabel and a move: the scans are found under their new labels, and a
// condition on the subject or session they are now under holds for them.
func testSearchFollowsRelabelsAndMoves(e *cenv) {
	e.setSearchRows()
	e.setupMove()
	s1, s2, s3 := "scan-"+uidOf(2, 100), "scan-"+uidOf(2, 101), "scan-"+uidOf(2, 200)
	e.wantFound("archived", `Manufacturer == "ACME" and has subject (subject.label = "sub-01")`, "sub-01/ses-1/"+s1, "sub-01/ses-1/"+s2)
	c, made, err := e.prepareMove(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, "sub-02", 500)
	e.ok("PrepareCorrection of a move", err)
	if !made {
		e.Fatal("a move was not made")
	}
	e.commitMove(c, 600)
	e.wantFound("a session moved", `Manufacturer == "ACME" and has subject (subject.label = "sub-01")`)
	e.wantFound("a session moved", `Manufacturer == "ACME" and has subject (subject.label = "sub-02")`, "sub-02/ses-1/"+s1, "sub-02/ses-1/"+s2)
	_, got := e.searchAll(tenant.SearchQuery{Level: query.LevelSubject, Condition: e.searchCondition(`has session (has scan (Manufacturer == "ACME"))`), Now: T0}, 10)
	if !slices.Equal(got, []string{"sub-02"}) {
		e.Errorf("the subjects holding an ACME scan are %v after the move, want sub-02 alone", got)
	}
	// The move purged, so its labels are free, a relabel of the subject left with the other session: its scan under
	// the new label.
	mv := e.wantState(c.ID, record.StateCommitted)
	_, err = e.p.StartPurge(e.ctx, mv.ID, mv.PurgeAfter, nil, nil)
	e.ok("StartPurge of the move", err)
	_, err = e.p.FinishPurge(e.ctx, tenant.PurgeFinish{ID: mv.ID, Now: mv.PurgeAfter.Add(1)})
	e.ok("FinishPurge of the move", err)
	r, _, err := e.prepareRelabel(tenant.CorrectionTarget{Subject: "sub-01"}, "sub-03", 700)
	e.ok("PrepareCorrection of a relabel", err)
	scans, err := e.p.CorrectionScans(e.ctx, r.ID)
	e.ok("CorrectionScans", err)
	manifests := map[string]tenant.SHA256{}
	for i, s := range scans {
		manifests[s.SeriesUID] = sum(1900 + i)
	}
	e.commit(r.ID, 800, manifests)
	e.wantFound("a subject relabeled", `Manufacturer == "ACME"`, "sub-02/ses-1/"+s1, "sub-02/ses-1/"+s2)
	e.wantFound("a subject relabeled", `Manufacturer == "OTHERCO"`, "sub-03/ses-2/"+s3)
	e.wantFound("a subject relabeled", `Manufacturer == "OTHERCO" and has subject (subject.label = "sub-03")`, "sub-03/ses-2/"+s3)
}

// testSearchFollowsInstanceDelete guards a delete of the instance a scan's
// search row was read from: the publish gives the scan the row read from
// the next lowest instance, an instance whose row was not the scan's
// leaves it, and the undo gives back the row the first instance gave.
func testSearchFollowsInstanceDelete(e *cenv) {
	e.setSearchRows()
	a, _ := e.setupTwo()
	scan := "sub-01/ses-1/scan-" + uidOf(2, 100)
	series := uidOf(2, 100)
	first, second := a.files[0].SOPInstanceUID, a.files[1].SOPInstanceUID
	target := func(uid string) tenant.CorrectionTarget {
		return tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + series, Instance: uid}
	}
	publish := func(id string, s int, row search.Row) {
		e.Helper()
		_, err := e.p.CommitCorrection(e.ctx, tenant.CorrectionCommit{ID: id, Now: at(s), Grace: grace,
			Search: map[string]search.Row{series: row}})
		e.ok("CommitCorrection of an instance delete", err)
	}
	next := search.Extract(dataSetOf(e.penv, map[string]string{"Modality": "MR", "Manufacturer": "NEXTCO"}), second)
	// The second instance is not the row's source: the row stays.
	c := e.prepareDelete(target(second), 500)
	publish(c.ID, 510, next)
	e.wantFound("a later instance deleted", `Manufacturer == "ACME"`, scan, "sub-01/ses-1/scan-"+uidOf(2, 101))
	if _, err := e.p.UndoCorrection(e.ctx, c.ID, "no", at(520), e.audit(), nil); err != nil {
		e.Fatalf("UndoCorrection: %v", err)
	}
	// The first instance is: the row is read again.
	c = e.prepareDelete(target(first), 600)
	publish(c.ID, 610, next)
	e.wantFound("the source instance deleted", `Manufacturer == "NEXTCO"`, scan)
	e.wantFound("the source instance deleted", `Manufacturer == "ACME"`, "sub-01/ses-1/scan-"+uidOf(2, 101))
	if _, err := e.p.UndoCorrection(e.ctx, c.ID, "no", at(700), e.audit(), nil); err != nil {
		e.Fatalf("UndoCorrection: %v", err)
	}
	e.wantFound("the delete undone", `Manufacturer == "ACME"`, scan, "sub-01/ses-1/scan-"+uidOf(2, 101))
	e.wantFound("the delete undone", `Manufacturer == "NEXTCO"`)
}
