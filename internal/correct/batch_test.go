package correct

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The files a batch's tests archive: the CT study without its last file,
// which arrives later; and three studies of their own, the last through
// the second receiver.
var (
	batchCT     = studyFiles[:6]
	batchLateCT = studyFiles[6]
)

// batchRig is a rig holding four sessions: the CT study, MR_small and
// liver_1frame through "dimse", and SC_rgb_rle_2frame through "dimse2".
type batchRig struct {
	*rig
	ct, mr, liver, sc tenant.CorrectionTarget
}

// newBatchRig archives the four sessions.
func newBatchRig(t *testing.T, pg bool) *batchRig {
	t.Helper()
	r := newRig(t, pg)
	if _, err := r.p.AddReceiver(r.ctx, tenant.Receiver{Name: "dimse2", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "ARCHIVE2", AnyCaller: true}}, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	p := r.pipeline()
	r.accept(p, append(slices.Clone(batchCT), "MR_small.dcm", "liver_1frame.dcm")...)
	r.acceptVia(p, "dimse2", "SC_rgb_rle_2frame.dcm")
	r.waitEnded()
	if err := p.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	b := &batchRig{rig: r}
	for _, s := range r.scans() {
		target := tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel}
		switch s.Values.Modality {
		case "CT":
			b.ct = target
		case "MR":
			b.mr = target
		case "OT":
			b.sc = target
		default:
			b.liver = target
		}
	}
	for _, x := range []tenant.CorrectionTarget{b.ct, b.mr, b.liver, b.sc} {
		if x.Subject == "" {
			t.Fatalf("the rig's sessions are %+v", r.scans())
		}
	}
	return b
}

// acceptVia hands the corpus files named to a running pipeline through
// the receiver named.
func (r *rig) acceptVia(p *receive.Pipeline, receiver string, names ...string) {
	r.t.Helper()
	for _, n := range names {
		b, err := os.ReadFile(filepath.Join(corpusDir, n)) // #nosec G304 -- a test's own file names.
		if err != nil {
			r.t.Fatal(err)
		}
		out := p.Accept(r.ctx, receive.Arrival{Receiver: receiver, Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}})
		if out.Status != receive.Received {
			r.t.Fatalf("%s: outcome %v (%s)", n, out.Status, out.Reason)
		}
	}
}

// queue previews targets and makes their batch job, answering its id.
func (b *batchRig) queue(targets ...tenant.CorrectionTarget) string {
	b.t.Helper()
	now := time.Now()
	p, err := b.p.PreviewDeleteBatch(b.ctx, targets, "consent withdrawn", now)
	if err != nil {
		b.t.Fatal(err)
	}
	id := "batch-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	j, made, err := b.p.QueueDeleteBatch(b.ctx, tenant.DeleteBatchQueue{ID: id, Targets: targets, Reason: "consent withdrawn", Confirm: p.Confirm,
		KeyID: "K1234567890a", Max: tenant.MaxDeleteBatchTargets, Now: now}, b.audit(), nil)
	if err != nil || !made {
		b.t.Fatalf("QueueDeleteBatch: made %v, %v", made, err)
	}
	return j.ID
}

// run runs one pass of the job on e.
func (b *batchRig) run(e *Engine, id string, met func(tenant.DeleteBatchTarget, tenant.BatchOutcome)) (BatchResult, error) {
	return e.RunBatch(b.ctx, BatchRun{Store: b.p, JobID: id, Met: met,
		Audit: func(tenant.CorrectionTarget, time.Time) master.AuditEntry { return b.audit() }})
}

// outcomes returns each target's outcome, in order.
func (b *batchRig) outcomes(id string) []tenant.BatchOutcome {
	b.t.Helper()
	ts, err := b.p.DeleteBatchTargets(b.ctx, id, 0, tenant.MaxBatchTargetPage)
	if err != nil {
		b.t.Fatal(err)
	}
	out := make([]tenant.BatchOutcome, len(ts))
	for i, t := range ts {
		out[i] = t.Outcome
	}
	return out
}

// deletes returns the tenant's deletes, by state.
func (b *batchRig) deletes() map[record.State][]tenant.Correction {
	b.t.Helper()
	cs, _, err := b.p.ListCorrections(b.ctx, tenant.CorrectionFilter{Kinds: []record.Kind{record.KindDelete}}, master.Page{Limit: 1000})
	if err != nil {
		b.t.Fatal(err)
	}
	out := map[record.State][]tenant.Correction{}
	for _, c := range cs {
		out[c.State] = append(out[c.State], c)
	}
	return out
}

// sessionHeld reports whether the catalog shows the session.
func (b *batchRig) sessionHeld(t tenant.CorrectionTarget) bool {
	b.t.Helper()
	_, err := b.p.CatalogSession(b.ctx, t.Subject, t.Session)
	if err != nil && fault.KindOf(err) != fault.NotFound {
		b.t.Fatal(err)
	}
	return err == nil
}

