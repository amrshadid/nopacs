package deidjob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// errNoRandom is the failure of a source of keys a test plants.
var errNoRandom = errors.New("the test's source of keys failed")

// generatedSession archives n instances made from the MR image as
// received into session ses-gen, in one scan.
func (f *fixture) generatedSession(n int) {
	f.t.Helper()
	g := testkit.NewGenerator(f.t, corpus(f.t, "MR_small.dcm"))
	s := scanFiles{label: "gen"}
	for i := range n {
		r, _ := g.File(i, 0)
		b, err := io.ReadAll(r)
		if err != nil {
			f.t.Fatal(err)
		}
		s.files = append(s.files, b)
	}
	f.archive("ses-gen", s)
}

// TestCancelEndsUnitsWithinABatch guards a cancel of a deid job while its
// unit judges: the unit learns of it at its next write, records what it
// judged, and ends; the job ends canceled with fewer instances judged than
// it holds, and nothing more is judged after.
func TestCancelEndsUnitsWithinABatch(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.generatedSession(3 * MaxBatch)
	var seen recorded
	canceled := make(chan struct{})
	var job tenant.Job
	cfg := withJudged(f.config(), func(u string) {
		seen.add(u)
		if seen.count() == 10 {
			if _, err := f.p.CancelJob(f.ctx, job.ID, f.clock.Now(), f.audit(), nil); err != nil {
				t.Error(err)
			}
			close(canceled)
		}
	})
	job = f.createDeid(tenant.DeidDryRun, ref, "ses-gen")
	s := f.start(cfg)
	s.Poke("lab")
	<-canceled
	j := f.waitJob(s, job.ID, 30*time.Second, func(j tenant.Job) bool { return j.State == tenant.JobCanceled && j.Counts.ScansDone == j.Counts.Scans })
	judged := j.Counts.InstancesSent + j.Counts.InstancesWarned + j.Counts.InstancesRefused
	if judged == 0 || judged >= int64(3*MaxBatch) || j.Counts.InstancesRemaining() == 0 {
		t.Fatalf("a job canceled midway counts %+v", j.Counts)
	}
	time.Sleep(200 * time.Millisecond)
	if n := seen.count(); n > 10+MaxBatch {
		t.Errorf("the unit judged %d instances, more than one batch after the cancel", n)
	}
}

