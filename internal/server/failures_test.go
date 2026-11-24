package server

import (
	"math"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestFailureTable guards the limits on failed calls: past the limit a
// refusal is slowed, at twice it a block starts once and lasts its time, a
// window that passed starts the count again, and the table never holds more
// callers than its bound nor forgets a block to make room.
func TestFailureTable(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tab := newFailureTable(3)
	var got []verdict
	for range 4 {
		got = append(got, tab.fail("a", now, 2, time.Minute, time.Hour))
	}
	if got[0].slow || got[1].slow || !got[2].slow || got[2].blocked || !got[3].blocked || !got[3].until.Equal(now.Add(time.Hour)) {
		t.Errorf("verdicts %+v", got)
	}
	if b, until := tab.blocked("a", now.Add(59*time.Minute)); !b || !until.Equal(now.Add(time.Hour)) {
		t.Error("not blocked within the block")
	}
	if b, _ := tab.blocked("a", now.Add(time.Hour)); b {
		t.Error("still blocked when the block ended")
	}
	tab.fail("b", now, 2, time.Minute, time.Hour)
	tab.fail("b", now, 2, time.Minute, time.Hour)
	if v := tab.fail("b", now.Add(2*time.Minute), 2, time.Minute, time.Hour); v.slow {
		t.Error("a failure after the window counted the old ones")
	}
	tab.fail("c", now, 2, time.Minute, time.Hour)
	// Full: a, b, c. A new caller makes room by forgetting one that is not
	// blocked, never a.
	if v := tab.fail("d", now, 2, time.Minute, time.Hour); v.slow || v.blocked {
		t.Errorf("d: %+v", v)
	}
	if n, _ := tab.size(); n != 3 {
		t.Errorf("%d callers held, bound 3", n)
	}
	if b, _ := tab.blocked("a", now.Add(time.Minute)); !b {
		t.Error("a block was forgotten to make room")
	}
	// A flood of new callers forgets the oldest counts, and never a block.
	for i := range 100 {
		tab.fail(floodAddress(9, i), now, 2, time.Minute, time.Hour)
	}
	if n, _ := tab.size(); n != 3 {
		t.Errorf("%d callers held after a flood, bound 3", n)
	}
	if b, _ := tab.blocked("a", now.Add(time.Minute)); !b {
		t.Error("a flood of new callers lifted a block")
	}

	// Blocks are bounded too: a table full of running blocks starts no
	// other (the refusal is still slowed, and counted as untracked); one
	// whose block has ended makes room.
	full := newFailureTable(1)
	full.fail("x", now, 1, time.Minute, time.Hour)
	if v := full.fail("x", now, 1, time.Minute, time.Hour); !v.blocked {
		t.Fatalf("x: %+v", v)
	}
	full.fail("y", now, 1, time.Minute, time.Hour)
	if v := full.fail("y", now, 1, time.Minute, time.Hour); v.blocked || !v.slow {
		t.Errorf("y in a table full of blocks: %+v, want slowed and not blocked", v)
	}
	if b, _ := full.blocked("x", now); !b {
		t.Error("x's block was forgotten for y's")
	}
	if _, untracked := full.size(); untracked != 1 {
		t.Errorf("%d blocks not started, want 1", untracked)
	}
	later := now.Add(2 * time.Hour)
	full.fail("y", later, 1, time.Minute, time.Hour)
	if v := full.fail("y", later, 1, time.Minute, time.Hour); !v.blocked {
		t.Errorf("y after x's block ended: %+v, want blocked", v)
	}
}

// TestBlockedNeverWaitsOnAFlood guards that the check every request makes,
// whether its caller is blocked, does not wait on the failures of a flood:
// with the counts' lock held, as a flood holds it, blocked still answers.
// Making room by a scan of the whole table under one lock that blocked
// shared held each request for milliseconds while a flood ran.
func TestBlockedNeverWaitsOnAFlood(t *testing.T) {
	now := time.Unix(1000, 0)
	tab := newFailureTable(4)
	tab.fail("a", now, 1, time.Minute, time.Hour)
	tab.fail("a", now, 1, time.Minute, time.Hour)
	tab.mu.Lock()
	defer tab.mu.Unlock()
	done := make(chan bool, 1)
	go func() {
		b, _ := tab.blocked("a", now)
		done <- b
	}()
	select {
	case b := <-done:
		if !b {
			t.Error("a is not blocked")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked waited on the counts' lock")
	}
}

// floodAddress returns the address of the i-th caller of flood number f, a
// distinct IPv6 /64 for each, so each counts as a new caller.
func floodAddress(f, i int) string {
	return addressKey(netip.AddrFrom16([16]byte{0x20, 0x01, 0x0d, 0xb8, byte(f), byte(i >> 16), byte(i >> 8), byte(i), 15: 1})) //nolint:gosec // G115: each byte is the low eight bits on purpose.
}

// fullTable returns a table of maxTrackedAddresses callers, each counting a
// failure at now.
func fullTable(now time.Time) *failureTable {
	tab := newFailureTable(maxTrackedAddresses)
	for i := range maxTrackedAddresses {
		tab.fail(floodAddress(0, i), now, 10, time.Minute, 15*time.Minute)
	}
	return tab
}

// BenchmarkFailNewCallerFullTable measures a failure of a caller not yet
// counted when the table is full of callers still counting, as in a flood
// of bad keys from many addresses: the cost of making room for it. Run with
//
//	go test -run '^$' -bench 'BenchmarkFailNewCallerFullTable|BenchmarkBlockedDuringFlood' ./internal/server
func BenchmarkFailNewCallerFullTable(b *testing.B) {
	now := time.Unix(1000, 0)
	tab := fullTable(now)
	b.ResetTimer()
	for i := range b.N {
		tab.fail(floodAddress(1, i), now.Add(time.Second), 10, time.Minute, 15*time.Minute)
	}
}

// BenchmarkBlockedDuringFlood measures the check every request off the
// local socket makes, whether its address is blocked, while four floods of
// new addresses fail into a full table at once.
func BenchmarkBlockedDuringFlood(b *testing.B) {
	now := time.Unix(1000, 0)
	tab := fullTable(now)
	var stop atomic.Bool
	var wg sync.WaitGroup
	for f := range 4 {
		wg.Go(func() {
			for i := 0; !stop.Load(); i++ {
				tab.fail(floodAddress(2+f, i), now.Add(time.Second), 10, time.Minute, 15*time.Minute)
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	b.ResetTimer()
	for range b.N {
		tab.blocked("198.51.100.1", now)
	}
	b.StopTimer()
	stop.Store(true)
	wg.Wait()
}

// TestFailureTableHugeLimit guards the block at twice the limit against an
// int overflow: on a 32-bit build 2 * (1 << 30) is negative, which blocked
// a caller at its first refusal.
func TestFailureTableHugeLimit(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	tab := newFailureTable(3)
	for _, limit := range []int{1 << 30, math.MaxInt} {
		v := tab.fail("huge", now, limit, time.Minute, time.Hour)
		if v.slow || v.blocked {
			t.Errorf("limit %d: the first refusal gave %+v, want neither slowed nor blocked", limit, v)
		}
	}
}