// TestBatchSkipsAndDeletes guards a batch's run: a target a hold covers
// and one a rule keeps are skipped with those reasons, one that changed
// since the job was made (an instance arrived) is skipped as changed, and
// the others are deleted by ordinary deletes carrying the batch's reason
// and the key that made it; the job ends partial, every target with its
// outcome.
func TestBatchSkipsAndDeletes(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		b := newBatchRig(t, pg)
		hs, err := b.p.HoldStorages(b.ctx, tenant.HoldTarget{Subject: b.liver.Subject, Session: b.liver.Session})
		if err != nil {
			t.Fatal(err)
		}
		hid, _ := tenant.NewCorrectionID(time.Now())
		if _, _, err := b.p.PlaceHold(b.ctx, tenant.HoldPlace{ID: hid, Target: tenant.HoldTarget{Subject: b.liver.Subject, Session: b.liver.Session},
			Reason: "litigation", Written: []string{hs[0].Name}, Now: time.Now()}, b.audit(), nil); err != nil {
			t.Fatal(err)
		}
		if _, err := b.p.AddRetentionRule(b.ctx, tenant.RetentionRule{Name: "keep", Selector: tenant.RetentionSelector{Receiver: "dimse2"},
			KeepAtLeast: tenant.RetentionPeriod{Years: 10}, Action: tenant.RetentionReview},
			tenant.RetentionChange{Now: time.Now(), Notice: tenant.DefaultShortenNotice}, b.audit(), nil); err != nil {
			t.Fatal(err)
		}
		id := b.queue(b.ct, b.liver, b.mr, b.sc)
		b.archive(batchLateCT)
		res, err := b.run(b.engine(), id, nil)
		if err != nil || !res.Ended {
			t.Fatalf("the run answers %+v, %v", res, err)
		}
		want := []tenant.BatchOutcome{tenant.BatchSkippedChanged, tenant.BatchSkippedHeld, tenant.BatchDeleted, tenant.BatchSkippedKept}
		if got := b.outcomes(id); !slices.Equal(got, want) {
			t.Fatalf("the outcomes are %v, want %v", got, want)
		}
		ds := b.deletes()
		if len(ds[record.StateCommitted]) != 1 || len(ds) != 1 {
			t.Fatalf("the deletes are %+v, want one committed", ds)
		}
		c := ds[record.StateCommitted][0]
		if c.Target != b.mr || c.Reason != "consent withdrawn" || c.KeyID != "K1234567890a" || c.Origin != tenant.OriginPerson {
			t.Fatalf("the batch's delete is %+v", c)
		}
		if b.sessionHeld(b.mr) || !b.sessionHeld(b.ct) || !b.sessionHeld(b.liver) || !b.sessionHeld(b.sc) {
			t.Fatal("the catalog does not show exactly the targets not deleted")
		}
		j, err := b.p.Job(b.ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if j.State != tenant.JobPartial || j.Counts.ScansDone != 4 || j.Counts.InstancesSent != 1 {
			t.Fatalf("the ended job is %+v", j)
		}
	})
}

