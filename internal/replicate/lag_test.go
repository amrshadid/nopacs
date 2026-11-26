package replicate

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// lagDB is a tenant database's replicas as a test sets them, and the
// events appended to it.
type lagDB struct {
	mu       sync.Mutex
	replicas []tenant.ReplicaStatus
	events   []tenant.ReplicaBehind
	fail     error
}

func (d *lagDB) ListReplicas(context.Context, string, master.Page) ([]tenant.ReplicaStatus, string, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]tenant.ReplicaStatus(nil), d.replicas...), "", nil
}

func (d *lagDB) AppendEvents(_ context.Context, events []tenant.EventAppend) (int, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.fail != nil {
		return 0, d.fail
	}
	for _, e := range events {
		p, ok := tenant.DecodeEvent(e.Event.Type(), e.Event.Payload())
		if b, isBehind := p.(*tenant.ReplicaBehind); ok && isBehind {
			d.events = append(d.events, *b)
		}
	}
	return len(events), nil
}

// set sets the replica copy's oldest queued scan and its scans behind.
func (d *lagDB) set(oldest time.Time, pending int64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.replicas = []tenant.ReplicaStatus{{Replica: "copy", Primary: "main", OldestQueued: oldest, Pending: pending}}
}

// TestLagReportsBehindOnceAndRecoveredOnce guards replica.lag-alert's
// event: none while the oldest queued scan is younger than the alert; one
// REPLICA_BEHIND when it passes it, never a second while it stays past;
// one recovered when the replica catches up; an event that could not be
// written reported at the next look; the alert read at each look; and the
// lag kept for the metrics.
func TestLagReportsBehindOnceAndRecoveredOnce(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 26, 12, 0, 0, 0, time.UTC)
	alert := time.Hour
	var mu sync.Mutex
	db := &lagDB{}
	n := 0
	l, err := NewLag(LagConfig{Replicas: db, Now: func() time.Time { mu.Lock(); defer mu.Unlock(); return now },
		Alert: func(context.Context) (time.Duration, error) { mu.Lock(); defer mu.Unlock(); return alert, nil },
		NewID: func() string { n++; return "ev" + strconv.Itoa(n) }})
	if err != nil {
		t.Fatal(err)
	}
	look := func(want int) {
		t.Helper()
		if err := l.Check(ctx); err != nil {
			t.Fatal(err)
		}
		db.mu.Lock()
		defer db.mu.Unlock()
		if len(db.events) != want {
			t.Fatalf("after a look %d events: %+v; want %d", len(db.events), db.events, want)
		}
	}
	db.set(now.Add(-59*time.Minute), 2)
	look(0)
	if got := l.Lags(); len(got) != 1 || got[0].Lag != 59*time.Minute || got[0].ScansBehind != 2 || got[0].Primary != "main" {
		t.Fatalf("the lag kept is %+v", got)
	}
	db.set(now.Add(-61*time.Minute), 3)
	look(1)
	if e := db.events[0]; e.Recovered || e.Replica != "copy" || e.Primary != "main" || e.LagMicros != (61*time.Minute).Microseconds() || e.ScansBehind != 3 {
		t.Errorf("the behind event is %+v", e)
	}
	db.set(now.Add(-5*time.Hour), 4)
	look(1)
	db.set(time.Time{}, 0)
	db.fail = errors.New("the database does not answer")
	if err := l.Check(ctx); err == nil {
		t.Fatal("a look whose event could not be written answered no error")
	}
	db.fail = nil
	look(2)
	if e := db.events[1]; !e.Recovered || e.LagMicros != 0 || e.ScansBehind != 0 {
		t.Errorf("the recovered event is %+v", e)
	}
	look(2)
	// The alert is read at each look: lowered under the lag, it reports.
	db.set(now.Add(-10*time.Minute), 1)
	look(2)
	mu.Lock()
	alert = 5 * time.Minute
	mu.Unlock()
	look(3)
	// A replica no longer listed is forgotten without an event.
	db.mu.Lock()
	db.replicas = nil
	db.mu.Unlock()
	look(3)
	if got := l.Lags(); len(got) != 0 {
		t.Errorf("the lag of a replica no longer listed is kept: %+v", got)
	}
}
