package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// BenchmarkKeyByID measures the first read of every request with a key.
func BenchmarkKeyByID(b *testing.B) { mastertest.BenchKeyByID(b, openTest) }

// BenchmarkGrantsByKey measures the second read of every request with a key.
func BenchmarkGrantsByKey(b *testing.B) { mastertest.BenchGrantsByKey(b, openTest) }

// BenchmarkRolesFor measures the access check's read of a key's roles.
func BenchmarkRolesFor(b *testing.B) { mastertest.BenchRolesFor(b, openTest) }

// BenchmarkAppendAudit measures the audit write every request ends with.
func BenchmarkAppendAudit(b *testing.B) { mastertest.BenchAppendAudit(b, openTest) }

// BenchmarkMarkKeyUsedNoWrite measures marking a busy key used within the
// minute, which writes nothing.
func BenchmarkMarkKeyUsedNoWrite(b *testing.B) { mastertest.BenchMarkKeyUsedNoWrite(b, openTest) }

// BenchmarkSetSettingByKey measures a change by a key, checked inside the
// change's transaction.
func BenchmarkSetSettingByKey(b *testing.B) { mastertest.BenchSetSettingByKey(b, openTest) }

// BenchmarkSetSettingByLocalOperator measures the same change by the local
// operator, whom no store checks.
func BenchmarkSetSettingByLocalOperator(b *testing.B) {
	mastertest.BenchSetSettingByLocalOperator(b, openTest)
}

// benchCallers are the numbers of concurrent callers the change benchmarks
// measure: one alone, which must not wait for company, and enough at once
// that changes can share a commit.
var benchCallers = []int{1, 8, 64, 256}

// benchChanges runs change from each of callers goroutines until b.N changes
// are made, for each number of benchCallers, and reports changes a second
// and the mean time a caller waits for its change to be answered. With one
// caller, ns/op is the latency of a change alone. change is given a number
// no other call in the benchmark is given, for unique audit ids. Each of
// after, when given, runs at the end of each run, to report more.
func benchChanges(b *testing.B, change func(ctx context.Context, n int64) error, after ...func(*testing.B)) {
	var seq atomic.Int64
	for _, callers := range benchCallers {
		b.Run(fmt.Sprintf("callers=%d", callers), func(b *testing.B) {
			ctx := context.Background()
			var done, waited atomic.Int64
			var wg sync.WaitGroup
			errs := make(chan error, callers)
			b.ReportAllocs()
			b.ResetTimer()
			start := time.Now()
			for range callers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for done.Add(1) <= int64(b.N) {
						t := time.Now()
						if err := change(ctx, seq.Add(1)); err != nil {
							errs <- err
							return
						}
						waited.Add(int64(time.Since(t)))
					}
				}()
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			close(errs)
			for err := range errs {
				b.Fatal(err)
			}
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "changes/s")
			b.ReportMetric(float64(waited.Load())/float64(b.N)/1e6, "ms-wait/change")
			for _, f := range after {
				f(b)
			}
		})
	}
}

// settingChange returns a change that sets one of 512 settings with its
// audit entry, the shape of every change: a row and the entry that records
// it, in one transaction.
func settingChange(set func(ctx context.Context, name, value string, audit master.AuditEntry) error) func(context.Context, int64) error {
	return func(ctx context.Context, n int64) error {
		a := master.AuditEntry{ID: fmt.Sprintf("bench-%09d", n), Time: mastertest.T0, Method: "Bench", Outcome: master.OutcomeDone}
		return set(ctx, fmt.Sprintf("bench.%03d", n%512), "v", a)
	}
}

// byLocalOperator returns s's SetSetting made by the local operator, whom no
// store checks, so these benchmarks measure the write path alone.
func byLocalOperator(s master.Store) func(ctx context.Context, name, value string, audit master.AuditEntry) error {
	return func(ctx context.Context, name, value string, audit master.AuditEntry) error {
		return s.SetSetting(ctx, name, value, audit, master.LocalOperator())
	}
}

// BenchmarkMasterChanges measures changes to the master, each a setting with
// its audit entry, from 1, 8, 64 and 256 concurrent callers.
func BenchmarkMasterChanges(b *testing.B) {
	benchChanges(b, settingChange(byLocalOperator(openTest(b))))
}

// BenchmarkMasterChangesOneInEightFails measures changes to the master when
// one in eight is refused (its audit id is taken): the refused change is
// rolled back to its savepoint, and answered after its batch's commit when a
// neighbor's write came before it. It also reports the mean time a refused
// change waits for its answer.
func BenchmarkMasterChangesOneInEightFails(b *testing.B) {
	s := openTest(b)
	ctx := context.Background()
	if err := s.SetSetting(ctx, "taken", "v", master.AuditEntry{ID: "bench-taken", Time: mastertest.T0, Method: "Bench", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
		b.Fatal(err)
	}
	set := settingChange(byLocalOperator(s))
	var refused, refusedWait atomic.Int64
	benchChanges(b, func(ctx context.Context, n int64) error {
		if n%8 != 0 {
			return set(ctx, n)
		}
		t := time.Now()
		err := s.SetSetting(ctx, "refused", "v", master.AuditEntry{ID: "bench-taken", Time: mastertest.T0, Method: "Bench", Outcome: master.OutcomeDone}, master.LocalOperator())
		if !errors.Is(err, master.ErrDuplicateAuditID) {
			return fmt.Errorf("a change with a taken audit id: %w", err)
		}
		refusedWait.Add(int64(time.Since(t)))
		refused.Add(1)
		return nil
	}, func(b *testing.B) {
		if n := refused.Swap(0); n > 0 {
			b.ReportMetric(float64(refusedWait.Swap(0))/float64(n)/1e6, "ms-wait/refused")
		}
	})
}

// BenchmarkRefusedAlone measures a refused change made by a caller alone,
// which no other change's write can precede, so it is answered without
// waiting for any commit, as before changes were grouped.
func BenchmarkRefusedAlone(b *testing.B) {
	s := openTest(b)
	ctx := context.Background()
	a := master.AuditEntry{ID: "bench-taken", Time: mastertest.T0, Method: "Bench", Outcome: master.OutcomeDone}
	if err := s.SetSetting(ctx, "taken", "v", a, master.LocalOperator()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := s.SetSetting(ctx, "refused", "v", a, master.LocalOperator()); !errors.Is(err, master.ErrDuplicateAuditID) {
			b.Fatal(err)
		}
	}
}

// BenchmarkWriteMachinery measures what the write path itself costs a
// caller alone, apart from the disk: a transaction that writes nothing, so
// that its commit has nothing to sync.
func BenchmarkWriteMachinery(b *testing.B) {
	m := openTest(b).(*Master)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if err := m.writeTx(ctx, "bench", func(*sql.Tx) error { return nil }); err != nil {
			b.Fatal(err)
		}
	}
}
