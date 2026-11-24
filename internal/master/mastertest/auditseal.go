package mastertest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// SealLog is what the audit seal's suite needs of a store: its audit log and
// the log's seal. master.Store and tenant.Store both are one, so the master's
// suite and the tenant's run the same tests.
type SealLog interface {
	// AppendAudit is master.Store.AppendAudit.
	AppendAudit(ctx context.Context, entries ...master.AuditEntry) error
	// ListAudit is master.Store.ListAudit.
	ListAudit(ctx context.Context, filter master.AuditFilter, page master.Page) ([]master.AuditEntry, string, error)
	// Close is master.Store.Close.
	Close() error
	master.AuditSeals
}

// OpenSealLog returns a fresh, empty, migrated store, closed when tb ends.
type OpenSealLog func(tb testing.TB) SealLog

// OpenSealLogPair returns two stores on one fresh database, as two processes
// would open it, closed when tb ends.
type OpenSealLogPair func(tb testing.TB) (first, second SealLog)

// RunAuditSeal runs the audit seal's contract against the stores open
// makes. The master's suite (Run) and the tenant's (tenanttest.Run) both
// call it, so the two logs keep one set of rules. The stores keep what they
// are given; the checkpoints here carry made-up digests and signatures of
// the right sizes, since a store checks neither (the code that seals and
// the code that verifies do).
func RunAuditSeal(t *testing.T, open OpenSealLog) {
	tests := []struct {
		name string
		fn   func(*sealEnv)
	}{
		{"SealGivesSeq", testSealGivesSeq},
		{"SealedSeqNeverChanges", testSealedSeqNeverChanges},
		{"LateCommitSealedNext", testLateCommitSealedNext},
		{"UnsealedPaged", testUnsealedPaged},
		{"CheckpointsRead", testCheckpointsRead},
		{"PruneKeepsCheckpoints", testPruneKeepsCheckpoints},
		{"Anchors", testAnchors},
		{"SealRefusals", testSealRefusals},
		{"SealClosed", testSealClosed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.fn(&sealEnv{T: t, s: open(t), ctx: context.Background()})
		})
	}
}

// RunAuditSealShared runs the audit seal's tests across two instances on one
// database.
func RunAuditSealShared(t *testing.T, open OpenSealLogPair) {
	t.Run("SealRaceAcrossInstances", func(t *testing.T) {
		t.Parallel()
		first, second := open(t)
		testSealRace(&sealEnv{T: t, s: first, ctx: context.Background()}, second)
	})
}

// sealEnv is one seal test's store and its helpers.
type sealEnv struct {
	*testing.T
	s   SealLog
	ctx context.Context
	seq int
}

// entry returns a new audit entry at T0 plus offset, with an id that sorts
// in the order entries are made.
func (e *sealEnv) entry(offset time.Duration) master.AuditEntry {
	e.seq++
	return master.AuditEntry{
		ID: fmt.Sprintf("s%06d", e.seq), Time: T0.Add(offset), LocalUser: "server", RemoteAddress: "local",
		Method: "Seal", Outcome: master.OutcomeDone, RequestID: "r" + strconv.Itoa(e.seq),
		Details: map[string]string{"n": strconv.Itoa(e.seq)},
	}
}

// append writes entries.
func (e *sealEnv) append(entries ...master.AuditEntry) {
	e.Helper()
	if err := e.s.AppendAudit(e.ctx, entries...); err != nil {
		e.Fatalf("AppendAudit: %v", err)
	}
}

// digestOf returns a made-up digest of the text.
func digestOf(text string) master.AuditDigest { return sha256.Sum256([]byte(text)) }

// checkpoint returns a checkpoint over seq from to to after prev, made at at.
func checkpoint(from, to int64, prev master.AuditDigest, at time.Time) master.AuditCheckpoint {
	sig := make([]byte, master.AuditSignatureSize)
	copy(sig, fmt.Sprintf("sig %d %d", from, to))
	return master.AuditCheckpoint{
		Installation: "inst-1", Log: master.MasterAuditLog, From: from, To: to,
		Head: digestOf(fmt.Sprintf("head %d", to)), Previous: prev, Digest: digestOf(fmt.Sprintf("checkpoint %d %d %x", from, to, prev)),
		Time: at, KeyVersion: 1, Signature: sig,
	}
}

