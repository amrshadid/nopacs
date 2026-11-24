package auditseal

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/auditchain"
	"github.com/amrshadid/nopacs/internal/master"
)

// sealedSQLite returns a SQLite backend whose master holds 3 checkpoints of
// 4 entries each, sealed, and the public keys that check them.
func sealedSQLite(t *testing.T) (*backend, []PublicKey) {
	t.Helper()
	b := sqliteBackend(t)
	keys := keyFile(t, 1)
	pub, err := PublicKeys(keys, testInstallation)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: t0}
	s := b.sealer(t, signerOf(t, keys, 1), c, 0)
	for i := range 3 {
		write(t, b.master, 4, time.Duration(i)*time.Second)
		write(t, b.tenants["lab"], 2, time.Duration(i)*time.Second)
		c.set(t0.Add(time.Duration(i+1) * time.Minute))
		s.Pass(context.Background())
	}
	return b, pub
}

// tamper runs statements on a log's SQLite file directly, as someone with
// the file would.
func tamper(t *testing.T, path string, statements ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, s := range statements {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

// verifyOne verifies the master's whole log with o's checks added.
func verifyOne(t *testing.T, b *backend, pub []PublicKey, o VerifyOptions) VerifyResult {
	t.Helper()
	o.Installation, o.Log, o.Keys = testInstallation, master.MasterAuditLog, pub
	r, err := Verify(context.Background(), b.master, o)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// wantProblem fails unless r holds exactly one problem, of kind at seq.
func wantProblem(t *testing.T, what string, r VerifyResult, kind ProblemKind, seq int64) {
	t.Helper()
	if r.Intact || len(r.Problems) != 1 || r.Problems[0].Kind != kind || r.Problems[0].Seq != seq {
		t.Errorf("%s: intact %v, problems %+v; want one of kind %d at seq %d", what, r.Intact, r.Problems, kind, seq)
	}
}

// TestVerifyFindsTampering guards that each way of changing a sealed log in
// its database is found, of the contract's kind and at its seq: an entry's
// field changed, an entry removed, an entry added past the last checkpoint,
// a checkpoint's signature or head changed, the digest kept with one
// changed, and its order broken.
func TestVerifyFindsTampering(t *testing.T) {
	cases := []struct {
		name       string
		statements []string
		kind       ProblemKind
		seq        int64
	}{
		{"an entry's reason changed", []string{`UPDATE audit SET reason = 'nothing to see' WHERE seq = 6`}, ChainBroken, 8},
		{"an entry's time changed", []string{`UPDATE audit SET at = at + 1 WHERE seq = 2`}, ChainBroken, 4},
		{"an entry removed", []string{`DELETE FROM audit WHERE seq = 7`}, EntryMissing, 7},
		{"the last entry of a checkpoint removed", []string{`DELETE FROM audit WHERE seq = 12`}, EntryMissing, 12},
		{"two entries swapped", []string{`UPDATE audit SET seq = 100 WHERE seq = 5`, `UPDATE audit SET seq = 5 WHERE seq = 6`, `UPDATE audit SET seq = 6 WHERE seq = 100`}, ChainBroken, 8},
		{"an entry put past the last checkpoint", []string{`INSERT INTO audit (id, at, key_id, local_user, remote_addr, method, tenant_name, object, outcome, reason, details, request_id, seq)
			 VALUES ('forged', 0, '', '', '', 'X', '', '', 'done', '', '{}', '', 13)`}, ChainBroken, 13},
		{"a signature changed", []string{`UPDATE audit_checkpoints SET signature = zeroblob(64) WHERE to_seq = 4`}, BadSignature, 4},
		{"a head changed", []string{`UPDATE audit_checkpoints SET head = zeroblob(32) WHERE to_seq = 8`}, ChainBroken, 8},
		{"the digest kept changed", []string{`UPDATE audit_checkpoints SET digest = zeroblob(32) WHERE to_seq = 12`}, OutOfLine, 12},
		{"a checkpoint removed", []string{`DELETE FROM audit_checkpoints WHERE to_seq = 8`}, OutOfLine, 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, pub := sealedSQLite(t)
			if r := verifyOne(t, b, pub, VerifyOptions{}); !r.Intact || r.EntriesChecked != 12 || r.CheckpointsChecked != 3 {
				t.Fatalf("before the change: %+v", r)
			}
			tamper(t, b.where[master.MasterAuditLog], tc.statements...)
			wantProblem(t, tc.name, verifyOne(t, b, pub, VerifyOptions{}), tc.kind, tc.seq)
		})
	}
}

// TestVerifyAnchorsAndOutsideValues guards what the chain alone cannot
// show: a tenant's log cut back to an earlier checkpoint, entries and
// checkpoints together, verifies on its own but is rolled back past the
// anchor the master keeps of it; a checkpoint at the anchor's seq with
// another digest disagrees with it; and a log cut back is found by a seq
// or a digest kept outside the installation.
func TestVerifyAnchorsAndOutsideValues(t *testing.T) {
	b, pub := sealedSQLite(t)
	ctx := context.Background()
	lab := b.ids["lab"]
	last := latestOf(t, b.master)
	tamper(t, b.where[lab], `DELETE FROM audit WHERE seq > 4`, `DELETE FROM audit_checkpoints WHERE to_seq > 4`)
	alone, err := Verify(ctx, b.tenants["lab"], VerifyOptions{Installation: testInstallation, Log: lab, Keys: pub})
	if err != nil || !alone.Intact {
		t.Fatalf("the cut log on its own: %+v, %v; want intact (the chain alone cannot show it)", alone, err)
	}
	r, err := Verify(ctx, b.tenants["lab"], VerifyOptions{Installation: testInstallation, Log: lab, Keys: pub, Anchors: b.anchorsOf(t, lab)})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "with the master's anchor", r, RolledBack, 6)
	other := b.anchorsOf(t, lab)
	other[0].To, other[0].Digest = 4, master.AuditDigest{1}
	r, err = Verify(ctx, b.tenants["lab"], VerifyOptions{Installation: testInstallation, Log: lab, Keys: pub, Anchors: other})
	if err != nil {
		t.Fatal(err)
	}
	wantProblem(t, "an anchor of another digest", r, AnchorDisagrees, 4)

	tamper(t, b.where[master.MasterAuditLog], `DELETE FROM audit WHERE seq > 8`, `DELETE FROM audit_checkpoints WHERE to_seq > 8`)
	wantProblem(t, "a seq kept outside", verifyOne(t, b, pub, VerifyOptions{ExpectedMinSeq: 12}), RolledBack, 12)
	wantProblem(t, "a digest kept outside", verifyOne(t, b, pub, VerifyOptions{ExpectedDigest: last.Digest}), RolledBack, 8)
	if r := verifyOne(t, b, pub, VerifyOptions{ExpectedMinSeq: 8}); !r.Intact {
		t.Errorf("a seq the log reaches: %+v", r.Problems)
	}
}

// TestVerifyRanges guards verification in several calls: a bound on the
// entries stops at a checkpoint's end and says where to go on, a start in
// the middle of a checkpoint is moved back to its start, and a range's end
// stops it.
func TestVerifyRanges(t *testing.T) {
	b, pub := sealedSQLite(t)
	r := verifyOne(t, b, pub, VerifyOptions{MaxEntries: 4})
	if !r.Intact || r.EntriesChecked != 4 || r.NextSeq != 5 {
		t.Fatalf("bounded to 4 entries: %+v; want 4 checked, next 5", r)
	}
	r = verifyOne(t, b, pub, VerifyOptions{From: 6})
	if !r.Intact || r.EntriesChecked != 8 || r.CheckpointsChecked != 2 || r.NextSeq != 0 {
		t.Errorf("from seq 6: %+v; want the checkpoints from 5, 8 entries", r)
	}
	r = verifyOne(t, b, pub, VerifyOptions{From: 2, To: 5})
	if !r.Intact || r.CheckpointsChecked != 2 {
		t.Errorf("seq 2 to 5: %+v; want the 2 checkpoints that cover them", r)
	}
	if _, err := Verify(context.Background(), b.master, VerifyOptions{From: 5, To: 4}); err == nil {
		t.Error("a range that ends before it starts was accepted")
	}
}

// TestVerifyBoundedByCheckpoints guards the bounds of one call that are not
// entries: a log of many one-entry checkpoints stops after the bound of
// checkpoints with where to go on, the calls from each NextSeq cover it
// whole; and a context whose deadline is nearer than the margin stops after
// the first checkpoint rather than running into the deadline.
func TestVerifyBoundedByCheckpoints(t *testing.T) {
	b := sqliteBackend(t)
	keys := keyFile(t, 1)
	pub, err := PublicKeys(keys, testInstallation)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: t0}
	s := b.sealer(t, signerOf(t, keys, 1), c, 0)
	const n = 25
	for i := range n {
		write(t, b.master, 1, time.Duration(i)*time.Second)
		c.set(t0.Add(time.Duration(i+1) * time.Minute))
		s.Pass(context.Background())
	}
	r := verifyOne(t, b, pub, VerifyOptions{MaxCheckpoints: 10})
	if !r.Intact || r.CheckpointsChecked != 10 || r.EntriesChecked != 10 || r.NextSeq != 11 {
		t.Fatalf("bounded to 10 checkpoints: %+v; want 10 checked, next 11", r)
	}
	var calls, checked int64
	for from := int64(0); ; calls++ {
		r := verifyOne(t, b, pub, VerifyOptions{MaxCheckpoints: 10, From: from})
		if !r.Intact {
			t.Fatalf("a call from %d: %+v", from, r.Problems)
		}
		checked += r.CheckpointsChecked
		if r.NextSeq == 0 {
			break
		}
		from = r.NextSeq
	}
	if calls != 2 || checked != n {
		t.Errorf("calls from each next seq: %d more calls, %d checkpoints; want 2 and %d", calls, checked, n)
	}
	if r := verifyOne(t, b, pub, VerifyOptions{}); r.CheckpointsChecked != n || r.NextSeq != 0 {
		t.Errorf("the default bound: %+v; want the whole log in one call", r)
	}

	ctx, cancel := context.WithTimeout(context.Background(), VerifyDeadlineMargin/2)
	defer cancel()
	r, err = Verify(ctx, b.master, VerifyOptions{Installation: testInstallation, Log: master.MasterAuditLog, Keys: pub})
	if err != nil || !r.Intact || r.CheckpointsChecked != 1 || r.NextSeq != 2 {
		t.Errorf("a deadline within the margin: %+v, %v; want one checkpoint checked, next 2", r, err)
	}
}

