package scrub

import (
	"context"
	"math"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// pagedSource answers a fixed set of verifications two at a time.
type pagedSource []tenant.ScanVerification

// ScanVerifications answers the page after the cursor.
func (s pagedSource) ScanVerifications(_ context.Context, _ string, after string, _ int) ([]tenant.ScanVerification, string, error) {
	from := 0
	if after != "" {
		from, _ = strconv.Atoi(after)
	}
	to := min(from+2, len(s))
	next := ""
	if to < len(s) {
		next = strconv.Itoa(to)
	}
	return s[from:to], next, nil
}

// TestCoverageOfAKnownSet guards a storage's coverage on a set whose
// numbers are known: the objects read whole within the interval are
// verified, those checked by presence only are not, an object read longer
// ago than the interval is overdue, one never read is overdue only once an
// interval has passed since its archiving, and the oldest unverified age
// is from the later of its last read and its archiving; with no interval,
// nothing is overdue.
func TestCoverageOfAKnownSet(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	src := pagedSource{
		{Objects: 10, Held: now.Add(-200 * day), Verified: now.Add(-1 * day)},
		{Objects: 5, Held: now.Add(-100 * day), Verified: now.Add(-40 * day)},
		{Objects: 3, Held: now.Add(-10 * day)},
		{Objects: 2, Held: now.Add(-60 * day)},
		{Objects: 4, Held: now.Add(-5 * day), Presence: now.Add(-2 * day)},
	}
	got, err := Coverage(context.Background(), src, "main", 30*day, now)
	if err != nil {
		t.Fatal(err)
	}
	want := StorageCoverage{Storage: "main", Objects: 24, Verified: 10, PresenceOnly: 4, OldestUnverified: 60 * day, Overdue: 7}
	share := got.Share
	got.Share = 0
	if got != want || math.Abs(share-10.0/24) > 1e-9 {
		t.Errorf("Coverage = %+v share %v; want %+v share %v", got, share, want, 10.0/24)
	}
	never, err := Coverage(context.Background(), src, "main", 0, now)
	if err != nil {
		t.Fatal(err)
	}
	if never.Overdue != 0 || never.Verified != 15 {
		t.Errorf("Coverage with no interval = %+v; want nothing overdue and every object ever read verified", never)
	}
	empty, err := Coverage(context.Background(), pagedSource{}, "main", day, now)
	if err != nil || empty.Share != 1 || empty.Objects != 0 {
		t.Errorf("Coverage of an empty storage = %+v, %v; want a share of 1", empty, err)
	}
}

// memVerifications records verifications in memory.
type memVerifications struct {
	mu  sync.Mutex
	got map[string]bool
}

// RecordScanVerified records v by storage, scan and whether by presence.
func (m *memVerifications) RecordScanVerified(_ context.Context, v tenant.ScanVerified) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.got[v.Storage+" "+v.Path.Scan+" presence="+strconv.FormatBool(v.ByPresence)] = true
	return nil
}

// list returns what was recorded, in order.
func (m *memVerifications) list() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for k := range m.got {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestScrubRecordsEachCopyVerified guards that a scan's unit records, for
// coverage, each copy it read to its end: the primary, a replica in step,
// and an archive-tier replica checked by presence (said so), and not a
// replica whose copy of the scan is still pending.
func TestScrubRecordsEachCopyVerified(t *testing.T) {
	r := newReplicaRig(t)
	r.archiveTwo("s1", "1.2.826.0.1.3680043.10.508")
	r.addReplica("copy")
	r.addColdReplica("cold")
	r.copyAll()
	r.archiveTwo("s2", "1.2.826.0.1.3680043.10.509") // pending on both replicas
	mem := &memVerifications{got: map[string]bool{}}
	cfg := r.config()
	cfg.Verifications = func(tenant.Store) tenant.VerificationRecorder { return mem }
	r.scrubAll(cfg)
	want := []string{"cold s1 presence=true", "copy s1 presence=false", "main s1 presence=false", "main s2 presence=false"}
	if got := mem.list(); len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Errorf("the verifications recorded are %v; want %v", got, want)
	}
}
