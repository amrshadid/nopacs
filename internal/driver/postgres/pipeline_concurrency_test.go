package postgres

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests in this file need several writers at once, which only a
// database such as Postgres has: many connections, through several stores
// on one schema as several processes would be, calling at the same moment.
// Each runs its race many rounds and then checks the rows themselves, read
// with a connection of its own.

// TestOpenOneReceptionAtOnce guards OpenReception with many writers: 24
// calls at once, through three stores, for one study at three receivers
// open exactly one reception per receiver, every call at a receiver
// answers that one, and the study's arrivals are 1, 2 and 3, each once;
// twenty rounds, each a new study.
func TestOpenOneReceptionAtOnce(t *testing.T) {
	e := newCenv(t, 3, 8)
	e.storage("store")
	receivers := []string{"a", "b", "c"}
	for i, r := range receivers {
		e.receiver(r, "store", 11112+i)
	}
	for round := range 20 {
		study := cUID(1, round)
		const n = 24
		got := make([]tenant.Reception, n)
		errs := make([]error, n)
		atOnce(n, func(i int) {
			got[i], _, errs[i] = e.stores[i%3].OpenReception(e.ctx, receivers[i%3], study, fmt.Sprintf("o%02d-%02d", round, i), cT0)
		})
		byReceiver := map[string]string{}
		arrivals := map[int]string{}
		for i := range n {
			e.ok("OpenReception", errs[i])
			r := got[i]
			if id, seen := byReceiver[r.Receiver]; seen && id != r.ID {
				t.Fatalf("round %d: opens at %s made two receptions, %s and %s", round, r.Receiver, id, r.ID)
			}
			byReceiver[r.Receiver] = r.ID
			if id, seen := arrivals[r.Arrival]; seen && id != r.ID {
				t.Fatalf("round %d: arrival %d given to %s and %s", round, r.Arrival, id, r.ID)
			}
			arrivals[r.Arrival] = r.ID
		}
		if len(byReceiver) != 3 || len(arrivals) != 3 || arrivals[1] == "" || arrivals[2] == "" || arrivals[3] == "" {
			t.Fatalf("round %d: receptions %v, arrivals %v; want one at each receiver, arrivals 1 to 3", round, byReceiver, arrivals)
		}
		if rows := e.scalar(`SELECT count(*) FROM receptions WHERE study_uid = $1`, study); rows != 3 {
			t.Fatalf("round %d: %d rows for the study, want 3", round, rows)
		}
	}
	if n := e.uniqueRetries(); n != 0 {
		t.Errorf("%d opens were run again after a unique violation: an open did not meet the reception that won", n)
	}
	e.checkTotals("after the opens")
}

