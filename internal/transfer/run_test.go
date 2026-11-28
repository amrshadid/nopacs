package transfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// sourceUIDs returns the SOP Instance UIDs the source archived in the
// session ("" for the whole subject), in order.
func (r *rig) sourceUIDs(session string) []string {
	r.t.Helper()
	var out []string
	err := EachScan(r.src.ctx, r.src, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: session}, func(p label.Path, _ tenant.CatalogScan) error {
		held, _, err := r.src.ListHeldInstances(r.src.ctx, p, master.Page{Limit: 100})
		for _, in := range held {
			out = append(out, in.SOPInstanceUID)
		}
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
	slices.Sort(out)
	return out
}

// TestCopyArrivesThroughTheJobReceiver guards a copy of a session: every
// archived file goes to the target's receiver of type job with exactly the
// bytes the source archived, the instance is found in the target by the UID
// and the checksum the target's own mapping gives, confirmed, and recorded in
// the target's provenance under the target's UID, which is not the source's;
// the source keeps its data and the job ends done.
func TestCopyArrivesThroughTheJobReceiver(t *testing.T) {
	r := newRig(t, true)
	id := r.queue("ses-1", false)
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobDone || tj.Step != tenant.TransferEnded || tj.Items.Confirmed != 3 || tj.Items.Total() != 3 ||
		tj.Job.Counts.InstancesSent != 3 || tj.Job.Counts.Instances != 3 {
		t.Fatalf("the job ended %+v, want done with three confirmed", tj)
	}
	if n := r.tgt.timesAccepted(); n != 3 {
		t.Fatalf("the target took %d files, want 3", n)
	}
	want := r.sourceUIDs("ses-1")
	if len(want) != 3 {
		t.Fatalf("the source holds %d instances in the session, want 3", len(want))
	}
	for _, src := range want {
		row, _ := r.tgt.TransferRow(context.Background(), "xfer", src, tenant.SHA256(sha256.Sum256(r.src.files[src])))
		if row.SOPInstanceUID == src {
			t.Fatalf("the target's UID of %s is the source's", src)
		}
		if !bytes.Equal(r.tgt.bodies[row.SOPInstanceUID], r.src.files[src]) {
			t.Errorf("the target received other bytes than the source archived for %s", src)
		}
		p, ok, err := r.tgt.TransferProvenanceOf(context.Background(), row.SOPInstanceUID)
		if err != nil || !ok {
			t.Fatalf("provenance of %s: %v, %v", row.SOPInstanceUID, ok, err)
		}
		if p.SourceTenantID != "T0000000000001" || p.JobID != id || p.Revision != 1 || p.Path.Subject != "sub-01" || p.Path.Session != "ses-1" {
			t.Errorf("the provenance is %+v, want the source tenant, job, revision and labels", p)
		}
		if p.SOPInstanceUID == src {
			t.Errorf("the provenance is keyed by the source's UID %s", src)
		}
	}
	for uid := range r.tgt.prov {
		if slices.Contains(want, uid) {
			t.Errorf("the target holds provenance under the source's UID %s", uid)
		}
	}
	if got := r.sourceUIDs("ses-1"); !slices.Equal(got, want) {
		t.Errorf("a copy changed the source: %v, was %v", got, want)
	}
	if r.deleter.previews.Load() != 0 || r.deleter.deletes.Load() != 0 {
		t.Errorf("a copy asked the source's delete (%d previews, %d deletes)", r.deleter.previews.Load(), r.deleter.deletes.Load())
	}
}

// TestMoveDeletesTheSourceAfterConfirmation guards a move: the source's
// preview and delete are asked only once every item is confirmed (the fake
// deleter checks the job's items when its delete comes), with the reason
// "moved to <target>", the delete is recorded on the job, and the job ends
// done.
func TestMoveDeletesTheSourceAfterConfirmation(t *testing.T) {
	r := newRig(t, false)
	id := r.queue("ses-1", true)
	r.tgt.delay = 2 // the target's catalog shows each file only on its third lookup
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobDone || tj.Items.Confirmed != 3 || tj.DeleteCorrection == "" {
		t.Fatalf("the move ended %+v, want done, three confirmed and a delete recorded", tj)
	}
	if r.deleter.deletes.Load() != 1 || len(r.deleter.reasons) != 1 || r.deleter.reasons[0] != "moved to other (transfer "+id+")" {
		t.Fatalf("the source's delete was made %d times with reasons %v, want once for the target and the job", r.deleter.deletes.Load(), r.deleter.reasons)
	}
}

// TestHeldMoveEndsPartialAndKeepsTheCopy guards a move whose source a hold
// covers: the delete is never asked, the job ends partial with its reason,
// and what was copied is confirmed and stays; so does a retention rule that
// keeps the source.
func TestHeldMoveEndsPartialAndKeepsTheCopy(t *testing.T) {
	for name, set := range map[string]func(*fakeDeleter){
		"hold":      func(d *fakeDeleter) { d.holds = []string{"h1"} },
		"retention": func(d *fakeDeleter) { d.minimum = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, false)
			set(r.deleter)
			id := r.queue("ses-1", true)
			tj := r.runUntilEnded(id)
			if tj.Job.State != tenant.JobPartial || tj.Items.Confirmed != 3 || tj.DeleteCorrection != "" {
				t.Fatalf("the move ended %+v, want partial with the copy confirmed and no delete", tj)
			}
			if tj.Job.Reason != reasonHeld && tj.Job.Reason != reasonKept {
				t.Errorf("the reason is %q, want the hold's or the retention rule's", tj.Job.Reason)
			}
			if r.deleter.deletes.Load() != 0 {
				t.Errorf("the source was deleted despite the %s", name)
			}
			if n := len(r.tgt.held); n != 3 {
				t.Errorf("the target holds %d instances, want the 3 copied", n)
			}
		})
	}
}

