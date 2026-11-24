package dirimport

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// bareConfig is a supervisor's configuration whose ports answer nothing
// useful, for tests of the supervisor's own state.
func bareConfig() Config {
	settings := &fakeSettings{}
	return Config{
		Tenants: &oneTenant{}, Installation: settings,
		TenantSettings: func(tenant.Store) TenantSettings { return settings },
		Roots:          treeRoot{},
		Sink:           func(string) receive.Sink { return nil },
		Interval:       time.Hour,
	}
}

// TestAStoppedSupervisorDoesNotStartAgain guards Start after Stop: it is
// refused, since a stopped supervisor's channels are closed and a run
// started on them would end at once while saying it runs.
func TestAStoppedSupervisorDoesNotStartAgain(t *testing.T) {
	for _, started := range []bool{true, false} {
		s, err := New(bareConfig())
		if err != nil {
			t.Fatal(err)
		}
		if started {
			if err := s.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.Start(context.Background()); err == nil {
			t.Errorf("a supervisor stopped (started before: %v) started again", started)
		}
	}
}

// countingReclaim is a tenant's database that counts the reclaims asked
// of it.
type countingReclaim struct {
	tenant.Pipeline
	n atomic.Int64
}

// ReclaimWorkOfKinds counts the call.
func (c *countingReclaim) ReclaimWorkOfKinds(context.Context, []string, time.Time) (int, error) {
	c.n.Add(1)
	return 0, nil
}

// TestATenantBackInTheListDoesNotReclaimItsOwnUnits guards reclaiming once
// per process and tenant: a tenant the list dropped and gave back gets a
// new worker, whose first claim must not take back the units the old
// worker's units still hold.
func TestATenantBackInTheListDoesNotReclaimItsOwnUnits(t *testing.T) {
	s, err := New(bareConfig())
	if err != nil {
		t.Fatal(err)
	}
	p := &countingReclaim{}
	for range 2 {
		w := &tenantRun{sup: s, name: "lab", log: slog.New(slog.DiscardHandler), poke: make(chan struct{}, 1), retired: make(chan struct{})}
		w.reclaim(p)
	}
	if n := p.n.Load(); n != 1 {
		t.Errorf("%d reclaims for one tenant in one process, want 1", n)
	}
}
