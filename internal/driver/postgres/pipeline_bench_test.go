package postgres

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The pipeline's benchmarks, the SQLite driver's own (internal/driver/sqlite,
// pipeline_bench_test.go) on the same operations and sizes, each from one
// caller and from sixteen at once: Postgres has several writers, so sixteen
// callers show what the row locks let run side by side.

// benchTime is the time the pipeline's benchmarks start at.
var benchTime = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// benchCallers are the numbers of callers each benchmark runs with.
var benchCallers = []int{1, 16}

// benchTenant opens a migrated tenant database on a new schema with a pool
// for sixteen callers, with a storage and a receiver, and returns it and a
// connection to its schema.
func benchTenant(b *testing.B) (*Tenant, *pgx.Conn) {
	b.Helper()
	ctx := context.Background()
	address := withParams(b, newSchema(b), map[string]string{"pool_max_conns": "20"})
	s := openTenantAt(b, address, "bench")
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: benchTime, Method: "Bench", Outcome: master.OutcomeDone}
	}
	if _, err := s.AddStorage(ctx, tenant.Storage{Name: "store", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast"}},
		benchTime, audit("a1"), nil); err != nil {
		b.Fatal(err)
	}
	r := tenant.Receiver{Name: "dimse", Type: tenant.ReceiverDIMSE, Storage: "store", DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE"},
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes}
	if _, err := s.AddReceiver(ctx, r, benchTime, audit("a2"), nil); err != nil {
		b.Fatal(err)
	}
	return s, admin(b, address)
}

// benchFile returns received file n of reception r.
func benchFile(r tenant.Reception, n int) tenant.ReceivedFile {
	var sum tenant.SHA256
	for i := range sum {
		sum[i] = byte(n >> (8 * (i % 4)) & 0xff)
	}
	sum[31] = 1
	return tenant.ReceivedFile{
		ReceptionID: r.ID, SOPInstanceUID: fmt.Sprintf("2.25.3.%d", n+1), SeriesUID: "2.25.2.1", StudyUID: r.StudyUID,
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 524288, SHA256: sum,
		Incoming: fmt.Sprintf("%s/%08d.dcm", r.ID, n), Caller: tenant.Caller{CallingAETitle: "SCANNER1", Address: "192.0.2.7:51234"},
		Association: "assoc1",
	}
}

// runCallers runs b.N operations split among callers goroutines; op is
// given its caller and a number unique to the operation. The time is the
// whole run's.
func runCallers(b *testing.B, callers int, op func(caller, n int) error) {
	b.Helper()
	var next atomic.Int64
	var wg sync.WaitGroup
	var failed atomic.Value
	b.ResetTimer()
	for c := range callers {
		wg.Go(func() {
			for {
				n := int(next.Add(1)) - 1
				if n >= b.N {
					return
				}
				if err := op(c, n); err != nil {
					failed.Store(err)
					return
				}
			}
		})
	}
	wg.Wait()
	b.StopTimer()
	if err, ok := failed.Load().(error); ok {
		b.Fatal(err)
	}
}

// BenchmarkReceivedBatch measures recording received files in batches of 1,
// 100 and 1000 new files, each batch one transaction, as a receiver writes
// them, each caller into a reception of its own; it reports rows a second.
func BenchmarkReceivedBatch(b *testing.B) {
	for _, callers := range benchCallers {
		for _, size := range []int{1, 100, 1000} {
			b.Run(fmt.Sprintf("callers=%d/rows=%d", callers, size), func(b *testing.B) {
				s, _ := benchTenant(b)
				ctx := context.Background()
				recs := make([]tenant.Reception, callers)
				for c := range recs {
					r, _, err := s.OpenReception(ctx, "dimse", fmt.Sprintf("2.25.1.%d", c+1), fmt.Sprintf("rc%d", c), benchTime)
					if err != nil {
						b.Fatal(err)
					}
					recs[c] = r
				}
				b.ReportAllocs()
				runCallers(b, callers, func(c, n int) error {
					batch := make([]tenant.ReceivedFile, size)
					for i := range batch {
						batch[i] = benchFile(recs[c], n*size+i)
					}
					_, err := s.AddReceivedFiles(ctx, batch, benchTime)
					return err
				})
				b.ReportMetric(float64(b.N*size)/b.Elapsed().Seconds(), "rows/s")
			})
		}
	}
}