// latest returns the log's latest checkpoint, or the zero one.
func (e *sealEnv) latest() master.AuditCheckpoint {
	e.Helper()
	cs, err := e.s.RecentAuditCheckpoints(e.ctx, 0, 1)
	if err != nil {
		e.Fatalf("RecentAuditCheckpoints: %v", err)
	}
	if len(cs) == 0 {
		return master.AuditCheckpoint{}
	}
	return cs[0]
}

// sealAll seals every unsealed entry in one checkpoint made at at, as a
// sealing pass would, and returns the checkpoint and the ids in seq order.
func (e *sealEnv) sealAll(at time.Time) (master.AuditCheckpoint, []string) {
	e.Helper()
	unsealed, err := e.s.UnsealedAudit(e.ctx, master.AuditPosition{}, master.MaxAuditSealBatch)
	if err != nil {
		e.Fatalf("UnsealedAudit: %v", err)
	}
	if len(unsealed) == 0 {
		e.Fatal("nothing to seal")
	}
	ids := ids(unsealed)
	last := e.latest()
	c := checkpoint(last.To+1, last.To+int64(len(ids)), last.Digest, at)
	if err := e.s.SealAudit(e.ctx, master.AuditSeal{IDs: ids, Checkpoint: c}); err != nil {
		e.Fatalf("SealAudit: %v", err)
	}
	return c, ids
}

// ids returns the entries' ids in order.
func ids(entries []master.AuditEntry) []string {
	out := make([]string, len(entries))
	for i, a := range entries {
		out[i] = a.ID
	}
	return out
}

// sealed returns every sealed entry, in seq order.
func (e *sealEnv) sealed() []master.SealedAuditEntry {
	e.Helper()
	var out []master.SealedAuditEntry
	from := int64(1)
	for {
		page, err := e.s.SealedAudit(e.ctx, from, 7)
		if err != nil {
			e.Fatalf("SealedAudit: %v", err)
		}
		out = append(out, page...)
		if len(page) < 7 {
			return out
		}
		from = page[len(page)-1].Seq + 1
	}
}

// seqs returns each sealed entry's seq by id.
func (e *sealEnv) seqs() map[string]int64 {
	out := map[string]int64{}
	for _, s := range e.sealed() {
		out[s.Entry.ID] = s.Seq
	}
	return out
}

// unsealed returns the count of unsealed entries.
func (e *sealEnv) unsealed() int64 {
	e.Helper()
	n, err := e.s.CountUnsealedAudit(e.ctx)
	if err != nil {
		e.Fatalf("CountUnsealedAudit: %v", err)
	}
	return n
}

// wantKind fails unless err has the kind want.
func (e *sealEnv) wantKind(what string, err error, want fault.Kind) {
	e.Helper()
	if got := fault.KindOf(err); err == nil || got != want {
		e.Errorf("%s: %v (kind %v), want kind %v", what, err, got, want)
	}
}

// diffCheckpoint describes how got differs from want, or returns "".
func diffCheckpoint(got, want master.AuditCheckpoint) string {
	gt, wt := got.Time, want.Time
	got.Time, want.Time = time.Time{}, time.Time{}
	gs, ws := got.Signature, want.Signature
	got.Signature, want.Signature = nil, nil
	switch {
	case !reflect.DeepEqual(got, want):
		return fmt.Sprintf("%+v, want %+v", got, want)
	case !SameTime(gt, wt):
		return fmt.Sprintf("time %v, want %v", gt, wt)
	case string(gs) != string(ws):
		return "the signature differs"
	}
	return ""
}

