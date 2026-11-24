package registry

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/secret"
)

// TestStoreDoesNotReachMaster guards that getting a ready tenant's store,
// the path of every request, makes no call to the master: a master that
// counts its calls sees none in 100,000 of them.
func TestStoreDoesNotReachMaster(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	counting := &countingMaster{Store: e.master}
	cfg := e.config()
	cfg.Master = counting
	r := e.newRegistry(cfg)
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	e.create(r, "id-down", "down", flakyDriver, flakyAddress("down"))
	e.flaky.set("down", down)
	if err := r.Start(ctx); err != nil {
		t.Fatal(err)
	}
	before := counting.calls.Load()
	for range 100000 {
		if _, err := r.Store("lab"); err != nil {
			t.Fatal(err)
		}
		if _, err := r.Store("down"); err == nil {
			t.Fatal("an unreachable tenant was served")
		}
	}
	if n := counting.calls.Load() - before; n != 0 {
		t.Errorf("200,000 calls of Store made %d calls to the master, want 0", n)
	}
	// The counter counts: the start and the creations did reach it.
	if before == 0 {
		t.Error("the counting master counted nothing at all")
	}
}

// startMany starts a registry over n SQLite tenants held in a master in
// memory, opening at most maxOpening at once, and returns it, the master,
// the counting driver and how long Start took.
func startMany(tb testing.TB, n, maxOpening int) (*Registry, *memMaster, *countingDriver, time.Duration) {
	tb.Helper()
	keys := newKeys(tb)
	m := newMemMaster()
	for i := range n {
		m.add(tb, keys, fmt.Sprintf("id-%04d", i), fmt.Sprintf("t%04d", i), driver.SQLiteDriver, "", master.StateReady)
	}
	drv := &countingDriver{Driver: driver.SQLiteTenants{DataDir: privateDir(tb)}}
	r, took := startOn(tb, m, keys, drv, maxOpening)
	return r, m, drv, took
}

// startOn starts a registry over the tenants of m, and returns it and how
// long Start took.
func startOn(tb testing.TB, m master.Store, keys *secret.Keys, drv Driver, maxOpening int) (*Registry, time.Duration) {
	tb.Helper()
	r, err := New(Config{
		Master: m, Keys: keys, Drivers: map[string]Driver{driver.SQLiteDriver: drv},
		Now: newClock().Now, Interval: time.Hour, OpenTimeout: time.Minute, PingTimeout: 10 * time.Second, MaxOpening: maxOpening,
	})
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = r.Close(context.Background()) })
	begin := time.Now()
	if err := r.Start(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return r, time.Since(begin)
}

