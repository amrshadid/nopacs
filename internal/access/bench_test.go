package access_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
)

// The benchmarks of the check on the SQLite master, with the last-use worker
// and the audit writer running as a server runs them: the cost every later
// API call pays before its handler runs. They are reported, not claimed
// against a target yet. Run them without the race detector.

// benchEnv returns an env with a key that holds one grant, writer on lab,
// marked used a moment before, as a busy key is.
func benchEnv(b *testing.B) (*env, testKey, access.Requirement) {
	e := newEnv(b)
	k := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleWriter))
	if _, err := e.master.MarkKeyUsed(e.ctx, k.id, t0.Add(-time.Second), ""); err != nil {
		b.Fatal(err)
	}
	return e, k, access.OnTenant("lab", master.RoleReader)
}

// reportAudit reports the audit writer's entries written and dropped per
// operation.
func reportAudit(b *testing.B, e *env) {
	b.StopTimer()
	e.flush()
	s := e.access.AuditStats()
	b.ReportMetric(float64(s.Written)/float64(b.N), "written/op")
	b.ReportMetric(float64(s.Dropped())/float64(b.N), "dropped/op")
}

// BenchmarkCheck measures the check alone of a good key with one grant, on a
// tenant: parse, read the key, compare, read the roles and the tenant in one
// statement. The key was marked within the minute, so no mark is asked for.
func BenchmarkCheck(b *testing.B) {
	e, k, need := benchEnv(b)
	cred := access.APIKey(k.full)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.access.Check(e.ctx, cred, need, facts("bench")); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckAndRecord measures an allowed read in all, with the writer
// keeping up: the check, its entry handed to Record, and, every 256
// operations, a wait for the writer to write what it holds, inside the
// timing. So every entry is written (written/op is 1) and the time per
// operation includes its share of the batched, synced appends.
func BenchmarkCheckAndRecord(b *testing.B) {
	e, k, need := benchEnv(b)
	cred := access.APIKey(k.full)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		d, err := e.access.Check(e.ctx, cred, need, facts("bench"))
		if err != nil {
			b.Fatal(err)
		}
		e.access.Record(d, master.OutcomeDone, "", access.Facts{})
		if i++; i%256 == 0 {
			e.flush()
		}
	}
	reportAudit(b, e)
}