// testSealGivesSeq guards that a seal gives the entries named their seq in
// the order named, which a pass takes from UnsealedAudit's order (time,
// then id), that each sealed entry reads back exactly with its seq, that
// none is left unsealed, and that the checkpoint reads back exactly.
func testSealGivesSeq(e *sealEnv) {
	// Made out of time order: the third is the oldest, and two share a time.
	a, b, c, d := e.entry(2*time.Second), e.entry(time.Second), e.entry(0), e.entry(time.Second)
	e.append(a, b, c, d)
	if n := e.unsealed(); n != 4 {
		e.Fatalf("%d unsealed entries, want 4", n)
	}
	unsealed, err := e.s.UnsealedAudit(e.ctx, master.AuditPosition{}, 10)
	if err != nil {
		e.Fatalf("UnsealedAudit: %v", err)
	}
	if got, want := ids(unsealed), []string{c.ID, b.ID, d.ID, a.ID}; !slices.Equal(got, want) {
		e.Fatalf("unsealed order %v, want %v (time, then id)", got, want)
	}
	cp, _ := e.sealAll(T0.Add(time.Minute))
	sealed := e.sealed()
	want := []master.AuditEntry{c, b, d, a}
	if len(sealed) != len(want) {
		e.Fatalf("%d sealed entries, want %d", len(sealed), len(want))
	}
	for i, s := range sealed {
		if s.Seq != int64(i+1) {
			e.Errorf("sealed entry %d has seq %d, want %d", i, s.Seq, i+1)
		}
		if d := DiffAudit(s.Entry, want[i]); d != "" {
			e.Errorf("sealed entry %d: %s", i, d)
		}
	}
	if n := e.unsealed(); n != 0 {
		e.Errorf("%d unsealed entries after the seal, want 0", n)
	}
	if d := diffCheckpoint(e.latest(), cp); d != "" {
		e.Errorf("checkpoint: %s", d)
	}
	if got := e.latest().Entries(); got != 4 {
		e.Errorf("checkpoint covers %d entries, want 4", got)
	}
}

// testSealedSeqNeverChanges guards that a sealed entry's seq never changes:
// a seal that names an entry sealed already, or that does not follow the
// latest checkpoint (another pass sealed first, or a pass restarted with
// what it read before), is Conflict and changes nothing, not even the
// unsealed entries it named with it; and appending more changes no seq.
func testSealedSeqNeverChanges(e *sealEnv) {
	first := []master.AuditEntry{e.entry(0), e.entry(time.Second), e.entry(2 * time.Second)}
	e.append(first...)
	cp1, _ := e.sealAll(T0.Add(time.Minute))
	before := e.seqs()
	fresh := e.entry(3 * time.Second)
	e.append(fresh)

	// The sealed entry named again, with the fresh one, in a seal that
	// otherwise follows the log.
	again := checkpoint(cp1.To+1, cp1.To+2, cp1.Digest, T0.Add(2*time.Minute))
	e.wantKind("SealAudit naming a sealed entry", e.s.SealAudit(e.ctx, master.AuditSeal{IDs: []string{fresh.ID, first[0].ID}, Checkpoint: again}), fault.Conflict)
	// A seal made from what was read before the first seal: it starts at 1.
	stale := checkpoint(1, 1, master.AuditDigest{}, T0.Add(2*time.Minute))
	e.wantKind("SealAudit from before the last seal", e.s.SealAudit(e.ctx, master.AuditSeal{IDs: []string{fresh.ID}, Checkpoint: stale}), fault.Conflict)
	// One that starts where it should but names another previous digest.
	wrongPrev := checkpoint(cp1.To+1, cp1.To+1, digestOf("not the last"), T0.Add(2*time.Minute))
	e.wantKind("SealAudit with another previous digest", e.s.SealAudit(e.ctx, master.AuditSeal{IDs: []string{fresh.ID}, Checkpoint: wrongPrev}), fault.Conflict)
	// One that leaves a gap.
	gap := checkpoint(cp1.To+2, cp1.To+2, cp1.Digest, T0.Add(2*time.Minute))
	e.wantKind("SealAudit leaving a gap", e.s.SealAudit(e.ctx, master.AuditSeal{IDs: []string{fresh.ID}, Checkpoint: gap}), fault.Conflict)
	// One naming an entry that does not exist.
	ghost := checkpoint(cp1.To+1, cp1.To+2, cp1.Digest, T0.Add(2*time.Minute))
	e.wantKind("SealAudit naming no entry", e.s.SealAudit(e.ctx, master.AuditSeal{IDs: []string{fresh.ID, "no-such-entry"}, Checkpoint: ghost}), fault.Conflict)

	after := e.seqs()
	if len(after) != len(before) {
		e.Errorf("%d sealed entries after refused seals, want %d", len(after), len(before))
	}
	for id, seq := range before {
		if after[id] != seq {
			e.Errorf("entry %s has seq %d, was %d", id, after[id], seq)
		}
	}
	if n := e.unsealed(); n != 1 {
		e.Errorf("%d unsealed entries, want the fresh one: a refused seal changed it", n)
	}
	if d := diffCheckpoint(e.latest(), cp1); d != "" {
		e.Errorf("the latest checkpoint changed after refused seals: %s", d)
	}
	e.append(e.entry(-time.Hour))
	e.sealAll(T0.Add(3 * time.Minute))
	for id, seq := range before {
		if got := e.seqs()[id]; got != seq {
			e.Errorf("entry %s has seq %d after another seal, was %d", id, got, seq)
		}
	}
}