// TestClaimsInOppositeOrders guards ClaimInstances with two writers: two
// receptions claim the same 200 instances at once, one given them in
// ascending order and the other in descending, through two stores; forty
// rounds. Every UID is claimed now by exactly one and answered Claimed, by
// that one, to the other; no error reaches either caller; and since every
// claim takes its rows in sorted order, no deadlock happened at all, not
// even one the driver hid by running the claim again.
func TestClaimsInOppositeOrders(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	e.receiver("a", "store", 11112)
	e.receiver("b", "store", 11113)
	const files = 200
	for round := range 40 {
		study := cUID(1, round)
		mkFiles := func(r tenant.Reception) []tenant.ReceivedFile {
			out := make([]tenant.ReceivedFile, files)
			for i := range out {
				out[i] = cFile(r, round, round*1000+i)
			}
			return out
		}
		r1, f1 := e.identified("a", study, "store", "sub-01", "ses-"+study, mkFiles)
		r2, f2 := e.identified("b", study, "store", "sub-01", "ses-"+study, mkFiles)
		c1, c2 := make2Claims(f1, false), make2Claims(f2, true)
		recs := []tenant.Reception{r1, r2}
		claims := [][]tenant.InstanceClaim{c1, c2}
		answers := make([][]tenant.Duplicate, 2)
		errs := make([]error, 2)
		atOnce(2, func(i int) {
			answers[i], errs[i] = e.stores[i].ClaimInstances(e.ctx, recs[i].ID, claims[i], cT0, nil)
		})
		e.ok("ClaimInstances", errs[0])
		e.ok("ClaimInstances", errs[1])
		kinds := map[string][2]tenant.Duplicate{}
		for i := range 2 {
			for k, c := range claims[i] {
				v := kinds[c.SOPInstanceUID]
				v[i] = answers[i][k]
				kinds[c.SOPInstanceUID] = v
			}
		}
		for uid, v := range kinds {
			a, b := v[0], v[1]
			switch {
			case a.Kind == tenant.ClaimedNow && b.Kind == tenant.Claimed && b.HeldBy == r1.ID:
			case b.Kind == tenant.ClaimedNow && a.Kind == tenant.Claimed && a.HeldBy == r2.ID:
			default:
				t.Fatalf("round %d: %s answered %v and %v, want one claimed now and the other told so", round, uid, a, b)
			}
		}
	}
	if n := e.deadlocks(); n != 0 {
		t.Errorf("%d claims were run again after a deadlock: the claims did not take their rows in one order", n)
	}
	if n := e.uniqueRetries(); n != 0 {
		t.Errorf("%d claims were run again after a unique violation", n)
	}
	e.checkTotals("after the claims")
}

// make2Claims returns the claims of files in ascending order of UID, or
// descending.
func make2Claims(files []tenant.ReceivedFile, descending bool) []tenant.InstanceClaim {
	out := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		out[i] = claimOfFile(f)
	}
	slices.SortFunc(out, func(a, b tenant.InstanceClaim) int {
		if descending {
			a, b = b, a
		}
		switch {
		case a.SOPInstanceUID < b.SOPInstanceUID:
			return -1
		case a.SOPInstanceUID > b.SOPInstanceUID:
			return 1
		}
		return 0
	})
	return out
}

