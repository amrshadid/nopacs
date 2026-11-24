package auditseal

import (
	"bytes"
	"context"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// capture points s's log at a buffer and returns it.
func capture(s *Sealer) *bytes.Buffer {
	var buf bytes.Buffer
	s.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// TestAnchorKeptWhenLogRolledBack guards the cross-anchor against a log
// taken back with its database alone, no key: a tenant's last checkpoint
// removed, one of its entries removed and the rest made unsealed again,
// then new entries written. The next pass seals them into a checkpoint
// that ends past the master's anchor; the master must keep the anchor it
// held (the log no longer descends from it), the pass logs an error naming
// the log and the seq the anchor holds and counts it, and Verify of the
// tenant reports the log rolled back against that anchor, while the
// master's log stays intact.
func TestAnchorKeptWhenLogRolledBack(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			keys := keyFile(t, 1)
			pub, _ := PublicKeys(keys, testInstallation)
			lab := b.ids["lab"]
			c := &clock{t: t0}
			s := b.sealer(t, signerOf(t, keys, 1), c, 0)
			write(t, b.tenants["lab"], 5, 0)
			s.Pass(ctx)
			write(t, b.tenants["lab"], 5, time.Second)
			c.set(t0.Add(time.Minute))
			s.Pass(ctx)
			held := anchorOf(t, b.master, lab)
			if held.To != 10 {
				t.Fatalf("the master's anchor of the tenant is at %d, want 10", held.To)
			}

			b.tamper(t, lab,
				`DELETE FROM audit_checkpoints WHERE to_seq > 5`,
				`DELETE FROM audit WHERE seq = 7`,
				`UPDATE audit SET seq = NULL WHERE seq > 5`)
			write(t, b.tenants["lab"], 3, 2*time.Second)
			c.set(t0.Add(2 * time.Minute))
			logs := capture(s)
			s.Pass(ctx)

			if latestOf(t, b.tenants["lab"]).To != 12 {
				t.Fatalf("the tenant's log was not sealed again to 12")
			}
			if got := anchorOf(t, b.master, lab); got != held {
				t.Errorf("the master's anchor moved from %d to %d over a log that went back", held.To, got.To)
			}
			if s.Stats().RolledBack == 0 {
				t.Error("the rolled-back log was not counted")
			}
			text := logs.String()
			if !strings.Contains(text, "level=ERROR") || !strings.Contains(text, "anchored_log="+lab) || !strings.Contains(text, "held_to_seq=10") {
				t.Errorf("the pass's log does not name the log and the anchor's seq at error level:\n%s", text)
			}
			r, err := b.verifyAll(t, lab, pub, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			wantProblem(t, "the tenant with the master's anchor", r, RolledBack, 10)
			b.verify(t, master.MasterAuditLog, pub)
		})
	}
}

// anchorOf returns the anchor st keeps of log.
func anchorOf(t *testing.T, st master.AuditSealReads, log string) master.AuditAnchor {
	t.Helper()
	as, err := st.AuditAnchors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range as {
		if a.Log == log {
			return a
		}
	}
	t.Fatalf("no anchor of %s", log)
	return master.AuditAnchor{}
}

// TestDescends guards the check an anchor is moved by: a log that went on
// from the anchored checkpoint descends from it; one whose anchored
// checkpoint is gone, has another digest, or has a checkpoint on the way
// whose previous digest was rewritten does not (Conflict).
func TestDescends(t *testing.T) {
	b, _ := sealedSQLite(t)
	ctx := context.Background()
	cps, err := b.master.AuditCheckpoints(ctx, 1, 10)
	if err != nil || len(cps) != 3 {
		t.Fatalf("checkpoints %+v, %v", cps, err)
	}
	held := master.AuditAnchor{Log: master.MasterAuditLog, To: cps[0].To, Digest: cps[0].Digest}
	if err := Descends(ctx, b.master, held, cps[2]); err != nil {
		t.Fatalf("a log that went on: %v", err)
	}
	if err := Descends(ctx, b.master, master.AuditAnchor{Log: held.Log, To: 3, Digest: held.Digest}, cps[2]); err == nil {
		t.Error("an anchor at a seq no checkpoint ends at was taken")
	}
	if err := Descends(ctx, b.master, master.AuditAnchor{Log: held.Log, To: 4, Digest: master.AuditDigest{9}}, cps[2]); err == nil {
		t.Error("an anchor of another digest was taken")
	}
	if err := Descends(ctx, b.master, master.AuditAnchor{Log: held.Log, To: 8, Digest: cps[1].Digest}, cps[0]); err == nil {
		t.Error("a checkpoint before the anchor was taken")
	}
	tamper(t, b.where[master.MasterAuditLog], `UPDATE audit_checkpoints SET previous = zeroblob(32) WHERE to_seq = 8`)
	if err := Descends(ctx, b.master, held, cps[2]); err == nil {
		t.Error("a chain of checkpoints with a rewritten previous digest was taken")
	}
}