// TestChecksumMismatchIsNotConfirmed guards the confirmation: an instance the
// target holds with another received checksum than the source's bytes give is
// failed with its detail, never confirmed, provenance is not recorded for it,
// the move does not delete the source, and the job ends partial.
func TestChecksumMismatchIsNotConfirmed(t *testing.T) {
	r := newRig(t, true)
	uids := r.sourceUIDs("ses-1")
	r.tgt.wrongSum[uids[1]] = true
	id := r.queue("ses-1", true)
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobPartial || tj.Items.Confirmed != 2 || tj.Items.Failed != 1 || tj.DeleteCorrection != "" || r.deleter.deletes.Load() != 0 {
		t.Fatalf("the move ended %+v (deletes %d), want partial, two confirmed, one failed, the source kept", tj, r.deleter.deletes.Load())
	}
	failed, err := r.src.TransferItems(r.src.ctx, id, tenant.TransferItemFailed, tenant.TransferItemKey{}, 10)
	if err != nil || len(failed) != 1 {
		t.Fatalf("failed items: %v, %v", failed, err)
	}
	if f := failed[0]; f.SOPInstanceUID != uids[1] || f.Code != tenant.FailurePeerRefused || f.Detail != tenant.TransferDetailMismatch {
		t.Errorf("the failed item is %+v, want the mismatch", f)
	}
	row, _ := r.tgt.TransferRow(context.Background(), "xfer", uids[1], tenant.SHA256(sha256.Sum256(r.src.files[uids[1]])))
	if _, ok, _ := r.tgt.TransferProvenanceOf(context.Background(), row.SOPInstanceUID); ok {
		t.Errorf("provenance was recorded for an instance whose checksum differs")
	}
}

// TestUnaccountedCopyFails guards the wait: a file the target took and its
// catalog never shows is failed as unaccounted once the wait has passed, not
// confirmed and not waited for for ever.
func TestUnaccountedCopyFails(t *testing.T) {
	r := newRig(t, false)
	r.tgt.delay = 1 << 30
	id := r.queue("ses-1", false)
	run := r.runner()
	res, err := run.Run(r.src.ctx, id)
	if err != nil || !res.Waiting || res.Step != tenant.TransferConfirming {
		t.Fatalf("a pass while the target shows nothing: %+v, %v, want waiting at confirming", res, err)
	}
	r.src.now.Add(int64(2 * time.Hour / time.Microsecond))
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobPartial || tj.Items.Failed != 3 {
		t.Fatalf("the job ended %+v, want partial with three failed", tj)
	}
	fails, _, err := r.src.ListJobFailures(r.src.ctx, id, true, master.Page{Limit: 10})
	if err != nil || len(fails) != 3 || fails[0].Code != tenant.FailurePeerUnaccounted {
		t.Fatalf("failures: %+v, %v", fails, err)
	}
}

