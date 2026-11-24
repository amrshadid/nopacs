package auditseal

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestPassSealsEveryLog guards a pass over a master and two tenants on each
// database type: every entry gets a seq, every log verifies with the public
// keys derived from the key file, and the master keeps each tenant's latest
// checkpoint and each tenant the master's.
func TestPassSealsEveryLog(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			keys := keyFile(t, 1)
			pub, err := PublicKeys(keys, testInstallation)
			if err != nil {
				t.Fatal(err)
			}
			c := &clock{t: t0.Add(time.Hour)}
			s := b.sealer(t, signerOf(t, keys, 1), c, 0)
			write(t, b.master, 30, 0)
			write(t, b.tenants["lab"], 20, 0)
			write(t, b.tenants["clinic"], 5, 0)
			s.Pass(context.Background())

			for log, want := range map[string]int64{master.MasterAuditLog: 30, b.ids["lab"]: 20, b.ids["clinic"]: 5} {
				r := b.verify(t, log, pub)
				if r.EntriesChecked != want || r.Unsealed != 0 || r.Last == nil || r.Last.To != want {
					t.Errorf("%s: checked %d, unsealed %d, last %+v; want %d checked, none unsealed", log, r.EntriesChecked, r.Unsealed, r.Last, want)
				}
			}
			ctx := context.Background()
			anchors, err := b.master.AuditAnchors(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if len(anchors) != 2 {
				t.Fatalf("the master keeps %d anchors, want one per tenant", len(anchors))
			}
			for _, a := range anchors {
				st := b.logs()[a.Log]
				if l := latestOf(t, st); a.To != l.To || a.Digest != l.Digest {
					t.Errorf("the master's anchor of %s is %d, not its latest checkpoint %d", a.Log, a.To, l.To)
				}
			}
			ml := latestOf(t, b.master)
			for name, st := range b.tenants {
				as, err := st.AuditAnchors(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if len(as) != 1 || as[0].Log != master.MasterAuditLog || as[0].Digest != ml.Digest {
					t.Errorf("tenant %s keeps anchors %+v, want the master's latest", name, as)
				}
			}
		})
	}
}

// TestLateEntrySealedNextPass guards that an entry written after a pass,
// even one whose time is older than every entry sealed, is sealed by the
// next pass after them, while the entries sealed before keep their seq, and
// the log still verifies across both checkpoints.
func TestLateEntrySealedNextPass(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			keys := keyFile(t, 1)
			pub, _ := PublicKeys(keys, testInstallation)
			s := b.sealer(t, signerOf(t, keys, 1), &clock{t: t0.Add(time.Hour)}, 0)
			first := write(t, b.master, 10, time.Minute)
			s.Pass(context.Background())
			before := seqsOf(t, b.master)
			late := write(t, b.master, 1, -time.Hour)
			s.Pass(context.Background())
			after := seqsOf(t, b.master)
			for _, e := range first {
				if after[e.ID] != before[e.ID] {
					t.Errorf("entry %s moved from seq %d to %d", e.ID, before[e.ID], after[e.ID])
				}
			}
			if after[late[0].ID] != 11 {
				t.Errorf("the late entry has seq %d, want 11", after[late[0].ID])
			}
			if r := b.verify(t, master.MasterAuditLog, pub); r.CheckpointsChecked != 2 {
				t.Errorf("%d checkpoints checked, want 2", r.CheckpointsChecked)
			}
		})
	}
}

// seqsOf returns every sealed entry's seq by id.
func seqsOf(t *testing.T, st master.AuditSealReads) map[string]int64 {
	t.Helper()
	out := map[string]int64{}
	from := int64(1)
	for {
		page, err := st.SealedAudit(context.Background(), from, master.MaxAuditSealBatch)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range page {
			out[s.Entry.ID] = s.Seq
		}
		if len(page) < master.MaxAuditSealBatch {
			return out
		}
		from = page[len(page)-1].Seq + 1
	}
}

// racingLog is a log whose first SealAudit is preceded by another sealer's
// seal of the same entries, as when a second server on the same database,
// or this one before a restart, seals between the pass's read and its seal.
type racingLog struct {
	master.AuditSeals
	race  func()
	raced bool
}

// SealAudit lets the other sealer seal first, once.
func (r *racingLog) SealAudit(ctx context.Context, s master.AuditSeal) error {
	if !r.raced {
		r.raced = true
		r.race()
	}
	return r.AuditSeals.SealAudit(ctx, s)
}

