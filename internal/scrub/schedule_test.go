package scrub

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// intervalOf answers scrub.interval as it is set now, so a test changes it
// between two looks.
type intervalOf struct {
	settingsOf
	d *atomic.Int64
}

// ScrubInterval answers the interval set now.
func (s intervalOf) ScrubInterval(context.Context) (time.Duration, error) {
	return time.Duration(s.d.Load()), nil
}

// TestScheduleFollowsTheLastScrub guards the scheduled scrubs: each storage
// of the tenant never scrubbed gets one at once, made by the server
// (scheduled) with its audit entry; a second look makes nothing while it
// is queued; the next is made one scrub.interval after the last scrub of
// the storage ended, not before; a storage being scrubbed is passed over;
// and a change of scrub.interval moves the next scrub from the last one's
// end rather than starting one at once.
func TestScheduleFollowsTheLastScrub(t *testing.T) {
	f := newFixture(t)
	if _, err := f.p.AddStorage(f.ctx, tenant.Storage{Name: "cold", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "cold"}}, time.Now(), f.audit(), nil); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Add(time.Hour).Truncate(time.Second)
	clock := &fakeClock{now: now}
	set := func(at time.Time) {
		clock.mu.Lock()
		clock.now = at
		clock.mu.Unlock()
	}
	interval := &atomic.Int64{}
	interval.Store(int64(24 * time.Hour))
	cfg := f.config()
	cfg.Now = clock.Now
	cfg.TenantSettings = func(tenant.Store) TenantSettings {
		return intervalOf{settingsOf: settingsOf{rate: 64 << 30}, d: interval}
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	run := &tenantRun{sup: s, name: "lab", log: s.log, poke: make(chan struct{}, 1), retired: make(chan struct{}), pacers: map[string]*pacer{}}
	scrubs := func() []string {
		t.Helper()
		jobs, _, err := f.p.ListJobs(f.ctx, tenant.JobFilter{}, master.Page{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, j := range jobs {
			if j.Kind == tenant.JobScrub {
				if !j.Scheduled {
					t.Errorf("a scheduled scrub reads not scheduled: %+v", j)
				}
				out = append(out, j.ID+" "+j.Storage)
			}
		}
		slices.Sort(out)
		return out
	}
	cancel := func(id string, at time.Time) {
		t.Helper()
		if _, err := f.p.CancelJob(f.ctx, id, at, f.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	run.scheduleOnce()
	cold1, main1 := ScheduledJobID("cold", ""), ScheduledJobID("main", "")
	want := []string{cold1 + " cold", main1 + " main"}
	slices.Sort(want)
	if got := scrubs(); !slices.Equal(got, want) {
		t.Fatalf("the first look made %v, want %v", got, want)
	}
	run.scheduleOnce()
	if got := scrubs(); len(got) != 2 {
		t.Errorf("a second look made more: %v", got)
	}
	cancel(cold1, now)
	set(now.Add(23 * time.Hour))
	run.scheduleOnce()
	if got := scrubs(); len(got) != 2 {
		t.Errorf("a look before the interval ran out made %v", got)
	}
	ended := now.Add(24 * time.Hour)
	set(ended)
	run.scheduleOnce()
	cold2 := ScheduledJobID("cold", cold1)
	got := scrubs()
	if len(got) != 3 || !slices.Contains(got, cold2+" cold") {
		t.Errorf("the next look made %v: want cold's next, and main passed over while its scrub runs", got)
	}
	// The interval changes an hour after the last scrub of cold ended:
	// nothing is made until the new interval has run from that end.
	cancel(cold2, ended)
	set(ended.Add(time.Hour))
	interval.Store(int64(12 * time.Hour))
	run.scheduleOnce()
	if got := scrubs(); len(got) != 3 {
		t.Errorf("a change of scrub.interval made a scrub at once: %v", got)
	}
	set(ended.Add(12 * time.Hour))
	run.scheduleOnce()
	if got := scrubs(); len(got) != 4 || !slices.Contains(got, ScheduledJobID("cold", cold2)+" cold") {
		t.Errorf("12 hours after the last scrub ended the looks made %v", got)
	}
	entries, _, err := f.p.ListAudit(f.ctx, master.AuditFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	made := 0
	for _, e := range entries {
		if e.Method == ScheduleMethod {
			made++
			if e.KeyID != "" || e.Details["storage"] == "" || e.Outcome != master.OutcomeDone {
				t.Errorf("a scheduled scrub's audit entry reads %+v", e)
			}
		}
	}
	if made != 4 {
		t.Errorf("%d audit entries of scheduled scrubs, want 4", made)
	}

	off := newFixture(t)
	cfg = off.config()
	cfg.TenantSettings = func(tenant.Store) TenantSettings { return settingsOf{rate: 64 << 30} }
	s, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	run = &tenantRun{sup: s, name: "lab", log: s.log, poke: make(chan struct{}, 1), retired: make(chan struct{}), pacers: map[string]*pacer{}}
	run.scheduleOnce()
	jobs, _, err := off.p.ListJobs(off.ctx, tenant.JobFilter{}, master.Page{Limit: 10})
	if err != nil || len(jobs) != 0 {
		t.Errorf("scrub.interval 0 made %v, %v", jobs, err)
	}
}

// TestScheduledJobIDIsOnePerScrubFollowed guards the ids of scheduled
// scrubs: the same for the same storage and scrub followed, another for
// another of either, and a valid id.
func TestScheduledJobIDIsOnePerScrubFollowed(t *testing.T) {
	a, b := ScheduledJobID("main", ""), ScheduledJobID("main", "")
	c, d := ScheduledJobID("main", a), ScheduledJobID("cold", "")
	if a != b || a == c || a == d || c == d {
		t.Errorf("ids %s %s %s %s: want one per storage and scrub followed", a, b, c, d)
	}
	for _, id := range []string{a, c, d, ScheduledJobID(strings.Repeat("s", 63), strings.Repeat("j", 64))} {
		if err := master.ValidateID("job id", id); err != nil {
			t.Errorf("%s: %v", id, err)
		}
	}
}
