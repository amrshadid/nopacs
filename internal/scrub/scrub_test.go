package scrub

import (
	"bytes"
	"context"
	"io"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// changed returns b with its byte at i flipped.
func changed(b []byte, i int) []byte {
	out := slices.Clone(b)
	out[i] ^= 0xFF
	return out
}

// TestScrubFindsWhatChangedAndWritesNothing guards a scrub of a storage
// whose objects changed under the archive: an instance's object with one
// byte flipped is found damaged and one deleted found missing, each
// refused in the job's counts with its failure, recorded with what was
// archived and what was found, and its instance marked so the catalog
// answers it damaged; an object in the scan's tree that nothing names, and
// objects under a scan the catalog does not hold, are orphans; the
// instance left whole is read good and not marked; the job ends partial
// with its tallies; and the storage is exactly what it was, no object
// written or removed by the scrub.
func TestScrubFindsWhatChangedAndWritesNothing(t *testing.T) {
	f := newFixture(t)
	a, b, c := corpus(t, "MR_small.dcm"), corpus(t, "ct2n-6293.dcm"), corpus(t, "ct5n-2062.dcm")
	p := f.archive("scan-a", a, b, c)
	_, _, ua := uidsOf(t, a)
	_, _, ub := uidsOf(t, b)
	_, _, uc := uidsOf(t, c)
	if _, err := f.store.Put(f.ctx, f.keys[ua], bytes.NewReader(changed(a, len(a)/2))); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(f.ctx, f.keys[ub]); err != nil {
		t.Fatal(err)
	}
	strays := []string{"data/sub-01/ses-scan-a/scan-a/2.25.999.dcm", "data/sub-02/ses-9/scan-z/2.25.998.dcm", "data/stray.txt",
		"meta/sub-02/ses-9/scan-z/manifest.json"}
	for _, k := range strays {
		if _, err := f.store.Put(f.ctx, k, strings.NewReader("not archived")); err != nil {
			t.Fatal(err)
		}
	}
	before := f.snapshot()
	j := f.scrub(f.config())
	if j.State != tenant.JobPartial || j.Counts.Instances != 3 || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 2 ||
		j.Kind != tenant.JobScrub || j.Storage != "main" {
		t.Errorf("the scrub ended as %+v, want partial with one read good and two refused", j)
	}
	want := []string{"damaged " + f.keys[ua], "missing " + f.keys[ub]}
	for _, k := range strays {
		want = append(want, "orphan "+k)
	}
	slices.Sort(want)
	if got := f.findings(false); !slices.Equal(got, want) {
		t.Errorf("findings:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	got, _, err := f.p.ListScrubFindings(f.ctx, tenant.ScrubFindingFilter{Kinds: []tenant.ScrubFindingKind{tenant.FindingDamaged}},
		master.Page{Limit: 10})
	if err != nil || len(got) != 1 {
		t.Fatalf("the damaged findings: %v, %v", got, err)
	}
	d := got[0]
	if d.SOPInstanceUID != ua || d.Path != p || *d.ExpectedSize != int64(len(a)) || *d.FoundSize != int64(len(a)) ||
		d.ExpectedSHA256 == d.FoundSHA256 || d.FoundSHA256.IsZero() || d.Revision != 1 || d.Job != j.ID {
		t.Errorf("the damaged finding reads %+v", d)
	}
	if !f.marked(p, ua) || !f.marked(p, ub) || f.marked(p, uc) {
		t.Errorf("the marks read %v %v %v, want the damaged and the missing instance marked", f.marked(p, ua), f.marked(p, ub), f.marked(p, uc))
	}
	tallies := f.tallies(j.ID)
	if tallies[tenant.ScrubTally(tenant.FindingDamaged)] != 1 || tallies[tenant.ScrubTally(tenant.FindingMissing)] != 1 ||
		tallies[tenant.ScrubTally(tenant.FindingOrphan)] != 4 || tallies[tenant.TallyOrphanObjects] != 4 {
		t.Errorf("the job's tallies are %v", tallies)
	}
	failures, _, err := f.p.ListJobFailures(f.ctx, j.ID, true, master.Page{Limit: 10})
	if err != nil || len(failures) != 2 {
		t.Fatalf("the job's failures: %v, %v", failures, err)
	}
	for _, fl := range failures {
		if fl.Code != tenant.FailureDamagedInStorage {
			t.Errorf("a refused instance's code is %s", fl.Code)
		}
	}
	after := f.snapshot()
	if !maps.Equal(before, after) || f.writes.Load() != 0 {
		t.Errorf("the scrub changed the storage: %d writes, %d keys before and %d after", f.writes.Load(), len(before), len(after))
	}
}

// TestScrubOfAWholeArchiveFindsNothing guards that a scrub of an archive
// whose objects are what was archived ends done, every instance read good,
// with no finding, no mark and no tally but its scan's missing metadata
// file.
func TestScrubOfAWholeArchiveFindsNothing(t *testing.T) {
	f := newFixture(t)
	a, b := corpus(t, "MR_small.dcm"), corpus(t, "ct2n-6293.dcm")
	p := f.archive("scan-a", a, b)
	j := f.scrub(f.config())
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 2 {
		t.Errorf("a scrub of a whole archive ended as %+v", j)
	}
	if got := f.findings(true); len(got) != 0 {
		t.Errorf("a whole archive has findings %v", got)
	}
	// The fixture writes no metadata file, as a scan archived before they
	// were written has none: counted, never a finding.
	if tallies := f.tallies(j.ID); len(tallies) != 1 || tallies[tenant.TallySidecarMissing] != 1 {
		t.Errorf("a whole archive's scrub has tallies %v, want only its scan without a metadata file", tallies)
	}
	_, _, ua := uidsOf(t, a)
	if f.marked(p, ua) {
		t.Error("an instance of a whole archive is marked")
	}
}

// TestScrubReadsAMismatchTwice guards that a difference is believed only
// when a second read finds it too: an object damaged at the first read and
// whole again at the second (a read that failed midway, a file written
// again meanwhile) is read good, with no finding and no mark.
func TestScrubReadsAMismatchTwice(t *testing.T) {
	f := newFixture(t)
	a := corpus(t, "MR_small.dcm")
	p := f.archive("scan-a", a)
	_, _, ua := uidsOf(t, a)
	key := f.keys[ua]
	if _, err := f.store.Put(f.ctx, key, bytes.NewReader(changed(a, 10))); err != nil {
		t.Fatal(err)
	}
	var seconds int
	cfg := withRead(f.config(), func(k string, attempt int) {
		if k == key && attempt == 2 {
			seconds++
			if _, err := f.store.Put(context.Background(), key, bytes.NewReader(a)); err != nil {
				t.Error(err)
			}
		}
	})
	j := f.scrub(cfg)
	if seconds != 1 {
		t.Fatalf("the damaged object was read a second time %d times, want once", seconds)
	}
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 1 {
		t.Errorf("the scrub ended as %+v, want the instance read good at its second read", j)
	}
	if got := f.findings(true); len(got) != 0 || f.marked(p, ua) {
		t.Errorf("a difference seen once is believed: findings %v, marked %v", got, f.marked(p, ua))
	}
}

// TestScrubClearsWhatReadsGoodAgain guards that a finding a later scrub
// reads good is cleared, with its mark: an instance repaired on its
// storage and an orphan removed; the later job counts them cleared; the
// findings keep their rows, cleared by the later job.
func TestScrubClearsWhatReadsGoodAgain(t *testing.T) {
	f := newFixture(t)
	a := corpus(t, "MR_small.dcm")
	p := f.archive("scan-a", a)
	_, _, ua := uidsOf(t, a)
	key := f.keys[ua]
	orphan := "data/sub-01/ses-scan-a/scan-a/2.25.999.dcm"
	if _, err := f.store.Put(f.ctx, key, bytes.NewReader(changed(a, 10))); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Put(f.ctx, orphan, strings.NewReader("stray")); err != nil {
		t.Fatal(err)
	}
	cfg := f.config()
	first := f.createScrub("main")
	s := f.start(cfg)
	f.waitEnded(s, first.ID)
	if got := f.findings(false); len(got) != 2 || !f.marked(p, ua) {
		t.Fatalf("the first scrub found %v, marked %v", got, f.marked(p, ua))
	}
	if _, err := f.store.Put(f.ctx, key, bytes.NewReader(a)); err != nil {
		t.Fatal(err)
	}
	if err := f.store.Delete(f.ctx, orphan); err != nil {
		t.Fatal(err)
	}
	second := f.createScrub("main")
	j := f.waitEnded(s, second.ID)
	if j.State != tenant.JobDone {
		t.Errorf("the scrub after the repair ended %s", j.State)
	}
	if got := f.findings(false); len(got) != 0 || f.marked(p, ua) {
		t.Errorf("after the repair the findings that hold are %v, marked %v", got, f.marked(p, ua))
	}
	if n := f.tallies(second.ID)[tenant.TallyCleared]; n != 2 {
		t.Errorf("the later job cleared %d findings, want 2", n)
	}
	all, _, err := f.p.ListScrubFindings(f.ctx, tenant.ScrubFindingFilter{IncludeCleared: true}, master.Page{Limit: 10})
	if err != nil || len(all) != 2 {
		t.Fatalf("the findings cleared: %v, %v", all, err)
	}
	for _, g := range all {
		if g.Holds() || g.ClearJob != second.ID || g.Job != first.ID {
			t.Errorf("a cleared finding reads %+v", g)
		}
	}
}

// coldStore is a store that answers one key as held in a tier it cannot
// read at once.
type coldStore struct {
	blob.Store
	key string
}

// Read answers the key not online.
func (n coldStore) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key == n.key {
		return nil, fault.Wrap(fault.Unavailable, blob.ErrNotOnline, "the object is in an archive tier")
	}
	return n.Store.Read(ctx, key, offset, length)
}

// TestScrubTalliesAnObjectNotOnline guards that an object the storage
// answers as not online is tallied and refused as not online, never a
// finding and never a mark: not readable now is not missing or damaged.
func TestScrubTalliesAnObjectNotOnline(t *testing.T) {
	f := newFixture(t)
	a, b := corpus(t, "MR_small.dcm"), corpus(t, "ct2n-6293.dcm")
	p := f.archive("scan-a", a, b)
	_, _, ua := uidsOf(t, a)
	cfg := f.config()
	cfg.OpenStorages = func(string, tenant.Pipeline) Storages { return storages{s: coldStore{Store: f.store, key: f.keys[ua]}} }
	j := f.scrub(cfg)
	if got := f.findings(true); len(got) != 0 || f.marked(p, ua) {
		t.Errorf("an object not online made findings %v, marked %v", got, f.marked(p, ua))
	}
	if n := f.tallies(j.ID)[tenant.TallyNotOnline]; n != 1 {
		t.Errorf("the job tallied %d objects not online, want 1", n)
	}
	failures, _, err := f.p.ListJobFailures(f.ctx, j.ID, true, master.Page{Limit: 10})
	if err != nil || len(failures) != 1 || failures[0].Code != tenant.FailureNotOnline {
		t.Errorf("the job's failures are %v, %v; want one not online", failures, err)
	}
}

// TestScrubFindsAManifestDamagedOrDisagreeing guards the checks of a scan's
// manifest: one whose bytes are not of the checksum the catalog holds is
// found damaged; one whole by its checksum whose line says another
// checksum of an instance than the catalog is found disagreeing on that
// instance's key, with both checksums; neither marks an instance.
func TestScrubFindsAManifestDamagedOrDisagreeing(t *testing.T) {
	f := newFixture(t)
	a, b := corpus(t, "MR_small.dcm"), corpus(t, "ct2n-6293.dcm")
	_, _, ua := uidsOf(t, a)
	f.editManifest = func(lines []manifest.File) {
		for i := range lines {
			if lines[i].SOPInstanceUID == ua {
				lines[i].SHA256 = strings.Repeat("ab", 32)
			}
		}
	}
	p := f.archive("scan-a", a)
	f.editManifest = nil
	q := f.archive("scan-b", b)
	mk, _ := layout.ManifestKey(q)
	if _, err := f.store.Put(f.ctx, mk, strings.NewReader("{}\n")); err != nil {
		t.Fatal(err)
	}
	j := f.scrub(f.config())
	want := []string{"disagreeing " + f.keys[ua], "manifest_damaged " + mk}
	if got := f.findings(false); !slices.Equal(got, want) {
		t.Errorf("findings %v, want %v", got, want)
	}
	got, _, err := f.p.ListScrubFindings(f.ctx, tenant.ScrubFindingFilter{Kinds: []tenant.ScrubFindingKind{tenant.FindingDisagreeing}},
		master.Page{Limit: 10})
	if err != nil || len(got) != 1 || got[0].FoundSHA256.String() != strings.Repeat("ab", 32) || got[0].ExpectedSHA256.IsZero() ||
		got[0].SOPInstanceUID != ua || got[0].Path != p {
		t.Errorf("the disagreeing finding reads %+v, %v", got, err)
	}
	if f.marked(p, ua) || j.Counts.InstancesSent != 2 {
		t.Errorf("a manifest's finding marked an instance or refused it: %+v", j.Counts)
	}
}

// fakeClock is a clock a unit's waits move: each wait is recorded and
// advances it, so a pacer's reservations are measured without sleeping.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	waited time.Duration
}

