//go:build unix

package receive

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
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

// deidCrashPoints are the points of the archive step around the
// de-identified copy, its put and its commit, and of the verify step's
// removals, a child of a receiver that de-identifies is killed at.
var deidCrashPoints = []string{
	"batch.before-add", "batch.after-add",
	"archive.after-claim", "archive.before-deid", "archive.after-deid", "archive.before-put", "archive.after-put",
	"archive.after-readback", "archive.before-sidecar", "archive.before-commit", "archive.after-commit",
	"step.before-hold", "verify.before-remove", "verify.before-finish",
}

// TestDeidCrashMatrix guards de-identification at reception against a
// crash: a process of a receiver that de-identifies, killed with signal 9
// before and after each effect around the de-identified copy, then started
// again with what it did not acknowledge sent again, leaves every file it
// acknowledged archived once de-identified or held, the database holding
// only remapped UIDs and keyed checksums, every object on the storage
// de-identified and explained by the catalog, and the incoming area
// holding nothing but what quarantine keeps: no de-identified copy left.
func TestDeidCrashMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	for _, point := range deidCrashPoints {
		t.Run(point, func(t *testing.T) {
			t.Parallel()
			dataDir := t.TempDir()
			stdout, killed := runChild(t, dataDir, 1, point, 1, "", crashDeidEnv+"=1")
			t.Logf("killed at %s: %v", point, killed)
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
			if _, k := runChild(t, dataDir, 2, "", 0, strings.Join(resend, ","), crashDeidEnv+"=1"); k {
				t.Fatalf("%s: the second run was killed", point)
			}
			checkDeidInvariants(t, point, dataDir)
		})
	}
}

// checkDeidInvariants opens what the children of a receiver that
// de-identifies left and checks every invariant.
func checkDeidInvariants(t *testing.T, round, dataDir string) {
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
	held, _, err := store.ListQuarantined(ctx, tenant.QuarantineFilter{}, page)
	if err != nil {
		t.Fatal(err)
	}
	heldUID := map[string]int{}
	keptPaths := map[string]bool{}
	for _, q := range held {
		heldUID[q.SOPInstanceUID]++
		if q.State == tenant.QuarantineHeld && q.ContentKept {
			keptPaths[q.Incoming] = true
		}
	}
	archived := map[string]bool{}
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
		if heldUID[u] > 0 {
			fail("%s is held under its original UID", n)
		}
		q := tenant.DuplicateQuery{SOPInstanceUID: remapped(u), StudyUID: remapped(st), SeriesUID: remapped(se),
			SHA256: keyedSum(testDeidKeys, sha256.Sum256(b))}
		ds, err := store.CheckDuplicates(ctx, []tenant.DuplicateQuery{q})
		if err != nil {
			t.Fatal(err)
		}
		switch ds[0].Kind {
		case tenant.SameFile:
			archived[remapped(u)] = true
		case tenant.DifferentFile:
			if heldUID[remapped(u)] == 0 {
				fail("%s differs from the instance held under its UID and is not held", n)
			}
		default:
			fail("%s is neither archived nor held: %v", n, ds[0].Kind)
		}
	}
	if len(archived) != 8 {
		fail("%d instances archived, want 8", len(archived))
	}
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
	pg, err := storage.List(ctx, blob.ListOptions{Prefix: "data/"})
	if err != nil {
		t.Fatal(err)
	}
	onStorage := 0
	for _, k := range pg.Keys {
		if !strings.HasSuffix(k, ".dcm") {
			continue
		}
		u := strings.TrimSuffix(k[strings.LastIndexByte(k, '/')+1:], ".dcm")
		if !archived[u] {
			fail("object %s is not an instance the catalog holds", k)
			continue
		}
		onStorage++
		obj := readAll(t, storage, k)
		h, err := header.Read(bytes.NewReader(obj), int64(len(obj)))
		if err != nil {
			fail("object %s does not read: %v", k, err)
			continue
		}
		if got, _ := h.UID(header.SOPInstanceUID); got != u {
			fail("object %s holds the instance %s", k, got)
		}
		for _, o := range []string{"Doe^Peter", "CompressedSamples"} {
			if bytes.Contains(obj, []byte(o)) {
				fail("object %s holds the patient's name", k)
			}
		}
	}
	if onStorage != len(archived) {
		fail("%d objects on the storage for %d instances", onStorage, len(archived))
	}
}

