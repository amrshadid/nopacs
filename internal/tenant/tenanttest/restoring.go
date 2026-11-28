package tenanttest

import (
	"context"
	"slices"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runRestoring runs the tests of what a restore asks of a tenant's
// database around its rebuild (tenant.Restoring): the quarantined files it
// marks lost, the labels a subject's delete holds, the rebuilds it ends.
func runRestoring(t *testing.T, open OpenEvents, scratch Scratch) {
	tests := []struct {
		name string
		fn   func(*xenv)
	}{
		{"QuarantineLostButKept", testQuarantineLostButKept},
		{"EarlierSubjectLabels", testEarlierSubjectLabels},
		{"EndRebuilds", testEndRebuilds},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			rs, ok := s.(tenant.Restoring)
			if !ok {
				t.Fatal("the store does not implement tenant.Restoring")
			}
			e := &xenv{cenv: &cenv{senv: &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s}}}, rs: rs}
			if scratch != nil {
				e.scratch = scratch(t)
			}
			tt.fn(e)
		})
	}
}

// xenv is a test of what a restore asks: the corrections' environment, the
// store as tenant.Restoring, and the scratch schema its rebuilds name.
type xenv struct {
	*cenv
	rs      tenant.Restoring
	scratch string
}

// testQuarantineLostButKept guards MarkQuarantineLost: of three files held
// with their bytes kept, those keep does not name become incoming lost (no
// bytes kept, no place in incoming) and the one it names stays as it was;
// marked again with nothing kept, it is the only one marked.
func testQuarantineLostButKept(e *xenv) {
	e.setupTwo()
	var files []tenant.QuarantinedFile
	for i := range 3 {
		files = append(files, tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineWasDeleted, Receiver: "recv", Size: int64(10 + i),
			SHA256: sum(40 + i), SOPClassUID: "1.2.3", SOPInstanceUID: uidOf(3, 900+i), StudyUID: uidOf(1, 900), SeriesUID: uidOf(2, 900),
			TransferSyntaxUID: "1.2.840.10008.1.2.1", Incoming: "quarantine/q" + string(rune('a'+i)), ContentKept: true, Quarantined: at(300),
			Details: map[string]string{"correction": e.newID(1)}})
	}
	_, err := e.p.HoldFiles(e.ctx, files, nil)
	e.ok("HoldFiles", err)
	n, err := e.rs.MarkQuarantineLost(e.ctx, []string{files[1].ID})
	e.ok("MarkQuarantineLost", err)
	if n != 2 {
		e.Fatalf("MarkQuarantineLost with one kept marked %d, want 2", n)
	}
	for i, f := range files {
		q, err := e.p.QuarantinedFile(e.ctx, f.ID)
		e.ok("QuarantinedFile", err)
		lost := q.Reason == tenant.QuarantineIncomingLost && !q.ContentKept && q.Incoming == ""
		if i == 1 && (lost || !q.ContentKept || q.Incoming != f.Incoming || q.Reason != f.Reason) {
			e.Fatalf("the file kept became %+v", q)
		}
		if i != 1 && !lost {
			e.Fatalf("a file not kept is %+v, want incoming lost", q)
		}
	}
	if n, err = e.rs.MarkQuarantineLost(e.ctx, nil); err != nil || n != 1 {
		e.Fatalf("MarkQuarantineLost with none kept marked %d (%v), want the one left", n, err)
	}
}

// adoptRecord adopts rec, made committed at second s, on the storage
// "store", failing the test on an error.
func (e *xenv) adoptRecord(rec record.Correction, s int) {
	e.Helper()
	rec.Format, rec.State, rec.ReasonSHA256 = record.Format, record.StateCommitted, record.ReasonDigest("a reason")
	rec.Created, rec.CommittedAt, rec.PurgeAfter = at(s), at(s+1), at(s+1).Add(grace)
	_, err := e.rs.AdoptCorrection(e.ctx, rec, "store", at(s+2))
	e.ok("AdoptCorrection", err)
}

