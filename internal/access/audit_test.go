package access_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// refuse makes n requests with no key, each a refusal whose entry goes to
// the audit writer, with the method given.
func refuse(t testing.TB, a *access.Access, n int, method string) {
	t.Helper()
	for range n {
		_, err := a.Check(context.Background(), access.Credential{}, access.Authenticated(), facts(method))
		if fault.KindOf(err) != fault.Unauthenticated {
			t.Fatalf("refusal: %v", err)
		}
	}
}

// waitFor polls cond for up to two seconds, for what a worker does after
// the call that asked for it has returned.
func waitFor(t testing.TB, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// stored returns how many entries m kept, and the size of every append.
func (m *memStore) stored() (int, []int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.audit), append([]int(nil), m.batches...)
}

// kept returns the entries m kept whose method is method.
func (m *memStore) kept(method string) []master.AuditEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []master.AuditEntry
	for _, e := range m.audit {
		if e.Method == method {
			out = append(out, e)
		}
	}
	return out
}

// noticed returns the sum of the "dropped" details of the drop notices m
// kept.
func (m *memStore) noticed(t testing.TB) int {
	t.Helper()
	sum := 0
	for _, e := range m.kept("access.AuditWriter") {
		n, err := strconv.Atoi(e.Details["dropped"])
		if err != nil {
			t.Fatalf("notice %+v", e)
		}
		sum += n
	}
	return sum
}

// isNotice reports whether entries is one drop notice.
func isNotice(entries []master.AuditEntry) bool {
	return len(entries) == 1 && entries[0].Method == "access.AuditWriter"
}

// accounted returns every entry the stats count, which must equal the
// entries handed in.
func accounted(s access.AuditStats) uint64 {
	return s.Written + s.Failed + s.Dropped()
}

// TestAuditBatches guards batching: entries are appended in batches of the
// size given, a full batch is written without waiting for the interval, and
// FlushAudit writes the rest.
func TestAuditBatches(t *testing.T) {
	m, _ := newMem(t)
	a := newMemAccessWith(t, m, access.Config{AuditBatch: 3, AuditInterval: time.Hour})
	refuse(t, a, 7, "m")
	waitFor(t, "two full batches", func() bool { _, b := m.stored(); return len(b) == 2 })
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, batches := m.stored()
	if n != 7 || !reflect.DeepEqual(batches, []int{3, 3, 1}) {
		t.Fatalf("%d entries in batches %v, want 7 in [3 3 1]", n, batches)
	}
	if s := a.AuditStats(); s != (access.AuditStats{Written: 7}) {
		t.Fatalf("stats %+v", s)
	}
}

// TestAuditInterval guards the interval: a batch that does not fill is
// written when the interval passes, with no flush asked for.
func TestAuditInterval(t *testing.T) {
	m, _ := newMem(t)
	a := newMemAccessWith(t, m, access.Config{AuditBatch: 1000, AuditInterval: 10 * time.Millisecond})
	refuse(t, a, 2, "m")
	waitFor(t, "the interval's batch", func() bool { n, _ := m.stored(); return n == 2 })
}

// TestAuditFallback guards the fallback for a failure of an entry: when a
// batch fails because of one entry (here a duplicate id, as the port reports
// it), each entry is appended alone, once, so that entry is lost, counted
// and logged without a retry, and the others are kept.
func TestAuditFallback(t *testing.T) {
	m, _ := newMem(t)
	m.appendFn = func(_ context.Context, entries []master.AuditEntry) error {
		for _, e := range entries {
			if e.Method == "poison" {
				return fault.Wrap(fault.Internal, master.ErrDuplicateAuditID, "test: duplicate")
			}
		}
		return nil
	}
	a := newMemAccessWith(t, m, access.Config{AuditInterval: time.Hour})
	refuse(t, a, 2, "a")
	refuse(t, a, 1, "poison")
	refuse(t, a, 2, "b")
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, batches := m.stored()
	if n != 4 || !reflect.DeepEqual(batches, []int{5, 1, 1, 1, 1, 1}) {
		t.Fatalf("%d entries kept in calls %v, want 4 in [5 1 1 1 1 1]", n, batches)
	}
	if s := a.AuditStats(); s != (access.AuditStats{Written: 4, Failed: 1}) {
		t.Fatalf("stats %+v", s)
	}
}

