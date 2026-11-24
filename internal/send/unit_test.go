package send

import (
	"context"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// feedPipeline is a tenant's database with its event feed, as a driver's
// is.
type feedPipeline interface {
	tenant.Pipeline
	tenant.Events
}

// recording is a tenant's database that records the units given back,
// with and without an attempt spent.
type recording struct {
	feedPipeline
	mu       sync.Mutex
	released []tenant.WorkFence
	reasons  []string
	failed   []tenant.WorkFence
}

// ReleaseJobUnit records the release, then makes it.
func (r *recording) ReleaseJobUnit(ctx context.Context, fence tenant.WorkFence, due time.Time, reason string, now time.Time) error {
	r.mu.Lock()
	r.released, r.reasons = append(r.released, fence), append(r.reasons, reason)
	r.mu.Unlock()
	return r.feedPipeline.ReleaseJobUnit(ctx, fence, due, reason, now)
}

// FailWork records the failure, then makes it.
func (r *recording) FailWork(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Work, error) {
	r.mu.Lock()
	r.failed = append(r.failed, fence)
	r.mu.Unlock()
	return r.feedPipeline.FailWork(ctx, fence, reason, now)
}

// counts returns how many units were given back each way.
func (r *recording) counts() (released, failed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.released), len(r.failed)
}

// record makes the fixture's supervisor work through a recording database.
func (f *fixture) record() *recording {
	r := &recording{feedPipeline: f.p.(feedPipeline)}
	f.ten.p = r
	return r
}

// oneBatch is the most instances the contract lets a cancel or a crash
// send after the last write (job.proto: batches of at most 64), and the
// one in flight; written out, so a change of MaxBatch fails the tests.
const oneBatch = 64 + 1

// waitFor waits until cond holds, failing the test after bound.
func waitFor(t *testing.T, bound time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(bound)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen within %s", what, bound)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settle waits until the peer has received nothing new for quiet.
func settle(peer *dimsePeer, quiet time.Duration) int {
	last := peer.arrivals()
	for {
		time.Sleep(quiet)
		n := peer.arrivals()
		if n == last {
			return n
		}
		last = n
	}
}

// TestCancelIsSeenWithinOneBatch guards the cancel of a job a unit is
// sending: the unit learns of it at its next write and stops, so the peer
// receives at most one batch (64 instances, and the one in flight)
// after the cancel, and what the peer answered for that batch is still
// counted, so the job's count of sent instances is what the peer holds.
func TestCancelIsSeenWithinOneBatch(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{delay: 2 * time.Millisecond}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 150, 256)})
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	f.start(f.config())
	waitFor(t, 30*time.Second, "the first instances' arrival", func() bool { return peer.arrivals() >= 20 })
	if _, err := f.p.CancelJob(f.ctx, job.ID, f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	atCancel := peer.arrivals()
	final := settle(peer, 1500*time.Millisecond)
	if extra := final - atCancel; extra > oneBatch {
		t.Errorf("the peer received %d instances after the cancel, more than one batch (%d)", extra, oneBatch)
	}
	j, err := f.p.Job(f.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State != tenant.JobCanceled || j.Counts.InstancesSent != int64(final) {
		t.Errorf("job %s with %d sent, want canceled with the %d the peer holds", j.State, j.Counts.InstancesSent, final)
	}
	t.Logf("arrived before the cancel %d, after it %d", atCancel, final-atCancel)
}

// TestTakeoverResendsAtMostOneBatch guards the at-least-once window: a
// server that dies mid-scan (its workers cut off with no write) leaves the
// unit leased; once the lease has run out another server takes the unit
// over and goes on after the cursor, so the job ends done with every
// instance at the peer, and at most one batch (and the one in flight)
// arrives twice.
func TestTakeoverResendsAtMostOneBatch(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{delay: time.Millisecond}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 150, 256)})
	f.addDIMSE("peer", addr)
	job := f.createJob("peer", "ses-1")
	first, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "seventy instances' arrival", func() bool { return peer.arrivals() >= 70 })
	// A crash: everything in flight ends with nothing more written.
	first.cancel()
	_ = first.Stop(context.Background())
	before := peer.arrivals()
	f.clock.add(Lease + time.Second)
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 150 {
		t.Fatalf("job %s %+v after the takeover, want done with 150 sent", j.State, j.Counts)
	}
	peer.mu.Lock()
	defer peer.mu.Unlock()
	twice := 0
	for uid := range f.files {
		switch n := peer.count[uid]; {
		case n == 0:
			t.Errorf("instance %s never reached the peer", uid)
		case n > 2:
			t.Errorf("instance %s reached the peer %d times", uid, n)
		case n == 2:
			twice++
		}
	}
	if twice > oneBatch {
		t.Errorf("%d instances were sent twice, more than one batch (%d)", twice, oneBatch)
	}
	t.Logf("arrived before the crash %d; sent twice after the takeover %d", before, twice)
}