// TestBatchPauseResumeCancel guards that a paused batch deletes nothing
// until it is resumed, a pause during a run stops it before its next
// target, and a canceled batch deletes nothing more and ends with no
// outcome for what it did not reach.
func TestBatchPauseResumeCancel(t *testing.T) {
	b := newBatchRig(t, false)
	id := b.queue(b.ct, b.mr, b.liver)
	if _, err := b.p.PauseDeleteBatch(b.ctx, id, true, time.Now(), b.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e := b.engine()
	res, err := b.run(e, id, nil)
	if err != nil || !res.Stopped || len(b.deletes()) != 0 {
		t.Fatalf("a paused batch's run answers %+v, %v, and made %+v", res, err, b.deletes())
	}
	if _, err := b.p.PauseDeleteBatch(b.ctx, id, false, time.Now(), b.audit(), nil); err != nil {
		t.Fatal(err)
	}
	res, err = b.run(e, id, func(tenant.DeleteBatchTarget, tenant.BatchOutcome) {
		if _, err := b.p.PauseDeleteBatch(b.ctx, id, true, time.Now(), b.audit(), nil); err != nil {
			t.Error(err)
		}
	})
	if err != nil || !res.Stopped || res.Met[tenant.BatchDeleted] != 1 {
		t.Fatalf("a run paused after its first target answers %+v, %v", res, err)
	}
	if got := b.outcomes(id); got[0] != tenant.BatchDeleted || got[1] != tenant.BatchPending {
		t.Fatalf("after the pause the outcomes are %v", got)
	}
	if _, err := b.p.PauseDeleteBatch(b.ctx, id, false, time.Now(), b.audit(), nil); err != nil {
		t.Fatal(err)
	}
	res, err = b.run(e, id, nil)
	if err != nil || !res.Ended || res.Met[tenant.BatchDeleted] != 2 {
		t.Fatalf("the resumed run answers %+v, %v", res, err)
	}
	if j, _ := b.p.Job(b.ctx, id); j.State != tenant.JobDone {
		t.Fatalf("the batch ended %s, want done", j.State)
	}

	b2 := newBatchRig(t, false)
	id2 := b2.queue(b2.ct, b2.mr)
	if _, err := b2.p.CancelJob(b2.ctx, id2, time.Now(), b2.audit(), nil); err != nil {
		t.Fatal(err)
	}
	res, err = b2.run(b2.engine(), id2, nil)
	if err != nil || !res.Ended || len(b2.deletes()) != 0 || !b2.sessionHeld(b2.ct) {
		t.Fatalf("a canceled batch's run answers %+v, %v, and made %+v", res, err, b2.deletes())
	}
	if got := b2.outcomes(id2); got[0] != tenant.BatchPending || got[1] != tenant.BatchPending {
		t.Fatalf("a canceled batch's outcomes are %v", got)
	}
}

// TestBatchRestart guards a restart in the middle of a batch: a stop after
// a target's delete was published but before its outcome was recorded is
// recorded deleted by that delete when the run goes on, and a stop before
// a delete's publish leaves it abandoned at the restart and the target
// deleted again once; no target is ever deleted twice.
func TestBatchRestart(t *testing.T) {
	for _, step := range []string{"committed", "prepared"} {
		t.Run(step, func(t *testing.T) {
			b := newBatchRig(t, false)
			targets := []tenant.CorrectionTarget{b.ct, b.mr, b.liver}
			id := b.queue(targets...)
			e := b.engine()
			e.SetCrashAt(crashAt(step))
			if _, err := b.run(e, id, nil); !errors.Is(err, errCrash) {
				t.Fatalf("the run did not stop at %s: %v", step, err)
			}
			restarted := b.engine()
			if err := restarted.Recover(b.ctx); err != nil {
				t.Fatal(err)
			}
			res, err := b.run(restarted, id, nil)
			if err != nil || !res.Ended {
				t.Fatalf("the run after the restart answers %+v, %v", res, err)
			}
			want := []tenant.BatchOutcome{tenant.BatchDeleted, tenant.BatchDeleted, tenant.BatchDeleted}
			if got := b.outcomes(id); !slices.Equal(got, want) {
				t.Fatalf("the outcomes are %v", got)
			}
			ds := b.deletes()
			if len(ds[record.StateCommitted]) != 3 {
				t.Fatalf("the deletes are %+v, want three committed", ds)
			}
			seen := map[tenant.CorrectionTarget]bool{}
			for _, c := range ds[record.StateCommitted] {
				if seen[c.Target] {
					t.Fatalf("%v was deleted twice", c.Target)
				}
				seen[c.Target] = true
			}
			ts, err := b.p.DeleteBatchTargets(b.ctx, id, 0, 10)
			if err != nil {
				t.Fatal(err)
			}
			for _, x := range ts {
				if c := b.state(x.Correction); c.State != record.StateCommitted || c.Target != x.Target {
					t.Fatalf("target %d names %+v", x.Ord, c)
				}
			}
			if wantAbandoned := step == "prepared"; (len(ds[record.StateAbandoned]) == 1) != wantAbandoned {
				t.Fatalf("the abandoned deletes are %+v", ds[record.StateAbandoned])
			}
		})
	}
}

// TestBatchUndo guards that a delete a batch made is undone as any delete
// is: its session shown again, the batch's record of it unchanged.
func TestBatchUndo(t *testing.T) {
	b := newBatchRig(t, false)
	id := b.queue(b.ct, b.mr)
	e := b.engine()
	if res, err := b.run(e, id, nil); err != nil || !res.Ended {
		t.Fatalf("the run answers %+v, %v", res, err)
	}
	ts, err := b.p.DeleteBatchTargets(b.ctx, id, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	u, err := e.Undo(b.ctx, ts[1].Correction, "deleted by mistake", b.audit(), nil)
	if err != nil || u.State != record.StateUndone {
		t.Fatalf("the undo answers %+v, %v", u, err)
	}
	if !b.sessionHeld(b.mr) || b.sessionHeld(b.ct) {
		t.Fatal("after the undo the catalog does not show the session undone alone")
	}
	if got := b.outcomes(id); got[1] != tenant.BatchDeleted {
		t.Fatalf("after the undo the batch's outcomes are %v", got)
	}
}

// TestBatchStopsForItsKey guards that a batch whose key may no longer
// delete stops: the guard's refusal ends the job failed with the reason,
// nothing deleted.
func TestBatchStopsForItsKey(t *testing.T) {
	b := newBatchRig(t, false)
	id := b.queue(b.ct, b.mr)
	res, err := b.engine().RunBatch(b.ctx, BatchRun{Store: b.p, JobID: id,
		Audit: func(tenant.CorrectionTarget, time.Time) master.AuditEntry { return b.audit() },
		Guard: func(context.Context) error {
			return &KeyRefused{Why: "key revoked", Err: fault.New(fault.Denied, "revoked")}
		}})
	if err != nil || !res.Stopped {
		t.Fatalf("the run answers %+v, %v", res, err)
	}
	j, err := b.p.Job(b.ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != tenant.JobFailed || j.Reason != "stopped: the key that made the batch may no longer delete (key revoked)" || len(b.deletes()[record.StateCommitted]) != 0 {
		t.Fatalf("the stopped job is %+v", j)
	}
}
