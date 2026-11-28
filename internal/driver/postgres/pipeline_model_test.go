package postgres

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// mworker is one worker of the model: it takes receptions through the
// pipeline as the server does, on its own random sequence, through one of
// the stores, while the others do the same on the same studies and UIDs.
type mworker struct {
	e     *cenv
	rng   *rand.Rand
	store *Tenant
	errs  []string
	// others are receptions other workers made, which this one may fail.
	mu     *sync.Mutex
	others *[]string
}

// clock is when the model's calls are made: the order of times does not
// matter to any rule tested here.
var clock = cT0.Add(time.Hour)

// answer records err unless it is nil or one of the port's own answers to a
// change that met another, and reports whether the call succeeded.
func (w *mworker) answer(what string, err error) bool {
	if err == nil {
		return true
	}
	if !portAnswer(err) {
		w.errs = append(w.errs, fmt.Sprintf("%s: %v (kind %v)", what, err, fault.KindOf(err)))
	}
	return false
}

// receive opens a reception of one of two studies, at one of two
// receivers, or finds one another worker opened, and records a few files drawn from a small set of UIDs shared
// by every worker, sometimes twice, and sometimes holds one of them.
func (w *mworker) receive() (tenant.Reception, bool) {
	study := w.rng.IntN(2)
	r, opened, err := w.store.OpenReception(w.e.ctx, []string{"a", "b"}[w.rng.IntN(2)], cUID(1, study), w.e.id("rc"), clock)
	if !w.answer("open", err) {
		return r, false
	}
	var files []tenant.ReceivedFile
	seen := map[string]bool{}
	for range 1 + w.rng.IntN(6) {
		n := w.rng.IntN(24)
		f := cFile(r, study*10+n%2, n)
		f.SHA256 = cSum(n*10 + w.rng.IntN(2))
		f.Size = int64(1000 + n*10 + int(f.SHA256[0]))
		if seen[f.SOPInstanceUID+f.SHA256.String()] {
			continue
		}
		seen[f.SOPInstanceUID+f.SHA256.String()] = true
		files = append(files, f)
	}
	if _, err := w.store.AddReceivedFiles(w.e.ctx, files, clock, nil); !w.answer("add", err) {
		return r, false
	}
	if w.rng.IntN(3) == 0 {
		_, err := w.e.stores[w.rng.IntN(len(w.e.stores))].AddReceivedFiles(w.e.ctx, files, clock, nil)
		w.answer("add again", err)
	}
	if !opened {
		// Another worker's reception, receiving: its files are recorded,
		// and it is left to the worker that runs it, as a reception has one
		// supervisor.
		return r, false
	}
	if w.rng.IntN(4) == 0 {
		f := files[0]
		q := tenant.QuarantinedFile{ID: w.e.id("q"), Reason: tenant.QuarantineBadLabel,
			Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: r.Receiver, ReceptionID: r.ID,
			SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
			TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: clock}
		_, err := w.store.HoldFiles(w.e.ctx, []tenant.QuarantinedFile{q}, nil)
		w.answer("hold a received file", err)
	}
	return r, true
}

// step takes one step of r from the state it reads.
func (w *mworker) step(id string, ev reception.Event, counts *tenant.StepCounts) (tenant.Reception, bool) {
	r, _, err := w.store.Reception(w.e.ctx, id)
	if !w.answer("read", err) {
		return r, false
	}
	st := tenant.ReceptionStep{ID: id, From: r.State, Event: ev, At: clock, Counts: counts}
	switch ev {
	case reception.Quiet:
		st.LastFile = r.LastFile
	case reception.Identify:
		st.SubjectLabel, st.SessionLabel, st.Storage = "sub-01", "ses-"+r.StudyUID, "store"
	case reception.Fail:
		st.Failure = "storage unreachable"
	}
	r, err = w.store.StepReception(w.e.ctx, st, nil)
	return r, w.answer("step "+string(ev), err)
}