// TestNoReadSlotReleasesWithoutAnAttempt guards the release of a unit for
// a cause that is not its own: with every read slot of the sender taken
// (checkedread.MaxOpenReads, shared with the API's count), the unit is
// given back with ReleaseJobUnit, never FailWork, so however long the
// slots stay taken no attempt is spent; once a slot is free the job ends
// done.
func TestNoReadSlotReleasesWithoutAnAttempt(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 3, 256)})
	f.addDIMSE("peer", addr)
	rec := f.record()
	var frees []func()
	for range 16 {
		free, err := f.reads.Take("send:lab/peer")
		if err != nil {
			t.Fatal(err)
		}
		frees = append(frees, free)
	}
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	releases := func() int { n, _ := rec.counts(); return n }
	f.stepOnEach(s, "three releases", time.Minute, SlotDelay+time.Second, releases, func() bool { return releases() >= 3 })
	released, failed := rec.counts()
	if failed != 0 {
		t.Errorf("%d units failed with an attempt spent, want none", failed)
	}
	rec.mu.Lock()
	fence, reason := rec.released[0], rec.reasons[0]
	rec.mu.Unlock()
	if reason != reasonNoSlot {
		t.Errorf("release reason %q, want %q", reason, reasonNoSlot)
	}
	for _, free := range frees {
		free()
	}
	f.clock.add(SlotDelay + time.Second)
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || peer.arrivals() != 3 {
		t.Errorf("job %s, peer holds %d, want done with 3", j.State, peer.arrivals())
	}
	w := f.unit(fence.ID)
	if w.MaxAttempts-w.Attempt != tenant.JobUnitAttempts-1 {
		t.Errorf("after %d releases the unit is at attempt %d of %d: releases spent attempts", released, w.Attempt, w.MaxAttempts)
	}
	if _, _, err := f.p.ListJobFailures(f.ctx, job.ID, true, master.Page{Limit: 10}); err != nil {
		t.Fatal(err)
	}
}

// TestDisabledSenderGivesItsUnitBack guards a sender disabled while it
// sends: its unit ends after the instance in flight, the peer's answers so
// far recorded, and is given back without spending an attempt; the sender
// is down for disabled and nothing more reaches the peer; enabled again, it
// is unknown until it connects, and the job ends done.
func TestDisabledSenderGivesItsUnitBack(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{delay: 2 * time.Millisecond}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 150, 256)})
	row := f.addDIMSE("peer", addr)
	rec := f.record()
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	waitFor(t, 30*time.Second, "the first instances' arrival", func() bool { return peer.arrivals() >= 10 })
	set := func(enabled bool) {
		row.Enabled = enabled
		if _, err := f.p.SetSender(f.ctx, row, []tenant.SenderField{tenant.FieldSenderEnabled}, f.clock.Now(), f.audit(), nil); err != nil {
			t.Fatal(err)
		}
		s.Poke("lab")
	}
	set(false)
	waitFor(t, 30*time.Second, "the unit's release", func() bool { n, _ := rec.counts(); return n >= 1 })
	stopped := settle(peer, 500*time.Millisecond)
	rec.mu.Lock()
	reason := rec.reasons[0]
	rec.mu.Unlock()
	if _, failed := rec.counts(); failed != 0 || reason != reasonDisabled {
		t.Errorf("given back with reason %q and %d attempts spent, want %q and none", reason, failed, reasonDisabled)
	}
	if h, _ := s.Health("lab", "peer"); h.State != tenant.SenderDown || h.DownReason != tenant.SenderDownDisabled {
		t.Errorf("health %+v, want down for disabled", h)
	}
	j, err := f.p.Job(f.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if j.State.Ended() || j.Counts.InstancesSent != int64(stopped) {
		t.Errorf("job %s with %d sent while disabled, want running with the %d the peer holds", j.State, j.Counts.InstancesSent, stopped)
	}
	set(true)
	if j := f.waitJob(s, job.ID, 30*time.Second, ended); j.State != tenant.JobDone {
		t.Errorf("job %s once enabled again, want done", j.State)
	}
}