// readAll reads a whole object.
func readAll(t *testing.T, s blob.Store, key string) []byte {
	t.Helper()
	rc, err := s.Read(t.Context(), key, 0, -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// crashReleaseEnv, when set, makes the crash child the one of
// TestDeidReleaseCrash.
const crashReleaseEnv = "NOPACS_RECEIVE_CRASH_RELEASE"

// releaseCrashEnv is the env of TestDeidReleaseCrash: dimse archives as
// received and takes only SOP class 1.2.3.4, so a CT file it receives is
// held; deidb de-identifies.
func releaseCrashEnv(t *testing.T, dataDir string, run int, hook func(string)) *env {
	t.Helper()
	opts := envOptions{deid: &deidOptions{asReceived: true}, hook: hook, sopClasses: []string{"1.2.3.4"}}
	if run == 2 {
		// The first receiver takes every class now, so a file checked by
		// it would be recorded and archived as received: started only
		// once its list is cleared.
		opts.manual = true
	}
	e := newEnvAt(t, dataDir, t0.Add(time.Duration(run)*time.Hour), opts)
	if run == 2 {
		if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDIMSESOPClasses}, t0, e.audit(), nil); err != nil {
			t.Fatal(err)
		}
		_ = e.p.area.Close()
		e.manual = false
		e.start()
	}
	if run == 1 {
		if _, err := e.store.AddReceiver(e.ctx, tenant.Receiver{
			Name: "deidb", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main", Profile: testProfile,
			QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
			DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "DEIDB", AnyCaller: true},
		}, t0, e.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

// TestDeidReleaseCrashChild is the child of TestDeidReleaseCrash. Its
// first run holds a CT file at dimse, then releases it through deidb and
// is killed with signal 9 when the store has recorded the release
// (release.after-store), before the file is checked again; its second run,
// with dimse now taking every class, runs until every reception has ended.
// Run alone, it does nothing.
func TestDeidReleaseCrashChild(t *testing.T) {
	dataDir := os.Getenv(crashReleaseEnv)
	if dataDir == "" {
		t.Skip("the child of TestDeidReleaseCrash")
	}
	run, _ := strconv.Atoi(os.Getenv(crashRunEnv))
	if run == 1 {
		e := releaseCrashEnv(t, dataDir, 1, func(p string) {
			if p == "release.after-store" {
				_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
				select {}
			}
		})
		e.clock.Advance(2 * time.Second)
		e.send(studyFiles[0])
		e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
		q := e.held()[0]
		_, _ = e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, Receiver: "deidb"}, e.audit(), nil)
		t.Fatal("the release was not killed at release.after-store")
	}
	e := releaseCrashEnv(t, dataDir, 2, nil)
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

// TestDeidReleaseCrash guards a release through a receiver that
// de-identifies against a crash between the store's release and the
// check of the file again: the process killed with signal 9 at
// release.after-store, then started again, archives the file through the
// receiver the release named, de-identified (its reception at deidb,
// holding the remapped study), and no object on the storage holds the
// patient's name.
func TestDeidReleaseCrash(t *testing.T) {
	if testing.Short() {
		t.Skip("starts child processes")
	}
	dataDir := t.TempDir()
	for run := 1; run <= 2; run++ {
		cmd := exec.Command(os.Args[0], "-test.run=^TestDeidReleaseCrashChild$", "-test.count=1") // #nosec G204 G702 -- the test binary itself.
		cmd.Env = append(os.Environ(), crashReleaseEnv+"="+dataDir, crashRunEnv+"="+strconv.Itoa(run))
		out, err := cmd.CombinedOutput()
		var ee *exec.ExitError
		killed := errors.As(err, &ee) && ee.Sys().(syscall.WaitStatus).Signaled() && ee.Sys().(syscall.WaitStatus).Signal() == syscall.SIGKILL
		switch {
		case run == 1 && !killed:
			t.Fatalf("the first run was not killed at its point: %v\n%s", err, tail(string(out)))
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
	if err != nil {
		t.Fatal(err)
	}
	study, _, _ := fileUIDs(t, corpus(t, studyFiles[0]))
	if len(rs) != 1 || rs[0].Receiver != "deidb" || rs[0].State != reception.Done || rs[0].StudyUID != remapped(study) {
		t.Fatalf("receptions %+v; want one at deidb, done, of the remapped study", rs)
	}
	storage, err := dir.Open(filepath.Join(dataDir, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = storage.Close() }()
	pg, err := storage.List(t.Context(), blob.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range pg.Keys {
		if bytes.Contains(readAll(t, storage, k), []byte("Doe^Peter")) {
			t.Errorf("%s holds the patient's name", k)
		}
	}
}
