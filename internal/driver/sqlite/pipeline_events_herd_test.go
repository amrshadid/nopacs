package sqlite

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/events"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/tenant/tenanttest"
)

// scaleWatches is how many idle watches TestScaleWriterBesideIdleWatches
// keeps open on one tenant: far more than a tenant's monitors would hold.
const scaleWatches = 1000

// TestScaleWriterBesideIdleWatches measures how long an append takes while
// scaleWatches idle watches of the same tenant are woken by the timer ten
// times a second, each reading the feed: what the timer's reads cost the
// one writer. It logs the times and guards nothing else; it runs in the
// scale run (skipScale).
func TestScaleWriterBesideIdleWatches(t *testing.T) {
	skipScale(t)
	n := scaleWatches
	ctx := context.Background()
	p := openTenantTest(t, tenanttest.SuiteTenant).(*Tenant)
	var batch []tenant.EventAppend
	for i := range 1000 {
		batch = append(batch, tenant.EventAppend{Key: fmt.Sprintf("seed%d", i), Event: spaceEvent(t, int64(i), fixtureAt(i))})
	}
	for k := range 100 { // 100k events
		for i := range batch {
			batch[i].Key = fmt.Sprintf("seed%d-%d", k, i)
		}
		if _, err := p.AppendEvents(ctx, batch); err != nil {
			t.Fatal(err)
		}
	}
	b, err := events.New(events.Config{Tick: 100 * time.Millisecond, MaxPerOwner: 100000, MaxPerTenant: 100000, MaxTotal: 100000})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	for i := range n {
		go func() {
			_ = b.Watch(wctx, events.Watch{Tenant: "t", Owner: fmt.Sprint(i), Reader: p, Types: []tenant.EventType{tenant.EventReceiverHealth}},
				func(context.Context, events.Message) error { return nil })
		}()
	}
	time.Sleep(time.Second)
	var lat []time.Duration
	deadline := time.Now().Add(3 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		t0 := time.Now()
		if _, err := p.AppendEvents(ctx, []tenant.EventAppend{{Key: fmt.Sprintf("w%d", i), Event: spaceEvent(t, 1, fixtureAt(1))}}); err != nil {
			t.Fatal(err)
		}
		lat = append(lat, time.Since(t0))
	}
	sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
	t.Logf("%d watches woken every 100ms (= %d reads/s): %d appends in 3s, p50 %v p99 %v max %v", n, n*10, len(lat), lat[len(lat)/2], lat[len(lat)*99/100], lat[len(lat)-1])
}
