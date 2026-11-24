package tenanttest

import (
	"errors"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The cases of jobs that need true concurrency on a database with several
// writers (Postgres); on SQLite's one writer they pass by serialization.

// workAtOnce works each unit on its own goroutine, each through its store,
// recording every instance in a write of its own, and returns the errors.
func (e *senv) workAtOnce(units []tenant.Work, stores []tenant.Pipeline, now time.Time) []error {
	errs := make([]error, len(units))
	var wg sync.WaitGroup
	begin := make(chan struct{})
	for i, w := range units {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-begin
			s := stores[i%len(stores)]
			if _, err := s.StartJobUnit(e.ctx, w.Fence(), now); err != nil {
				errs[i] = err
				return
			}
			ins, err := s.JobScanInstances(e.ctx, w.Fence(), "", tenant.MaxJobInstancePage)
			if err != nil {
				errs[i] = err
				return
			}
			cursor := ""
			for k := range ins {
				p := progress(w, cursor, ins[k:k+1], k, allSent, now)
				if _, err := s.RecordJobProgress(e.ctx, p); err != nil {
					errs[i] = err
					return
				}
				cursor = p.To
			}
			_, errs[i] = s.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), From: cursor, To: cursor,
				FailuresKept: tenant.DefaultFailuresKept, Now: now})
		}()
	}
	close(begin)
	wg.Wait()
	return errs
}

// testJobUnitsAtOnce guards two workers on two units of one job at once:
// each instance is counted once, neither worker's counts are lost, and the
// job ends done when the later of them ends.
func testJobUnitsAtOnce(e *senv) {
	_, target := e.setup("orthanc", 2, 40)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	units := []tenant.Work{e.claim("orthanc", "a", at(3700)), e.claim("orthanc", "b", at(3700))}
	for i, err := range e.workAtOnce(units, []tenant.Pipeline{e.p}, at(3700)) {
		e.ok("worker "+string(rune('a'+i)), err)
	}
	j, err := e.p.Job(e.ctx, "j1")
	e.ok("Job", err)
	e.wantCounts("a job worked by two at once", j, 80, 80, 0, 0, 2, 2, true)
	if j.State != tenant.JobDone {
		e.Errorf("the job ended %s, want done", j.State)
	}
}

// testUnitsShared is testJobUnitsAtOnce across two instances on one
// database, as two processes would work one job.
func testUnitsShared(e *senv, other EventStore) {
	_, target := e.setup("orthanc", 2, 30)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	units := []tenant.Work{e.claim("orthanc", "a", at(3700)), e.claim("orthanc", "b", at(3700))}
	for _, err := range e.workAtOnce(units, []tenant.Pipeline{e.p, other}, at(3700)) {
		e.ok("worker", err)
	}
	j, err := other.Job(e.ctx, "j1")
	e.ok("Job", err)
	e.wantCounts("a job worked across two instances", j, 60, 60, 0, 0, 2, 2, true)
}

// testCancelRacesUnit guards a cancel landing while a unit writes: the job
// ends canceled, every write that succeeded is counted exactly, the worker
// is told at its next write, and its unit then ends without changing the
// job's state.
func testCancelRacesUnit(e *senv) {
	_, target := e.setup("orthanc", 1, 60)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	w := e.claim("orthanc", "w", at(3700))
	_, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(3700))
	e.ok("StartJobUnit", err)
	ins := e.instances(w, "")
	var recorded int64
	var told bool
	cursor := ""
	done := make(chan error, 1)
	go func() {
		for k := range ins {
			p := progress(w, cursor, ins[k:k+1], k, allSent, at(3710))
			j, err := e.p.RecordJobProgress(e.ctx, p)
			if err != nil {
				done <- err
				return
			}
			recorded++
			cursor = p.To
			if j.State == tenant.JobCanceled {
				told = true
				break
			}
		}
		_, err := e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), From: cursor, To: cursor,
			FailuresKept: tenant.DefaultFailuresKept, Now: at(3720)})
		done <- err
	}()
	time.Sleep(5 * time.Millisecond)
	if _, err := e.p.CancelJob(e.ctx, "j1", at(3715), e.audit(), nil); err != nil {
		e.Fatalf("CancelJob: %v", err)
	}
	e.ok("the worker", <-done)
	j, err := e.p.Job(e.ctx, "j1")
	e.ok("Job", err)
	if j.State != tenant.JobCanceled && j.State != tenant.JobDone {
		e.Errorf("a job canceled while a unit wrote ended %s", j.State)
	}
	if j.Counts.InstancesSent != recorded || j.Counts.ScansDone != 1 {
		e.Errorf("the job counts %d sent and %d scans done; the worker recorded %d", j.Counts.InstancesSent, j.Counts.ScansDone, recorded)
	}
	if j.State == tenant.JobCanceled && recorded < int64(len(ins)) && !told {
		e.Error("the worker was not told of the cancel at its next write")
	}
}

// testCrashResumes guards a crash midway: the unit is taken over once its
// lease ends, goes on after the last instance recorded, and at most the
// instances sent since that record (one batch) reach the peer twice; the
// lease that crashed can write nothing after.
func testCrashResumes(e *senv) {
	_, target := e.setup("orthanc", 1, 10)
	e.createJob("j1", "orthanc", target, at(3600))
	e.list("orthanc", at(3600))
	const batch = 3
	var peer []string // every instance the peer received, repeats included
	a := e.claim("orthanc", "a", at(3700))
	_, err := e.p.StartJobUnit(e.ctx, a.Fence(), at(3700))
	e.ok("StartJobUnit", err)
	ins := e.instances(a, "")
	recorded := progress(a, "", ins[:batch], 0, allSent, at(3701))
	peer = append(peer, uidsOf(ins[:batch])...)
	_, err = e.p.RecordJobProgress(e.ctx, recorded)
	e.ok("RecordJobProgress", err)
	peer = append(peer, uidsOf(ins[batch:batch+2])...) // sent, then the process dies before recording them
	b := e.claim("orthanc", "b", at(3800))
	u, err := e.p.StartJobUnit(e.ctx, b.Fence(), at(3800))
	e.ok("StartJobUnit after the crash", err)
	rest := e.instances(b, u.Scan.Cursor)
	peer = append(peer, uidsOf(rest)...)
	_, err = e.p.RecordJobProgress(e.ctx, progress(a, recorded.To, ins[batch:batch+2], batch, allSent, at(3801)))
	if !errors.Is(err, tenant.ErrLeaseLost) {
		e.Errorf("the crashed lease wrote after the takeover: %v", err)
	}
	j := e.sendScan(b, batch, allSent, at(3810))
	counts := map[string]int{}
	twice := 0
	for _, uid := range peer {
		counts[uid]++
		if counts[uid] == 2 {
			twice++
		}
	}
	if len(counts) != 10 || twice > batch {
		e.Errorf("the peer holds %d instances, %d received twice; want all 10 and at most %d twice", len(counts), twice, batch)
	}
	e.wantCounts("a job resumed after a crash", j, 10, 10, 0, 0, 1, 1, true)
}