// testEarlierSubjectLabels guards HoldEarlierSubjectLabels: a subject's
// delete made from its record names only its record's label; after it, it
// also holds the label its subject had before a relabel made before the
// delete, but not the label of a subject relabeled to it after the delete
// (which gave that subject the deleted one's label once it was purged).
// Run again, it adds nothing.
func testEarlierSubjectLabels(e *xenv) {
	scan := func(subject string, n int) []record.Scan {
		return []record.Scan{{Subject: subject, Session: "ses-1", Scan: "scan-1", StudyUID: uidOf(1, 600+n), SeriesUID: uidOf(2, 600+n)}}
	}
	e.adoptRecord(record.Correction{ID: e.newID(10), Kind: record.KindRelabel, Scans: scan("sub-old", 1),
		Relabel: &record.Relabel{Level: record.LevelSubject, From: "sub-old", To: "sub-new"}}, 10)
	d := e.newID(20)
	e.adoptRecord(record.Correction{ID: d, Kind: record.KindDelete, Level: record.LevelSubject, Scans: scan("sub-new", 1)}, 20)
	e.adoptRecord(record.Correction{ID: e.newID(30), Kind: record.KindRelabel, Scans: scan("sub-late", 2),
		Relabel: &record.Relabel{Level: record.LevelSubject, From: "sub-late", To: "sub-new"}}, 30)
	if m := e.matches(tenant.DeletedProbe{Subject: "sub-old"}); m.Subject != "" {
		e.Fatalf("before, the earlier label is held by %q", m.Subject)
	}
	n, err := e.rs.HoldEarlierSubjectLabels(e.ctx)
	e.ok("HoldEarlierSubjectLabels", err)
	if n != 1 {
		e.Fatalf("HoldEarlierSubjectLabels added %d labels, want 1", n)
	}
	if m := e.matches(tenant.DeletedProbe{Subject: "sub-new"}); m.Subject != d {
		e.Fatalf("the delete's own label is held by %q, want %s", m.Subject, d)
	}
	if m := e.matches(tenant.DeletedProbe{Subject: "sub-old"}); m.Subject != d {
		e.Fatalf("the label before a relabel made before the delete is held by %q, want %s", m.Subject, d)
	}
	if m := e.matches(tenant.DeletedProbe{Subject: "sub-late"}); m.Subject != "" {
		e.Fatalf("the label of a subject relabeled after the delete is held by %q", m.Subject)
	}
	if n, err := e.rs.HoldEarlierSubjectLabels(e.ctx); err != nil || n != 0 {
		e.Fatalf("run again HoldEarlierSubjectLabels added %d (%v), want none", n, err)
	}
}

// testEndRebuilds guards EndRebuilds: a rebuild queued is ended canceled
// with JobReasonEndedByRestore and answered; run again it answers none;
// once its shadow is dropped, a new rebuild is made in the same scratch
// schema, which a rebuild not ended refuses.
func testEndRebuilds(e *xenv) {
	mk := func(id string) error {
		_, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare,
			Scratch: e.scratch}, Created: at(0)}, e.audit(), nil)
		return err
	}
	e.ok("CreateJob of a rebuild", mk("leftrebuild1"))
	e.wantKind("a second rebuild beside one not ended", mk("leftrebuild2"), fault.Precondition)
	ids, err := e.rs.EndRebuilds(e.ctx, at(100))
	e.ok("EndRebuilds", err)
	if !slices.Equal(ids, []string{"leftrebuild1"}) {
		e.Fatalf("EndRebuilds answered %v", ids)
	}
	j, err := e.p.Job(e.ctx, "leftrebuild1")
	e.ok("Job", err)
	if j.State != tenant.JobCanceled || j.Reason != tenant.JobReasonEndedByRestore {
		e.Fatalf("the rebuild ended is %s, %q", j.State, j.Reason)
	}
	if ids, err := e.rs.EndRebuilds(e.ctx, at(200)); err != nil || len(ids) != 0 {
		e.Fatalf("run again EndRebuilds answered %v (%v)", ids, err)
	}
	e.ok("DropShadow", e.p.DropShadow(e.ctx, "leftrebuild1"))
	e.ok("CreateJob of a rebuild after", mk("leftrebuild3"))
}
