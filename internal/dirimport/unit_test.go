package dirimport

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestAUnitWhoseFolderCannotBeOpenedFailsForNow guards a unit whose folder
// cannot be opened when it starts (no permission here, as too many open
// files, an input or output error or a stale handle would be): the unit
// fails for now, spending an attempt, and never ends its walk with the
// folder's files uncounted. Once the folder can be read again, the next
// attempt imports it, and the job ends as an import never troubled would.
func TestAUnitWhoseFolderCannotBeOpenedFailsForNow(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() == 0 {
		t.Skip("a folder without permission is readable on this system")
	}
	e := newEnv(t, "")
	files := e.buildTree()
	j := e.createJob()
	e.settings.maxUnits.Store(1) // no unit runs before the listing unit has queued every folder
	clock := &offsetClock{}
	cfg := e.config()
	cfg.Now = clock.Now
	sessB := filepath.Join(e.tree, "proj1", "sessB")
	t.Cleanup(func() { _ = os.Chmod(sessB, 0o700) }) // #nosec G302 -- the test's own directory, given back.
	var failed atomic.Bool
	cfg.seams.ended = func(folder string, how end, reason string) {
		if folder == "proj1" && how == endWalked {
			// Queued by the listing, not yet walked: shut now.
			if err := os.Chmod(sessB, 0); err != nil {
				t.Error(err)
			}
		}
		if folder == "proj1/sessB" && how == endTemporary && reason == reasonUnreadable && failed.CompareAndSwap(false, true) {
			if err := os.Chmod(sessB, 0o700); err != nil { // #nosec G302 -- the test's own directory, given back.
				t.Error(err)
			}
			clock.skip(tenant.RetryDelay(1) + time.Second)
		}
	}
	s := e.start(cfg)
	ended := e.waitEnded(s, j.ID, importBound)
	if !failed.Load() {
		t.Error("the unit whose folder could not be opened did not fail for now with the reason " + reasonUnreadable)
	}
	e.checkImport(files, ended)
}

// TestAUnitWhoseFolderIsNowALinkIsSkipped guards a unit's folder replaced
// by a link after the listing queued it: it is skipped as not_regular and
// listed under its own path, so its loss is counted rather than silent,
// and nothing is read through the link.
func TestAUnitWhoseFolderIsNowALinkIsSkipped(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("making a link needs a privilege on Windows")
	}
	e := newEnv(t, "")
	e.buildTree()
	j := e.createJob()
	e.settings.maxUnits.Store(1)
	cfg := e.config()
	sessB := filepath.Join(e.tree, "proj1", "sessB")
	moved := filepath.Join(e.dataDir, "moved")
	cfg.seams.ended = func(folder string, how end, _ string) {
		if folder == "proj1" && how == endWalked {
			// Queued by the listing, not yet walked: swapped now.
			if err := os.Rename(sessB, moved); err != nil {
				t.Error(err)
			}
			if err := os.Symlink(moved, sessB); err != nil {
				t.Error(err)
			}
		}
	}
	s := e.start(cfg)
	ended := e.waitEnded(s, j.ID, importBound)
	c := ended.Import.Counts
	if msg := c.Reconcile(); msg != "" {
		t.Errorf("the counts do not reconcile: %s: %+v", msg, c)
	}
	// sessB's two files are not found; its folder is, once.
	if c.Found != 17 || c.Skipped[tenant.SkipNotRegular] != 2 || c.HandedIn != 9 {
		t.Errorf("counts %+v, want 17 found, 2 not regular (the link in sessA and sessB itself), 9 handed in", c)
	}
	if got := e.listedSkips(j.ID)["proj1/sessB"]; got != tenant.SkipNotRegular {
		t.Errorf("the folder now a link is listed as %q, want not_regular", got)
	}
	e.sink.mu.Lock()
	defer e.sink.mu.Unlock()
	if n := e.sink.taken["proj1/sessB/mr/MR_small.dcm"]; n != 0 {
		t.Error("a file was read through the link")
	}
}