// TestCommitsShareCatalogRows guards CommitScan with many writers, in ten
// rounds each with a new subject:
//
//   - six receptions of one new study, at six receivers, commit their
//     scans at once (one series all share, and one of each's own): one
//     subject, one session, seven scans;
//   - six receptions of six new studies of that subject commit at once:
//     still one subject, and six more sessions;
//   - two new studies of the subject with one session label commit at
//     once: one is made, the other is a label conflict held by another
//     session, never an unclassified failure.
//
// Each commit carries a modality. After each round the running totals of
// the catalog, its subjects, sessions and scans equal a recount of the
// rows, and each session's modalities are its scans'.
func TestCommitsShareCatalogRows(t *testing.T) {
	e := newCenv(t, 3, 8)
	e.storage("store")
	const k = 6
	for i := range k {
		e.receiver(fmt.Sprintf("r%d", i), "store", 11112+i)
	}
	n := 0
	files := func(r tenant.Reception, series ...int) []tenant.ReceivedFile {
		var out []tenant.ReceivedFile
		for _, se := range series {
			for range 20 {
				n++
				out = append(out, cFile(r, se, n))
			}
		}
		return out
	}
	// Each commit carries a modality of its own among four, so commits of
	// one scan at once race to give it theirs and commits of one session
	// to add its modalities; checkTotals then compares the session's
	// modalities with its scans'.
	modalities := []string{"MR", "CT", "PT", "SR"}
	commitAll := func(round int, commits []tenant.ScanCommit) []error {
		errs := make([]error, len(commits))
		atOnce(len(commits), func(i int) {
			c := commits[i]
			c.Values = tenant.ScanValuesOf("20200101", modalities[(round+i)%len(modalities)], "", "")
			_, errs[i] = e.stores[i%len(e.stores)].CommitScan(e.ctx, c)
		})
		return errs
	}
	for round := range 10 {
		subject := fmt.Sprintf("sub-%02d", round)
		before, err := e.p().CatalogTotals(e.ctx)
		e.ok("CatalogTotals", err)

		// One new study at six receivers.
		study := cUID(1, 100+round)
		session := "ses-" + study
		shared := 10000 + round*100
		var commits []tenant.ScanCommit
		for i := range k {
			own := shared + 1 + i
			r, fs := e.identified(fmt.Sprintf("r%d", i), study, "store", subject, session, func(r tenant.Reception) []tenant.ReceivedFile {
				return files(r, shared, own)
			})
			claims := make([]tenant.InstanceClaim, len(fs))
			for j, f := range fs {
				claims[j] = claimOfFile(f)
			}
			_, err := e.p().ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
			e.ok("ClaimInstances", err)
			commits = append(commits, scanCommit(r, subject, session, "scan-shared", "store", fs[:20]),
				scanCommit(r, subject, session, fmt.Sprintf("scan-%d", i), "store", fs[20:]))
		}
		for _, err := range commitAll(round, commits) {
			e.ok("CommitScan of one study at once", err)
		}
		after, err := e.p().CatalogTotals(e.ctx)
		e.ok("CatalogTotals", err)
		if d := (tenant.CatalogCounts{Subjects: after.Subjects - before.Subjects, Sessions: after.Sessions - before.Sessions,
			Scans: after.Scans - before.Scans, Instances: after.Instances - before.Instances}); d != (tenant.CatalogCounts{Subjects: 1, Sessions: 1, Scans: 1 + k, Instances: 40 * k}) {
			t.Fatalf("round %d: one study at once made %+v, want 1 subject, 1 session, %d scans, %d instances", round, d, 1+k, 40*k)
		}

		// Six new studies of the subject.
		commits = commits[:0]
		for i := range k {
			st := cUID(1, 1000+round*10+i)
			r, fs := e.identified(fmt.Sprintf("r%d", i), st, "store", subject, "ses-"+st, func(r tenant.Reception) []tenant.ReceivedFile {
				return files(r, 20000+round*10+i)
			})
			claims := make([]tenant.InstanceClaim, len(fs))
			for j, f := range fs {
				claims[j] = claimOfFile(f)
			}
			_, err := e.p().ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
			e.ok("ClaimInstances", err)
			commits = append(commits, scanCommit(r, subject, "ses-"+st, "scan-1", "store", fs))
		}
		for _, err := range commitAll(round, commits) {
			e.ok("CommitScan of six studies of one subject at once", err)
		}
		final, err := e.p().CatalogTotals(e.ctx)
		e.ok("CatalogTotals", err)
		if final.Subjects != after.Subjects || final.Sessions != after.Sessions+k {
			t.Fatalf("round %d: six studies of one subject made %d subjects and %d sessions, want 0 and %d",
				round, final.Subjects-after.Subjects, final.Sessions-after.Sessions, k)
		}

		// Two studies, one session label.
		commits = commits[:0]
		for i := range 2 {
			st := cUID(1, 5000+round*10+i)
			r, fs := e.identified(fmt.Sprintf("r%d", i), st, "store", subject, "ses-twice", func(r tenant.Reception) []tenant.ReceivedFile {
				return files(r, 30000+round*10+i)
			})
			claims := make([]tenant.InstanceClaim, len(fs))
			for j, f := range fs {
				claims[j] = claimOfFile(f)
			}
			_, err := e.p().ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
			e.ok("ClaimInstances", err)
			commits = append(commits, scanCommit(r, subject, "ses-twice", "scan-1", "store", fs))
		}
		errs := commitAll(round, commits)
		made, refused := 0, 0
		for _, err := range errs {
			var lc *tenant.LabelConflict
			switch {
			case err == nil:
				made++
			case errors.As(err, &lc) && lc.Label == "session" && lc.Fault == tenant.FaultHeldByAnotherSession && fault.KindOf(err) == fault.Conflict:
				refused++
			default:
				t.Fatalf("round %d: a commit of a session label taken at once: %v", round, err)
			}
		}
		if made != 1 || refused != 1 {
			t.Fatalf("round %d: two sessions with one label at once: %d made, %d refused; want 1 and 1", round, made, refused)
		}
		e.checkTotals(fmt.Sprintf("round %d", round))
	}
	if n := e.uniqueRetries(); n != 0 {
		t.Errorf("%d commits were run again after a unique violation: a catalog row was not found or made as its insert met one", n)
	}
	if n := e.deadlocks(); n != 0 {
		t.Errorf("%d commits were run again after a deadlock", n)
	}
}