// testLateCommitSealedNext guards that an entry committed after a pass read
// the unsealed entries, even one whose time is older than every entry
// sealed, is not in that pass's seal and is sealed by the next, after the
// entries sealed before it, which keep their seq.
func testLateCommitSealedNext(e *sealEnv) {
	e1, e2 := e.entry(10*time.Second), e.entry(20*time.Second)
	e.append(e1, e2)
	read, err := e.s.UnsealedAudit(e.ctx, master.AuditPosition{}, 100)
	if err != nil {
		e.Fatalf("UnsealedAudit: %v", err)
	}
	// Committed between the read and the seal, as a Postgres transaction
	// begun before the pass may commit after it read.
	late, between := e.entry(5*time.Second), e.entry(15*time.Second)
	e.append(late, between)
	c1 := checkpoint(1, int64(len(read)), master.AuditDigest{}, T0.Add(time.Minute))
	if err := e.s.SealAudit(e.ctx, master.AuditSeal{IDs: ids(read), Checkpoint: c1}); err != nil {
		e.Fatalf("SealAudit: %v", err)
	}
	if n := e.unsealed(); n != 2 {
		e.Fatalf("%d unsealed entries after the first seal, want the 2 committed late", n)
	}
	next, err := e.s.UnsealedAudit(e.ctx, master.AuditPosition{}, 100)
	if err != nil {
		e.Fatalf("UnsealedAudit: %v", err)
	}
	if got, want := ids(next), []string{late.ID, between.ID}; !slices.Equal(got, want) {
		e.Fatalf("the next pass reads %v, want %v", got, want)
	}
	e.sealAll(T0.Add(2 * time.Minute))
	got := e.seqs()
	want := map[string]int64{e1.ID: 1, e2.ID: 2, late.ID: 3, between.ID: 4}
	for id, seq := range want {
		if got[id] != seq {
			e.Errorf("entry %s has seq %d, want %d", id, got[id], seq)
		}
	}
	if n := e.unsealed(); n != 0 {
		e.Errorf("%d unsealed entries after the second pass, want 0", n)
	}
}

// testUnsealedPaged guards the reading of unsealed entries in pages: from a
// position, in order of time then id, each entry once, ties in time broken
// by id, and sealed entries never among them.
func testUnsealedPaged(e *sealEnv) {
	var all []master.AuditEntry
	for i := range 25 {
		all = append(all, e.entry(time.Duration(i/3)*time.Second)) // threes share a time
	}
	e.append(all...)
	var got []string
	pos := master.AuditPosition{}
	for range 10 {
		page, err := e.s.UnsealedAudit(e.ctx, pos, 4)
		if err != nil {
			e.Fatalf("UnsealedAudit: %v", err)
		}
		got = append(got, ids(page)...)
		if len(page) < 4 {
			break
		}
		last := page[len(page)-1]
		pos = master.AuditPosition{Time: last.Time, ID: last.ID}
	}
	if want := ids(all); !slices.Equal(got, want) {
		e.Fatalf("paged unsealed entries %v, want %v", got, want)
	}
	e.sealAll(T0.Add(time.Hour))
	page, err := e.s.UnsealedAudit(e.ctx, master.AuditPosition{}, 10)
	if err != nil || len(page) != 0 {
		e.Errorf("UnsealedAudit after sealing every entry = %d entries, %v; want none", len(page), err)
	}
}