// BenchmarkCheckAndSyncedAudit measures the same read with its entry
// appended on the request's path, one synced commit each: what the audit
// writer saves a read.
func BenchmarkCheckAndSyncedAudit(b *testing.B) {
	e, k, need := benchEnv(b)
	cred := access.APIKey(k.full)
	b.ReportAllocs()
	for b.Loop() {
		d, err := e.access.Check(e.ctx, cred, need, facts("bench"))
		if err != nil {
			b.Fatal(err)
		}
		if err := e.master.AppendAudit(e.ctx, d.Entry(master.OutcomeDone, "", access.Facts{})); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheckRefused measures the refusal of a key with a known id and a
// wrong secret, alone: the cost of one bad key of a flood to the request's
// path. Its entries go to the writer, which does not keep up with a loop
// that does nothing else, so most are dropped and counted.
func BenchmarkCheckRefused(b *testing.B) {
	e, k, need := benchEnv(b)
	// The secret with its last character changed to another one, so the
	// secret is wrong whatever that character was.
	sec := k.full[len("nopacs_")+13:]
	last := "1"
	if strings.HasSuffix(sec, last) {
		last = "2"
	}
	cred := access.APIKey("nopacs_" + k.id + "_" + sec[:len(sec)-1] + last)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.access.Check(e.ctx, cred, need, facts("bench")); err == nil {
			b.Fatal("a wrong secret was allowed")
		}
	}
	reportAudit(b, e)
}

// BenchmarkCheckDenied measures the refusal of a good key without the role
// the request needs (admin on lab, where it is writer): the other refusal a
// flood can make, which reads the key and its roles before it refuses.
func BenchmarkCheckDenied(b *testing.B) {
	e, k, _ := benchEnv(b)
	cred := access.APIKey(k.full)
	need := access.OnTenant("lab", master.RoleAdmin)
	b.ReportAllocs()
	for b.Loop() {
		if _, err := e.access.Check(e.ctx, cred, need, facts("bench")); err == nil {
			b.Fatal("a key without the role was allowed")
		}
	}
	reportAudit(b, e)
}

// BenchmarkCheckParallel64 measures 64 goroutines making allowed reads at
// once, each a check and a Record, as a busy server does: checks per second
// over the whole run, and the entries written and dropped per operation.
func BenchmarkCheckParallel64(b *testing.B) {
	e, k, need := benchEnv(b)
	cred := access.APIKey(k.full)
	var next atomic.Int64
	total := int64(b.N)
	var wg sync.WaitGroup
	b.ResetTimer()
	start := time.Now()
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for next.Add(1) <= total {
				d, err := e.access.Check(context.Background(), cred, need, facts("bench"))
				if err != nil {
					b.Error(err)
					return
				}
				e.access.Record(d, master.OutcomeDone, "", access.Facts{})
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	b.ReportMetric(float64(b.N)/elapsed.Seconds(), "checks/s")
	reportAudit(b, e)
}

// BenchmarkCheckWithMark measures the check with its last-use mark written
// in line, the clock moved past the minute each time so every mark writes:
// what a request would cost if the mark were on its path, and the worst
// case of the last-use worker's load.
func BenchmarkCheckWithMark(b *testing.B) {
	e, k, need := benchEnv(b)
	cred := access.APIKey(k.full)
	ctx := context.Background()
	at := t0
	b.ReportAllocs()
	for b.Loop() {
		at = at.Add(61 * time.Second)
		e.clock.Set(at)
		d, err := e.access.Check(ctx, cred, need, facts("bench"))
		if err != nil {
			b.Fatal(err)
		}
		if _, err := e.master.MarkKeyUsed(ctx, d.Principal().KeyID(), at, ""); err != nil {
			b.Fatal(err)
		}
	}
}

// TestMeasureDeniedGap measures the two Denied cases, interleaved, 20,000
// times each on the SQLite master: a key naming a tenant it has no grant
// on, and the same key naming a tenant that does not exist. It is a
// measurement, not a check, so it runs only when NOPACS_MEASURE is set,
// and reports the median and the 90th percentile of each and the gap
// between the medians. Run it without the race detector.
func TestMeasureDeniedGap(t *testing.T) {
	if os.Getenv("NOPACS_MEASURE") == "" {
		t.Skip("a measurement; set NOPACS_MEASURE=1 to run it")
	}
	e := newEnv(t)
	k := e.key(time.Time{}, on(master.TenantScope(e.clinic.ID), master.RoleAdmin))
	if _, err := e.master.MarkKeyUsed(e.ctx, k.id, t0.Add(-time.Second), ""); err != nil {
		t.Fatal(err)
	}
	cred := access.APIKey(k.full)
	noGrant, noTenant := access.OnTenant("lab", master.RoleReader), access.OnTenant("nosuch", master.RoleReader)
	once := func(req access.Requirement) time.Duration {
		start := time.Now()
		_, err := e.access.Check(e.ctx, cred, req, facts("m"))
		d := time.Since(start)
		if err == nil {
			t.Fatal("allowed")
		}
		return d
	}
	for range 2000 {
		once(noGrant)
		once(noTenant)
	}
	const n = 20000
	a, b := make([]time.Duration, 0, n), make([]time.Duration, 0, n)
	for i := range n {
		if i%2 == 0 {
			a = append(a, once(noGrant))
			b = append(b, once(noTenant))
		} else {
			b = append(b, once(noTenant))
			a = append(a, once(noGrant))
		}
	}
	slices.Sort(a)
	slices.Sort(b)
	ma, mb := a[n/2], b[n/2]
	gap := 100 * (float64(mb) - float64(ma)) / float64(ma)
	t.Logf("no grant:  median %v, p90 %v", ma, a[n*9/10])
	t.Logf("no tenant: median %v, p90 %v", mb, b[n*9/10])
	t.Logf("gap between the medians: %+.1f%% (no tenant against no grant)", gap)
}
