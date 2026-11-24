//go:build unix

package receive

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The crash child's environment.
const (
	crashDirEnv    = "NOPACS_RECEIVE_CRASH_DIR"
	crashPointEnv  = "NOPACS_RECEIVE_CRASH_POINT"
	crashHitEnv    = "NOPACS_RECEIVE_CRASH_HIT"
	crashRunEnv    = "NOPACS_RECEIVE_CRASH_RUN"
	crashResendEnv = "NOPACS_RECEIVE_CRASH_RESEND"
	// crashDeidEnv, when set, makes the child's receiver de-identify
	// (deid_crash_unix_test.go).
	crashDeidEnv = "NOPACS_RECEIVE_CRASH_DEID"
)

// crashPoints are the points, before and after every durable effect of
// every step, a child is killed at.
var crashPoints = []string{
	"accept.landed",
	"batch.before-hold", "batch.after-hold", "batch.before-add", "batch.after-add",
	"quiet.before-step", "quiet.after-step",
	"identify.before-step", "identify.after-step",
	"archive.before-claim", "archive.after-claim", "archive.before-put", "archive.after-put",
	"archive.before-readback", "archive.after-readback",
	"archive.before-sidecar", "archive.before-manifest", "archive.after-manifest",
	"archive.before-commit", "archive.after-commit", "archive.before-step", "archive.after-step",
	"step.before-hold",
	"verify.before-check", "verify.before-stat", "verify.before-step", "verify.after-step", "verify.before-remove", "verify.before-finish", "verify.after-finish",
	"step.done:" + kindVerify,
}

// crashSend are the files a crash child hands in: a study of two series, a
// file of another study, a file of a third under the first one's SOP
// Instance UID with other bytes, and a file that is not DICOM.
var crashSend = append(append([]string{}, studyFiles...), "MR_small.dcm", "MR_small_RLE.dcm", "not-dicom")

// crashBytes returns the bytes of a file crashSend names.
func crashBytes(t *testing.T, name string) []byte {
	if name == "not-dicom" {
		return []byte("plain text that a sender called a DICOM file\n")
	}
	return corpus(t, name)
}

