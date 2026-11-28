package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestIdleConnectionsAreClosedAndOpenedAgain guards the bound on the files a
// tenant's store holds open: connections a burst opened are closed once
// nothing has used them for the idle time, in both pools, and the store
// answers the next call on a new connection as if nothing had happened; a
// connection in use (a read transaction open across the idle time) is not
// closed, and its rows are still read after it.
func TestIdleConnectionsAreClosedAndOpenedAgain(t *testing.T) {
	ctx := context.Background()
	const idle = 150 * time.Millisecond
	s, err := OpenTenantIdle(ctx, filepath.Join(t.TempDir(), "tenant.db"), "t1", idle)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// A burst: four reads at once hold four read connections.
	var wg sync.WaitGroup
	conns := make([]*sql.Conn, 4)
	for i := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := s.p.read.Conn(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			conns[i] = c
		}()
	}
	wg.Wait()
	for _, c := range conns {
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if n := s.p.read.Stats().OpenConnections; n != 4 {
		t.Fatalf("%d read connections after the burst, want 4", n)
	}
	// Held open across the idle time: not closed.
	tx, err := s.p.read.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for s.p.read.Stats().OpenConnections > 1 || s.p.write.Stats().OpenConnections > 0 {
		if time.Now().After(deadline) {
			t.Fatalf("after the idle time: %d read and %d write connections open, want 1 and 0",
				s.p.read.Stats().OpenConnections, s.p.write.Stats().OpenConnections)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM work`).Scan(&n); err != nil {
		t.Fatalf("a read on a connection held across the idle time: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	for s.p.read.Stats().OpenConnections > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the last read connection was not closed once idle")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Transparent reopening, for a read and for a write.
	if err := s.Ping(ctx); err != nil {
		t.Fatalf("a ping after every connection was closed: %v", err)
	}
	if _, err := s.Version(ctx); err != nil {
		t.Fatalf("a read after every connection was closed: %v", err)
	}
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("a write after every connection was closed: %v", err)
	}
	// A prepared statement on a pool whose connections were all closed.
	for s.p.read.Stats().OpenConnections > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the read connection was not closed once idle")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := s.Setting(ctx, "no.such.setting"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("a prepared read after every connection was closed: %v, want NotFound", err)
	}
}

// TestOpenTenantAppliesTheDefaultIdleTime guards the way the registry opens
// a tenant's store (OpenTenant, not OpenTenantIdle): both pools close
// connections left unused for DefaultIdleConnection, and the master's pools,
// which have the same code, never do.
func TestOpenTenantAppliesTheDefaultIdleTime(t *testing.T) {
	ctx := context.Background()
	s, err := OpenTenant(ctx, filepath.Join(t.TempDir(), "tenant.db"), "t1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.p.idle != DefaultIdleConnection || DefaultIdleConnection <= 0 {
		t.Fatalf("a store opened by OpenTenant has idle time %v, want %v", s.p.idle, DefaultIdleConnection)
	}
	m, err := openPools(ctx, filepath.Join(t.TempDir(), "master.db"), masterDB, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.close() })
	if m.idle != 0 {
		t.Fatalf("the master's pools have idle time %v, want none", m.idle)
	}
}