// testCheckpointsRead guards both readings of checkpoints: from a seq,
// oldest first, taking the one that covers it; and before a seq, newest
// first, with 0 for no bound.
func testCheckpointsRead(e *sealEnv) {
	var cps []master.AuditCheckpoint
	for i := range 5 {
		e.append(e.entry(time.Duration(i)*time.Second), e.entry(time.Duration(i)*time.Second+time.Millisecond))
		c, _ := e.sealAll(T0.Add(time.Duration(i+1) * time.Minute))
		cps = append(cps, c)
	}
	// Each covers two entries: 1-2, 3-4, 5-6, 7-8, 9-10.
	from, err := e.s.AuditCheckpoints(e.ctx, 4, 2)
	if err != nil {
		e.Fatalf("AuditCheckpoints: %v", err)
	}
	if len(from) != 2 || diffCheckpoint(from[0], cps[1]) != "" || diffCheckpoint(from[1], cps[2]) != "" {
		e.Errorf("AuditCheckpoints from seq 4 = %+v, want the checkpoints to 4 and to 6", from)
	}
	recent, err := e.s.RecentAuditCheckpoints(e.ctx, 7, 10)
	if err != nil {
		e.Fatalf("RecentAuditCheckpoints: %v", err)
	}
	if len(recent) != 3 || recent[0].To != 6 || recent[1].To != 4 || recent[2].To != 2 {
		e.Errorf("RecentAuditCheckpoints before 7 = %d checkpoints, want those to 6, 4 and 2", len(recent))
	}
	all, err := e.s.RecentAuditCheckpoints(e.ctx, 0, 1000)
	if err != nil || len(all) != 5 || diffCheckpoint(all[0], cps[4]) != "" {
		e.Errorf("RecentAuditCheckpoints with no bound = %d, %v; want the 5, newest first", len(all), err)
	}
	past, err := e.s.AuditCheckpoints(e.ctx, 11, 10)
	if err != nil || len(past) != 0 {
		e.Errorf("AuditCheckpoints past the last = %d, %v; want none", len(past), err)
	}
	_, err = e.s.AuditCheckpoints(e.ctx, 0, 10)
	e.wantKind("AuditCheckpoints from seq 0", err, fault.Invalid)
	_, err = e.s.AuditCheckpoints(e.ctx, 1, 1001)
	e.wantKind("AuditCheckpoints limit 1001", err, fault.Invalid)
	_, err = e.s.RecentAuditCheckpoints(e.ctx, -1, 10)
	e.wantKind("RecentAuditCheckpoints before -1", err, fault.Invalid)
	_, err = e.s.SealedAudit(e.ctx, 0, 10)
	e.wantKind("SealedAudit from seq 0", err, fault.Invalid)
	_, err = e.s.SealedAudit(e.ctx, 1, master.MaxAuditSealBatch+1)
	e.wantKind("SealedAudit past its limit", err, fault.Invalid)
	_, err = e.s.UnsealedAudit(e.ctx, master.AuditPosition{}, 0)
	e.wantKind("UnsealedAudit limit 0", err, fault.Invalid)
}