// TestDestinationChangeMovesTheUnit guards a change of where a sender
// connects while it sends: the unit on the old peer ends after the
// instance in flight and is given back without spending an attempt, the
// old peer receives nothing more, and the rest goes to the new one.
func TestDestinationChangeMovesTheUnit(t *testing.T) {
	f := newFixture(t)
	oldPeer := &dimsePeer{delay: 5 * time.Millisecond}
	newPeer := &dimsePeer{}
	oldAddr, newAddr := startPeer(t, oldPeer), startPeer(t, newPeer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 150, 256)})
	row := f.addDIMSE("peer", oldAddr)
	rec := f.record()
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	waitFor(t, 30*time.Second, "the first instances' arrival", func() bool { return oldPeer.arrivals() >= 10 })
	row.DIMSE.Port = int(netip.MustParseAddrPort(newAddr).Port())
	if _, err := f.p.SetSender(f.ctx, row, []tenant.SenderField{tenant.FieldDIMSEPeerPort}, f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	s.Poke("lab")
	waitFor(t, 30*time.Second, "the unit's release", func() bool { n, _ := rec.counts(); return n >= 1 })
	moved := settle(oldPeer, 300*time.Millisecond)
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone {
		t.Fatalf("job %s, want done", j.State)
	}
	if after := oldPeer.arrivals(); after != moved {
		t.Errorf("the old peer received %d more after the change", after-moved)
	}
	if oldPeer.arrivals()+newPeer.arrivals() < 150 || newPeer.arrivals() == 0 {
		t.Errorf("old peer %d, new peer %d, want all 150 between them and the rest at the new one", oldPeer.arrivals(), newPeer.arrivals())
	}
	if _, failed := rec.counts(); failed != 0 {
		t.Errorf("%d attempts spent by the change, want none", failed)
	}
}

// refusingProgress is a tenant's database that refuses every progress
// write as malformed, as a store answers a write that breaks its rules.
type refusingProgress struct {
	*recording
	writes atomic.Int64
}

// RecordJobProgress refuses the write.
func (r *refusingProgress) RecordJobProgress(context.Context, tenant.JobProgress) (tenant.Job, error) {
	r.writes.Add(1)
	return tenant.Job{}, fault.New(fault.Invalid, "a progress write the store refuses")
}

// EndJobScan refuses an end that records progress, and makes one that
// records none.
func (r *refusingProgress) EndJobScan(ctx context.Context, p tenant.JobProgress) (tenant.Job, error) {
	if p.From != p.To || p.Sent+p.Warned+p.Refused > 0 {
		r.writes.Add(1)
		return tenant.Job{}, fault.New(fault.Invalid, "a progress write the store refuses")
	}
	return r.recording.EndJobScan(ctx, p)
}

// TestInvalidProgressEndsTheScan guards a progress write the store refuses
// as malformed, a fault of the sender's own: the unit ends its scan from
// the cursor last written, without the batch, and is not sent again for
// each of its attempts; the job ends with those instances remaining.
func TestInvalidProgressEndsTheScan(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	addr := startPeer(t, peer)
	f.archive("ses-1", scanFiles{"mr", generated(t, 3, 256)})
	f.addDIMSE("peer", addr)
	rec := &refusingProgress{recording: f.record()}
	f.ten.p = rec
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, time.Minute, ended)
	if j.State != tenant.JobFailed || j.Counts.InstancesRemaining() != 3 {
		t.Errorf("job %s %+v, want failed with the 3 instances remaining", j.State, j.Counts)
	}
	if _, failed := rec.counts(); failed != 0 || rec.writes.Load() != 1 || peer.arrivals() != 3 {
		t.Errorf("%d failed attempts, %d progress writes, %d at the peer; want none, one, and the 3 sent once",
			failed, rec.writes.Load(), peer.arrivals())
	}
}

// reclaimingStore is a tenant's database that reclaims by kind and records
// what it was asked, and whether a unit was claimed before.
type reclaimingStore struct {
	*recording
	mu      sync.Mutex
	kinds   []string
	claimed bool
	early   bool
}