// TestAuditUnavailableNoAmplification guards a store that is down: a batch
// is tried once and then again the number of retries given, as a batch,
// never entry by entry, and then dropped and counted.
func TestAuditUnavailableNoAmplification(t *testing.T) {
	m, _ := newMem(t)
	var calls atomic.Int64
	m.appendFn = func(_ context.Context, entries []master.AuditEntry) error {
		if !isNotice(entries) {
			calls.Add(1)
		}
		return fault.New(fault.Unavailable, "test: down")
	}
	a := newMemAccessWith(t, m, access.Config{AuditInterval: time.Hour, AuditRetries: 2, AuditBackoff: time.Millisecond})
	refuse(t, a, 50, "m")
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("%d appends for one batch of 50 to a store that is down, want 3 (one and two retries)", got)
	}
	if s := a.AuditStats(); s.DroppedWrites != 50 || accounted(s) != 50 {
		t.Fatalf("stats %+v", s)
	}
}

// TestAuditHangingStoreBounded guards a store that hangs: an append that runs
// out of time is not tried again, so a batch holds the writer one timeout.
func TestAuditHangingStoreBounded(t *testing.T) {
	m, _ := newMem(t)
	var calls atomic.Int64
	m.appendFn = func(ctx context.Context, entries []master.AuditEntry) error {
		if !isNotice(entries) {
			calls.Add(1)
		}
		<-ctx.Done()
		return fault.Wrap(fault.Unavailable, ctx.Err(), "test: hung")
	}
	const timeout = 200 * time.Millisecond
	a := newMemAccessWith(t, m, access.Config{AuditInterval: time.Hour, AuditTimeout: timeout, AuditRetries: 5, AuditBackoff: time.Millisecond})
	refuse(t, a, 20, "m")
	start := time.Now()
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	// One timeout for the batch and one for the notice of its drop, with
	// room for a loaded machine; retrying the timeout would take six.
	if d := time.Since(start); d > 5*timeout {
		t.Fatalf("flushing one batch to a hanging store took %v, want about %v", d, 2*timeout)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("%d appends of the batch, want 1: a timeout is not retried", got)
	}
	if s := a.AuditStats(); s.DroppedWrites != 20 {
		t.Fatalf("stats %+v", s)
	}
}

// TestAuditEveryThirdFails guards the retries: with every third append
// failing as Unavailable, every entry is still written.
func TestAuditEveryThirdFails(t *testing.T) {
	m, _ := newMem(t)
	var n atomic.Int64
	m.appendFn = func(context.Context, []master.AuditEntry) error {
		if n.Add(1)%3 == 0 {
			return fault.New(fault.Unavailable, "test: every third")
		}
		return nil
	}
	a := newMemAccessWith(t, m, access.Config{AuditBatch: 4, AuditInterval: time.Hour, AuditBackoff: time.Millisecond})
	refuse(t, a, 40, "m")
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := a.AuditStats(); s != (access.AuditStats{Written: 40}) {
		t.Fatalf("stats %+v, want all 40 written", s)
	}
}