// fillHeld writes a million held instances into a tenant's catalog (1000
// sessions of one scan of 1000 instances) and sixteen identified receptions
// to claim with, straight through SQL, and runs ANALYZE.
func fillHeld(b *testing.B, conn *pgx.Conn) {
	b.Helper()
	mustExec(b, conn,
		`INSERT INTO subjects (label, folded, created) VALUES ('sub-01', 'sub-01', now())`,
		`INSERT INTO sessions (study_uid, subject_id, label, folded, storage, created)
		 SELECT '2.25.1.' || i, 1, 'ses-' || i, 'ses-' || i, 'store', now() FROM generate_series(1, 1000) i`,
		`INSERT INTO scans (session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated, storage, study_uid)
		 SELECT i, '2.25.2.' || i, 'scan-' || i, 'scan-' || i, 1000, 524288000, NULL, now(), now(), 'store', '2.25.1.' || i
		 FROM generate_series(1, 1000) i`,
		`INSERT INTO instances SELECT '2.25.3.' || (i + 1), i / 1000 + 1, 'held' || (i / 1000), sha256(i::text::bytea), NULL, '', 0, 524288,
		   524288, now() FROM generate_series(0, 999999) i`,
		`UPDATE catalog_totals SET subjects = 1, sessions = 1000, scans = 1000, instances = 1000000`,
		`INSERT INTO receptions SELECT 'claimer' || c, 'dimse', '2.25.1.' || (5000 + c), 1, 'identified', false, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		   'sub-01', 'ses-' || (5000 + c), 'store', '', now(), now(), now(), NULL, 0, '[]', false, '', '', '', NULL, '' FROM generate_series(0, 15) c`,
		`ANALYZE`)
}

// claimsOf returns size claims for caller c's operation n, every other one
// an instance held (spread across the table) and the others new.
func claimsOf(c, n, size int) []tenant.InstanceClaim {
	out := make([]tenant.InstanceClaim, size)
	for i := range out {
		held, study := (n*size+i)*997%1000000, 0
		uid := fmt.Sprintf("2.25.3.%d", held+1)
		if i%2 == 1 {
			uid = fmt.Sprintf("2.25.9.%d.%d.%d", c, n, i)
			study = 5000 + c - 1
		} else {
			study = held / 1000
		}
		out[i] = tenant.InstanceClaim{SOPInstanceUID: uid, StudyUID: fmt.Sprintf("2.25.1.%d", study+1),
			SeriesUID: fmt.Sprintf("2.25.2.%d", study+1), SHA256: tenant.SHA256{1}}
	}
	return out
}

// BenchmarkClaimInstances measures a claim of 1, 100 and 1000 instances,
// half of them held (answered at once) and half new (inserted, claimed), one
// transaction each, against a catalog of one million held instances, each
// caller for a reception of its own; skipped under -short.
func BenchmarkClaimInstances(b *testing.B) {
	if testing.Short() {
		b.Skip("builds a table of one million rows")
	}
	for _, callers := range benchCallers {
		s, conn := benchTenant(b)
		fillHeld(b, conn)
		ctx := context.Background()
		for _, size := range []int{1, 100, 1000} {
			b.Run(fmt.Sprintf("callers=%d/instances=%d", callers, size), func(b *testing.B) {
				// The claims are first recorded as files their reception
				// received, outside the timed part: a claim names only those.
				all := make([][]tenant.InstanceClaim, b.N)
				var rids, uids, studies, series []string
				var sums [][]byte
				for n := range all {
					c := n % callers
					all[n] = claimsOf(c, n+size*1000000, size)
					for _, cl := range all[n] {
						rids, uids = append(rids, fmt.Sprintf("claimer%d", c)), append(uids, cl.SOPInstanceUID)
						studies, series, sums = append(studies, cl.StudyUID), append(series, cl.SeriesUID), append(sums, sumBytes(cl.SHA256))
					}
				}
				if _, err := conn.Exec(ctx, `INSERT INTO received_files SELECT r, u, h, se, st, '1.2.840.10008.5.1.4.1.1.4',
					'1.2.840.10008.1.2.1', 524288, 'x', '', '', '', '', false, now(), false
					FROM unnest($1::text[], $2::text[], $3::bytea[], $4::text[], $5::text[]) AS t(r, u, h, se, st) ON CONFLICT DO NOTHING`,
					rids, uids, sums, series, studies); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				runCallers(b, callers, func(_, n int) error {
					_, err := s.ClaimInstances(ctx, fmt.Sprintf("claimer%d", n%callers), all[n], benchTime, nil)
					return err
				})
				b.ReportMetric(float64(size*b.N)/b.Elapsed().Seconds(), "instances/s")
			})
		}
	}
}