// run takes one reception from its first file to its end, or as far as the
// others let it: a step another worker's change came before is a Conflict
// and ends the run. Every received file is claimed, again when it was
// Claimed by another, and then committed, ignored as a duplicate, or held,
// so a reception that ends done holds the count identity.
func (w *mworker) run() {
	r, ok := w.receive()
	if !ok {
		return
	}
	w.mu.Lock()
	*w.others = append(*w.others, r.ID)
	w.mu.Unlock()
	if _, ok = w.step(r.ID, reception.Quiet, nil); !ok {
		return
	}
	if r, ok = w.step(r.ID, reception.Identify, nil); !ok {
		return
	}
	var files []tenant.ReceivedFile
	page := master.Page{Limit: 1000}
	got, _, err := w.store.ReceivedFiles(w.e.ctx, r.ID, page)
	if !w.answer("list", err) {
		return
	}
	// Every record of the reception, whatever its state: a file the
	// reception held is counted quarantined from then on, and another
	// worker may release or reject the record between the hold and this
	// list. Leaving out only records still held would claim such a file
	// again, and a second record of it would count it quarantined twice.
	kept, _, err := w.store.ListQuarantined(w.e.ctx, tenant.QuarantineFilter{ReceptionID: r.ID}, master.Page{Limit: 1000})
	if !w.answer("list quarantined", err) {
		return
	}
	for _, f := range got {
		if !slices.ContainsFunc(kept, func(q tenant.QuarantinedFile) bool {
			return q.SOPInstanceUID == f.SOPInstanceUID && q.SHA256 == f.SHA256
		}) {
			files = append(files, f)
		}
	}
	var dup tenant.StepCounts
	for try := 0; len(files) > 0; try++ {
		if try == 4 {
			w.step(r.ID, reception.Fail, nil)
			return
		}
		claims := make([]tenant.InstanceClaim, len(files))
		for i, f := range files {
			claims[i] = claimOfFile(f)
		}
		answers, err := w.store.ClaimInstances(w.e.ctx, r.ID, claims, clock, nil)
		if !w.answer("claim", err) {
			return
		}
		bySeries := map[string][]tenant.ReceivedFile{}
		var later []tenant.ReceivedFile
		var holds []tenant.QuarantinedFile
		for i, a := range answers {
			f := files[i]
			switch a.Kind {
			case tenant.ClaimedNow, tenant.ClaimedBefore:
				bySeries[f.SeriesUID] = append(bySeries[f.SeriesUID], f)
			case tenant.SameFile:
				dup.FilesDuplicate, dup.BytesDuplicate = dup.FilesDuplicate+1, dup.BytesDuplicate+f.Size
			case tenant.Claimed:
				later = append(later, f)
			case tenant.DifferentFile, tenant.UIDElsewhere:
				q := tenant.QuarantinedFile{ID: w.e.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: r.Receiver, ReceptionID: r.ID,
					SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
					TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, KeptSHA256: a.HeldSHA256, Size: f.Size,
					Incoming: f.Incoming, ContentKept: true, Quarantined: clock}
				if a.Kind == tenant.UIDElsewhere {
					q.Reason, q.KeptSHA256 = tenant.QuarantineUIDElsewhere, tenant.SHA256{}
				}
				holds = append(holds, q)
			case tenant.ArchivedBefore:
			default:
				w.errs = append(w.errs, fmt.Sprintf("claim answered %v", a.Kind))
			}
		}
		for series, fs := range bySeries {
			c := scanCommit(r, "sub-01", "ses-"+r.StudyUID, "scan-"+series, "store", fs)
			if _, err := w.store.CommitScan(w.e.ctx, c); !w.answer("commit", err) {
				return
			}
			if w.rng.IntN(4) == 0 {
				_, err := w.e.stores[w.rng.IntN(len(w.e.stores))].CommitScan(w.e.ctx, c)
				w.answer("commit again", err)
			}
		}
		if len(holds) > 0 {
			if _, err := w.store.HoldFiles(w.e.ctx, holds, nil); !w.answer("hold", err) {
				return
			}
		}
		files = later
		if len(later) > 0 {
			time.Sleep(time.Millisecond)
		}
	}
	for _, ev := range []reception.Event{reception.Archive, reception.Verify, reception.Finish} {
		counts := &dup
		if ev != reception.Archive {
			counts = nil
		}
		if _, ok := w.step(r.ID, ev, counts); !ok {
			return
		}
	}
}

