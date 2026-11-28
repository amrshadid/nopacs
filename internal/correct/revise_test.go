package correct

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// reviseRequest reads an instance as archived, makes its next file by
// removing KVP, and returns the request that publishes it.
func (r *rig) reviseRequest(t *testing.T, e *Engine, s tenant.CatalogScan, uid string) (ReviseRequest, []byte) {
	t.Helper()
	target := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label, Instance: uid}
	targets, err := e.editTargets(r.ctx, target)
	if err != nil || len(targets) != 1 {
		t.Fatalf("the instance's targets = %d, %v", len(targets), err)
	}
	old, err := readAll(r.ctx, r.storage, targets[0].Key, targets[0].Size)
	if err != nil {
		t.Fatal(err)
	}
	nb, err := receive.RewriteFile(old, 0, func(ds *dataset.Dataset) error { ds.Remove(tag.New(0x0018, 0x0060)); return nil })
	if err != nil {
		t.Fatal(err)
	}
	return ReviseRequest{Target: target, OldSHA256: targets[0].SHA256, File: nb, Profile: tenant.ProfileRef{Name: "research", Version: 2},
		Values: []EditChange{setDescription("Calcium score")}, Reason: "profile re-run", Audit: r.audit()}, nb
}

// hasKVP reads whether file holds KVP (0018,0060).
func hasKVP(t *testing.T, file []byte) bool {
	t.Helper()
	h, err := header.ReadWithin(bytes.NewReader(file), int64(len(file)), header.MaxHeader)
	if err != nil {
		t.Fatal(err)
	}
	ds, _, _, err := h.Dataset(bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	return ds.Contains(tag.New(0x0018, 0x0060))
}

// TestReviseGivesOneInstanceItsNextRevision guards the publish of a file a
// profile re-run made: only that instance moves to revision 2 and its bytes
// are the ones given, the scan's catalog values are the ones named, the
// superseded revision stays on the storage for the purge, a stale checksum
// is refused, and the undo puts the old revision and the old description
// back.
func TestReviseGivesOneInstanceItsNextRevision(t *testing.T) {
	onBoth(t, testReviseGivesOneInstanceItsNextRevision)
}

// testReviseGivesOneInstanceItsNextRevision is
// TestReviseGivesOneInstanceItsNextRevision on one database.
func testReviseGivesOneInstanceItsNextRevision(t *testing.T, pg bool) {
	r := newRig(t, pg)
	e := r.editEngine()
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	path := pathOf(s)
	before := r.held(path)
	req, nb := r.reviseRequest(t, e, s, before[0].SOPInstanceUID)
	stale := req
	stale.OldSHA256 = tenant.SHA256{1}
	if _, err := e.Revise(r.ctx, stale); !errors.Is(err, tenant.ErrTargetChanged) {
		t.Fatalf("a revision of a changed instance answered %v, want ErrTargetChanged", err)
	}
	c, err := e.Revise(r.ctx, req)
	if err != nil || c.State != record.StateCommitted || c.Kind != record.KindEdit {
		t.Fatalf("the revision = %s %s (%q), %v", c.Kind, c.State, c.StateReason, err)
	}
	after := r.held(path)
	objs := r.objects()
	for i, in := range after {
		wantRev := 1
		if i == 0 {
			wantRev = 2
		}
		want, _ := layout.RevisionKey(path, in.SOPInstanceUID, wantRev)
		if wantRev == 1 {
			want = before[i].Key
		}
		if in.Key != want {
			t.Fatalf("instance %d is at %s, want %s", i, in.Key, want)
		}
	}
	if _, ok := objs[before[0].Key]; !ok {
		t.Fatal("the superseded revision was removed before the purge")
	}
	got, err := readAll(r.ctx, r.storage, after[0].Key, after[0].Size)
	if err != nil || !bytes.Equal(got, nb) || hasKVP(t, got) {
		t.Fatalf("the new revision is not the file given (err %v)", err)
	}
	if sc, err := r.p.CatalogScan(r.ctx, path); err != nil || sc.Values.SeriesDescription != "Calcium score" {
		t.Fatalf("the catalog's description is %q (%v)", sc.Values.SeriesDescription, err)
	}
	if _, err := e.Undo(r.ctx, c.ID, "the older version was right", r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	back := r.held(path)
	if back[0].Key != before[0].Key {
		t.Fatalf("after the undo the instance is at %s, want %s", back[0].Key, before[0].Key)
	}
	if sc, err := r.p.CatalogScan(r.ctx, path); err != nil || sc.Values.SeriesDescription != s.Values.SeriesDescription {
		t.Fatalf("after the undo the description is %q (%v), want %q", sc.Values.SeriesDescription, err, s.Values.SeriesDescription)
	}
}

// TestReviseRefusesAHeldInstance guards that a hold over an instance
// refuses its revision as it refuses an edit, and writes nothing.
func TestReviseRefusesAHeldInstance(t *testing.T) {
	r := newRig(t, false)
	e := r.editEngine()
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	before := r.held(pathOf(s))
	req, _ := r.reviseRequest(t, e, s, before[0].SOPInstanceUID)
	id, err := tenant.NewCorrectionID(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.p.PlaceHold(r.ctx, tenant.HoldPlace{ID: id, Target: tenant.HoldTarget{Subject: s.SubjectLabel}, Reason: "audit",
		Written: []string{"main"}, Now: time.Now()}, r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	var held *tenant.HeldRefusal
	if _, err := e.Revise(r.ctx, req); !errors.As(err, &held) || held.Hold != id {
		t.Fatalf("a revision of held data answered %v, want the hold's refusal", err)
	}
	r.wantNoEdit(t, s, before)
}

// TestReviseLineDescribesTheRunThatMadeIt guards the line of the scan's
// metadata a profile's revision gets: the newer version's checksum, the base
// in force and the elements no rule of the newer version named, not those of
// the revision it replaces; the instance's other lines are untouched.
func TestReviseLineDescribesTheRunThatMadeIt(t *testing.T) {
	r := newRig(t, false)
	e := r.editEngine()
	r.archive(studyFiles[:7]...)
	s := r.scanWith(2)
	before := r.held(pathOf(s))
	req, _ := r.reviseRequest(t, e, s, before[0].SOPInstanceUID)
	req.ProfileSHA256 = strings.Repeat("ab", 32)
	req.Base = "floor@3"
	req.Unnamed = []sidecar.UnnamedElement{{Tag: 0x00090005, VR: "LO", Count: 2, Creator: "ACME"}}
	if _, err := e.Revise(r.ctx, req); err != nil {
		t.Fatal(err)
	}
	got := map[string]sidecar.Instance{}
	if err := readLines(r.ctx, r.storage, pathOf(s), func(in sidecar.Instance) error { got[in.SOPInstanceUID] = in; return nil }); err != nil {
		t.Fatal(err)
	}
	line := got[before[0].SOPInstanceUID]
	if line.Profile != "research@2" || line.ProfileSHA256 != req.ProfileSHA256 || line.BaseProfile != "floor@3" ||
		len(line.Unnamed) != 1 || line.Unnamed[0].Creator != "ACME" {
		t.Errorf("the revision's line is %+v, want the run that made it", line)
	}
	if other := got[before[1].SOPInstanceUID]; other.BaseProfile != "" || other.Unnamed != nil || other.ProfileSHA256 != "" {
		t.Errorf("an instance the revision did not touch has the line %+v", other)
	}
}
