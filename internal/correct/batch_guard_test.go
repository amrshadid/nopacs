package correct

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// twinUIDTail is the end every UID of MR_small.dcm shares (study, series
// and instance); twins replaces it with one of the same length, so each
// twin is a study of its own of the same patient, byte for byte as long.
const twinUIDTail = ".20040826185059.5457"

// archiveTwins archives n studies of one subject made from MR_small.dcm,
// each with its own Study, Series and SOP Instance UID and the same size,
// and answers their sessions with their scans' series, in the order made.
func (r *rig) archiveTwins(n int) []tenant.CatalogScan {
	r.t.Helper()
	src, err := os.ReadFile(filepath.Join(corpusDir, "MR_small.dcm")) // #nosec G304 -- a test's own file name.
	if err != nil {
		r.t.Fatal(err)
	}
	if bytes.Count(src, []byte(twinUIDTail)) < 3 {
		r.t.Fatalf("MR_small.dcm no longer ends its UIDs with %s", twinUIDTail)
	}
	p := r.pipeline()
	for i := range n {
		b := bytes.ReplaceAll(src, []byte(twinUIDTail), fmt.Appendf(nil, ".20040826185059.%d", 6000+i))
		out := p.Accept(r.ctx, receive.Arrival{Receiver: "dimse", Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}})
		if out.Status != receive.Received {
			r.t.Fatalf("twin %d: outcome %v (%s)", i, out.Status, out.Reason)
		}
	}
	r.waitEnded()
	if err := p.Stop(context.Background()); err != nil {
		r.t.Fatal(err)
	}
	scans := r.scans()
	if len(scans) != n {
		r.t.Fatalf("the twins made %d scans, want %d", len(scans), n)
	}
	for _, s := range scans[1:] {
		if s.SubjectLabel != scans[0].SubjectLabel {
			r.t.Fatalf("the twins are under subjects %s and %s, want one", scans[0].SubjectLabel, s.SubjectLabel)
		}
	}
	return scans
}

// relabelNow relabels target to label and purges the relabel at once.
func (r *rig) relabelNow(e *Engine, target tenant.CorrectionTarget, label string) {
	r.t.Helper()
	c, err := e.Relabel(r.ctx, RelabelRequest{Target: target, ToLabel: label, Reason: "sessions swapped at the scanner", Audit: r.audit()})
	if err != nil {
		r.t.Fatal(err)
	}
	if c, err = e.ContinueRelabel(r.ctx, c.ID); err != nil {
		r.t.Fatal(err)
	}
	if _, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
}

// TestBatchSkipsASessionSwappedIn guards that a batch's target is the data
// the person previewed, not its labels: two sessions of one subject, of
// the same size, swapped by three relabels after the job was made, so the
// target's label now names the other session, which has the same counts.
// The batch skips the target as changed and deletes neither session; a
// single delete's token of before the swap is refused the same way.
func TestBatchSkipsASessionSwappedIn(t *testing.T) {
	r := newRig(t, false)
	scans := r.archiveTwins(2)
	a := tenant.CorrectionTarget{Subject: scans[0].SubjectLabel, Session: scans[0].SessionLabel}
	other := tenant.CorrectionTarget{Subject: scans[1].SubjectLabel, Session: scans[1].SessionLabel}
	e := r.engine()
	before, err := r.p.PreviewDelete(r.ctx, a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	b := &batchRig{rig: r}
	id := b.queue(a)

	r.relabelNow(e, a, "swap-tmp")
	r.relabelNow(e, other, a.Session)
	r.relabelNow(e, tenant.CorrectionTarget{Subject: a.Subject, Session: "swap-tmp"}, other.Session)
	after, err := r.p.PreviewDelete(r.ctx, a, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if after.Counts != before.Counts {
		t.Fatalf("the swapped session counts %+v, the previewed one %+v: the case needs equal counts", after.Counts, before.Counts)
	}

	_, err = e.Delete(r.ctx, DeleteRequest{Target: a, Reason: "consent withdrawn", Confirm: before.Confirm, Audit: r.audit()})
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a delete's token of before the swap answers %v, want precondition", err)
	}
	if _, err := b.run(e, id, nil); err != nil {
		t.Fatal(err)
	}
	if got := b.outcomes(id); got[0] != tenant.BatchSkippedChanged {
		t.Fatalf("the swapped target is %q, want skipped as changed", got[0])
	}
	held := map[string]bool{}
	for _, s := range r.scans() {
		held[s.SeriesUID] = true
	}
	if !held[scans[0].SeriesUID] || !held[scans[1].SeriesUID] || len(b.deletes()) != 0 {
		t.Fatalf("after the run the catalog holds %v and the deletes are %+v; want both series and no delete", held, b.deletes())
	}
}

// TestBatchTwoRunnersRecordTheDelete guards a job worked by two runners at
// once (a pass still finishing on one engine as another starts): the
// second reads a target before the first prepares its delete, so the
// second's own prepare is refused. That refusal leaves the target for a
// later pass, never a skip; the first records it deleted.
func TestBatchTwoRunnersRecordTheDelete(t *testing.T) {
	b := newBatchRig(t, false)
	id := b.queue(b.mr)
	first, second := b.engine(), b.engine()
	prepared, release := make(chan struct{}), make(chan struct{})
	first.SetCrashAt(func(step string) error {
		if step == "prepared" {
			close(prepared)
			<-release
		}
		return nil
	})
	audit := func(tenant.CorrectionTarget, time.Time) master.AuditEntry { return b.audit() }
	var wg sync.WaitGroup
	var firstRes BatchResult
	var firstErr error
	var started sync.Once
	secondRes, secondErr := second.RunBatch(b.ctx, BatchRun{Store: b.p, JobID: id,
		Audit: func(tenant.CorrectionTarget, time.Time) master.AuditEntry {
			// The second runner has read and previewed the target; the first
			// prepares its delete before the second's prepare runs.
			started.Do(func() {
				wg.Add(1)
				go func() {
					defer wg.Done()
					firstRes, firstErr = first.RunBatch(b.ctx, BatchRun{Store: b.p, JobID: id, Audit: audit})
				}()
				<-prepared
			})
			return b.audit()
		}})
	close(release)
	wg.Wait()
	if secondErr != nil || secondRes.Waiting != 1 || len(secondRes.Met) != 0 {
		t.Fatalf("the second runner answers %+v, %v; want the target left waiting", secondRes, secondErr)
	}
	if firstErr != nil || firstRes.Met[tenant.BatchDeleted] != 1 || !firstRes.Ended {
		t.Fatalf("the first runner answers %+v, %v", firstRes, firstErr)
	}
	if got := b.outcomes(id); got[0] != tenant.BatchDeleted {
		t.Fatalf("the target is %q, want deleted", got[0])
	}
	if j, _ := b.p.Job(b.ctx, id); j.State != tenant.JobDone {
		t.Fatalf("the job ended %s, want done", j.State)
	}
}

// TestBatchRecordRefusesASkipOverADelete guards the store's side of the
// same: a skip recorded on a target whose delete was made and not abandoned
// is refused as taken, and the delete is then recorded.
func TestBatchRecordRefusesASkipOverADelete(t *testing.T) {
	b := newBatchRig(t, false)
	id := b.queue(b.mr)
	if res, err := b.run(b.engine(), id, nil); err != nil || res.Met[tenant.BatchDeleted] != 1 {
		t.Fatalf("the run answers %+v, %v", res, err)
	}
	id2 := b.queue(b.ct)
	e := b.engineWith(func(c *Config) {
		c.Published = func(ctx context.Context, pc tenant.Correction) {
			_, err := b.p.RecordBatchTarget(ctx, tenant.BatchRecord{JobID: id2, Ord: 1, Outcome: tenant.BatchSkippedRefused, Now: time.Now()})
			if !errors.Is(err, tenant.ErrBatchTargetTaken) {
				t.Errorf("a skip over a published delete answers %v, want the target taken", err)
			}
		}
	})
	if res, err := b.run(e, id2, nil); err != nil || res.Met[tenant.BatchDeleted] != 1 {
		t.Fatalf("the run answers %+v, %v", res, err)
	}
	if got := b.outcomes(id2); got[0] != tenant.BatchDeleted {
		t.Fatalf("the target is %q, want deleted", got[0])
	}
}

// failingStore is a storage whose writes fail while fail is set.
type failingStore struct {
	blob.Store
	fail *atomic.Bool
}

// Put fails while fail is set.
func (s failingStore) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	if s.fail.Load() {
		return blob.Written{}, fault.New(fault.Unavailable, "the storage does not answer")
	}
	return s.Store.Put(ctx, key, r)
}

