package backup

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/secret"
	"github.com/amrshadid/nopacs/internal/snapshot"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestABackupRefusesTenantsTheMasterDoesNotList guards the check of the
// master's section against the tenants given: a backup given no tenant
// while the master lists one, or given a tenant the master does not list,
// is refused as unavailable (a tenant created or removed while it was
// taken), since its restore would bring back a tenant without its database
// or refuse a section; given exactly the master's tenants, it is made.
func TestABackupRefusesTenantsTheMasterDoesNotList(t *testing.T) {
	r := newRig(t, false)
	dbs, closeAll, err := OpenTenants(r.ctx, r.m, r.keys, r.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAll()
	stranger := dbs[0]
	stranger.Tenant, stranger.TenantID = "other", "0123456789abcdef0123456789abcdef"
	for name, given := range map[string][]Database{"none": nil, "one more": {dbs[0], stranger}, "another": {stranger}} {
		_, err := Create(r.ctx, CreateConfig{Installation: r.id, Keys: r.keys, Master: r.m.(snapshot.Database), Tenants: given, Out: io.Discard})
		if fault.KindOf(err) != fault.Unavailable || !strings.Contains(err.Error(), "created or removed while the backup was being taken") {
			t.Errorf("%s: a backup whose tenants are not the master's answered %v", name, err)
		}
	}
	if _, err := Create(r.ctx, CreateConfig{Installation: r.id, Keys: r.keys, Master: r.m.(snapshot.Database), Tenants: dbs, Out: io.Discard}); err != nil {
		t.Fatalf("a backup of the master's tenants: %v", err)
	}
}

// countEvents returns how many rows of the events table the tenant's
// section of the backup b holds.
func countEvents(t *testing.T, b []byte, r *rig) int {
	t.Helper()
	br, err := secret.NewBackupReader(bytes.NewReader(b), r.keys)
	if err != nil {
		t.Fatal(err)
	}
	rd := newReader(br)
	if _, err := rd.readHeader(); err != nil {
		t.Fatal(err)
	}
	n := 0
	for {
		s, more, err := rd.nextSection()
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			return n
		}
		for s.Database == DatabaseTenant {
			tab, ok, err := rd.NextTable()
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			if tab.Name != "events" {
				continue
			}
			values := make([]any, len(tab.Columns))
			for {
				row, err := rd.NextRow(values)
				if err != nil {
					t.Fatal(err)
				}
				if !row {
					break
				}
				n++
			}
		}
		if _, _, err := rd.endSection(); err != nil {
			t.Fatal(err)
		}
	}
}

// heldOpen is a tenant's database whose Snapshot marks when it has begun
// and when it has returned, so a test knows its read transaction is open.
type heldOpen struct {
	snapshot.Database
	begun, returned atomic.Bool
}

// Snapshot implements snapshot.Database.
func (h *heldOpen) Snapshot(ctx context.Context, sink snapshot.Sink) (snapshot.Point, error) {
	h.begun.Store(true)
	defer h.returned.Store(true)
	return h.Database.Snapshot(ctx, sink)
}

// stalledOut is a backup's destination that stops taking bytes, as a client
// that reads nothing does, on its first write once the tenant's section has
// begun: it signals on reached and waits for release.
type stalledOut struct {
	db               *heldOpen
	reached, release chan struct{}
	stalled          bool
	buf              bytes.Buffer
}

// Write implements io.Writer.
func (s *stalledOut) Write(p []byte) (int, error) {
	if !s.stalled && s.db.begun.Load() {
		s.stalled = true
		s.reached <- struct{}{}
		<-s.release
	}
	return s.buf.Write(p)
}

// TestAStalledBackupHoldsNoEventBack guards what a backup whose client
// stops reading does to the tenant it is reading, on SQLite and on
// Postgres: with the tenant's read transaction open and the backup's
// writes stalled, an event is appended and read back from the feed at
// once, after the cursor taken before it, so the feed keeps moving (on
// Postgres the feed orders its writers by its own row lock, not by the
// oldest open transaction). Once the client reads again the backup ends
// whole, and its tenant's section holds the events as of its moment,
// without the one appended while it was open.
func TestAStalledBackupHoldsNoEventBack(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		events, ok := r.st.(tenant.Events)
		if !ok {
			t.Fatal("the tenant's store keeps no event feed")
		}
		// Enough events that the tenant's section spans many of the
		// cipher's 64 KiB chunks, so the backup writes while its read
		// transaction is open.
		const before = 3000
		var batch []tenant.EventAppend
		for i := range before {
			e, err := tenant.NewEvent(&tenant.StorageSpaceChanged{Storage: "main", State: tenant.StorageRecovered, UsedBytes: int64(i)}, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			batch = append(batch, tenant.EventAppend{Key: "before-" + strconv.Itoa(i), Event: e})
			if len(batch) == 1000 || i == before-1 {
				if _, err := events.AppendEvents(r.ctx, batch); err != nil {
					t.Fatal(err)
				}
				batch = batch[:0]
			}
		}
		dbs, closeAll, err := OpenTenants(r.ctx, r.m, r.keys, r.dataDir)
		if err != nil {
			t.Fatal(err)
		}
		defer closeAll()
		held := &heldOpen{Database: dbs[0].DB}
		dbs[0].DB = held
		out := &stalledOut{db: held, reached: make(chan struct{}), release: make(chan struct{})}
		type result struct {
			sum Summary
			err error
		}
		done := make(chan result, 1)
		go func() {
			sum, err := Create(r.ctx, CreateConfig{Installation: r.id, Keys: r.keys, Master: r.m.(snapshot.Database), Tenants: dbs, Out: out})
			done <- result{sum, err}
		}()
		select {
		case <-out.reached:
		case res := <-done:
			t.Fatalf("the backup ended without writing during the tenant's section: %v", res.err)
		}
		if held.returned.Load() {
			close(out.release)
			t.Fatal("the tenant's snapshot had returned before the backup stalled")
		}
		ctx, cancel := context.WithTimeout(r.ctx, 10*time.Second)
		defer cancel()
		cursor, err := events.EventCursor(ctx)
		if err != nil {
			close(out.release)
			t.Fatal(err)
		}
		e, err := tenant.NewEvent(&tenant.StorageSpaceChanged{Storage: "main", State: tenant.StorageRecovered, UsedBytes: 1 << 40}, time.Now())
		if err != nil {
			close(out.release)
			t.Fatal(err)
		}
		if _, err := events.AppendEvents(ctx, []tenant.EventAppend{{Key: "during", Event: e}}); err != nil {
			close(out.release)
			t.Fatalf("an event appended while the backup's read was open: %v", err)
		}
		page, err := events.ReadEvents(ctx, cursor, nil, 10)
		stillOpen := !held.returned.Load()
		close(out.release)
		if err != nil || len(page.Events) != 1 {
			t.Fatalf("the feed read while the backup's read was open: %d events, %v", len(page.Events), err)
		}
		if !stillOpen {
			t.Fatal("the tenant's snapshot returned before the event was read: the test showed nothing")
		}
		res := <-done
		if res.err != nil {
			t.Fatalf("the backup after its client read again: %v", res.err)
		}
		v, err := Verify(bytes.NewReader(out.buf.Bytes()), r.keys)
		if err != nil {
			t.Fatalf("the backup held open does not verify: %v", err)
		}
		if got := countEvents(t, out.buf.Bytes(), r); got != before {
			t.Fatalf("the tenant's section holds %d events, want the %d before its moment (sections %+v)", got, before, v.Sums)
		}
	})
}
