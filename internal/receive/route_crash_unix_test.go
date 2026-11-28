//go:build unix

package receive

import (
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// crashRouteEnv names the data directory of TestRouteCrash's child.
const crashRouteEnv = "NOPACS_RECEIVE_CRASH_ROUTE"

// crashUnmatchedEnv, when set, makes the child's first run hold what no
// route chooses, and route nothing.
const crashUnmatchedEnv = "NOPACS_RECEIVE_CRASH_UNMATCHED"

// routeTo is a rule set that sends what comes from the calling AE title
// SCANNER to the storage given, by an archive rule of that storage's name.
func routeTo(storage string) *tenant.RuleSet {
	return &tenant.RuleSet{Archive: []tenant.ArchiveRule{{Name: "to-" + storage, Storage: storage, When: `calling_ae == "SCANNER"`}}}
}

// TestRouteCrashChild is the child of TestRouteCrash. Its first run routes
// SCANNER's studies to storage a, hands in two files and is killed with
// signal 9 at the point it is told; its second routes them to storage b
// instead and runs until every reception has ended. Run alone, it does
// nothing.
func TestRouteCrashChild(t *testing.T) {
	dataDir := os.Getenv(crashRouteEnv)
	if dataDir == "" {
		t.Skip("the child of TestRouteCrash")
	}
	run, _ := strconv.Atoi(os.Getenv(crashRunEnv))
	point := os.Getenv(crashPointEnv)
	if run == 1 {
		first := routeTo("a")
		if os.Getenv(crashUnmatchedEnv) != "" {
			first.Archive[0].When = `calling_ae == "NOBODY"`
			first.QuarantineUnmatched = true
		}
		e := newEnvAt(t, dataDir, t0.Add(time.Hour), envOptions{routing: first, hook: func(p string) {
			if p == point {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}})
		e.sendAs("SCANNER", studyFiles[:2]...)
		e.waitFor("the kill at "+point, 3*time.Second, func() bool { return false })
		return
	}
	e := newEnvAt(t, dataDir, t0.Add(2*time.Hour), envOptions{routing: routeTo("b")})
	e.waitFor("every reception to end", 3*time.Second, func() bool {
		rs := e.receptions()
		for _, r := range rs {
			if !r.State.Ended() {
				return false
			}
		}
		return len(rs) > 0 && e.p.batch.idle()
	})
}

// TestRouteCrash guards that what a reception's routing chose is kept with
// its identify step: a process killed with signal 9 after the step, or in
// the archive step after it, then started again with routes that now send
// the study elsewhere, archives the study where the routes sent it when the
// step was taken; one killed before the step takes it again under the
// routes it then has, since nothing was chosen yet. Either way every file
// is archived once, and the catalog and the reception agree. A study held
// as unrouted is never split: killed after its holds, the next run holds
// the rest, though the routes now route it.
func TestRouteCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	for _, c := range []struct {
		point, want string
		unmatched   bool
	}{
		{"identify.before-step", "b", false}, {"identify.after-step", "a", false}, {"archive.before-put", "a", false},
		{"archive.after-commit", "a", false},
		// What no route chooses is held: killed before the first hold,
		// nothing was decided and the next run routes the study by the
		// routes it has then; killed after the holds, the next run
		// finishes the hold, whatever the routes say by then.
		{"identify.before-unrouted", "b", true}, {"step.before-quarantine", "", true},
	} {
		point, want := c.point, c.want
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			for run := 1; run <= 2; run++ {
				cmd := exec.Command(os.Args[0], "-test.run=^TestRouteCrashChild$", "-test.count=1") // #nosec G204 G702 -- the test binary itself.
				cmd.Env = append(os.Environ(), crashRouteEnv+"="+dataDir, crashRunEnv+"="+strconv.Itoa(run), crashPointEnv+"="+point)
				if c.unmatched {
					cmd.Env = append(cmd.Env, crashUnmatchedEnv+"=1")
				}
				out, err := cmd.CombinedOutput()
				var ee *exec.ExitError
				killed := errors.As(err, &ee) && ee.Sys().(syscall.WaitStatus).Signaled() && ee.Sys().(syscall.WaitStatus).Signal() == syscall.SIGKILL
				switch {
				case run == 1 && !killed:
					t.Fatalf("the first run was not killed at %s: %v\n%s", point, err, tail(string(out)))
				case run == 2 && err != nil:
					t.Fatalf("the second run failed: %v\n%s", err, tail(string(out)))
				}
			}
			s, err := (driver.SQLiteTenants{DataDir: dataDir}).Open(t.Context(), "T0000000000001", "lab", "")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Close() }()
			store := s.(tenant.Pipeline)
			rs, _, err := store.ListReceptions(t.Context(), tenant.ReceptionFilter{}, master.Page{Limit: 10})
			if err != nil || len(rs) != 1 {
				t.Fatalf("receptions: %d (%v), want 1", len(rs), err)
			}
			r := rs[0]
			if want == "" {
				qs, _, err := store.ListQuarantined(t.Context(), tenant.QuarantineFilter{ReceptionID: r.ID}, master.Page{Limit: 10})
				// Never identified: the hold was finished as first chosen, not
				// routed again (which, for a study held in several batches,
				// would archive the files not yet held).
				if err != nil || r.State != reception.Quarantined || r.Counts.FilesArchived != 0 || len(qs) != 2 || r.Storage != "" || !r.Route.IsZero() {
					t.Fatalf("after a kill at %s: %s with %+v, %d held (%v); want quarantined, never identified, nothing archived, both held (storage %q, route %+v)", point, r.State, r.Counts, len(qs), err, r.Storage, r.Route)
				}
				for _, q := range qs {
					if q.Reason != tenant.QuarantineUnrouted || q.Details["cause"] != "no_match" {
						t.Errorf("a file held as %s %v", q.Reason, q.Details)
					}
				}
				return
			}
			if !done(r) || r.Storage != want || r.Route.Route != "to-"+want || r.Counts.FilesArchived != 2 {
				t.Errorf("after a kill at %s: %s on %s by %+v with %+v, want done on %s by to-%s with 2 archived",
					point, r.State, r.Storage, r.Route, r.Counts, want, want)
			}
			c, err := store.CatalogLabels(t.Context(), r.StudyUID, nil)
			if err != nil || c.Session == nil || c.Session.Storage != want {
				t.Errorf("the session after a kill at %s: %+v (%v), want on %s", point, c.Session, err, want)
			}
		})
	}
}