// TestBatchRacesQuietStep guards the order of a received batch and the quiet
// step of its reception, a hundred rounds: a batch with a later file and a
// quiet step decided from the last file before it, at once, never both
// succeed. Either the step comes first, and the batch is refused as the
// reception closed (its files belong to the next arrival), or the batch
// comes first, and the step is refused as the state changed.
func TestBatchRacesQuietStep(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	e.receiver("a", "store", 11112)
	for round := range 100 {
		r, _, err := e.p().OpenReception(e.ctx, "a", cUID(1, round), e.id("rc"), cT0)
		e.ok("OpenReception", err)
		var batch, step error
		atOnce(2, func(i int) {
			if i == 0 {
				_, batch = e.stores[0].AddReceivedFiles(e.ctx, []tenant.ReceivedFile{cFile(r, round, round)}, cT0.Add(time.Second), nil)
			} else {
				_, step = e.stores[1].StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Receiving, Event: reception.Quiet,
					At: cT0.Add(time.Minute), LastFile: r.LastFile}, nil)
			}
		})
		switch {
		case batch == nil && errors.Is(step, tenant.ErrStateChanged):
		case step == nil && errors.Is(batch, tenant.ErrReceptionClosed):
		default:
			t.Fatalf("round %d: batch %v, step %v; want exactly one, the other refused by the port's rule", round, batch, step)
		}
	}
}

// TestClaimRacesEnd guards the order of a claim and the step that ends its
// reception, a hundred rounds: a claim of fifty instances and a Fail step
// at once leave no claim held by the ended reception, whichever comes
// first: the claim waits for the step and is refused, or the step waits
// for the claim and releases what it claimed.
func TestClaimRacesEnd(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	e.receiver("a", "store", 11112)
	for round := range 100 {
		study := cUID(1, round)
		r, fs := e.identified("a", study, "store", "sub-01", "ses-"+study, func(r tenant.Reception) []tenant.ReceivedFile {
			out := make([]tenant.ReceivedFile, 50)
			for i := range out {
				out[i] = cFile(r, round, round*100+i)
			}
			return out
		})
		claims := make([]tenant.InstanceClaim, len(fs))
		for i, f := range fs {
			claims[i] = claimOfFile(f)
		}
		var claim, end error
		atOnce(2, func(i int) {
			if i == 0 {
				_, claim = e.stores[0].ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
			} else {
				_, end = e.stores[1].StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Identified, Event: reception.Fail,
					At: cT0, Failure: "storage unreachable"}, nil)
			}
		})
		e.ok("the Fail step", end)
		if claim != nil && fault.KindOf(claim) != fault.Precondition {
			t.Fatalf("round %d: the claim: %v, want success or a precondition failure", round, claim)
		}
		if n := e.scalar(`SELECT count(*) FROM instance_claims WHERE reception_id = $1`, r.ID); n != 0 {
			t.Fatalf("round %d: %d claims held by a reception that ended", round, n)
		}
	}
}