// TestExportReachesEveryHead guards an export: the header opens it with the
// chain before the range and the anchors, each entry's encoded form is
// what the chain encodes, its chain value folds from the one before, each
// checkpoint follows its last entry with its head equal to the chain there,
// and a range starting in a checkpoint starts at its first entry.
func TestExportReachesEveryHead(t *testing.T) {
	b, pub := sealedSQLite(t)
	items := exportAll(t, b.master, ExportOptions{Installation: testInstallation, Log: master.MasterAuditLog, Keys: pub, From: 6})
	h := items[0].Header
	if h == nil || h.From != 5 || h.Log != master.MasterAuditLog || len(h.Anchors) != 1 || h.Anchors[0].Log != b.ids["lab"] || len(h.Keys) != 1 {
		t.Fatalf("header %+v; want from 5 with the sealed tenant's anchor and the key", h)
	}
	chain := auditchain.Digest(h.ChainBefore)
	if cp, _ := b.master.AuditCheckpoints(context.Background(), 4, 1); auditchain.Digest(cp[0].Head) != chain {
		t.Error("the chain before is not the previous checkpoint's head")
	}
	entries, checkpoints := 0, 0
	for _, it := range items[1:] {
		switch {
		case it.Entry != nil:
			enc, err := auditchain.Encode(chainEntry(it.Entry.Seq, it.Entry.Entry))
			if err != nil || string(enc) != string(it.Entry.Encoded) {
				t.Fatalf("entry %d's encoded form differs", it.Entry.Seq)
			}
			chain = auditchain.Digest(sha(chain, enc))
			if master.AuditDigest(chain) != it.Entry.Chain {
				t.Fatalf("entry %d's chain value does not fold from the one before", it.Entry.Seq)
			}
			entries++
		case it.Checkpoint != nil:
			if master.AuditDigest(chain) != it.Checkpoint.Head {
				t.Fatalf("checkpoint to %d: the chain does not reach its head", it.Checkpoint.To)
			}
			checkpoints++
		}
	}
	if entries != 8 || checkpoints != 2 {
		t.Errorf("%d entries and %d checkpoints exported, want 8 and 2", entries, checkpoints)
	}
}

// exportAll returns every item of an export.
func exportAll(t *testing.T, st master.AuditSealReads, o ExportOptions) []ExportItem {
	t.Helper()
	var items []ExportItem
	if _, err := Export(context.Background(), st, o, func(it ExportItem) error {
		items = append(items, it)
		return nil
	}); err != nil {
		t.Fatalf("Export: %v", err)
	}
	return items
}