// interfere fails a reception another worker is running, or releases or
// rejects a held record, so every worker meets changes made under it.
func (w *mworker) interfere() {
	switch w.rng.IntN(3) {
	case 0:
		w.mu.Lock()
		var id string
		if len(*w.others) > 0 {
			id = (*w.others)[w.rng.IntN(len(*w.others))]
		}
		w.mu.Unlock()
		if id != "" {
			w.step(id, reception.Fail, nil)
		}
	default:
		held, _, err := w.store.ListQuarantined(w.e.ctx, tenant.QuarantineFilter{State: tenant.QuarantineHeld}, master.Page{Limit: 20})
		if !w.answer("list held", err) || len(held) == 0 {
			return
		}
		q := held[w.rng.IntN(len(held))]
		if w.rng.IntN(2) == 0 {
			_, err = w.store.RejectQuarantined(w.e.ctx, q.ID, "not wanted", clock, w.e.audit(), nil)
		} else {
			_, err = w.store.ReleaseQuarantined(w.e.ctx, q.ID, tenant.ReleaseAsIs, "", w.e.id("rc"), clock, w.e.audit(), nil)
		}
		w.answer("decide", err)
	}
}

// TestPipelineModelConcurrent guards the pipeline's invariants under real
// concurrency: in each of twelve rounds six workers, through two stores,
// run receptions of two studies whose files share UIDs across studies and
// series, at once, each claiming, committing (and committing again),
// holding, finishing, and failing one another's receptions and deciding
// held records at random. No call fails but with the port's own answers;
// after each round the catalog's and the quarantine's totals equal the
// rows, each scan's and each reception's archived counts equal the
// instances, no claim is left by a reception that ended, every reception
// done holds received = archived + duplicate + quarantined, and each
// study's arrivals are numbered 1 to n, each once.
func TestPipelineModelConcurrent(t *testing.T) {
	e := newCenv(t, 2, 8)
	e.storage("store")
	e.receiver("a", "store", 11112)
	e.receiver("b", "store", 11113)
	rounds, workers := 12, 6
	if raceDetector {
		rounds = 6
	}
	var mu sync.Mutex
	var others []string
	ws := make([]*mworker, workers)
	for k := range ws {
		ws[k] = &mworker{e: e, rng: rand.New(rand.NewPCG(uint64(k)+1, 49)), store: e.stores[k%2], mu: &mu, others: &others} //nolint:gosec // G404: a seeded sequence for a test.
	}
	done := 0
	for round := range rounds {
		atOnce(workers, func(k int) {
			w := ws[k]
			for range 4 {
				if w.rng.IntN(5) == 0 {
					w.interfere()
				} else {
					w.run()
				}
			}
		})
		for k, w := range ws {
			for _, msg := range w.errs {
				t.Errorf("round %d, worker %d: %s", round, k, msg)
			}
			w.errs = nil
		}
		if t.Failed() {
			t.FailNow()
		}
		e.checkTotals(fmt.Sprintf("round %d", round))
		done = int(e.scalar(`SELECT count(*) FROM receptions WHERE state = 'done'`))
	}
	if done < rounds {
		t.Errorf("only %d receptions ended done, so the model tested little", done)
	}
	t.Logf("%d receptions, %d done, %d failed; %d deadlocks run again", e.scalar(`SELECT count(*) FROM receptions`), done,
		e.scalar(`SELECT count(*) FROM receptions WHERE state = 'failed'`), e.deadlocks())
}