// ReclaimWorkOfKinds records the kinds.
func (r *reclaimingStore) ReclaimWorkOfKinds(_ context.Context, kinds []string, _ time.Time) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, kinds...)
	r.early = r.claimed
	return 0, nil
}

// ClaimWork records the claim, then makes it.
func (r *reclaimingStore) ClaimWork(ctx context.Context, kind, owner string, now time.Time, lease time.Duration) (tenant.Work, error) {
	r.mu.Lock()
	r.claimed = true
	r.mu.Unlock()
	return r.recording.ClaimWork(ctx, kind, owner, now, lease)
}

// TestSendUnitsAreReclaimedBeforeWork guards the start of a tenant's
// sending: the send units of its senders, and only those kinds, are
// reclaimed once, before any of its workers claims a unit.
func TestSendUnitsAreReclaimedBeforeWork(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{}
	f.archive("ses-1", scanFiles{"mr", generated(t, 1, 256)})
	f.addDIMSE("peer", startPeer(t, peer))
	f.addDIMSE("other", startPeer(t, &dimsePeer{}))
	rs := &reclaimingStore{recording: f.record()}
	f.ten.p = rs
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	f.waitJob(s, job.ID, 30*time.Second, ended)
	time.Sleep(1500 * time.Millisecond) // a second reading of the senders
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if len(rs.kinds) != 2 || rs.kinds[0] != "send-other" || rs.kinds[1] != "send-peer" || rs.early {
		t.Errorf("reclaimed %v (after a claim: %v), want send-other and send-peer once, before any claim", rs.kinds, rs.early)
	}
}

// TestAETitleChangeMovesTheUnit guards a change of the AE title a DIMSE
// sender calls with while it sends: the unit in flight is given back,
// without spending an attempt, so the rest goes on an association with the
// new title, and the job ends done.
func TestAETitleChangeMovesTheUnit(t *testing.T) {
	f := newFixture(t)
	peer := &dimsePeer{delay: 5 * time.Millisecond}
	f.archive("ses-1", scanFiles{"mr", generated(t, 150, 256)})
	row := f.addDIMSE("peer", startPeer(t, peer))
	rec := f.record()
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	waitFor(t, 30*time.Second, "the first instances' arrival", func() bool { return peer.arrivals() >= 10 })
	row.DIMSE.CallingAETitle = "NOPACS2"
	if _, err := f.p.SetSender(f.ctx, row, []tenant.SenderField{tenant.FieldDIMSECallingAETitle}, f.clock.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	s.Poke("lab")
	waitFor(t, 30*time.Second, "the unit's release", func() bool { n, _ := rec.counts(); return n >= 1 })
	rec.mu.Lock()
	reason := rec.reasons[0]
	rec.mu.Unlock()
	if reason != reasonChanged {
		t.Errorf("release reason %q, want %q", reason, reasonChanged)
	}
	if j := f.waitJob(s, job.ID, 30*time.Second, ended); j.State != tenant.JobDone {
		t.Errorf("job %s, want done", j.State)
	}
	if _, failed := rec.counts(); failed != 0 {
		t.Errorf("%d attempts spent by the change, want none", failed)
	}
}

// TestSendersKeepToTheirShareOfReads guards the API's half of the server's
// read slots: the senders hold at most MaxSenderReads of them, one more is
// refused as exhausted, and the API's callers still find the other half.
func TestSendersKeepToTheirShareOfReads(t *testing.T) {
	f := newFixture(t)
	s, err := New(f.config())
	if err != nil {
		t.Fatal(err)
	}
	var releases []func()
	for i := range MaxSenderReads {
		release, err := s.takeRead("send:lab/s" + strconv.Itoa(i/checkedread.MaxOpenReads))
		if err != nil {
			t.Fatalf("read %d of the senders' share: %v", i, err)
		}
		releases = append(releases, release)
	}
	if _, err := s.takeRead("send:lab/more"); fault.KindOf(err) != fault.Exhausted {
		t.Errorf("a read past the senders' share: %v, want exhausted", err)
	}
	for i := range checkedread.MaxServerReads - MaxSenderReads {
		release, err := f.reads.Take("key" + strconv.Itoa(i))
		if err != nil {
			t.Fatalf("an API read %d with the senders at their share: %v", i, err)
		}
		releases = append(releases, release)
	}
	for _, r := range releases {
		r()
	}
}