// BenchmarkCommitScan measures one scan's catalog commit of 100 and 1000
// instances (a new session and scan of one subject, and the confirmation of
// each instance), one transaction each, against a catalog of one million
// held instances, each operation a reception of its own; it reports
// instances a second. Skipped under -short.
func BenchmarkCommitScan(b *testing.B) {
	if testing.Short() {
		b.Skip("builds a table of one million rows")
	}
	for _, callers := range benchCallers {
		s, conn := benchTenant(b)
		fillHeld(b, conn)
		ctx := context.Background()
		seq := 0
		for _, size := range []int{100, 1000} {
			b.Run(fmt.Sprintf("callers=%d/instances=%d", callers, size), func(b *testing.B) {
				// Each operation's reception, identified, with its claims,
				// made straight through SQL outside the timed part.
				commits := make([]tenant.ScanCommit, b.N)
				var ids, rstudies []string
				var uids, istudies, iseries, irecs []string
				for n := range commits {
					seq++
					id, study, series := fmt.Sprintf("commit%d", seq), fmt.Sprintf("2.25.11.%d", seq), fmt.Sprintf("2.25.12.%d", seq)
					p := label.Path{Subject: "sub-01", Session: fmt.Sprintf("ses-b%d", seq), Scan: "scan-1"}
					c := tenant.ScanCommit{ReceptionID: id, StudyUID: study, SeriesUID: series, SubjectLabel: p.Subject,
						SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "store", ManifestSHA256: tenant.SHA256{9}, At: benchTime}
					for i := range size {
						uid := fmt.Sprintf("2.25.13.%d.%d", seq, i+1)
						key, err := layout.InstanceKey(p, uid)
						if err != nil {
							b.Fatal(err)
						}
						c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: uid, Key: key, Length: 524288, Size: 524288,
							ArchivedSHA256: tenant.SHA256{2}})
						uids, istudies, iseries, irecs = append(uids, uid), append(istudies, study), append(iseries, series), append(irecs, id)
					}
					commits[n] = c
					ids, rstudies = append(ids, id), append(rstudies, study)
				}
				if _, err := conn.Exec(ctx, `INSERT INTO receptions SELECT id, 'dimse', st, 1, 'identified', false, 0, 0, 0, 0, 0, 0, 0, 0, 0,
					'sub-01', 'ses', 'store', '', now(), now(), now(), NULL, 0, '[]', false, '', '', '', NULL, ''
					FROM unnest($1::text[], $2::text[]) AS t(id, st)`, ids, rstudies); err != nil {
					b.Fatal(err)
				}
				if _, err := conn.Exec(ctx, `INSERT INTO instance_claims SELECT u, r, st, se, $5, now()
					FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS t(u, st, se, r)`,
					uids, istudies, iseries, irecs, sumBytes(tenant.SHA256{2})); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				runCallers(b, callers, func(_, n int) error {
					_, err := s.CommitScan(ctx, commits[n])
					return err
				})
				b.ReportMetric(float64(size*b.N)/b.Elapsed().Seconds(), "instances/s")
			})
		}
	}
}

// BenchmarkReceptionStep measures one step of a reception (receiving to
// complete, compared and set, with a hook), one transaction each; it
// reports steps a second.
func BenchmarkReceptionStep(b *testing.B) {
	for _, callers := range benchCallers {
		b.Run(fmt.Sprintf("callers=%d", callers), func(b *testing.B) {
			s, conn := benchTenant(b)
			ctx := context.Background()
			if _, err := conn.Exec(ctx, `INSERT INTO receptions SELECT 'rc' || i, 'dimse', '2.25.1.' || i, 1, 'receiving', false,
				0, 0, 0, 0, 0, 0, 0, 0, 0, '', '', '', '', $1, $1, $1, NULL, 0, '[]', false, '', '', '', NULL, ''
				FROM generate_series(0, $2::int - 1) i`, benchTime, b.N); err != nil {
				b.Fatal(err)
			}
			hook := func(context.Context, tenant.Reception, tenant.Reception) error { return nil }
			b.ReportAllocs()
			runCallers(b, callers, func(_, n int) error {
				_, err := s.StepReception(ctx, tenant.ReceptionStep{ID: fmt.Sprintf("rc%d", n), From: reception.Receiving, Event: reception.Quiet,
					At: benchTime.Add(time.Minute)}, hook)
				return err
			})
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "steps/s")
		})
	}
}

// BenchmarkClaimWork measures claiming a unit of work, one transaction
// each, from a table that also holds 100,000 finished units, which the
// claim's partial index leaves out; it reports claims a second.
func BenchmarkClaimWork(b *testing.B) {
	for _, callers := range benchCallers {
		b.Run(fmt.Sprintf("callers=%d", callers), func(b *testing.B) {
			s, conn := benchTenant(b)
			mustExec(b, conn, `INSERT INTO work SELECT 'done' || i, 'reception', 'rc' || i, '', NULL, 'done', now(), 1, 10, '', '', now(), now()
				FROM generate_series(1, 100000) i`)
			ctx := context.Background()
			if _, err := conn.Exec(ctx, `INSERT INTO work SELECT 'q' || i, 'reception', 'rc' || i, '', NULL, 'queued',
				$1::timestamptz + i * interval '1 microsecond', 0, 10, '', '', now(), now() FROM generate_series(1, $2::int) i`, benchTime, b.N); err != nil {
				b.Fatal(err)
			}
			mustExec(b, conn, `ANALYZE work`)
			b.ReportAllocs()
			runCallers(b, callers, func(c, _ int) error {
				_, err := s.ClaimWork(ctx, "reception", fmt.Sprintf("worker-%d", c), benchTime.Add(time.Hour), time.Minute)
				return err
			})
			b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "claims/s")
		})
	}
}
