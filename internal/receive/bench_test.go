package receive

import (
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// backend is where a benchmark's tenant keeps its catalog and its bytes.
type backend struct {
	name string
	// opts returns the environment's options for one run, or skips b when
	// the service it needs is not there.
	opts func(b *testing.B) envOptions
}

// backends are the catalogs and storages a reception is measured on: SQLite
// with a directory always; Postgres and S3 when their services are named
// (testkit), each run on a schema or a prefix of its own.
var backends = []backend{
	{name: "sqlite-dir", opts: func(*testing.B) envOptions { return envOptions{} }},
	{name: "postgres-dir", opts: func(b *testing.B) envOptions {
		address, ok := testkit.PostgresSchema(b)
		if !ok {
			b.Skipf("%s is not set", testkit.PostgresEnv)
		}
		return envOptions{postgres: address}
	}},
	{name: "sqlite-s3", opts: func(b *testing.B) envOptions {
		store := openTestS3(b, "bench-"+newID()[:12]+"/")
		return envOptions{storage: func(blob.Store) blob.Store { return store }}
	}},
}

// benchReception hands files to a pipeline, each made by the generator, and
// measures the time from the first landing to the reception verified and
// done: files and MiB a second.
func benchReception(b *testing.B, be backend, files int, pixelBytes int64) {
	g := newGenerator(b)
	var total int64
	var elapsed time.Duration
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		opts := be.opts(b)
		opts.workers = 4
		e := newEnv(b, opts)
		e.deadline = 20 * time.Minute
		b.StartTimer()
		start := time.Now()
		for n := range files {
			r, size := g.File(n, pixelBytes)
			e.sendReader(r)
			total += size
		}
		e.clock.Advance(10 * time.Second)
		e.settleFiles(files)
		elapsed += time.Since(start)
		b.StopTimer()
		if held := e.held(); len(held) > 0 {
			b.Fatalf("%d files held: %+v", len(held), held[0])
		}
		_ = e.p.Stop(e.ctx)
		b.StartTimer()
	}
	sec := elapsed.Seconds()
	b.ReportMetric(float64(files*b.N)/sec, "files/s")
	b.ReportMetric(float64(total)/(1<<20)/sec, "MiB/s")
}

// settleFiles waits until every reception has ended and n files are
// archived across them: a reception that went quiet while the files still
// came ends before the rest are archived by the next arrival, so the end of
// the first reception is not the end of the work.
func (e *env) settleFiles(n int) {
	e.t.Helper()
	e.waitFor(fmt.Sprintf("%d files archived", n), 3*time.Second, func() bool {
		archived := int64(0)
		for _, r := range e.receptions() {
			if !r.State.Ended() {
				return false
			}
			archived += r.Counts.FilesArchived
		}
		return archived >= int64(n) && e.p.batch.idle()
	})
}

// BenchmarkLandToVerified measures files and bytes a second from landing to
// verified, on each backend: a reception of 200 slices of 256 KiB, and one
// file of 256 MiB. Its sqlite-dir case is the one CI holds against the base
// branch (make bench-gate).
func BenchmarkLandToVerified(b *testing.B) {
	for _, be := range backends {
		b.Run(be.name+"/slices-256KiB", func(b *testing.B) { benchReception(b, be, 200, 256<<10) })
		b.Run(be.name+"/one-256MiB", func(b *testing.B) { benchReception(b, be, 1, 256<<20) })
	}
}

// BenchmarkLandToVerifiedDeidentified is BenchmarkLandToVerified's slices
// at a receiver that de-identifies, on SQLite and a directory: each file
// de-identified beside its original, checked by the check after, put and
// read back, so its cost beside the as-received case is what
// de-identification at reception costs.
func BenchmarkLandToVerifiedDeidentified(b *testing.B) {
	be := backend{name: "sqlite-dir-deid", opts: func(*testing.B) envOptions { return envOptions{deid: &deidOptions{}} }}
	b.Run(be.name+"/slices-256KiB", func(b *testing.B) { benchReception(b, be, 200, 256<<10) })
}

// BenchmarkLand measures the time a sender waits for each acknowledgement:
// one file of 256 KiB landed, synced and answered.
func BenchmarkLand(b *testing.B) {
	g := newGenerator(b)
	e := newEnv(b, envOptions{})
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, size := g.File(i%8000, 256<<10)
		b.SetBytes(size)
		e.sendReader(r)
	}
}

// BenchmarkDeidentifyAFile measures the stage a receiver that
// de-identifies adds to each file of the archive step, apart from the
// fsyncs of landing and storing that dominate a reception: a public CT
// slice's header read, the engine run over it, the de-identified copy
// written beside it and the check after's read of it.
func BenchmarkDeidentifyAFile(b *testing.B) {
	e := newEnv(b, envOptions{deid: &deidOptions{}, manual: true})
	b.Cleanup(func() { _ = e.p.area.Close() })
	body := corpus(b, studyFiles[0])
	f, err := e.p.area.Land(e.ctx, "dimse", bytesReader(body), 1<<30)
	if err != nil {
		b.Fatal(err)
	}
	inst := ""
	if h, err := header.Read(bytesReader(body), int64(len(body))); err == nil {
		inst, _ = h.UID(header.SOPInstanceUID)
	}
	st, se, _ := fileUIDsB(b, body)
	wr := written{f: tenant.ReceivedFile{SOPInstanceUID: remapped(inst), StudyUID: remapped(st), SeriesUID: remapped(se),
		Incoming: f.Path, Size: f.Size, SHA256: keyedSum(testDeidKeys, f.SHA256)}}
	rcv, err := e.store.Receiver(e.ctx, "dimse")
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(f.Size)
	b.ResetTimer()
	for range b.N {
		out, q, err := e.p.prepareOne(e.ctx, tenant.Reception{ID: "r", Receiver: "dimse"}, rcv, ownPath(b, e, rcv, body, "ses-a", "scan-a"), wr)
		if err != nil || q != nil || !out.made {
			b.Fatalf("prepared %+v, held %+v: %v", out, q, err)
		}
	}
}
