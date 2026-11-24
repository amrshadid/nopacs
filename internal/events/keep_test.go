package events

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// keptFeed records what the Keeper asks of one tenant's feed, and answers a
// pruning with more due for its first prunesDue calls.
type keptFeed struct {
	mu          sync.Mutex
	checkpoints []time.Time
	prunes      []time.Time
	prunesDue   int
	failPrune   bool
}

func (f *keptFeed) CheckpointEvents(_ context.Context, at time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checkpoints = append(f.checkpoints, at)
	return nil
}

func (f *keptFeed) PruneEvents(_ context.Context, before time.Time, _ int) (int, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failPrune {
		return 0, false, errors.New("a test's failing pruning")
	}
	f.prunes = append(f.prunes, before)
	more := len(f.prunes) <= f.prunesDue
	return 1, more, nil
}

// keptTenants are the tenants of a Keeper's test: a feed per name, a
// retention per name, and names whose feed cannot be had.
type keptTenants struct {
	feeds     map[string]*keptFeed
	retention map[string]time.Duration
	down      map[string]bool
}

func (t keptTenants) TenantNames(context.Context) ([]string, error) {
	return []string{"down", "lab1", "lab2", "failing"}, nil
}

func (t keptTenants) Feed(_ context.Context, name string) (Feed, error) {
	if t.down[name] {
		return nil, errors.New("a test's tenant that does not answer")
	}
	return t.feeds[name], nil
}

func (t keptTenants) Retention(_ context.Context, name string) (time.Duration, error) {
	return t.retention[name], nil
}

// TestKeeperChecksAndPrunesEachTenant guards a round: each tenant's feed
// gets a checkpoint at the clock's time and is pruned to its own retention
// before that time, the pruning repeated while more is due; a tenant whose
// feed cannot be had, or whose pruning fails, is passed over without
// stopping the round for the others.
func TestKeeperChecksAndPrunesEachTenant(t *testing.T) {
	clock := newFakeClock()
	now := clock.Now()
	tenants := keptTenants{
		feeds: map[string]*keptFeed{
			"lab1":    {prunesDue: 2},
			"lab2":    {},
			"failing": {failPrune: true},
		},
		retention: map[string]time.Duration{"lab1": 90 * 24 * time.Hour, "lab2": 24 * time.Hour, "failing": time.Hour},
		down:      map[string]bool{"down": true},
	}
	NewKeeper(KeeperConfig{Tenants: tenants, Clock: clock}).Round(context.Background())

	lab1 := tenants.feeds["lab1"]
	if len(lab1.checkpoints) != 1 || !lab1.checkpoints[0].Equal(now) {
		t.Errorf("lab1's checkpoints: %v, want one at %v", lab1.checkpoints, now)
	}
	if len(lab1.prunes) != 3 {
		t.Errorf("lab1 was pruned %d times, want 3: while more was due, and once more", len(lab1.prunes))
	}
	for _, b := range lab1.prunes {
		if !b.Equal(now.Add(-90 * 24 * time.Hour)) {
			t.Errorf("lab1 was pruned before %v, want its 90 days before now", b)
		}
	}
	lab2 := tenants.feeds["lab2"]
	if len(lab2.checkpoints) != 1 || len(lab2.prunes) != 1 || !lab2.prunes[0].Equal(now.Add(-24*time.Hour)) {
		t.Errorf("lab2: checkpoints %v, prunes %v; want one of each, by its own day", lab2.checkpoints, lab2.prunes)
	}
	if f := tenants.feeds["failing"]; len(f.checkpoints) != 1 {
		t.Errorf("the tenant whose pruning fails got %d checkpoints, want 1", len(f.checkpoints))
	}
}

// TestKeeperRunsOnItsTimerAndStops guards Start and Stop: a round at once,
// another at each tick, and none after Stop has returned.
func TestKeeperRunsOnItsTimerAndStops(t *testing.T) {
	clock := newFakeClock()
	feed := &keptFeed{}
	tenants := keptTenants{feeds: map[string]*keptFeed{"lab1": feed}, retention: map[string]time.Duration{"lab1": time.Hour},
		down: map[string]bool{"down": true, "lab2": true, "failing": true}}
	k := NewKeeper(KeeperConfig{Tenants: tenants, Clock: clock})
	if err := k.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	checks := func() int {
		feed.mu.Lock()
		defer feed.mu.Unlock()
		return len(feed.checkpoints)
	}
	until(t, "the first round", func() bool { return checks() == 1 })
	clock.Tick()
	until(t, "the round of a tick", func() bool { return checks() == 2 })
	if err := k.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	clock.Tick()
	if n := checks(); n != 2 {
		t.Errorf("after Stop a tick made a round: %d checkpoints", n)
	}
}