// benchStore measures Store of one ready tenant, as every request makes it.
func benchStore(b *testing.B, r *Registry, name string) {
	b.ReportAllocs()
	for b.Loop() {
		if _, err := r.Store(name); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkStore10 measures getting a ready tenant's store among 10.
func BenchmarkStore10(b *testing.B) {
	r, _, _, _ := startMany(b, 10, 8)
	benchStore(b, r, "t0005")
}

// BenchmarkStore1000 measures getting a ready tenant's store among 1,000.
func BenchmarkStore1000(b *testing.B) {
	if testing.Short() {
		b.Skip("1,000 tenants: skipped under -short")
	}
	r, _, _, _ := startMany(b, 1000, 32)
	benchStore(b, r, "t0500")
}

// BenchmarkStoreParallel measures getting stores from many goroutines at
// once, as concurrent requests do: there is no lock for them to meet.
func BenchmarkStoreParallel(b *testing.B) {
	r, _, _, _ := startMany(b, 10, 8)
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := r.Store("t0003"); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// TestScaleThousandTenants guards the registry at 1,000 tenants, a size an
// installation can reach: every one is ready after Start, at most MaxOpening
// were opened at once, getting one tenant's store costs the same as with 10,
// and nothing is written to the master for tenants whose state did not
// change. It reports the time until every tenant is ready, the memory in use
// and the open files at that size.
//
// It runs once per CI run, without the race detector, as its own step (make
// test-scale): under the detector it measured the detector, not the
// registry, and made the race step a third longer; the 32-bit step leaves it
// out for the same reason. The registry's concurrency is raced by the other
// tests of this package.
func TestScaleThousandTenants(t *testing.T) {
	if testing.Short() {
		t.Skip("1,000 tenants: skipped under -short")
	}
	if os.Getenv("NOPACS_TEST_SCALE") == "" {
		t.Skip("1,000 tenants: run by make test-scale, which sets NOPACS_TEST_SCALE")
	}
	if raceDetector {
		t.Skip("1,000 tenants: run without the race detector by make test-scale")
	}
	if runtime.GOARCH == "386" {
		t.Skip("1,000 tenants: run on 64 bits by make test-scale")
	}
	if runtime.GOOS == "windows" {
		// Nothing in the registry differs by system, a Windows minute of
		// CI costs two, and the open files and memory are not read there.
		t.Skip("1,000 tenants: measured on Linux (CI) and macOS, not Windows")
	}
	const n, maxOpening = 1000, 32
	fdsBefore := openFiles()
	var msBefore runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&msBefore)

	r, m, drv, took := startMany(t, n, maxOpening)
	for i := range n {
		if _, err := r.Store(fmt.Sprintf("t%04d", i)); err != nil {
			t.Fatalf("tenant %d is not ready: %v", i, err)
		}
	}
	if peak := drv.maxOpening.Load(); peak > maxOpening {
		t.Errorf("%d tenants opened at once, want at most %d", peak, maxOpening)
	}
	if w := m.writes.Load(); w != 0 {
		t.Errorf("%d state writes for tenants that stayed ready, want 0", w)
	}

	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	fds := openFiles()

	small, _, _, _ := startMany(t, 10, 8)
	at10 := testing.Benchmark(func(b *testing.B) { benchStore(b, small, "t0005") })

	// A restart: every database exists and is at this schema already, which
	// is what a server's start meets on every day but the first.
	closeBegin := time.Now()
	if err := r.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := time.Since(closeBegin)
	r, again := startOn(t, m, r.cfg.Keys, drv, maxOpening)
	for i := range n {
		if _, err := r.Store(fmt.Sprintf("t%04d", i)); err != nil {
			t.Fatalf("tenant %d is not ready after the restart: %v", i, err)
		}
	}
	at1000 := testing.Benchmark(func(b *testing.B) { benchStore(b, r, "t0500") })

	t.Logf("1,000 tenants ready in %v when each is created and migrated, %v when each exists (a restart); Close took %v; at most %d opening at once (limit %d)",
		took.Round(time.Millisecond), again.Round(time.Millisecond), closed.Round(time.Millisecond), drv.maxOpening.Load(), maxOpening)
	t.Logf("Go heap in use: %.1f MiB before, %.1f MiB after (%.1f KiB a tenant); memory from the system: %.1f MiB",
		mib(msBefore.HeapInuse), mib(ms.HeapInuse), float64(ms.HeapInuse-msBefore.HeapInuse)/1024/n, mib(ms.Sys))
	t.Logf("peak resident memory of the process: %.1f MiB (the SQLite library's own allocations included)", mib(uint64(max(peakRSS(), 0))))
	if fds >= 0 {
		t.Logf("open files: %d before, %d after (%.1f a tenant)", fdsBefore, fds, float64(fds-fdsBefore)/n)
	}
	t.Logf("Store: %d ns/op, %d allocs/op among 10; %d ns/op, %d allocs/op among 1,000",
		at10.NsPerOp(), at10.AllocsPerOp(), at1000.NsPerOp(), at1000.AllocsPerOp())
	if at1000.AllocsPerOp() != 0 {
		t.Errorf("Store allocates %d times among 1,000", at1000.AllocsPerOp())
	}
	// A map read does not grow with the map; a margin is left for a busy
	// machine, and a lock or a master call would be far past it.
	if at1000.NsPerOp() > 3*at10.NsPerOp()+50 {
		t.Errorf("Store costs %d ns among 1,000 and %d ns among 10", at1000.NsPerOp(), at10.NsPerOp())
	}
}

// mib returns bytes in MiB.
func mib(b uint64) float64 { return float64(b) / (1 << 20) }