// TestStopAndRestartGoOnFromTheCursor guards a deid job across a stop of
// the server: the first supervisor stops midway, records what it judged
// and gives its unit back without spending an attempt; a second one goes
// on after the recorded cursor, judges again at most one batch, and the
// job ends done with every instance counted once.
func TestStopAndRestartGoOnFromTheCursor(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	const total = 3 * MaxBatch
	f.generatedSession(total)
	var first, second recorded
	reached := make(chan struct{})
	cfg := withJudged(f.config(), func(u string) {
		first.add(u)
		if first.count() == MaxBatch+5 {
			close(reached)
		}
	})
	job := f.createDeid(tenant.DeidDryRun, ref, "ses-gen")
	s1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s1.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	s1.Poke("lab")
	<-reached
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	if err := s1.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	mid, err := f.p.Job(f.ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if mid.State.Ended() || mid.Counts.InstancesSent == 0 {
		t.Fatalf("after the stop the job reads %+v", mid)
	}
	if w, err := f.p.Work(f.ctx, tenant.JobUnitID(job.ID, "")); err != nil || w.State != tenant.WorkDone {
		t.Errorf("the listing unit after the stop: %+v, %v", w, err)
	}
	s2 := f.start(withJudged(f.config(), second.add))
	s2.Poke("lab")
	j := f.waitJob(s2, job.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != total || j.Counts.InstancesRemaining() != 0 {
		t.Fatalf("the job after a restart ended %s with %+v", j.State, j.Counts)
	}
	again := first.count() + second.count() - total
	if again < 0 || again > MaxBatch {
		t.Errorf("the restart judged %d instances again, want at most one batch", again)
	}
	// The stop gave the unit back without spending an attempt: two claims,
	// and one attempt allowed more for the one given back.
	w, err := f.p.Work(f.ctx, tenant.JobUnitID(job.ID, "1"))
	if err != nil || w.State != tenant.WorkDone || w.Attempt != 2 || w.MaxAttempts != tenant.JobUnitAttempts+1 {
		t.Errorf("the scan's unit after a stop and a restart: %+v, %v", w, err)
	}
	// Each instance tallied once, whatever was judged again.
	tallies := f.tallies(job.ID)
	if n := tallies["class|1.2.840.10008.5.1.4.1.1.4|passed"]; n != total {
		t.Errorf("the class is tallied %d times, want %d (tallies %v)", n, total, tallies)
	}
	if n := tallies["rule|maker|remove"]; n != total {
		t.Errorf("the maker rule is tallied %d times, want %d", n, total)
	}
}

// TestRetryJudgesWhatWasRefused guards RetryJob of an ended deid job: the
// new job, of the same mode and version, judges exactly the instances the
// ended one refused.
func TestRetryJudgesWhatWasRefused(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	names := f.rawSession()
	s := f.start(f.config())
	j := f.createDeid(tenant.DeidDryRun, ref, "ses-raw")
	s.Poke("lab")
	f.waitJob(s, j.ID, 30*time.Second, ended)
	var seen recorded
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	s2 := f.start(withJudged(f.config(), seen.add))
	_, retry, err := f.p.RetryJob(f.ctx, j.ID, f.id("job"), f.clock.Now(), f.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Kind != tenant.JobDeid || retry.Mode != tenant.DeidDryRun || retry.Profile != ref {
		t.Fatalf("the retry reads %+v", retry)
	}
	s2.Poke("lab")
	r := f.waitJob(s2, retry.ID, 30*time.Second, ended)
	if r.Counts.Instances != 2 || r.Counts.InstancesRefused != 2 {
		t.Errorf("the retry counts %+v, want the 2 refused judged again", r.Counts)
	}
	seen.mu.Lock()
	defer seen.mu.Unlock()
	if len(seen.n) != 2 || seen.n[names["SC_rgb_rle_2frame.dcm"]] != 1 || seen.n[names["jpeg_3frame_nobot.dcm"]] != 1 {
		t.Errorf("the retry judged %v", seen.n)
	}
}

// TestTheUIDKeyIsZeroedWhenTheUnitEnds guards the one tenant secret a
// check holds: the UID key it was given is all zeros once its unit ended.
func TestTheUIDKeyIsZeroedWhenTheUnitEnds(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.deidSession("ses-1", ref, map[string][]string{"mr": {"MR_small.dcm"}}, nil)
	var mu sync.Mutex
	var given [][]byte
	cfg := f.config()
	cfg.UIDKey = func(context.Context, string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		k := bytes.Clone(tenantKeys.UID)
		given = append(given, k)
		return k, nil
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	j := f.createDeid(tenant.DeidCheck, tenant.ProfileRef{}, "ses-1")
	s.Poke("lab")
	f.waitJob(s, j.ID, 30*time.Second, ended)
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(given) == 0 {
		t.Fatal("the check never read the UID key")
	}
	for i, k := range given {
		if !bytes.Equal(k, make([]byte, len(k))) {
			t.Errorf("UID key %d is not zeroed after its unit ended", i)
		}
	}
}

// TestAFileTooLargeIsANote guards a file past the bound a unit reads: it
// is not read, and passes with a note (too_large), so a job of only such
// files ends done.
func TestAFileTooLargeIsANote(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.rawSession()
	s := f.start(withMaxFileBytes(f.config(), 1000))
	j := f.createDeid(tenant.DeidDryRun, ref, "ses-raw")
	s.Poke("lab")
	j = f.waitJob(s, j.ID, 30*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesWarned != 5 || j.Counts.InstancesRefused != 0 {
		t.Fatalf("files too large to read: %s %+v", j.State, j.Counts)
	}
	for _, x := range f.failures(j.ID) {
		if x.Code != tenant.FailureTooLarge {
			t.Errorf("a file too large is noted %+v", x)
		}
	}
}

// TestKeysThatCannotBeMadeSpendAnAttempt guards a unit whose own keys
// cannot be made: it spends an attempt with its reason (FailWork), as the
// other early ends do, rather than holding its lease until it runs out.
func TestKeysThatCannotBeMadeSpendAnAttempt(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.rawSession()
	var tries atomic.Int64
	cfg := withKeys(f.config(), func() (engine.Keys, error) {
		tries.Add(1)
		return engine.Keys{}, errNoRandom
	})
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	j := f.createDeid(tenant.DeidDryRun, ref, "ses-raw")
	s.Poke("lab")
	deadline := time.Now().Add(20 * time.Second)
	for tries.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	ctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	if tries.Load() == 0 {
		t.Fatal("no unit tried to make its keys")
	}
	// Due again after the wait between attempts; a unit still leased
	// would not be claimable for its whole lease.
	w, err := f.p.ClaimWork(f.ctx, tenant.DeidWorkKind, "probe", f.clock.Now().Add(tenant.RetryBase+time.Second), Lease)
	if err != nil || w.LastError != reasonInternal || w.Attempt < 2 {
		t.Fatalf("the unit after its keys failed: %+v, %v", w, err)
	}
	if got, _ := f.p.Job(f.ctx, j.ID); got.State.Ended() {
		t.Errorf("the job ended: %+v", got)
	}
}

// TestJobMeetsTheArchiveBetweenInstances guards a deid job over a scan from
// which a correction deletes an instance after the unit read its stream,
// before the unit reaches it: the instance is not read or judged, and is
// refused as deleted in the archive; every other instance is judged, and
// the counts add up to the instances counted.
func TestJobMeetsTheArchiveBetweenInstances(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.generatedSession(6)
	var uids []string
	for uid := range f.files {
		uids = append(uids, uid)
	}
	sort.Strings(uids)
	victim := uids[len(uids)-1]
	var seen recorded
	var once sync.Once
	cfg := withJudged(f.config(), func(u string) {
		seen.add(u)
		once.Do(func() {
			testkit.DeleteTarget(t, f.p, f.audit(), tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-gen", Scan: "gen",
				Instance: victim}, f.clock.Now())
		})
	})
	job := f.createDeid(tenant.DeidDryRun, ref, "ses-gen")
	s := f.start(cfg)
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	if _, judged := seen.n[victim]; judged {
		t.Errorf("the deleted instance %s was judged", victim)
	}
	got := f.failures(job.ID)[victim]
	if got.Code != tenant.FailureDeletedInArchive {
		t.Errorf("the deleted instance's failure is %+v, want deleted_in_archive", got)
	}
	c := j.Counts
	if c.Instances != 6 || c.InstancesRefused < 1 || c.InstancesRemaining() != 0 ||
		c.InstancesSent+c.InstancesWarned+c.InstancesRefused != 6 {
		t.Errorf("counts %+v, want the 6 instances counted, each answered once, the deleted one refused", c)
	}
	if n := f.tallies(job.ID)[tallyKey(tallyCheck, "deleted_in_archive")]; n != 1 {
		t.Errorf("tally of deleted instances is %d, want 1", n)
	}
}

// notServed is the installation's tenants with one that can stop being
// ready, as a tenant moved or fenced is.
type notServed struct {
	Tenants
	down atomic.Bool
}

// Store answers the database, or the refusal of a tenant not ready.
func (n *notServed) Store(name string) (tenant.Store, error) {
	if n.down.Load() {
		return nil, fault.New(fault.Unavailable, "the tenant is not ready")
	}
	return n.Tenants.Store(name)
}

// TestUnitStopsWhenItsTenantIsNoLongerServed guards a deid unit whose
// tenant stops being ready while it judges: it stops before its next
// instance and keeps its place, and when the tenant is served again judges
// the rest, no instance judged twice.
func TestUnitStopsWhenItsTenantIsNoLongerServed(t *testing.T) {
	f := newFixture(t)
	ref := f.storeVersion("research", basicProfile())
	f.generatedSession(20)
	tenants := &notServed{Tenants: &oneTenant{p: f.p}}
	var seen recorded
	stopped := make(chan struct{})
	var once sync.Once
	cfg := withJudged(f.config(), func(u string) {
		seen.add(u)
		if seen.count() == 5 {
			once.Do(func() {
				tenants.down.Store(true)
				close(stopped)
			})
		}
	})
	cfg.Tenants = tenants
	job := f.createDeid(tenant.DeidDryRun, ref, "ses-gen")
	s := f.start(cfg)
	<-stopped
	time.Sleep(300 * time.Millisecond)
	if n := seen.count(); n != 5 {
		t.Errorf("%d instances were judged, want the 5 before the tenant stopped being served", n)
	}
	tenants.down.Store(false)
	j := f.waitJob(s, job.ID, 60*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 20 {
		t.Fatalf("job %s %+v, want done with 20 judged", j.State, j.Counts)
	}
	for uid, n := range seen.n {
		if n != 1 {
			t.Errorf("instance %s was judged %d times, want once", uid, n)
		}
	}
}