// TestBatchBacksOffAFailingStorage guards a batch's pace when its deletes
// keep failing: each failure makes one abandoned delete, then the runner
// waits 5s, then twice as long after each next failure, trying nothing
// before the wait ends; once a delete succeeds the wait is forgotten.
func TestBatchBacksOffAFailingStorage(t *testing.T) {
	b := newBatchRig(t, false)
	id := b.queue(b.mr, b.liver)
	var fail atomic.Bool
	fail.Store(true)
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	advance := func(d time.Duration) { clock.Add(int64(d)) }
	e := b.engineWith(func(c *Config) {
		c.Now = now
		c.OpenStorage = func(context.Context, string) (blob.Store, error) {
			return failingStore{Store: b.storage, fail: &fail}, nil
		}
	})
	abandoned := func() int { return len(b.deletes()[record.StateAbandoned]) }
	try := func(what string, wantTried bool, wantWait time.Duration) {
		t.Helper()
		before := abandoned()
		res, err := b.run(e, id, nil)
		switch {
		case wantTried && (err == nil || abandoned() != before+1 || !res.Until.Equal(now().Add(wantWait))):
			t.Fatalf("%s: %+v, %v, %d abandoned deletes; want one more and a wait of %v", what, res, err, abandoned(), wantWait)
		case !wantTried && (err != nil || !res.BackingOff || abandoned() != before):
			t.Fatalf("%s: %+v, %v, %d abandoned deletes; want nothing tried", what, res, err, abandoned())
		}
	}
	try("the first failure", true, 5*time.Second)
	try("at once after it", false, 0)
	advance(5 * time.Second)
	try("the second failure", true, 10*time.Second)
	advance(9 * time.Second)
	try("before the doubled wait ends", false, 0)
	advance(time.Second)
	try("the third failure", true, 20*time.Second)
	for range 8 {
		advance(batchMostWait)
		before := abandoned()
		if _, err := b.run(e, id, nil); err == nil || abandoned() != before+1 {
			t.Fatalf("a failure past the doubling: %v, %d abandoned", err, abandoned())
		}
	}
	if w := e.batchWaits[id]; w.wait != batchMostWait {
		t.Fatalf("the wait grew to %v, want at most %v", w.wait, batchMostWait)
	}
	fail.Store(false)
	advance(batchMostWait)
	if res, err := b.run(e, id, nil); err != nil || !res.Ended || res.Met[tenant.BatchDeleted] != 2 {
		t.Fatalf("once the storage answers the run is %+v, %v", res, err)
	}
	if _, ok := e.batchWaits[id]; ok {
		t.Fatal("a met target leaves the job's wait")
	}
}