// TestReceiveCrashChild is the child process of TestCrashMatrix. In its
// first run it hands in files, printing each one acknowledged, and is killed
// with signal 9 at the point it was told; in its second it hands in what was
// not acknowledged, as a sender would, and runs until every reception has
// ended. Run alone, it does nothing.
func TestReceiveCrashChild(t *testing.T) {
	dataDir := os.Getenv(crashDirEnv)
	if dataDir == "" {
		t.Skip("the child of the crash matrix; run by TestCrashMatrix")
	}
	run, _ := strconv.Atoi(os.Getenv(crashRunEnv))
	point := os.Getenv(crashPointEnv)
	hit, _ := strconv.Atoi(os.Getenv(crashHitEnv))
	hits := 0
	var kill func(string)
	if point != "" {
		kill = func(p string) {
			if p != point {
				return
			}
			hits++
			if hits == hit {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		}
	}
	opts := envOptions{hook: kill}
	if os.Getenv(crashDeidEnv) != "" {
		opts.deid = &deidOptions{}
	}
	e := newEnvAt(t, dataDir, t0.Add(time.Duration(run)*time.Hour), opts)
	out := bufio.NewWriter(os.Stdout)
	names := crashSend
	if run > 1 {
		names = nil
		if s := os.Getenv(crashResendEnv); s != "" {
			names = strings.Split(s, ",")
		}
	}
	for _, n := range names {
		if e.sendBytes(crashBytes(t, n), Received).Status == Received {
			fmt.Fprintf(out, "ACK %s\n", n)
			_ = out.Flush()
		}
	}
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

// TestCrashMatrix guards the pipeline's order of effects: a process killed
// with signal 9 before and after every durable effect of every step, on
// several hits of each, then started again with the files it did not
// acknowledge sent again, leaves every invariant holding: each file it
// acknowledged archived exactly once or held, no claim left by an ended
// reception, the incoming area holding only what quarantine keeps, every
// reception's counts adding up, and every object on the storage explained
// by the catalog.
func TestCrashMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	hits := []int{1, 2, 5}
	if raceEnabled {
		hits = []int{1}
	}
	points := crashPoints
	if only := os.Getenv("NOPACS_RECEIVE_CRASH_ONLY"); only != "" {
		points = strings.Split(only, ",")
	}
	// Each round has its own data directory and its own two child
	// processes, which open no port and share nothing but the read-only
	// corpus, so rounds run side by side, a few at a time: every child
	// fsyncs, and more of them than cores only measures the disk.
	slots := make(chan struct{}, min(runtime.GOMAXPROCS(0), 4))
	var rounds, killed atomic.Int64
	t.Run("rounds", func(t *testing.T) {
		for _, point := range points {
			for _, hit := range hits {
				rounds.Add(1)
				t.Run(fmt.Sprintf("%s/hit%d", point, hit), func(t *testing.T) {
					t.Parallel()
					slots <- struct{}{}
					defer func() { <-slots }()
					dataDir := t.TempDir()
					stdout, wasKilled := runChild(t, dataDir, 1, point, hit, "")
					if wasKilled {
						killed.Add(1)
					}
					acked := map[string]bool{}
					for _, line := range strings.Split(stdout, "\n") {
						if n, ok := strings.CutPrefix(line, "ACK "); ok {
							acked[n] = true
						}
					}
					var resend []string
					for _, n := range crashSend {
						if !acked[n] {
							resend = append(resend, n)
						}
					}
					if _, k := runChild(t, dataDir, 2, "", 0, strings.Join(resend, ",")); k {
						t.Fatalf("%s hit %d: the second run was killed", point, hit)
					}
					checkInvariants(t, fmt.Sprintf("%s hit %d", point, hit), dataDir)
				})
			}
		}
	})
	t.Logf("%d rounds, %d of them killed at their point", rounds.Load(), killed.Load())
}

// runChild runs the crash child and returns its output and whether it was
// killed.
// extra is more of the child's environment, each NAME=value.
func runChild(t *testing.T, dataDir string, run int, point string, hit int, resend string, extra ...string) (string, bool) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReceiveCrashChild$", "-test.count=1") // #nosec G204 G702 -- the test binary itself.
	cmd.Env = append(os.Environ(), crashDirEnv+"="+dataDir, crashRunEnv+"="+strconv.Itoa(run),
		crashPointEnv+"="+point, crashHitEnv+"="+strconv.Itoa(hit), crashResendEnv+"="+resend)
	cmd.Env = append(cmd.Env, extra...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err == nil {
		return stdout.String(), false
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() && ws.Signal() == syscall.SIGKILL {
			return stdout.String(), true
		}
	}
	t.Fatalf("child run %d (%s %d) failed: %v\n%s\n%s", run, point, hit, err, stdout.String(), tail(stderr.String()))
	return "", false
}

// tail returns the end of a long text.
func tail(s string) string {
	if len(s) > 4000 {
		return s[len(s)-4000:]
	}
	return s
}

// checkInvariants opens what the children left and checks every invariant.
func checkInvariants(t *testing.T, round, dataDir string) {
	t.Helper()
	ctx := t.Context()
	s, err := (driver.SQLiteTenants{DataDir: dataDir}).Open(ctx, "T0000000000001", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	store := s.(tenant.Pipeline)
	storage, err := dir.Open(filepath.Join(dataDir, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()
	fail := func(format string, args ...any) {
		t.Helper()
		t.Errorf(round+": "+format, args...)
	}

	// Every reception ended, none failed, each one's counts add up.
	page := master.Page{Limit: 1000}
	rs, _, err := store.ListReceptions(ctx, tenant.ReceptionFilter{}, page)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		c := r.Counts
		switch {
		case !r.State.Ended():
			fail("reception %s is %s", r.ID, r.State)
		case r.State == reception.Failed:
			fail("reception %s failed: %s", r.ID, r.Failure)
		case c.FilesReceived != c.FilesArchived+c.FilesDuplicate+c.FilesQuarantined:
			fail("reception %s counts do not add up: %+v", r.ID, c)
		}
	}

	// Each DICOM file sent is archived exactly once or held; no claim is
	// left.
	held, _, err := store.ListQuarantined(ctx, tenant.QuarantineFilter{}, page)
	if err != nil {
		t.Fatal(err)
	}
	heldUID := map[string]int{}
	notDICOM := 0
	keptPaths := map[string]bool{}
	for _, q := range held {
		heldUID[q.SOPInstanceUID]++
		if q.Reason == tenant.QuarantineNotDICOM {
			notDICOM++
		}
		if q.State == tenant.QuarantineHeld && q.ContentKept {
			keptPaths[q.Incoming] = true
		}
	}
	if notDICOM == 0 {
		fail("the file that is not DICOM is not held")
	}
	archived := 0
	archivedSum := map[string][32]byte{}
	for _, n := range crashSend {
		if n == "not-dicom" {
			continue
		}
		b := corpus(t, n)
		h, err := header.Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		u, _ := h.UID(header.SOPInstanceUID)
		st, _ := h.UID(header.StudyUID)
		se, _ := h.UID(header.SeriesUID)
		ds, err := store.CheckDuplicates(ctx, []tenant.DuplicateQuery{{SOPInstanceUID: u, StudyUID: st, SeriesUID: se, SHA256: sha256.Sum256(b)}})
		if err != nil {
			t.Fatal(err)
		}
		switch ds[0].Kind {
		case tenant.SameFile:
			archived++
			archivedSum[u] = sha256.Sum256(b)
		case tenant.DifferentFile:
			if heldUID[u] == 0 {
				fail("%s differs from the instance held under its UID and is not held", n)
			}
		case tenant.Claimed:
			fail("%s: a claim was left behind", n)
		default:
			fail("%s is neither archived nor held: %v", n, ds[0].Kind)
		}
	}
	totals, err := store.CatalogTotals(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if archived != 8 || totals.Instances != 8 {
		fail("%d files archived, catalog holds %d instances; want 8 and 8", archived, totals.Instances)
	}

	// The incoming area holds only what quarantine keeps.
	area, err := incoming.Open(filepath.Join(dataDir, "incoming"), "lab")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = area.Close() }()
	if _, err := area.Scan(func(en incoming.Entry) error {
		if en.State != incoming.Recorded || !keptPaths[en.Path] {
			fail("the incoming area holds %s (%v), which nothing keeps", en.ID, en.State)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Every object on the storage is explained by the catalog: an
	// instance it holds with the object's checksum.
	var token string
	onStorage := map[string]bool{}
	for {
		pg, err := storage.List(ctx, blob.ListOptions{Prefix: "data/", Token: token})
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range pg.Keys {
			if !strings.HasSuffix(k, ".dcm") {
				continue
			}
			u := strings.TrimSuffix(k[strings.LastIndexByte(k, '/')+1:], ".dcm")
			want, ok := archivedSum[u]
			if !ok {
				fail("object %s is not an instance the catalog holds", k)
				continue
			}
			onStorage[u] = true
			rc, err := storage.Read(ctx, k, 0, -1)
			if err != nil {
				t.Fatal(err)
			}
			h := sha256.New()
			_, _ = bufio.NewReader(rc).WriteTo(h)
			_ = rc.Close()
			if [32]byte(h.Sum(nil)) != want {
				fail("object %s is not the bytes the catalog holds", k)
			}
		}
		if pg.Next == "" {
			break
		}
		token = pg.Next
	}
	for u := range archivedSum {
		if !onStorage[u] {
			fail("instance %s is in the catalog and not on the storage", u)
		}
	}
}