// TestRestartLosesNothingAndCopiesNothingTwice guards the saved steps: a stop
// during listing, during copying (right after the target took a file, before
// the job recorded it), during confirming, and during the delete, each followed
// by a new runner as a restarted server makes, ends with every instance
// confirmed, the target having taken each file once, and the source deleted
// once.
func TestRestartLosesNothingAndCopiesNothingTwice(t *testing.T) {
	type stop struct {
		name  string
		setup func(r *rig, cancel context.CancelFunc)
	}
	stops := []stop{
		{"listing", func(r *rig, cancel context.CancelFunc) {
			r.cfg.Source = &failingSource{source: r.src, cancel: cancel}
		}},
		{"copying", func(r *rig, cancel context.CancelFunc) {
			r.tgt.afterAccept = func(n int) {
				if n == 2 {
					cancel()
				}
			}
		}},
		{"confirming", func(r *rig, cancel context.CancelFunc) {
			// The target's catalog shows nothing on the first lookup, so the
			// first run stops at confirming, waiting.
			r.tgt.delay = 1
		}},
		{"deleting", func(r *rig, cancel context.CancelFunc) { r.deleter.failOnce.Store(true) }},
	}
	for _, s := range stops {
		t.Run(s.name, func(t *testing.T) {
			r := newRig(t, true)
			id := r.queue("ses-1", true)
			ctx, cancel := context.WithCancel(r.src.ctx)
			defer cancel()
			s.setup(r, cancel)
			if _, err := r.runner().Run(ctx, id); err == nil && s.name != "confirming" && s.name != "deleting" {
				t.Fatalf("the first run was not stopped")
			}
			if s.name == "listing" {
				r.cfg.Source = r.src
			}
			r.tgt.afterAccept = nil
			tj := r.runUntilEnded(id)
			if tj.Job.State != tenant.JobDone || tj.Items.Confirmed != 3 {
				t.Fatalf("after the restart the job ended %+v, want done with three confirmed", tj)
			}
			for uid, n := range r.tgt.accepted {
				if n != 1 {
					t.Errorf("the target took %s %d times, want once", uid, n)
				}
			}
			if len(r.tgt.accepted) != 3 || r.deleter.deletes.Load() != 1 {
				t.Errorf("the target took %d distinct files and the source was deleted %d times, want 3 and 1", len(r.tgt.accepted), r.deleter.deletes.Load())
			}
		})
	}
}

// failingSource is a source whose listing is stopped after its first scan is
// listed.
type failingSource struct {
	*source
	cancel context.CancelFunc
	calls  int
}

// AddTransferItems adds the items and stops the listing after the first call.
func (s *failingSource) AddTransferItems(ctx context.Context, id string, items []tenant.TransferNewItem) (int, error) {
	n, err := s.source.AddTransferItems(ctx, id, items)
	s.calls++
	if s.calls == 1 {
		s.cancel()
		return n, context.Canceled
	}
	return n, err
}

// TestSourceChangedKeepsTheSource guards the move's check that the source
// holds exactly what was copied: an instance that arrived in the subject
// after the listing ends the job partial, the source's delete never asked.
func TestSourceChangedKeepsTheSource(t *testing.T) {
	r := newRig(t, false)
	id := r.queue("", true)
	r.tgt.afterAccept = func(n int) {
		if n != 3 {
			return
		}
		// A session reaches the subject while the job copies.
		extra := generated(t, 6)[5]
		r.src.archive("ses-9", false, scanFiles{label: "scan-c", files: [][]byte{extra}})
	}
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobPartial || tj.Job.Reason != reasonSourceChanged || r.deleter.deletes.Load() != 0 || tj.Items.Confirmed != 4 {
		t.Fatalf("the move ended %+v (deletes %d), want partial %q with the source kept", tj, r.deleter.deletes.Load(), reasonSourceChanged)
	}
}

// TestAsReceivedFollowsTheSendRule guards what leaves the source: an instance
// archived as received is copied only into a receiver that de-identifies and
// is otherwise refused as the send rule refuses it, the job ending partial
// with the failure listed and nothing sent; an instance whose image a pixel
// rule accepted at risk is refused into a receiver that does not de-identify
// as well.
func TestAsReceivedFollowsTheSendRule(t *testing.T) {
	t.Run("not de-identifying", func(t *testing.T) {
		r := newRig(t, false)
		r.src.archive("ses-3", true, scanFiles{label: "scan-a", files: generated(t, 8)[6:]})
		id := r.queue("ses-3", true)
		tj := r.runUntilEnded(id)
		if tj.Job.State != tenant.JobPartial || tj.Items.Failed != 2 || r.tgt.timesAccepted() != 0 || r.deleter.deletes.Load() != 0 {
			t.Fatalf("the transfer ended %+v (target took %d), want partial, two failed, nothing sent", tj, r.tgt.timesAccepted())
		}
		fails, _, err := r.src.ListJobFailures(r.src.ctx, id, true, master.Page{Limit: 10})
		if err != nil || len(fails) != 2 || fails[0].Code != tenant.FailureAsReceived || fails[0].Detail != tenant.TransferDetailAsRecv {
			t.Fatalf("failures: %+v, %v", fails, err)
		}
	})
	t.Run("de-identifying", func(t *testing.T) {
		r := newRig(t, true)
		r.src.archive("ses-3", true, scanFiles{label: "scan-a", files: generated(t, 8)[6:]})
		id := r.queue("ses-3", false)
		tj := r.runUntilEnded(id)
		if tj.Job.State != tenant.JobDone || tj.Items.Confirmed != 2 || r.tgt.timesAccepted() != 2 {
			t.Fatalf("the transfer ended %+v (target took %d), want done with both copied", tj, r.tgt.timesAccepted())
		}
		for _, p := range r.tgt.prov {
			if p.Path != (label.Path{}) {
				t.Errorf("the provenance of an instance archived as received holds labels %+v, which may be an original identifier", p.Path)
			}
		}
	})
}