// TestCommitRacesEnd guards the order of a scan's commit and the step that
// ends its reception, a hundred rounds: a commit of a hundred claimed
// instances and a Fail step at once leave either the instances held (the
// commit came first) or released (the step came first, and the commit is
// refused), no claim of the ended reception, the counts equal to the rows,
// and no deadlock at all: a commit takes the reception's row first, as the
// step does, so neither holds a row the other waits for.
func TestCommitRacesEnd(t *testing.T) {
	e := newCenv(t, 2, 6)
	e.storage("store")
	e.receiver("a", "store", 11112)
	for round := range 100 {
		study := cUID(1, round)
		r, fs := e.identified("a", study, "store", "sub-01", "ses-"+study, func(r tenant.Reception) []tenant.ReceivedFile {
			out := make([]tenant.ReceivedFile, 100)
			for i := range out {
				out[i] = cFile(r, round, round*1000+i)
			}
			return out
		})
		claims := make([]tenant.InstanceClaim, len(fs))
		for i, f := range fs {
			claims[i] = claimOfFile(f)
		}
		_, err := e.p().ClaimInstances(e.ctx, r.ID, claims, cT0, nil)
		e.ok("ClaimInstances", err)
		var commit, end error
		atOnce(2, func(i int) {
			if i == 0 {
				_, commit = e.stores[0].CommitScan(e.ctx, scanCommit(r, "sub-01", "ses-"+study, "scan-1", "store", fs))
			} else {
				_, end = e.stores[1].StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Identified, Event: reception.Fail,
					At: cT0, Failure: "storage unreachable"}, nil)
			}
		})
		e.ok("the Fail step", end)
		if commit != nil && fault.KindOf(commit) != fault.Precondition {
			t.Fatalf("round %d: the commit: %v, want success or a precondition failure", round, commit)
		}
		held := e.scalar(`SELECT count(*) FROM instances WHERE reception_id = $1`, r.ID)
		if (commit == nil && held != 100) || (commit != nil && held != 0) {
			t.Fatalf("round %d: commit %v, and %d instances held", round, commit, held)
		}
	}
	if n := e.deadlocks(); n != 0 {
		t.Errorf("%d changes were run again after a deadlock", n)
	}
	e.checkTotals("after the commits")
}

// TestOpenSameIDAtOnce guards OpenReception when two calls for one receiver
// and study carry the same id, as a sender's association run again does:
// the second is answered with the reception the first opened (opened false),
// never a Conflict, as one writer at a time would answer it. The first call
// is a transaction that has written its row and stays open until the second
// is waiting on that row, so the two meet at the id every run.
func TestOpenSameIDAtOnce(t *testing.T) {
	e := newCenv(t, 1, 4)
	e.storage("store")
	e.receiver("a", "store", 11112)
	study := cUID(1, 0)

	first, err := e.p().pool.Begin(e.ctx)
	e.ok("begin", err)
	defer func() { _ = first.Rollback(e.ctx) }() // After the commit below it does nothing.
	inserted, err := insertReception(e.ctx, first, "rc-same", "a", study, reception.Receiving, "", cT0)
	e.ok("the first call's insert", err)
	if !inserted {
		t.Fatal("the first call's insert wrote nothing")
	}

	type result struct {
		r      tenant.Reception
		opened bool
		err    error
	}
	done := make(chan result, 1)
	go func() {
		r, opened, err := e.p().OpenReception(e.ctx, "a", study, "rc-same", cT0)
		done <- result{r, opened, err}
	}()
	e.waitBlocked(1)
	e.ok("commit the first call", first.Commit(e.ctx))
	got := <-done
	if got.err != nil {
		t.Fatalf("OpenReception at once with the same id: %v (kind %v); want the open reception rc-same, opened false",
			got.err, fault.KindOf(got.err))
	}
	if got.r.ID != "rc-same" || got.opened || got.r.State != reception.Receiving {
		t.Fatalf("OpenReception = %s in %s, opened %v; want rc-same receiving, opened false", got.r.ID, got.r.State, got.opened)
	}
	if n := e.scalar(`SELECT count(*) FROM receptions`); n != 1 {
		t.Fatalf("%d receptions; want 1", n)
	}
}