// Now returns the clock's time.
func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// sleep records d and advances the clock by it.
func (c *fakeClock) sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.waited += d
	c.now = c.now.Add(d)
	return ctx.Err()
}

// TestScrubIsPacedByItsRate guards scrub.rate: a scrub that reads N bytes
// of a storage waits about N divided by the rate, less the one second a
// pacer lets a storage idle before bank, and no more than that for all it
// reads.
func TestScrubIsPacedByItsRate(t *testing.T) {
	f := newFixture(t)
	files := [][]byte{corpus(t, "ct2n-6293.dcm"), corpus(t, "ct2n-6924.dcm"), corpus(t, "ct5n-2062.dcm"), corpus(t, "ct5n-2392.dcm")}
	f.archive("scan-a", files...)
	var instances int64
	for _, b := range files {
		instances += int64(len(b))
	}
	const rate = 64 << 10
	// After the job's creation, so its units are due at once.
	clock := &fakeClock{now: time.Now().Add(time.Hour)}
	cfg := withSleep(f.config(), clock.sleep)
	cfg.Now = clock.Now
	cfg.TenantSettings = func(tenant.Store) TenantSettings { return settingsOf{rate: rate} }
	f.scrub(cfg)
	// Every byte read counts: the instances, then the manifest, read whole.
	low := time.Duration(float64(instances)/rate*float64(time.Second)) - maxBurst
	high := time.Duration(float64(instances+4096)/rate*float64(time.Second)) + 10*time.Millisecond
	clock.mu.Lock()
	waited := clock.waited
	clock.mu.Unlock()
	if waited < low || waited > high {
		t.Errorf("reading %d bytes of instances at %d bytes a second waited %v, want %v to %v", instances, rate, waited, low, high)
	}
}