// revocableRoot is the root "old", until the test takes it away: then the
// installation no longer allots it, as a changed import.allowed-roots says.
type revocableRoot struct {
	treeRoot
	revoked atomic.Bool
}

// Root answers the root, or its refusal once revoked.
func (r *revocableRoot) Root(ctx context.Context, tenantName string, st tenant.Store, name string) (string, error) {
	if r.revoked.Load() {
		return "", fault.New(fault.Precondition, "import.allowed-roots no longer allots the root "+name+" to the tenant")
	}
	return r.treeRoot.Root(ctx, tenantName, st, name)
}

// TestARootRevokedWhileAUnitRunsStopsIt guards the root checked again while
// a unit runs: the root taken away after the unit handed in its first file,
// the unit hands in nothing more, fails with the reason "root unavailable",
// and so does the job.
func TestARootRevokedWhileAUnitRunsStopsIt(t *testing.T) {
	e := newEnv(t, "")
	e.buildTree()
	j := e.createJob()
	e.settings.maxUnits.Store(1)
	root := &revocableRoot{treeRoot: treeRoot{path: e.tree}}
	cfg := e.config()
	cfg.Roots = root
	cfg.seams.rootCheck = time.Nanosecond // read again before every file handed in
	var mu sync.Mutex
	var failedFolders []string
	cfg.seams.judged = func(string) {
		e.sink.mu.Lock()
		handed := len(e.sink.taken)
		e.sink.mu.Unlock()
		if handed > 0 {
			root.revoked.Store(true)
		}
	}
	cfg.seams.ended = func(folder string, how end, reason string) {
		if how == endRoot && reason == tenant.ImportReasonRootUnavailable {
			mu.Lock()
			failedFolders = append(failedFolders, folder)
			mu.Unlock()
		}
	}
	s := e.start(cfg)
	ended := e.waitEnded(s, j.ID, importBound)
	if ended.State != tenant.JobFailed || ended.Reason != tenant.ImportReasonRootUnavailable {
		t.Errorf("job %s (%q), want failed with %q", ended.State, ended.Reason, tenant.ImportReasonRootUnavailable)
	}
	e.sink.mu.Lock()
	var first string
	for p := range e.sink.taken {
		first = p
	}
	if len(e.sink.taken) != 1 {
		t.Errorf("files handed in %v, want only the one before the root was taken away", e.sink.taken)
	}
	e.sink.mu.Unlock()
	mu.Lock()
	defer mu.Unlock()
	// The unit that handed the file in is the first to fail: in flight,
	// not at its next start.
	if segs := strings.SplitN(first, "/", 3); len(failedFolders) == 0 || len(segs) < 3 || failedFolders[0] != segs[0]+"/"+segs[1] {
		t.Errorf("units failed with the root's reason: %v, want first the unit of %q", failedFolders, first)
	}
}

// TestAStalledWalkFailsForNow guards the progress watchdog: a unit whose
// walk is held in a read of the file system that does not return stops
// renewing its lease and fails for now with an attempt spent, and another
// claim finishes the job from the cursor, no file handed in twice, the
// stalled walker writing nothing once it returns.
func TestAStalledWalkFailsForNow(t *testing.T) {
	e := newEnv(t, "")
	files := e.buildTree()
	j := e.createJob()
	e.settings.maxUnits.Store(1)
	clock := &offsetClock{}
	cfg := e.config()
	cfg.Now = clock.Now
	cfg.seams.renew = 100 * time.Millisecond
	cfg.seams.stall = time.Second
	block := make(chan struct{})
	var released sync.Once
	release := func() { released.Do(func() { close(block) }) }
	t.Cleanup(release)
	var held sync.Once
	cfg.seams.open = func(p string) {
		if p == "proj1/sessA/ct2/ct2n-6924.dcm" {
			held.Do(func() { <-block })
		}
	}
	var stalled atomic.Bool
	cfg.seams.ended = func(folder string, how end, reason string) {
		if folder == "proj1/sessA" && how == endTemporary && reason == reasonStalled && stalled.CompareAndSwap(false, true) {
			release()
			clock.skip(tenant.RetryDelay(1) + time.Second)
		}
	}
	s := e.start(cfg)
	ended := e.waitEnded(s, j.ID, importBound)
	if !stalled.Load() {
		t.Error("the stalled unit did not fail for now with the reason " + reasonStalled)
	}
	e.checkImport(files, ended)
	if twice := e.sink.twice(); len(twice) > 0 {
		t.Errorf("files handed in more than once: %v", twice)
	}
}