// TestVerifyRefusesForgedPruned guards the pruned flag, a column anyone
// with the database can set: a checkpoint in the middle of the log marked
// pruned and its entries removed; the first one so marked while it is
// younger than the shortest retention; and the first one so marked, old
// enough, with no pruning recorded by the server: each is a missing entry
// at the checkpoint, not an intact log.
func TestVerifyRefusesForgedPruned(t *testing.T) {
	cases := []struct {
		name       string
		statements []string
		now        time.Time
		seq        int64
	}{
		{"a checkpoint in the middle", []string{`UPDATE audit_checkpoints SET pruned = 1 WHERE to_seq = 8`, `DELETE FROM audit WHERE seq BETWEEN 5 AND 8`}, time.Time{}, 8},
		{"the first, a minute old", []string{`UPDATE audit_checkpoints SET pruned = 1 WHERE to_seq = 4`, `DELETE FROM audit WHERE seq BETWEEN 1 AND 4`}, t0.Add(time.Hour), 4},
		{"the first, old, no pruning recorded", []string{`UPDATE audit_checkpoints SET pruned = 1 WHERE to_seq = 4`, `DELETE FROM audit WHERE seq BETWEEN 1 AND 4`}, t0.Add(100 * day), 4},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, pub := sealedSQLite(t)
			tamper(t, b.where[master.MasterAuditLog], tc.statements...)
			wantProblem(t, tc.name, verifyOne(t, b, pub, VerifyOptions{Now: tc.now}), EntryMissing, tc.seq)
		})
	}
}

// TestVerifyAcceptsRetentionAcrossPrunings guards the other side of the
// pruned flag: two prunings a retention apart, the second pruning the
// first one's own entry, leave a log that verifies; the checkpoints the
// first pruned are accepted because they come before the first one a
// pruning's entry still in the log covers.
func TestVerifyAcceptsRetentionAcrossPrunings(t *testing.T) {
	b := sqliteBackend(t)
	ctx := context.Background()
	keys := keyFile(t, 1)
	pub, _ := PublicKeys(keys, testInstallation)
	c := &clock{t: t0}
	s := b.sealer(t, signerOf(t, keys, 1), c, 90*day)
	write(t, b.master, 3, 0)
	s.Pass(ctx)
	c.set(t0.Add(91 * day))
	write(t, b.master, 2, time.Hour)
	s.Pass(ctx) // prunes the first checkpoint; its entry sealed at once
	c.set(t0.Add(200 * day))
	write(t, b.master, 2, 2*time.Hour)
	s.Pass(ctx) // prunes the next ones, the first pruning's entry among them
	cps, err := b.master.AuditCheckpoints(ctx, 1, 20)
	if err != nil {
		t.Fatal(err)
	}
	pruned := 0
	for _, cp := range cps {
		if cp.Pruned {
			pruned++
		}
	}
	if pruned < 3 {
		t.Fatalf("checkpoints %+v; want the first pruning's entry pruned too", cps)
	}
	whole := b.verifyAt(t, master.MasterAuditLog, pub, c.now())

	// The same log one checkpoint a call, so the calls split the pruned
	// checkpoints: each call is intact and together they check every one.
	var checked int64
	for from := int64(0); ; {
		r, err := Verify(ctx, b.master, VerifyOptions{Installation: testInstallation, Log: master.MasterAuditLog, Keys: pub,
			Now: c.now(), From: from, MaxCheckpoints: 1})
		if err != nil || !r.Intact {
			t.Fatalf("one checkpoint a call, from %d: %+v, %v", from, r, err)
		}
		checked += r.CheckpointsChecked
		if r.NextSeq == 0 {
			break
		}
		from = r.NextSeq
	}
	if checked != whole.CheckpointsChecked {
		t.Errorf("one checkpoint a call checked %d checkpoints; the whole log has %d", checked, whole.CheckpointsChecked)
	}
}