// TestSealRestartsAfterConflict guards a pass's restart safety: a seal made
// from what was read before another sealer sealed is refused by the store,
// the pass reads the log again and seals what is left, and the log verifies
// with every entry once; a new Sealer, as after a restart, goes on from the
// checkpoints in the database; and a pass cut short leaves logs the next
// pass completes.
func TestSealRestartsAfterConflict(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			keys := keyFile(t, 1)
			pub, _ := PublicKeys(keys, testInstallation)
			signer := signerOf(t, keys, 1)
			write(t, b.master, 6, 0)
			r := &racingLog{AuditSeals: b.master}
			r.race = func() {
				write(t, b.master, 2, time.Second)
				if _, _, err := SealLog(ctx, b.master, testInstallation, master.MasterAuditLog, signer, time.Now, nil); err != nil {
					t.Fatalf("the other sealer: %v", err)
				}
			}
			last, ok, err := SealLog(ctx, r, testInstallation, master.MasterAuditLog, signer, time.Now, nil)
			if err != nil || !ok {
				t.Fatalf("SealLog after a conflict: %v, %v", ok, err)
			}
			if !r.raced || last.To != 8 {
				t.Errorf("raced %v, last checkpoint to %d; want the race and 8 entries", r.raced, last.To)
			}
			write(t, b.master, 3, 2*time.Second)
			// A new sealer, as after a restart, whose pass is cut short
			// before the tenants: the master is sealed, the tenants are not.
			write(t, b.tenants["lab"], 4, 0)
			cut, cancel := context.WithCancel(ctx)
			s := b.sealer(t, signer, &clock{t: t0.Add(time.Hour)}, 0)
			s.cfg.Tenants = cancelOnList{Tenants: b, cancel: cancel}
			s.Pass(cut)
			if n, _ := b.tenants["lab"].CountUnsealedAudit(ctx); n != 4 {
				t.Fatalf("the cut pass sealed the tenant (%d unsealed left), want it left for the next", n)
			}
			b.sealer(t, signer, &clock{t: t0.Add(2 * time.Hour)}, 0).Pass(ctx)
			if r := b.verify(t, master.MasterAuditLog, pub); r.EntriesChecked != 11 {
				t.Errorf("master: %d entries checked, want 11", r.EntriesChecked)
			}
			b.verify(t, b.ids["lab"], pub)
		})
	}
}

// cancelOnList cancels the pass's context when the tenants are listed, as a
// stop or a crash would cut a pass after the master's seal.
type cancelOnList struct {
	Tenants
	cancel context.CancelFunc
}

// List cancels, then lists.
func (c cancelOnList) List(ctx context.Context) ([]TenantLog, error) {
	c.cancel()
	return c.Tenants.List(ctx)
}

// day is a day, for retention's periods.
const day = 24 * time.Hour

// TestRetentionPrunesWholeCheckpoints guards retention in a pass: the
// checkpoints made before audit.retention ago lose their entries and stay,
// marked pruned; the pruning's own entry is the server's and is sealed in
// the same pass, so the log verifies across the pruned checkpoint at once;
// and a retention of 0 prunes nothing.
func TestRetentionPrunesWholeCheckpoints(t *testing.T) {
	for _, b := range backends(t) {
		t.Run(b.name, func(t *testing.T) {
			ctx := context.Background()
			keys := keyFile(t, 1)
			pub, _ := PublicKeys(keys, testInstallation)
			c := &clock{t: t0}
			s := b.sealer(t, signerOf(t, keys, 1), c, 0)
			write(t, b.master, 5, 0)
			s.Pass(ctx)
			c.set(t0.Add(12 * time.Hour))
			write(t, b.master, 4, time.Hour)
			s.Pass(ctx)
			c.set(t0.Add(30 * time.Hour))
			write(t, b.master, 3, 2*time.Hour)
			s.Pass(ctx) // retention 0: nothing pruned
			if n := len(seqsOf(t, b.master)); n != 12 {
				t.Fatalf("%d sealed entries with retention 0, want 12", n)
			}
			// Ninety days and six hours on, with a new entry: the cutoff is six
			// hours after the first checkpoint, before the second.
			c.set(t0.Add(90*day + 6*time.Hour))
			write(t, b.master, 1, 3*time.Hour)
			kept := b.sealer(t, signerOf(t, keys, 1), c, 90*day)
			kept.Pass(ctx)
			cps, err := b.master.AuditCheckpoints(ctx, 1, 10)
			if err != nil {
				t.Fatal(err)
			}
			if len(cps) != 5 || !cps[0].Pruned || cps[1].Pruned || cps[2].Pruned || cps[3].Pruned || cps[4].Pruned {
				t.Fatalf("checkpoints after pruning: %+v; want 5 (the new entry's, the pruning's), the first alone pruned", cps)
			}
			seqs := seqsOf(t, b.master)
			if len(seqs) != 9 {
				t.Errorf("%d sealed entries after pruning, want 9", len(seqs))
			}
			all, _, err := b.master.ListAudit(ctx, master.AuditFilter{}, master.Page{Limit: 100})
			if err != nil {
				t.Fatal(err)
			}
			var prune *master.AuditEntry
			for i, e := range all {
				if e.Method == "AuditRetention" {
					prune = &all[i]
				}
			}
			if prune == nil || prune.LocalUser != ServerUser || prune.Details[master.DetailPrunedEntries] != "5" ||
				prune.Details[master.DetailPrunedFromSeq] != "1" || prune.Details[master.DetailPrunedToSeq] != "5" {
				t.Fatalf("the pruning's entry is %+v, want the server's, with 5 entries, seq 1 to 5", prune)
			}
			if _, sealed := seqs[prune.ID]; !sealed {
				t.Error("the pruning's entry was not sealed in the pass that pruned")
			}
			if r := b.verifyAt(t, master.MasterAuditLog, pub, c.now()); r.CheckpointsChecked != 5 {
				t.Errorf("%d checkpoints checked across the pruned one, want 5", r.CheckpointsChecked)
			}
		})
	}
}