// refusingSink is a pipeline that fails every file for a cause of its own.
type refusingSink struct{}

// refusingReason is the cause refusingSink gives.
const refusingReason = "the file's sender is not described in a form the archive keeps"

// Accept fails the file.
func (refusingSink) Accept(context.Context, receive.Arrival) receive.Outcome {
	return receive.Outcome{Status: receive.Failed, Reason: refusingReason}
}

// TestAFileFailedByThePipelineSpendsAnAttempt guards a hand-in the pipeline
// fails for a cause that is neither the file's size nor the job's end: the
// unit fails for now with that reason, spending an attempt, so a cause that
// stays fails the job rather than holds it forever.
func TestAFileFailedByThePipelineSpendsAnAttempt(t *testing.T) {
	e := newEnv(t, "")
	e.writeFiles(map[string]string{"proj1/sessA/ct2/ct2n-6293.dcm": "ct2n-6293.dcm"})
	e.createJob()
	cfg := e.config()
	cfg.Sink = func(string) receive.Sink { return refusingSink{} }
	got := make(chan end, 1)
	cfg.seams.ended = func(folder string, how end, reason string) {
		if folder == "proj1/sessA" && reason == refusingReason {
			select {
			case got <- how:
			default:
			}
		}
	}
	e.start(cfg)
	select {
	case how := <-got:
		if how != endTemporary {
			t.Errorf("the unit ended %d, want endTemporary (%d): an attempt spent", how, endTemporary)
		}
	case <-time.After(importBound):
		t.Fatal("the unit whose file the pipeline failed did not end")
	}
}

// TestAUnitUsesOnlyTheMapItsJobKept guards where a unit's map comes from:
// the map the job kept when it was made, never the map file read again, so
// a file changed or swapped since changes nothing; a job that names a map
// file, its own or its receiver's, and kept no map is refused.
func TestAUnitUsesOnlyTheMapItsJobKept(t *testing.T) {
	rcv := testReceiver()
	layout, err := rcv.Import.ParsedLayout()
	if err != nil {
		t.Fatal(err)
	}
	fromReceiver := rcv
	fromReceiver.Import.MapFile = "proj1/map.csv"
	for _, c := range []struct {
		name string
		rcv  tenant.Receiver
		spec tenant.ImportJob
		ok   bool
	}{
		{"an inline map", rcv, tenant.ImportJob{MapCSV: []byte(testMap)}, true},
		{"a job's map file, kept", rcv, tenant.ImportJob{MapCSV: []byte(testMap), MapFile: "proj1/not-there.csv"}, true},
		{"a receiver's map file, kept", fromReceiver, tenant.ImportJob{MapCSV: []byte(testMap)}, true},
		{"a job's map file, not kept", rcv, tenant.ImportJob{MapFile: "proj1/map.csv"}, false},
		{"a receiver's map file, not kept", fromReceiver, tenant.ImportJob{}, false},
	} {
		r, err := storedResolver(c.rcv, c.spec, layout)
		switch {
		case c.ok && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.ok:
			parts, why := r.Resolve("proj1/sessA/ct2/x.dcm")
			if why != importpath.NoSkip || parts["subject"] != "subj1" {
				t.Errorf("%s: the kept map gave %v (%v), want subj1", c.name, parts, why)
			}
		case !errors.Is(err, errMapNotKept):
			t.Errorf("%s: %v, want the refusal of a map not kept", c.name, err)
		}
	}
}