// TestDamagedSourceIsNotCopied guards the checked read: an instance whose
// stored bytes are not what the catalog records is failed as damaged, and the
// target is never given a whole file of it.
func TestDamagedSourceIsNotCopied(t *testing.T) {
	r := newRig(t, false)
	uid := r.sourceUIDs("ses-1")[0]
	bad := bytes.Clone(r.src.files[uid])
	bad[len(bad)-1] ^= 0xff
	if _, err := r.src.store.Put(r.src.ctx, r.src.keys[uid], bytes.NewReader(bad)); err != nil {
		t.Fatal(err)
	}
	id := r.queue("ses-1", false)
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobPartial || tj.Items.Confirmed != 2 || tj.Items.Failed != 1 || r.tgt.accepted[uid] != 0 {
		t.Fatalf("the job ended %+v (target took %d of the damaged file), want partial with two confirmed", tj, r.tgt.accepted[uid])
	}
	fails, _, err := r.src.ListJobFailures(r.src.ctx, id, true, master.Page{Limit: 10})
	if err != nil || len(fails) != 1 || fails[0].Code != tenant.FailureDamagedInStorage || fails[0].SOPInstanceUID != uid {
		t.Fatalf("failures: %+v, %v", fails, err)
	}
}

// TestKeyNoLongerAdminStopsTheJob guards the check made again when the job
// runs: a key the access check refuses ends the job failed with the key's
// refusal in its reason, and nothing is copied.
func TestKeyNoLongerAdminStopsTheJob(t *testing.T) {
	r := newRig(t, false)
	r.cfg.Authorize = func(_ context.Context, key, target string) error {
		if key != "k-admin" || target != "T0000000000002" {
			t.Errorf("Authorize(%q, %q)", key, target)
		}
		return &correct.KeyRefused{Why: "key revoked", Err: errors.New("revoked")}
	}
	id := r.queue("ses-1", false)
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobFailed || r.tgt.timesAccepted() != 0 || tj.Job.Reason == "" || tj.Step != tenant.TransferEnded {
		t.Fatalf("the job ended %+v (target took %d), want failed with a reason and nothing copied", tj, r.tgt.timesAccepted())
	}
}

// TestTargetWithoutJobReceiverFails guards the target's receiver: a job whose
// target holds no enabled receiver of type job by that name ends failed
// before it reads a byte.
func TestTargetWithoutJobReceiverFails(t *testing.T) {
	r := newRig(t, false)
	r.tgt.rcv.Enabled = false
	id := r.queue("ses-1", false)
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobFailed || tj.Job.Reason != reasonNoReceiver || r.stores.opened.Load() != 0 {
		t.Fatalf("the job ended %+v (opened %d objects), want failed %q before any read", tj, r.stores.opened.Load(), reasonNoReceiver)
	}
}

// TestTargetBusyWaits guards a target that refuses a file for now: the item
// stays listed, the pass says it waits, and a later pass copies it.
func TestTargetBusyWaits(t *testing.T) {
	r := newRig(t, false)
	r.tgt.busy.Store(true)
	id := r.queue("ses-1", false)
	res, err := r.runner().Run(r.src.ctx, id)
	if err != nil || !res.Waiting || res.Step != tenant.TransferCopying {
		t.Fatalf("a pass against a busy target: %+v, %v, want waiting at copying", res, err)
	}
	tj, _ := r.src.TransferJob(r.src.ctx, id)
	if tj.Items.Listed != 3 || tj.Items.Failed != 0 {
		t.Fatalf("the items are %+v, want three still listed", tj.Items)
	}
	r.tgt.busy.Store(false)
	if tj = r.runUntilEnded(id); tj.Job.State != tenant.JobDone || tj.Items.Confirmed != 3 {
		t.Fatalf("the job ended %+v after the target took files again", tj)
	}
}

// TestSubjectTransferListsEverySession guards the listing: a transfer of the
// subject copies the instances of each of its sessions.
func TestSubjectTransferListsEverySession(t *testing.T) {
	r := newRig(t, false)
	id := r.queue("", false)
	tj := r.runUntilEnded(id)
	if tj.Job.State != tenant.JobDone || tj.Items.Confirmed != 4 || len(r.tgt.held) != 4 {
		t.Fatalf("the transfer ended %+v with %d held in the target, want four of both sessions", tj, len(r.tgt.held))
	}
}