// TestPruneCutoff guards the cutoff of retention against a clock: a clock
// stepped forward with no new checkpoint prunes nothing, since the cutoff
// is counted back from the earlier of now and the log's latest
// checkpoint's signed time; and a period below the floor of audit.retention
// is raised to it, so a checkpoint younger than ninety days is never
// pruned.
func TestPruneCutoff(t *testing.T) {
	b := sqliteBackend(t)
	ctx := context.Background()
	keys := keyFile(t, 1)
	c := &clock{t: t0}
	write(t, b.master, 3, 0)
	b.sealer(t, signerOf(t, keys, 1), c, 0).Pass(ctx)
	c.set(t0.Add(200 * day))
	b.sealer(t, signerOf(t, keys, 1), c, 90*day).Pass(ctx)
	if cps, _ := b.master.AuditCheckpoints(ctx, 1, 10); len(cps) != 1 || cps[0].Pruned {
		t.Fatalf("a clock 200 days on with no new checkpoint: %+v; want nothing pruned", cps)
	}

	short := sqliteBackend(t)
	c.set(t0)
	write(t, short.master, 3, 0)
	b2 := short.sealer(t, signerOf(t, keys, 1), c, day)
	b2.Pass(ctx)
	c.set(t0.Add(30 * day))
	write(t, short.master, 1, time.Hour)
	b2.Pass(ctx)
	if cps, _ := short.master.AuditCheckpoints(ctx, 1, 10); len(cps) != 2 || cps[0].Pruned {
		t.Fatalf("a retention of a day after 30 days: %+v; want nothing pruned under the 90-day floor", cps)
	}
}

// TestKeyRotation guards a log sealed under two key versions: it verifies
// with both public keys, and a verification given only the old one finds
// the new checkpoint's signature bad, so a key is never taken on trust from
// the log.
func TestKeyRotation(t *testing.T) {
	b := sqliteBackend(t)
	ctx := context.Background()
	keys := keyFile(t, 2)
	pub, err := PublicKeys(keys, testInstallation)
	if err != nil || len(pub) != 2 || pub[0].Current || !pub[1].Current {
		t.Fatalf("PublicKeys = %+v, %v; want versions 1 and 2, 2 current", pub, err)
	}
	c := &clock{t: t0}
	write(t, b.master, 3, 0)
	b.sealer(t, signerOf(t, keys, 1), c, 0).Pass(ctx)
	write(t, b.master, 3, time.Second)
	c.set(t0.Add(time.Minute))
	b.sealer(t, signerOf(t, keys, 2), c, 0).Pass(ctx)
	b.verify(t, master.MasterAuditLog, pub)
	r, err := Verify(ctx, b.master, VerifyOptions{Installation: testInstallation, Log: master.MasterAuditLog, Keys: pub[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if r.Intact || len(r.Problems) != 1 || r.Problems[0].Kind != BadSignature || r.Problems[0].Seq != 6 {
		t.Errorf("verified with the old key alone: %+v; want a bad signature at seq 6", r.Problems)
	}
	// A pass with the old key after the new one has signed is refused: the
	// chain never goes back to a retired key.
	write(t, b.master, 1, 2*time.Second)
	_, _, err = SealLog(ctx, b.master, testInstallation, master.MasterAuditLog, signerOf(t, keys, 1), c.now, nil)
	if fault.KindOf(err) != fault.Internal {
		t.Errorf("sealing with the retired key: %v, want refused", err)
	}
}

// TestStopSealsWhatIsLeft guards the stop: Start's first pass runs at once;
// Stop ends the timer and seals nothing itself, since the server still
// drains requests and writes the audit entries it held after it; and Last,
// which the server runs once those are written, seals what was written up
// to then.
func TestStopSealsWhatIsLeft(t *testing.T) {
	b := sqliteBackend(t)
	ctx := context.Background()
	keys := keyFile(t, 1)
	write(t, b.master, 2, 0)
	s := b.sealer(t, signerOf(t, keys, 1), &clock{t: t0}, 0)
	s.cfg.Interval = func(context.Context) (time.Duration, error) { return time.Hour, nil }
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if n, _ := b.master.CountUnsealedAudit(ctx); n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Start's first pass did not seal")
		}
		time.Sleep(10 * time.Millisecond)
	}
	write(t, b.master, 3, time.Second)
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// Written after Stop, as during the server's drain and audit flush.
	write(t, b.master, 2, 2*time.Second)
	if n, _ := b.master.CountUnsealedAudit(ctx); n != 5 {
		t.Fatalf("%d entries unsealed after Stop, want the 5 written since the first pass", n)
	}
	if err := s.Last(ctx); err != nil {
		t.Fatal(err)
	}
	if n, _ := b.master.CountUnsealedAudit(ctx); n != 0 {
		t.Errorf("%d entries unsealed after Last, want 0", n)
	}
}