// TestAuditDropsWhenFull guards the full queue: with the master stuck,
// refusals still answer at once, the entries past the queue are dropped and
// counted as refusals, and once the master answers again one entry says how
// many were dropped.
func TestAuditDropsWhenFull(t *testing.T) {
	m, _ := newMem(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	m.appendFn = func(ctx context.Context, entries []master.AuditEntry) error {
		if isNotice(entries) {
			return nil
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a := newMemAccessWith(t, m, access.Config{RefusalQueue: 2, AuditBatch: 1, AuditInterval: time.Hour})
	refuse(t, a, 1, "m")
	<-entered // the writer holds the first entry, stuck in the append
	start := time.Now()
	refuse(t, a, 7, "m") // two fill the queue, five are dropped
	if d := time.Since(start); d > time.Second {
		t.Fatalf("7 refusals took %v with the master stuck", d)
	}
	if got := a.AuditStats().DroppedRefusals; got != 5 {
		t.Fatalf("%d dropped, want 5", got)
	}
	close(release)
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.noticed(t); got != 5 {
		t.Fatalf("notices report %d dropped, want 5", got)
	}
	if s := a.AuditStats(); s != (access.AuditStats{Written: 3, DroppedRefusals: 5, Notices: 1}) {
		t.Fatalf("stats %+v", s)
	}
}

// TestRefusalsDoNotPushOutReads guards the separate queues: while the writer
// is held in an append, a flood of malformed keys interleaved with reads
// overflows the refusals' queue and drops refusals, and every read recorded
// meanwhile is kept and written once the writer moves again. It holds the
// writer on purpose rather than race it, so it does not depend on how a
// system schedules goroutines or how long it sleeps.
func TestRefusalsDoNotPushOutReads(t *testing.T) {
	m, full := newMem(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var held atomic.Bool
	held.Store(true)
	m.appendFn = func(ctx context.Context, entries []master.AuditEntry) error {
		if !held.Load() || isNotice(entries) {
			return nil
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a := newMemAccessWith(t, m, access.Config{RefusalQueue: 8, RecordQueue: 400, AuditBatch: 1, AuditInterval: time.Hour})
	_, _ = a.Check(context.Background(), access.APIKey("nopacs_flood"), access.Authenticated(), facts("flood"))
	<-entered // the writer is held with the first refusal
	const reads = 300
	for range reads {
		for range 5 {
			_, _ = a.Check(context.Background(), access.APIKey("nopacs_flood"), access.Authenticated(), facts("flood"))
		}
		d, err := a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("read"))
		if err != nil {
			t.Fatal(err)
		}
		a.Record(d, master.OutcomeDone, "", access.Facts{})
	}
	held.Store(false)
	close(release)
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := a.AuditStats()
	if got := len(m.kept("read")); got != reads || s.DroppedRecords != 0 {
		t.Fatalf("%d of %d reads written, stats %+v", got, reads, s)
	}
	// 1 + 1,500 refusals: one held, eight queued, the rest dropped.
	if s.DroppedRefusals != 1501-9 {
		t.Fatalf("stats %+v, want %d refusals dropped", s, 1501-9)
	}
	t.Logf("flood: %+v", s)
}

// TestFloodDuringClose guards the accounting while Close runs: entries and
// marks handed in by many goroutines while Close begins are each counted
// once, as written, failed or dropped, round after round.
func TestFloodDuringClose(t *testing.T) {
	for round := range 40 {
		m, full := newMem(t)
		a, err := access.New(access.Config{Store: m, Clock: func() time.Time { return t0 }, NewID: ids("f"),
			RefusalQueue: 64, RecordQueue: 64, MarkQueue: 16, AuditBatch: 16, AuditInterval: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		const workers, each = 8, 400
		var handedEntries, handedMarks atomic.Uint64
		var wg sync.WaitGroup
		for w := range workers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range each {
					if (w+i)%2 == 0 {
						_, _ = a.Check(context.Background(), access.Credential{}, access.Authenticated(), facts("refuse"))
						handedEntries.Add(1)
						continue
					}
					d, err := a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("read"))
					if err != nil {
						t.Error(err)
						return
					}
					handedMarks.Add(1) // the key is never marked, so each check asks
					a.Record(d, master.OutcomeDone, "", access.Facts{})
					handedEntries.Add(1)
				}
			}()
		}
		time.Sleep(time.Duration(round%5) * time.Millisecond)
		if err := a.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		wg.Wait()
		s, ms := a.AuditStats(), a.MarkStats()
		if got, want := accounted(s), handedEntries.Load(); got != want {
			t.Fatalf("round %d: %d entries accounted of %d handed in: %+v", round, got, want, s)
		}
		if got, want := ms.Written+ms.Dropped+ms.Failed, handedMarks.Load(); got != want {
			t.Fatalf("round %d: %d marks accounted of %d asked for: %+v", round, got, want, ms)
		}
	}
}

// TestFinalNoticeOnClose guards the last notice: drops not yet reported when
// Close runs are reported then, whatever the rate limit, here within the
// minute of an earlier notice.
func TestFinalNoticeOnClose(t *testing.T) {
	m, _ := newMem(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	m.appendFn = func(ctx context.Context, entries []master.AuditEntry) error {
		if isNotice(entries) {
			return nil
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	a := newMemAccessWith(t, m, access.Config{RefusalQueue: 1, AuditBatch: 1, AuditInterval: time.Hour})
	// An early drop, reported at once: the clock then stands still, so any
	// later notice falls within the minute of this one.
	refuse(t, a, 1, "m")
	<-entered
	refuse(t, a, 2, "m") // one queued, one dropped
	release <- struct{}{}
	release <- struct{}{}
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.noticed(t); got != 1 {
		t.Fatalf("the first notice reports %d, want 1", got)
	}
	// The queued entry's append also signaled entered, and nobody read it.
	// Left there, it would let the wait below return before the writer
	// holds the next entry, so that entry would sit in the queue and all
	// eleven refusals after it would be dropped: twelve in all, truly
	// dropped and truly reported, but not what this test sets up.
	select {
	case <-entered:
	default:
	}
	refuse(t, a, 1, "m")
	<-entered             // the writer holds this entry now, stuck in its append
	refuse(t, a, 11, "m") // one queued, ten dropped: eleven in all
	if got := a.AuditStats().DroppedRefusals; got != 11 {
		t.Fatalf("%d refusals dropped before Close, want 11", got)
	}
	close(release)
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.noticed(t); got != 11 {
		t.Fatalf("notices report %d dropped, want 11", got)
	}
}

// TestNoticeRateLimit guards the notice's rate limit and its advance: a
// second notice within the minute waits, the next one after it reports
// every drop since the last, and a notice the master did not take moves
// nothing, so it is tried again.
func TestNoticeRateLimit(t *testing.T) {
	m, _ := newMem(t)
	var failNotices atomic.Bool
	gate := make(chan struct{})
	var stuck atomic.Bool
	m.appendFn = func(ctx context.Context, entries []master.AuditEntry) error {
		if isNotice(entries) {
			if failNotices.Load() {
				return fault.New(fault.Unavailable, "test: notices fail")
			}
			return nil
		}
		if stuck.Load() {
			select {
			case <-gate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	clk := &clock{now: t0}
	a, err := access.New(access.Config{Store: m, Clock: clk.Now, NewID: ids("n"), RefusalQueue: 1, AuditBatch: 1, AuditInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	// drop makes n drops: the writer is held in an append while the queue
	// of one fills.
	drop := func(n uint64) {
		t.Helper()
		gate = make(chan struct{})
		stuck.Store(true)
		before := a.AuditStats().DroppedRefusals
		_, earlier := m.stored()
		refuse(t, a, 1, "m")
		// The count of appends grows by one when the writer takes this
		// phase's entry and stops in its append; a count from an earlier
		// phase would let the wait pass at once.
		waitFor(t, "the writer to hold this phase's entry", func() bool { _, b := m.stored(); return len(b) > len(earlier) })
		for a.AuditStats().DroppedRefusals < before+n {
			refuse(t, a, 1, "m")
		}
		stuck.Store(false)
		close(gate)
		if err := a.FlushAudit(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	failNotices.Store(true)
	drop(3)
	if s := a.AuditStats(); s.Notices != 0 {
		t.Fatalf("a notice the master refused was counted: %+v", s)
	}
	failNotices.Store(false)
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.noticed(t); got != 3 {
		t.Fatalf("after the master took notices again, %d reported, want 3", got)
	}
	drop(2)
	if got := m.noticed(t); got != 3 {
		t.Fatalf("a second notice within the minute: %d reported, want still 3", got)
	}
	clk.Set(t0.Add(61 * time.Second))
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := m.noticed(t); got != 5 {
		t.Fatalf("after the minute, %d reported, want 5", got)
	}
}

// TestCloseReturnsWithStoreIgnoringContext guards Close against a store that
// never returns and ignores its context: Close returns at its deadline,
// counts what is left as dropped, and logs one line.
func TestCloseReturnsWithStoreIgnoringContext(t *testing.T) {
	m, _ := newMem(t)
	never := make(chan struct{})
	t.Cleanup(func() { close(never) })
	entered := make(chan struct{}, 1)
	m.appendFn = func(context.Context, []master.AuditEntry) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-never
		return nil
	}
	var log bytes.Buffer
	var logMu sync.Mutex
	a, err := access.New(access.Config{Store: m, Clock: func() time.Time { return t0 }, NewID: ids("c"), AuditBatch: 1,
		AuditInterval: time.Hour, Log: slog.New(slog.NewTextHandler(lockedWriter{&logMu, &log}, nil))})
	if err != nil {
		t.Fatal(err)
	}
	refuse(t, a, 1, "m")
	<-entered
	refuse(t, a, 9, "m")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if err := a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Close took %v with a deadline of 100ms: it waited for the stuck store", d)
	}
	if s := a.AuditStats(); s.DroppedWrites != 10 || accounted(s) != 10 {
		t.Fatalf("stats %+v, want 10 dropped", s)
	}
	logMu.Lock()
	lines := strings.Count(log.String(), "Close's deadline passed")
	logMu.Unlock()
	if lines != 1 {
		t.Fatalf("%d lines about the deadline, want 1:\n%s", lines, log.String())
	}
}

// TestAuditDrainOnClose guards shutdown: Close writes every entry the writer
// holds before it returns.
func TestAuditDrainOnClose(t *testing.T) {
	m, _ := newMem(t)
	a := newMemAccessWith(t, m, access.Config{AuditInterval: time.Hour})
	refuse(t, a, 10, "m")
	if err := a.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.stored(); n != 10 {
		t.Fatalf("%d entries written by Close, want 10", n)
	}
	refuse(t, a, 1, "m")
	if s := a.AuditStats(); s != (access.AuditStats{Written: 10, DroppedRefusals: 1}) {
		t.Fatalf("stats %+v", s)
	}
}

// TestCloseDeadlineLeavesNoGoroutine guards a shutdown with a stuck store
// that honors its context: Close returns at its deadline, what it could not
// write is counted as dropped, and neither worker is left running.
func TestCloseDeadlineLeavesNoGoroutine(t *testing.T) {
	before := runtime.NumGoroutine()
	m, _ := newMem(t)
	m.appendFn = func(ctx context.Context, _ []master.AuditEntry) error {
		<-ctx.Done()
		return ctx.Err()
	}
	a, err := access.New(access.Config{Store: m, Clock: func() time.Time { return t0 }, NewID: ids("g"), AuditInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	refuse(t, a, 5, "m")
	access.WaitForWorkersAfterCancel(a)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: %v, want the deadline", err)
	}
	if s := a.AuditStats(); s != (access.AuditStats{DroppedWrites: 5}) {
		t.Fatalf("stats %+v, want exactly 5 dropped", s)
	}
	waitFor(t, "the workers to end, "+strconv.Itoa(before)+" goroutines before New", func() bool {
		return runtime.NumGoroutine() <= before
	})
}

// TestCloseCutTenantBatchNotMoved guards a tenant's batch that Close's
// deadline cuts short: it is counted as dropped, once, and not moved to the
// master's log as if the tenant's log had failed. Close waits for the
// writer to settle the batch before counting, the order in which the batch
// used to be counted as moved as well.
func TestCloseCutTenantBatchNotMoved(t *testing.T) {
	m, full := newMem(t)
	a := newMemAccessWith(t, m, access.Config{AuditInterval: time.Hour, AuditBackoff: time.Millisecond})
	d, err := a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("read"))
	if err != nil {
		t.Fatal(err)
	}
	lab := &hangingSink{entered: make(chan struct{}, 1)}
	for range 4 {
		a.RecordTo(lab, d, master.OutcomeDone, "", access.Facts{TenantName: "lab"})
	}
	go func() { _ = a.FlushAudit(context.Background()) }()
	<-lab.entered // the writer is in the tenant's append
	access.WaitForWorkersAfterCancel(a)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := a.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close: %v, want the deadline", err)
	}
	if s := a.AuditStats(); s != (access.AuditStats{DroppedWrites: 4}) {
		t.Fatalf("stats %+v, want exactly 4 dropped and none moved", s)
	}
	if got := len(m.kept("read")); got != 0 {
		t.Fatalf("%d entries moved to the master's log after Close's deadline", got)
	}
}

// hangingSink is a tenant's log whose append waits until its context ends.
type hangingSink struct {
	entered chan struct{}
}

// AppendAudit signals that it was called, then waits for its context.
func (s *hangingSink) AppendAudit(ctx context.Context, _ ...master.AuditEntry) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return fault.Wrap(fault.Unavailable, ctx.Err(), "test: the tenant's database hangs")
}

// TestRefusalOffPath guards that a refusal's entry is not written on the
// request's path: with every append taking 200 ms, five refusals still
// answer at once.
func TestRefusalOffPath(t *testing.T) {
	m, _ := newMem(t)
	m.appendFn = func(context.Context, []master.AuditEntry) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	}
	a := newMemAccessWith(t, m, access.Config{AuditBatch: 1, AuditInterval: time.Hour})
	start := time.Now()
	refuse(t, a, 5, "m")
	if d := time.Since(start); d > 150*time.Millisecond {
		t.Fatalf("5 refusals took %v with each append taking 200ms", d)
	}
}

// memSink is a tenant's audit log in memory, which can be made to fail.
type memSink struct {
	mu      sync.Mutex
	entries []master.AuditEntry
	down    bool
}

// AppendAudit keeps entries, or fails as Unavailable when the sink is down.
func (s *memSink) AppendAudit(_ context.Context, entries ...master.AuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.down {
		return fault.New(fault.Unavailable, "test: the tenant's database does not answer")
	}
	s.entries = append(s.entries, entries...)
	return nil
}

// TestRecordToTenant guards where an allowed request's entry goes: to the
// tenant's log it is recorded to, not the master's; and when the tenant's log
// does not answer, to the master's, marked as moved, with the tenant's name.
// A refusal goes to the master's log whatever it named, so a refused caller
// can never cause a write in a tenant's database.
func TestRecordToTenant(t *testing.T) {
	m, full := newMem(t)
	a := newMemAccessWith(t, m, access.Config{AuditInterval: time.Hour, AuditBackoff: time.Millisecond})
	lab := &memSink{}
	d, err := a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("read"))
	if err != nil {
		t.Fatal(err)
	}
	a.RecordTo(lab, d, master.OutcomeDone, "", access.Facts{TenantName: "lab"})
	refuse(t, a, 1, "refused")
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(lab.entries) != 1 || lab.entries[0].TenantName != "lab" || len(m.kept("read")) != 0 || len(m.kept("refused")) != 1 {
		t.Fatalf("tenant log %+v, master reads %d, master refusals %d", lab.entries, len(m.kept("read")), len(m.kept("refused")))
	}

	lab.down = true
	a.RecordTo(lab, d, master.OutcomeDone, "", access.Facts{TenantName: "lab"})
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	moved := m.kept("read")
	if len(moved) != 1 || moved[0].TenantName != "lab" || moved[0].Details[access.DetailRerouted] == "" {
		t.Fatalf("an entry for a tenant whose log is down: %+v", moved)
	}
	if s := a.AuditStats(); s.Rerouted != 1 || s.Written != 3 || accounted(s) != 3 {
		t.Fatalf("stats %+v", s)
	}
}

// unhashable is a sink that cannot be compared, which the writer could not
// batch by.
type unhashable struct {
	tags map[string]string
}

// AppendAudit keeps nothing; the writer must never reach it.
func (unhashable) AppendAudit(context.Context, ...master.AuditEntry) error { return nil }

// TestRecordToRefusesUncomparableSink guards RecordTo against a sink that
// cannot be a batch's key: the entry is counted as failed, nothing panics.
func TestRecordToRefusesUncomparableSink(t *testing.T) {
	m, full := newMem(t)
	a := newMemAccessWith(t, m, access.Config{AuditInterval: time.Hour})
	d, err := a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("read"))
	if err != nil {
		t.Fatal(err)
	}
	a.RecordTo(unhashable{tags: map[string]string{}}, d, master.OutcomeDone, "", access.Facts{})
	if s := a.AuditStats(); s.Failed != 1 {
		t.Fatalf("stats %+v", s)
	}
}

// TestValidatedAtEnqueue guards the validation at hand-off: an entry the
// master would refuse (here with the id a broken id source gave) is counted
// as failed and never joins a batch.
func TestValidatedAtEnqueue(t *testing.T) {
	m, full := newMem(t)
	n := 0
	a, err := access.New(access.Config{Store: m, Clock: func() time.Time { return t0 }, NewID: func() string {
		n++
		if n == 1 {
			return "good-1"
		}
		return "bad id"
	}, AuditInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close(context.Background()) }()
	d, err := a.Check(context.Background(), access.APIKey(full), access.Authenticated(), facts("read"))
	if err != nil {
		t.Fatal(err)
	}
	a.Record(d, master.OutcomeDone, "", access.Facts{})
	if err := a.FlushAudit(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s := a.AuditStats(); s != (access.AuditStats{Failed: 1}) {
		t.Fatalf("stats %+v", s)
	}
	if _, batches := m.stored(); len(batches) != 0 {
		t.Fatalf("an invalid entry reached the store: %v", batches)
	}
}