// TestPrunedCoveredInPart guards a range that holds part of the pruned
// checkpoints, as the bounds of one call make: each is judged as the whole
// log's verification judges it. A pruned checkpoint no pruning's entry
// covers is a missing entry when the one just before the range is covered,
// and when no entry covers anything past it; it is accepted, as one whose
// pruning's entry is gone, when an entry covers checkpoints past it.
func TestPrunedCoveredInPart(t *testing.T) {
	before := &master.AuditCheckpoint{From: 1, To: 4, Pruned: true}
	for _, tc := range []struct {
		name     string
		before   *master.AuditCheckpoint
		spans    []retentionSpan
		problems int
	}{
		{"the one before covered", before, []retentionSpan{{1, 4}, {9, 12}}, 1},
		{"the one before not covered, later ones covered", before, []retentionSpan{{9, 12}}, 0},
		{"nothing covered", nil, nil, 1},
	} {
		// The walk reached the log's end, so no entry is read past it.
		v := &verification{ctx: context.Background()}
		v.retention = retentionCheck{before: tc.before, pruned: []master.AuditCheckpoint{{From: 5, To: 8, Pruned: true}}, spans: tc.spans, walkedTo: 12}
		if err := v.prunedCovered(master.AuditCheckpoint{To: 12}); err != nil {
			t.Fatal(err)
		}
		if len(v.r.Problems) != tc.problems || (tc.problems == 1 && (v.r.Problems[0].Kind != EntryMissing || v.r.Problems[0].Seq != 8)) {
			t.Errorf("%s: problems %+v; want %d", tc.name, v.r.Problems, tc.problems)
		}
	}
}

// TestStalledSealingReported guards a log the passes cannot seal: a row
// put in by hand whose text is not valid UTF-8 sorts first among the
// unsealed entries and holds every later one back. The pass logs an error
// naming the entry's id and counts it, the oldest unsealed entry's time is
// kept for the metric, and Verify, two seal intervals on, reports the
// sealing stalled with that time, where before it would have said intact.
func TestStalledSealingReported(t *testing.T) {
	b := sqliteBackend(t)
	ctx := context.Background()
	keys := keyFile(t, 1)
	pub, _ := PublicKeys(keys, testInstallation)
	c := &clock{t: t0}
	s := b.sealer(t, signerOf(t, keys, 1), c, 0)
	write(t, b.master, 3, 0)
	s.Pass(ctx)
	badAt := t0.Add(time.Minute)
	tamper(t, b.where[master.MasterAuditLog], `INSERT INTO audit (id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details, request_id)
		VALUES ('bad-row', `+strconv.FormatInt(badAt.UnixMicro(), 10)+`, '', '', '', 'X', '', '', 'done', CAST(X'FF' AS TEXT), '{}', '')`)
	write(t, b.master, 2, 2*time.Minute)
	c.set(t0.Add(30 * time.Minute))
	logs := capture(s)
	s.Pass(ctx)
	if n, _ := b.master.CountUnsealedAudit(ctx); n != 3 {
		t.Fatalf("%d unsealed after the pass, want the bad row and the 2 after it", n)
	}
	text := logs.String()
	if !strings.Contains(text, "level=ERROR") || !strings.Contains(text, "entry_id=bad-row") {
		t.Errorf("the pass's log does not name the entry at error level:\n%s", text)
	}
	st := s.Stats()
	if st.Unencodable != 1 || !st.OldestUnsealed.Equal(badAt.Truncate(time.Microsecond)) {
		t.Errorf("stats %+v; want one unencodable pass and the bad row's time as the oldest unsealed", st)
	}
	o := VerifyOptions{Now: t0.Add(15 * time.Minute), SealInterval: 10 * time.Minute}
	if r := verifyOne(t, b, pub, o); !r.Intact {
		t.Errorf("fourteen minutes after the entry: %+v; want intact, not yet two intervals", r.Problems)
	}
	o.Now = t0.Add(30 * time.Minute)
	r := verifyOne(t, b, pub, o)
	wantProblem(t, "two intervals on", r, SealingStalled, 0)
	if len(r.Problems) == 1 && !r.Problems[0].Time.Equal(badAt.Truncate(time.Microsecond)) {
		t.Errorf("the stalled problem names %v, want the oldest unsealed entry's time %v", r.Problems[0].Time, badAt)
	}
}