// testPruneKeepsCheckpoints guards retention: a pruning removes the entries
// of whole checkpoints made before the time given, oldest first and at most
// its limit, marks them pruned and keeps every checkpoint row, never
// removes an unsealed entry however old, writes its own entry with what it
// removed in the same change, and writes nothing when nothing is due.
func testPruneKeepsCheckpoints(e *sealEnv) {
	var cps []master.AuditCheckpoint
	for i := range 3 {
		for j := range i + 2 {
			e.append(e.entry(time.Duration(10*i+j) * time.Second))
		}
		c, _ := e.sealAll(T0.Add(time.Duration(i) * time.Hour))
		cps = append(cps, c)
	}
	old := e.entry(-24 * time.Hour) // unsealed and older than everything
	e.append(old)

	prune := e.entry(0)
	got, err := e.s.PruneAudit(e.ctx, T0.Add(90*time.Minute), 1, prune)
	if err != nil {
		e.Fatalf("PruneAudit: %v", err)
	}
	if got.Checkpoints != 1 || got.Entries != 2 || !got.More {
		e.Errorf("PruneAudit with limit 1 = %+v, want 1 checkpoint, 2 entries, more", got)
	}
	again := e.entry(0)
	got, err = e.s.PruneAudit(e.ctx, T0.Add(90*time.Minute), master.MaxAuditPruneBatch, again)
	if err != nil {
		e.Fatalf("PruneAudit: %v", err)
	}
	if got.Checkpoints != 1 || got.Entries != 3 || got.More {
		e.Errorf("PruneAudit = %+v, want the second checkpoint's 3 entries, no more", got)
	}
	nothing := e.entry(0)
	got, err = e.s.PruneAudit(e.ctx, T0.Add(90*time.Minute), master.MaxAuditPruneBatch, nothing)
	if err != nil || got != (master.AuditPruned{}) {
		e.Errorf("PruneAudit with nothing due = %+v, %v; want nothing", got, err)
	}

	all, err := e.s.RecentAuditCheckpoints(e.ctx, 0, 10)
	if err != nil || len(all) != 3 {
		e.Fatalf("checkpoints after pruning = %d, %v; want all 3 kept", len(all), err)
	}
	slices.Reverse(all)
	for i, c := range all {
		want := cps[i]
		want.Pruned = i < 2
		if d := diffCheckpoint(c, want); d != "" {
			e.Errorf("checkpoint %d after pruning: %s", i, d)
		}
		if c.Entries() != want.Entries() {
			e.Errorf("checkpoint %d covers %d entries after pruning, want %d", i, c.Entries(), want.Entries())
		}
	}
	sealed := e.sealed()
	if len(sealed) != 4 || sealed[0].Seq != cps[2].From {
		e.Errorf("sealed entries after pruning start at %v (%d), want the third checkpoint's 4 from seq %d", sealed, len(sealed), cps[2].From)
	}
	listed := map[string]master.AuditEntry{}
	for _, a := range e.allAudit() {
		listed[a.ID] = a
	}
	if _, ok := listed[old.ID]; !ok {
		e.Error("the old unsealed entry was pruned; only sealed checkpoints are")
	}
	for _, p := range []struct {
		entry                 master.AuditEntry
		checkpoints, n, f, to string
	}{{prune, "1", "2", "1", "2"}, {again, "1", "3", "3", "5"}} {
		a, ok := listed[p.entry.ID]
		if !ok {
			e.Errorf("the pruning's entry %s was not written", p.entry.ID)
			continue
		}
		want := map[string]string{master.DetailPrunedCheckpoints: p.checkpoints, master.DetailPrunedEntries: p.n,
			master.DetailPrunedFromSeq: p.f, master.DetailPrunedToSeq: p.to}
		for k, v := range want {
			if a.Details[k] != v {
				e.Errorf("the pruning's entry has %s = %q, want %q", k, a.Details[k], v)
			}
		}
	}
	if _, ok := listed[nothing.ID]; ok {
		e.Error("a pruning with nothing due wrote its entry")
	}
	_, err = e.s.PruneAudit(e.ctx, T0, master.MaxAuditPruneBatch+1, e.entry(0))
	e.wantKind("PruneAudit past its limit", err, fault.Invalid)
	// Seal everything (the prunings' entries included), then write one
	// entry that stays unsealed, whose id the next pruning's entry takes.
	e.append(e.entry(0))
	e.sealAll(T0.Add(3 * time.Hour))
	kept := e.entry(0)
	e.append(kept)
	taken := e.entry(0)
	taken.ID = kept.ID
	before := len(e.sealed())
	_, err = e.s.PruneAudit(e.ctx, T0.Add(5*time.Hour), master.MaxAuditPruneBatch, taken)
	if fault.KindOf(err) != fault.Internal {
		e.Errorf("PruneAudit with a taken entry id: %v, want Internal", err)
	}
	if after := len(e.sealed()); after != before {
		e.Errorf("a pruning whose entry failed removed %d entries; want none", before-after)
	}
}

