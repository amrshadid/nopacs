package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// benchTime is the time the pipeline's benchmarks start at.
var benchTime = time.Date(2026, 11, 18, 9, 0, 0, 0, time.UTC)

// benchTenant opens a migrated tenant database in tb's temporary directory,
// with a storage and a receiver, and returns it and its path.
func benchTenant(tb testing.TB) (*Tenant, string) {
	tb.Helper()
	ctx := context.Background()
	path := filepath.Join(tb.TempDir(), "tenant.db")
	s, err := OpenTenant(ctx, path, "bench")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = s.Close() })
	if err := s.Migrate(ctx); err != nil {
		tb.Fatal(err)
	}
	audit := func(id string) master.AuditEntry {
		return master.AuditEntry{ID: id, Time: benchTime, Method: "Bench", Outcome: master.OutcomeDone}
	}
	if _, err := s.AddStorage(ctx, tenant.Storage{Name: "store", Type: tenant.StorageDirectory, Directory: tenant.DirectoryStorage{Root: "fast"}},
		benchTime, audit("a1"), nil); err != nil {
		tb.Fatal(err)
	}
	r := tenant.Receiver{Name: "dimse", Type: tenant.ReceiverDIMSE, Storage: "store", DIMSE: tenant.DIMSEReceiver{Port: 11112, AETitle: "ARCHIVE"},
		QuietPeriod: tenant.DefaultQuietPeriod, MaxOpenTime: tenant.DefaultMaxOpenTime, MaxFileBytes: tenant.DefaultMaxFileBytes}
	if _, err := s.AddReceiver(ctx, r, benchTime, audit("a2"), nil); err != nil {
		tb.Fatal(err)
	}
	return s, path
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

// BenchmarkReceivedBatch measures recording received files in batches of
// 1, 100 and 1000 new files, each batch one transaction, as a receiver
// writes them; it reports rows a second.
func BenchmarkReceivedBatch(b *testing.B) {
	for _, size := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("rows=%d", size), func(b *testing.B) {
			s, _ := benchTenant(b)
			ctx := context.Background()
			r, _, err := s.OpenReception(ctx, "dimse", "2.25.1.1", "rc1", benchTime)
			if err != nil {
				b.Fatal(err)
			}
			batch := make([]tenant.ReceivedFile, size)
			n := 0
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				for i := range batch {
					batch[i] = benchFile(r, n)
					n++
				}
				if _, err := s.AddReceivedFiles(ctx, batch, benchTime, nil); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(n)/b.Elapsed().Seconds(), "rows/s")
		})
	}
}

