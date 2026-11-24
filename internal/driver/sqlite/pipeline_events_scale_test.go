package sqlite

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// scaleEvents is how many events TestScaleEventFeed seeds: a feed a busy
// tenant keeps for its retention, keyed as appended events are.
const scaleEvents = 1_000_000

// TestScaleEventFeed measures, with scaleEvents events in the feed, a page
// read mid-feed with and without filters, a status snapshot, the feed's
// place, a pruning of MaxPruneBatch events and an append after it. It logs
// each time and guards nothing else; it runs in the scale run (skipScale).
func TestScaleEventFeed(t *testing.T) {
	skipScale(t)
	n := scaleEvents
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	if _, err := p.ready(ctx); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	pl := `{"storage":"store","state":"recovered","used_bytes":1}`
	hl := `{"receiver":"a","state":"up","down_reason":"","detail":"","since":"2026-01-01T00:00:00Z"}`
	if err := p.writeTx(ctx, "seed", func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO events (type, at, payload, key)
			WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM c WHERE i < ?)
			SELECT CASE WHEN i % 1000 = 0 THEN 'receiver_health' ELSE 'storage_space' END, ?+i, CASE WHEN i % 1000 = 0 THEN ? ELSE ? END, 'k' || i FROM c`,
			n, micros(fixtureAt(0)), hl, pl)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := p.writeTx(ctx, "analyze", func(tx *sql.Tx) error { _, err := tx.ExecContext(ctx, `ANALYZE`); return err }); err != nil {
		t.Fatal(err)
	}
	t.Logf("seeded %d in %v", n, time.Since(t0))
	mid := p.eventCursor(int64(n / 2))
	measure := func(what string, f func() error) {
		t0 := time.Now()
		if err := f(); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		t.Logf("%s: %v", what, time.Since(t0))
	}
	measure("page of 1000 mid-feed", func() error { _, err := p.ReadEvents(ctx, mid, nil, 1000); return err })
	measure("filtered page (rare type, 1 in 1000) mid-feed", func() error {
		_, err := p.ReadEvents(ctx, mid, []tenant.EventType{tenant.EventReceiverHealth}, 1000)
		return err
	})
	measure("filtered page of a type absent", func() error {
		_, err := p.ReadEvents(ctx, mid, []tenant.EventType{tenant.EventAssociationFailed}, 1000)
		return err
	})
	measure("filtered page of 2 types", func() error {
		_, err := p.ReadEvents(ctx, mid, []tenant.EventType{tenant.EventReceiverHealth, tenant.EventStorageSpace}, 1000)
		return err
	})
	measure("status snapshot", func() error { _, err := p.StatusSnapshot(ctx); return err })
	measure("event cursor", func() error { _, err := p.EventCursor(ctx); return err })
	// While pruning, another caller appends one event at a time: the
	// longest of its waits is how long a pruning holds the writer.
	stop := make(chan struct{})
	waits := make(chan time.Duration, 1)
	go func() {
		longest := time.Duration(0)
		for i := 0; ; i++ {
			select {
			case <-stop:
				waits <- longest
				return
			default:
			}
			t0 := time.Now()
			if _, err := p.AppendEvents(ctx, []tenant.EventAppend{{Key: "beside" + strconv.Itoa(i), Event: spaceEvent(t, 1, fixtureAt(1))}}); err != nil {
				t.Error(err)
			}
			longest = max(longest, time.Since(t0))
		}
	}()
	if err := p.CheckpointEvents(ctx, fixtureAt(999_999)); err != nil {
		t.Fatal(err)
	}
	measure("prune 10000", func() error { _, _, err := p.PruneEvents(ctx, fixtureAt(1_000_000), 10000); return err })
	measure("prune 10000 again", func() error { _, _, err := p.PruneEvents(ctx, fixtureAt(1_000_000), 10000); return err })
	close(stop)
	t.Logf("an append beside the prunings waited at most %v", <-waits)
	measure("append 1 after", func() error {
		_, err := p.AppendEvents(ctx, []tenant.EventAppend{{Key: "x", Event: spaceEvent(t, 1, fixtureAt(1))}})
		return err
	})
}
