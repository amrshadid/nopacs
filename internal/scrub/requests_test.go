package scrub

import (
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// stepClock is a clock a test moves.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

// Now answers the clock's time.
func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// add moves the clock by d.
func (c *stepClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// TestRequestsAreDeduplicatedBoundedAndRateLimited guards the queue of
// requests to scrub one instance: the same instance asked twice is one
// entry, also while it is being read; at most the rate is accepted a
// minute, and the window moves on; at most the bound waits; a request put
// back comes first again; and a request that names no instance is
// refused.
func TestRequestsAreDeduplicatedBoundedAndRateLimited(t *testing.T) {
	clock := &stepClock{t: time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)}
	q := &requests{rate: 3, max: 4, now: clock.Now}
	p := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "1"}
	req := func(n string) InstanceRequest {
		return InstanceRequest{Path: p, SOPInstanceUID: "1.2.826.0.1.3680043.10.506." + n}
	}
	steps := []struct {
		req  InstanceRequest
		want RequestOutcome
	}{
		{req("1"), RequestQueued},
		{req("1"), RequestDuplicate},
		{req("2"), RequestQueued},
		{req("3"), RequestQueued},
		{req("4"), RequestLimited},
	}
	for i, s := range steps {
		if got := q.add("lab", s.req); got != s.want {
			t.Errorf("request %d (%s) answered %s; want %s", i, s.req.SOPInstanceUID, got, s.want)
		}
	}
	if got := q.add("other", req("4")); got != RequestQueued {
		t.Errorf("another tenant's request answered %s; the rate is per tenant", got)
	}
	clock.add(61 * time.Second)
	if got := q.add("lab", req("4")); got != RequestQueued {
		t.Errorf("a request a minute later answered %s; want queued", got)
	}
	if got := q.add("lab", req("5")); got != RequestFull {
		t.Errorf("a request past the bound answered %s; want full", got)
	}
	taken := q.take("lab", 2)
	if len(taken) != 2 || taken[0].req != req("1") || taken[1].req != req("2") {
		t.Fatalf("take answered %v; want the first two in order", taken)
	}
	if got := q.add("lab", req("1")); got != RequestDuplicate {
		t.Errorf("a request for an instance being read answered %s; want duplicate", got)
	}
	q.done("lab", taken[0].req)
	q.putBack("lab", taken[1:])
	if first, ok := q.first("lab"); !ok || first != req("2") {
		t.Errorf("the first request after one was put back is %v, %v; want the one put back", first, ok)
	}
	again := q.take("lab", 1)
	again[0].attempts++
	q.again("lab", again)
	if first, ok := q.first("lab"); !ok || first != req("3") {
		t.Errorf("the first request after a failed one was queued again is %v, %v; want the next, the failed one behind", first, ok)
	}
	if got := q.add("lab", req("2")); got != RequestDuplicate {
		t.Errorf("a request queued again answered %s; want duplicate", got)
	}
	q.forget("lab")
	if _, ok := q.first("lab"); ok {
		t.Error("a forgotten tenant's queue still holds requests")
	}
	if got := q.add("lab", InstanceRequest{Path: p, SOPInstanceUID: "not a uid"}); got != RequestInvalid {
		t.Errorf("a request that names no instance answered %s; want invalid", got)
	}
}

// TestRequestedInstanceIsScrubbedFirst guards that a request to scrub an
// instance whose object on its storage is not what was archived, made
// while no scrub runs, makes a scrub of that storage whose first read of
// an instance is the one asked for, and that the difference is recorded
// and marks the instance as any scrub's would.
func TestRequestedInstanceIsScrubbedFirst(t *testing.T) {
	f := newFixture(t)
	files := synthetic("1.2.826.0.1.3680043.10.507", 2)
	p := f.archiveEntries("s1", "1.2.826.0.1.3680043.10.507.90", "1.2.826.0.1.3680043.10.507.80", files)
	asked := files[1].uid
	if _, err := f.store.Put(f.ctx, f.keys[asked], strings.NewReader("bytes that are not what was archived")); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var order []string
	cfg := withRead(f.config(), func(key string, _ int) {
		if strings.HasPrefix(key, layout.DataRoot) {
			mu.Lock()
			order = append(order, key)
			mu.Unlock()
		}
	})
	s := f.start(cfg)
	if got := s.RequestInstance("lab", InstanceRequest{Path: p, SOPInstanceUID: asked}); got != RequestQueued {
		t.Fatalf("the request answered %s; want queued", got)
	}
	if got := s.RequestInstance("lab", InstanceRequest{Path: p, SOPInstanceUID: asked}); got != RequestDuplicate {
		t.Errorf("the same request again answered %s; want duplicate", got)
	}
	deadline := time.Now().Add(30 * time.Second)
	for !f.marked(p, asked) {
		if time.Now().After(deadline) {
			t.Fatal("the instance asked for was not marked")
		}
		time.Sleep(20 * time.Millisecond)
	}
	jobs, _, err := f.p.ListJobs(f.ctx, tenant.JobFilter{}, pageOf(10))
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || !strings.HasPrefix(jobs[0].ID, "scrub-request-") || jobs[0].Storage != "main" {
		t.Fatalf("the jobs are %+v; want one scrub of main made for the request", jobs)
	}
	j := f.waitEnded(s, jobs[0].ID)
	if got := f.tallies(j.ID)[TallyRequestChecked]; got != 1 {
		t.Errorf("the scrub tallied %d requested instances read; want 1", got)
	}
	mu.Lock()
	first := ""
	if len(order) > 0 {
		first = order[0]
	}
	mu.Unlock()
	if first != f.keys[asked] {
		t.Errorf("the scrub's first read of an instance was %q; want the one asked for, %q", first, f.keys[asked])
	}
	if got := f.findings(false); len(got) != 1 || got[0] != "damaged "+f.keys[asked] {
		t.Errorf("the findings are %v; want the asked instance damaged", got)
	}
}