// fillHeld writes a million held instances into a tenant's catalog (1000
// sessions of one scan of 1000 instances), straight through SQL, and an
// identified reception to claim with, and runs ANALYZE.
func fillHeld(b *testing.B, path string) {
	b.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, stmt := range []string{
		`INSERT INTO subjects (id, label, folded, created) VALUES (1, 'sub-01', 'sub-01', 0)`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
		 INSERT INTO sessions (id, study_uid, subject_id, label, folded, storage, created)
		 SELECT i, '2.25.1.' || i, 1, 'ses-' || i, 'ses-' || i, 'store', 0 FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 1000)
		 INSERT INTO scans (id, session_id, series_uid, label, folded, instances, bytes, manifest_sha256, created, updated, storage, study_uid)
		 SELECT i, i, '2.25.2.' || i, 'scan-' || i, 'scan-' || i, 1000, 524288000, NULL, 0, 0, 'store', '2.25.1.' || i FROM n`,
		`WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM n WHERE i < 999999)
		 INSERT INTO instances SELECT i / 1000 + 1, '2.25.3.' || (i + 1), 'held' || (i / 1000), randomblob(32), NULL, '', 0, 524288, 524288, 0, NULL, 0
		 FROM n`,
		`UPDATE catalog_totals SET subjects = 1, sessions = 1000, scans = 1000, instances = 1000000`,
		`INSERT INTO receptions SELECT 'claimer', 'dimse', '2.25.1.5000', 1, 'identified', 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
		   'sub-01', 'ses-5000', 'store', '', 0, 0, 0, NULL, 0, '[]', 0, '', '', '', NULL, ''`,
		`ANALYZE`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			b.Fatal(err)
		}
	}
}

// queriesOf returns size duplicate queries, every other one an instance
// held (spread across the table) and the others new.
func queriesOf(size, round int) []tenant.InstanceClaim {
	out := make([]tenant.InstanceClaim, size)
	for i := range out {
		n, study := i*997%1000000, 0
		if i%2 == 1 {
			n = 2000000 + round*size + i
			study = 5000 - 1
		} else {
			study = n / 1000
		}
		out[i] = tenant.InstanceClaim{SOPInstanceUID: fmt.Sprintf("2.25.3.%d", n+1),
			StudyUID: fmt.Sprintf("2.25.1.%d", study+1), SeriesUID: fmt.Sprintf("2.25.2.%d", study+1), SHA256: tenant.SHA256{1}}
	}
	return out
}

// BenchmarkCheckDuplicates measures the duplicate check of batches of 1,
// 100 and 1000 files, half of them held, against a catalog of one million
// held instances, built once; skipped under -short.
func BenchmarkCheckDuplicates(b *testing.B) {
	if testing.Short() {
		b.Skip("builds a table of one million rows")
	}
	s, path := benchTenant(b)
	fillHeld(b, path)
	ctx := context.Background()
	for _, size := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("files=%d", size), func(b *testing.B) {
			claims := queriesOf(size, 0)
			queries := make([]tenant.DuplicateQuery, size)
			for i, c := range claims {
				queries[i] = c.Query()
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				got, err := s.CheckDuplicates(ctx, queries)
				if err != nil {
					b.Fatal(err)
				}
				if got[0].Kind != tenant.DifferentFile {
					b.Fatalf("a held instance answered %v", got[0].Kind)
				}
			}
			b.ReportMetric(float64(size*b.N)/b.Elapsed().Seconds(), "files/s")
		})
	}
}

// BenchmarkClaimInstances measures a claim of 1, 100 and 1000 instances,
// half of them held (answered at once) and half new (inserted, claimed),
// one transaction each, against a catalog of one million held instances;
// skipped under -short.
func BenchmarkClaimInstances(b *testing.B) {
	if testing.Short() {
		b.Skip("builds a table of one million rows")
	}
	s, path := benchTenant(b)
	fillHeld(b, path)
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	// receive records the claims as files the claiming reception received,
	// straight through SQL and outside the timed part: a claim names only
	// files its reception received.
	receive := func(claims []tenant.InstanceClaim) {
		tx, err := db.Begin()
		if err != nil {
			b.Fatal(err)
		}
		for _, c := range claims {
			if _, err := tx.Exec(`INSERT INTO received_files VALUES ('claimer', ?, ?, ?, ?, '1.2.840.10008.5.1.4.1.1.4',
				'1.2.840.10008.1.2.1', 524288, 'x', '', '', '', '', 0, 0, 0, 0) ON CONFLICT DO NOTHING`,
				c.SOPInstanceUID, c.SHA256[:], c.SeriesUID, c.StudyUID); err != nil {
				b.Fatal(err)
			}
		}
		if err := tx.Commit(); err != nil {
			b.Fatal(err)
		}
	}
	round := 0
	for _, size := range []int{1, 100, 1000} {
		b.Run(fmt.Sprintf("instances=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				round++
				b.StopTimer()
				claims := queriesOf(size, round)
				receive(claims)
				b.StartTimer()
				if _, err := s.ClaimInstances(ctx, "claimer", claims, benchTime, nil); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(size*b.N)/b.Elapsed().Seconds(), "instances/s")
		})
	}
}

// BenchmarkCommitScan measures one scan's catalog commit of 100 and 1000
// instances (a new subject, session and scan, and the confirmation of each
// instance), one transaction each, against a catalog of one million held
// instances; it reports instances a second. Skipped under -short.
func BenchmarkCommitScan(b *testing.B) {
	if testing.Short() {
		b.Skip("builds a table of one million rows")
	}
	s, path := benchTenant(b)
	fillHeld(b, path)
	ctx := context.Background()
	seq := 0
	for _, size := range []int{100, 1000} {
		b.Run(fmt.Sprintf("instances=%d", size), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				seq++
				study, series := fmt.Sprintf("2.25.11.%d", seq), fmt.Sprintf("2.25.12.%d", seq)
				r, _, err := s.OpenReception(ctx, "dimse", study, fmt.Sprintf("commit%d", seq), benchTime)
				if err != nil {
					b.Fatal(err)
				}
				files := make([]tenant.ReceivedFile, size)
				for i := range files {
					files[i] = tenant.ReceivedFile{ReceptionID: r.ID, SOPInstanceUID: fmt.Sprintf("2.25.13.%d.%d", seq, i+1),
						SeriesUID: series, StudyUID: study, SOPClassUID: "1.2.840.10008.5.1.4.1.1.4",
						TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 524288, SHA256: tenant.SHA256{2},
						Incoming: fmt.Sprintf("%s/%d.dcm", r.ID, i)}
				}
				if _, err := s.AddReceivedFiles(ctx, files, benchTime, nil); err != nil {
					b.Fatal(err)
				}
				for _, ev := range []reception.Event{reception.Quiet, reception.Identify} {
					st := tenant.ReceptionStep{ID: r.ID, From: r.State, Event: ev, At: benchTime, SubjectLabel: "sub-01",
						SessionLabel: fmt.Sprintf("ses-b%d", seq), Storage: "store"}
					if r, err = s.StepReception(ctx, st, nil); err != nil {
						b.Fatal(err)
					}
				}
				claims := make([]tenant.InstanceClaim, size)
				path := label.Path{Subject: "sub-01", Session: fmt.Sprintf("ses-b%d", seq), Scan: "scan-1"}
				c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: study, SeriesUID: series, SubjectLabel: path.Subject,
					SessionLabel: path.Session, ScanLabel: path.Scan, Storage: "store", ManifestSHA256: tenant.SHA256{9}, At: benchTime}
				for i := range claims {
					uid := fmt.Sprintf("2.25.13.%d.%d", seq, i+1)
					claims[i] = tenant.InstanceClaim{SOPInstanceUID: uid, StudyUID: study, SeriesUID: series, SHA256: tenant.SHA256{2}}
					key, err := layout.InstanceKey(path, uid)
					if err != nil {
						b.Fatal(err)
					}
					c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: uid, Key: key, Length: 524288, Size: 524288,
						ArchivedSHA256: tenant.SHA256{2}})
				}
				if _, err := s.ClaimInstances(ctx, r.ID, claims, benchTime, nil); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := s.CommitScan(ctx, c); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(size*b.N)/b.Elapsed().Seconds(), "instances/s")
		})
	}
}

// BenchmarkReceptionStep measures one step of a reception (receiving to
// complete, compared and set, with a hook), one transaction each, from one
// caller; it reports steps a second.
func BenchmarkReceptionStep(b *testing.B) {
	s, _ := benchTenant(b)
	ctx := context.Background()
	ids := make([]string, b.N)
	for i := range ids {
		r, _, err := s.OpenReception(ctx, "dimse", fmt.Sprintf("2.25.1.%d", i+1), fmt.Sprintf("rc%d", i), benchTime)
		if err != nil {
			b.Fatal(err)
		}
		ids[i] = r.ID
	}
	hook := func(context.Context, tenant.Reception, tenant.Reception) error { return nil }
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := s.StepReception(ctx, tenant.ReceptionStep{ID: ids[i], From: reception.Receiving, Event: reception.Quiet,
			At: benchTime.Add(time.Minute)}, hook); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "steps/s")
}

// BenchmarkClaimWork measures claiming a unit of work, one transaction
// each, from a table that also holds 100,000 finished units, which the
// claim's partial index leaves out; it reports claims a second.
func BenchmarkClaimWork(b *testing.B) {
	s, path := benchTenant(b)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 100000)
		INSERT INTO work SELECT 'done' || i, 'reception', 'rc' || i, '', NULL, 'done', i, 1, 10, '', '', 0, 0 FROM n`); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?)
		INSERT INTO work SELECT 'q' || i, 'reception', 'rc' || i, '', NULL, 'queued', i, 0, 10, '', '', 0, 0 FROM n`, b.N); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := s.ClaimWork(ctx, "reception", "worker-1", benchTime, time.Minute); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "claims/s")
}