// allAudit returns every entry of the log.
func (e *sealEnv) allAudit() []master.AuditEntry {
	e.Helper()
	var out []master.AuditEntry
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, page)
		if err != nil {
			e.Fatalf("ListAudit: %v", err)
		}
		out = append(out, entries...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// testAnchors guards the anchors a log keeps of others: kept and read back
// in order of log, the same one again changes nothing, a later one
// replaces it, and one that goes back, or names another digest at the same
// place, is Conflict and changes nothing.
func testAnchors(e *sealEnv) {
	a := master.AuditAnchor{Log: "tenant-b", To: 10, Digest: digestOf("b10")}
	m := master.AuditAnchor{Log: master.MasterAuditLog, To: 3, Digest: digestOf("m3")}
	for _, x := range []master.AuditAnchor{a, m, a} {
		if err := e.s.SetAuditAnchor(e.ctx, x); err != nil {
			e.Fatalf("SetAuditAnchor(%s): %v", x.Log, err)
		}
	}
	later := master.AuditAnchor{Log: "tenant-b", To: 12, Digest: digestOf("b12")}
	if err := e.s.SetAuditAnchor(e.ctx, later); err != nil {
		e.Fatalf("SetAuditAnchor, later: %v", err)
	}
	e.wantKind("SetAuditAnchor going back", e.s.SetAuditAnchor(e.ctx, a), fault.Conflict)
	e.wantKind("SetAuditAnchor, another digest at the same place",
		e.s.SetAuditAnchor(e.ctx, master.AuditAnchor{Log: "tenant-b", To: 12, Digest: digestOf("other")}), fault.Conflict)
	e.wantKind("SetAuditAnchor, seq 0", e.s.SetAuditAnchor(e.ctx, master.AuditAnchor{Log: "x", To: 0}), fault.Invalid)
	e.wantKind("SetAuditAnchor, bad log", e.s.SetAuditAnchor(e.ctx, master.AuditAnchor{Log: "bad log", To: 1}), fault.Invalid)
	got, err := e.s.AuditAnchors(e.ctx)
	if err != nil {
		e.Fatalf("AuditAnchors: %v", err)
	}
	if want := []master.AuditAnchor{m, later}; !slices.Equal(got, want) {
		e.Errorf("anchors %+v, want %+v", got, want)
	}
}

// testSealRefusals guards that a seal past the port's bounds is Invalid and
// changes nothing.
func testSealRefusals(e *sealEnv) {
	x, y := e.entry(0), e.entry(time.Second)
	e.append(x, y)
	good := checkpoint(1, 2, master.AuditDigest{}, T0)
	cases := map[string]master.AuditSeal{
		"no entries":             {Checkpoint: checkpoint(1, 1, master.AuditDigest{}, T0)},
		"fewer ids than covered": {IDs: []string{x.ID}, Checkpoint: good},
		"an id twice":            {IDs: []string{x.ID, x.ID}, Checkpoint: good},
		"a bad id":               {IDs: []string{x.ID, "bad id"}, Checkpoint: good},
	}
	bad := func(change func(*master.AuditCheckpoint)) master.AuditSeal {
		c := good
		change(&c)
		return master.AuditSeal{IDs: []string{x.ID, y.ID}, Checkpoint: c}
	}
	cases["from 0"] = bad(func(c *master.AuditCheckpoint) { c.From, c.To = 0, 1 })
	cases["a short signature"] = bad(func(c *master.AuditCheckpoint) { c.Signature = c.Signature[:63] })
	cases["key version 0"] = bad(func(c *master.AuditCheckpoint) { c.KeyVersion = 0 })
	cases["key version past 2^31-1"] = bad(func(c *master.AuditCheckpoint) { c.KeyVersion = master.MaxAuditKeyVersion + 1 })
	cases["no installation"] = bad(func(c *master.AuditCheckpoint) { c.Installation = "" })
	cases["a bad log"] = bad(func(c *master.AuditCheckpoint) { c.Log = "a log" })
	cases["already pruned"] = bad(func(c *master.AuditCheckpoint) { c.Pruned = true })
	cases["no time"] = bad(func(c *master.AuditCheckpoint) { c.Time = time.Time{} })
	for name, s := range cases {
		e.wantKind("SealAudit, "+name, e.s.SealAudit(e.ctx, s), fault.Invalid)
	}
	if n := e.unsealed(); n != 2 {
		e.Errorf("%d unsealed entries after refused seals, want 2", n)
	}
	if c := e.latest(); c.To != 0 {
		e.Errorf("a refused seal kept a checkpoint to %d", c.To)
	}
}

// testSealClosed guards that a closed store answers every seal operation as
// Unavailable.
func testSealClosed(e *sealEnv) {
	if err := e.s.Close(); err != nil {
		e.Fatalf("Close: %v", err)
	}
	_, err := e.s.UnsealedAudit(e.ctx, master.AuditPosition{}, 1)
	e.wantKind("UnsealedAudit after Close", err, fault.Unavailable)
	_, err = e.s.CountUnsealedAudit(e.ctx)
	e.wantKind("CountUnsealedAudit after Close", err, fault.Unavailable)
	_, err = e.s.SealedAudit(e.ctx, 1, 1)
	e.wantKind("SealedAudit after Close", err, fault.Unavailable)
	_, err = e.s.AuditCheckpoints(e.ctx, 1, 1)
	e.wantKind("AuditCheckpoints after Close", err, fault.Unavailable)
	_, err = e.s.AuditAnchors(e.ctx)
	e.wantKind("AuditAnchors after Close", err, fault.Unavailable)
	e.wantKind("SealAudit after Close", e.s.SealAudit(e.ctx, master.AuditSeal{IDs: []string{"a"}, Checkpoint: checkpoint(1, 1, master.AuditDigest{}, T0)}), fault.Unavailable)
	e.wantKind("SetAuditAnchor after Close", e.s.SetAuditAnchor(e.ctx, master.AuditAnchor{Log: "x", To: 1}), fault.Unavailable)
	_, err = e.s.PruneAudit(e.ctx, T0, 1, e.entry(0))
	e.wantKind("PruneAudit after Close", err, fault.Unavailable)
}

// testSealRace guards two instances sealing the same entries at once, as two
// servers started on one database by mistake would: in each round exactly
// one seal is kept and the other is Conflict, and every entry has one seq.
func testSealRace(e *sealEnv, other SealLog) {
	const rounds = 15
	for round := range rounds {
		batch := []master.AuditEntry{e.entry(time.Duration(round) * time.Second), e.entry(time.Duration(round)*time.Second + time.Millisecond)}
		e.append(batch...)
		last := e.latest()
		c := checkpoint(last.To+1, last.To+2, last.Digest, T0.Add(time.Duration(round)*time.Minute))
		seal := master.AuditSeal{IDs: ids(batch), Checkpoint: c}
		var wg sync.WaitGroup
		errs := make([]error, 2)
		for i, s := range []SealLog{e.s, other} {
			wg.Go(func() { errs[i] = s.SealAudit(e.ctx, seal) })
		}
		wg.Wait()
		ok, conflict := 0, 0
		for _, err := range errs {
			switch {
			case err == nil:
				ok++
			case fault.KindOf(err) == fault.Conflict:
				conflict++
			default:
				e.Fatalf("round %d: SealAudit: %v", round, err)
			}
		}
		if ok != 1 || conflict != 1 {
			e.Fatalf("round %d: %d seals kept and %d refused, want one of each", round, ok, conflict)
		}
	}
	seqs := e.seqs()
	if len(seqs) != 2*rounds {
		e.Fatalf("%d sealed entries, want %d", len(seqs), 2*rounds)
	}
	seen := map[int64]bool{}
	for _, s := range seqs {
		if seen[s] {
			e.Fatalf("seq %d given twice", s)
		}
		seen[s] = true
	}
}
