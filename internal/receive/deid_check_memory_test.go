package receive

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"sync/atomic"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// TestACheckPastTheMemoryBoundIsHeldAndReleasedOnceRaised guards the
// server's bound on de-identification's memory at reception: a file whose
// check after needs more than the whole bound is held (deid_failed,
// check_too_large) with its bytes kept and nothing put on the storage;
// once the bound is raised, its release archives it de-identified, and
// every byte taken from the bound is given back.
func TestACheckPastTheMemoryBoundIsHeldAndReleasedOnceRaised(t *testing.T) {
	var limit atomic.Int64
	limit.Store(1000)
	budget := check.NewBudget(func(context.Context) (int64, error) { return limit.Load(), nil }, 0)
	rc := &recordingCheck{real: check.New(check.Options{Memory: budget})}
	e := newEnv(t, envOptions{deid: &deidOptions{check: rc}})
	e.send(studyFiles[0])
	r := e.settle(1)[0]
	qs := e.held()
	if r.State != "quarantined" || len(qs) != 1 || qs[0].Reason != tenant.QuarantineDeidFailed || qs[0].Details["cause"] != "check_too_large" {
		t.Fatalf("reception %s, held %+v; want deid_failed check_too_large", r.State, qs)
	}
	if !qs[0].ContentKept {
		t.Fatal("a file held for the memory bound kept no bytes")
	}
	if objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"}); err != nil || len(objs.Keys) != 0 {
		t.Fatalf("the storage holds %v (%v)", objs.Keys, err)
	}
	limit.Store(1 << 30)
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: qs[0].ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	for _, x := range e.settle(2) {
		if done(x) {
			noOriginal(t, e, x)
			if u := budget.Used(); u != 0 {
				t.Fatalf("%d bytes still taken from the bound after the checks", u)
			}
			return
		}
	}
	t.Fatal("no reception archived the released file")
}

// TestATrialTakesItsCopiesFromTheMemoryBound guards a trial run under the
// server's bound: a file whose copies need more than the whole bound is
// held (deid_failed, check_too_large) before the engine runs; within it,
// the trial passes and gives back every byte it took; and a trial that
// waits for room ends with its context, which is no verdict.
func TestATrialTakesItsCopiesFromTheMemoryBound(t *testing.T) {
	en := trialEngine(t)
	b, err := os.ReadFile(filepath.Join("testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	var limit atomic.Int64
	limit.Store(int64(len(b)))
	budget := check.NewBudget(func(context.Context) (int64, error) { return limit.Load(), nil }, 0)
	o := TrialOptions{Keys: testDeidKeys, HeaderLimit: header.MaxHeader, Check: func(int64) Checker { return &recordingCheck{} }, Memory: budget}
	tr, err := TryFile(t.Context(), en, b, engine.Reception{}, o)
	if err != nil || tr.Hold == nil || tr.Hold.Reason != tenant.QuarantineDeidFailed || tr.Hold.Details["cause"] != "check_too_large" || tr.Record != nil {
		t.Fatalf("a file past the bound: %+v %v", tr.Hold, err)
	}
	limit.Store(1 << 30)
	if tr, err = TryFile(t.Context(), en, b, engine.Reception{}, o); err != nil || tr.Hold != nil {
		t.Fatalf("a file within the bound: %+v %v", tr.Hold, err)
	}
	if u := budget.Used(); u != 0 {
		t.Fatalf("%d bytes still taken after the trial", u)
	}
	hold, err := budget.Reserve(t.Context(), 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer hold()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if tr, err = TryFile(ctx, en, b, engine.Reception{}, o); err == nil || tr.Hold != nil {
		t.Fatalf("a trial waiting past its context: %+v %v, want an error and no verdict", tr.Hold, err)
	}
}

// trialSlack is what a trial holds beyond its copies whatever the file's
// size: the data sets of the header (two of them with values), the
// record, the check's automata.
const trialSlack = 4 << 20

// TestATrialsPeakIsWithinWhatItTakes guards the estimate a trial takes
// from the server's bound (trialBytes) against the heap it really uses: a
// public MR file, a 512 by 512 slice of two frames, a 20 MiB native cine
// and an RLE cine of 6 MiB decoded to 20 MiB by a pixel rule, each tried
// with its values and without, peak at or below the estimate (and a fixed
// slack of a few MiB for the header's data sets). The factor of each over
// the file's size is logged.
func TestATrialsPeakIsWithinWhatItTakes(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector's own memory hides the heap's")
	}
	// A low GC target keeps HeapAlloc near the live heap, so the peak
	// measured is what the trial holds, not garbage not yet collected.
	defer debug.SetGCPercent(debug.SetGCPercent(5))
	rule := &v1.PixelRule{Name: "bar", Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 64, Height: 8}}}}}
	read := func(path string) []byte {
		b, err := os.ReadFile(path) // #nosec G304 -- the test's own temporary file.
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	for _, c := range []struct {
		name string
		b    []byte
		en   *engine.Engine
	}{
		{"MR_small", corpus(t, "MR_small.dcm"), trialEngine(t)},
		{"a slice of 512 KiB", read(cine(t, 512, 512, 2, false)), pixelTestEngine(t, rule)},
		{"a 20 MiB cine", read(cine(t, 512, 512, 80, false)), pixelTestEngine(t, rule)},
		{"an RLE cine decoded to 20 MiB", read(cine(t, 512, 512, 80, true)), pixelTestEngine(t, rule)},
	} {
		for _, noValues := range []bool{false, true} {
			o := TrialOptions{Keys: testDeidKeys, HeaderLimit: header.MaxHeader, NoValues: noValues,
				Check: func(n int64) Checker { return check.New(check.Options{MaxFileBytes: n}) }}
			runtime.GC()
			var base runtime.MemStats
			runtime.ReadMemStats(&base)
			stop := testkit.HeapPeak()
			tr, err := TryFile(t.Context(), c.en, c.b, engine.Reception{}, o)
			peak := stop()
			if err != nil || tr.Hold != nil {
				t.Fatalf("%s: held %+v: %v", c.name, tr.Hold, err)
			}
			n := int64(len(c.b))
			held := n + int64(peak) - int64(base.HeapAlloc) //nolint:gosec // G115: heap sizes of a test, far below 2^63.
			// The headers of these files hold no text value past a few
			// hundred bytes, within the slack.
			written := n + grown(tr.Record) + trialWriteHeadroom
			want := trialBytes(len(c.b), grown(tr.Record), check.Reservation(t.Context(), written, 0, tr.Record.Needles, nil))
			t.Logf("%s (values %v): %d bytes, peak %.2f times the file, estimate %.2f times", c.name, !noValues, n,
				float64(held)/float64(n), float64(want)/float64(n))
			if held > want+trialSlack {
				t.Errorf("%s (values %v): the trial held %d bytes at its peak, more than the %d it takes and %d of slack",
					c.name, !noValues, held, want, trialSlack)
			}
			runtime.KeepAlive(tr)
		}
	}
}
