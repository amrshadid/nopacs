package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// BenchmarkReceptionStepEvent measures one step of a reception (receiving
// to complete) with the event the store writes in its transaction, one
// transaction each, from one caller; it reports steps a second. Compare
// BenchmarkReceptionStep, the same step with a hook that does nothing:
// both write the event now, so the cost of the event was measured against
// the store before it wrote one (a synced commit of about 6 to 8 ms on a
// laptop, which the event did not change measurably).
func BenchmarkReceptionStepEvent(b *testing.B) {
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
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if _, err := s.StepReception(ctx, tenant.ReceptionStep{ID: ids[i], From: reception.Receiving, Event: reception.Quiet,
			At: benchTime.Add(time.Minute)}, nil); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "steps/s")
}

// The payloads of the rows the page benchmark writes straight through SQL,
// each of the size a real one has: a reception step, a file quarantined and
// a receiver's health.
const (
	benchQuarantinedPayload = `{"file":"q0123456789","reception":"rc0123456789","receiver":"dimse","reason":"not_dicom",` +
		`"size_bytes":524288,"held_files":12,"held_bytes":6291456}`
	benchHealthPayload = `{"receiver":"dimse","state":"down","down_reason":"address_in_use","detail":"the port is taken by another program",` +
		`"since":"2026-11-18T09:00:00Z"}`
	benchEventPayload = `{"reception":"rc0123456789","receiver":"dimse","arrival":1,"step":"archived",` +
		`"counts":{"files_received":3000,"bytes_received":1572864000,"files_archived":3000,"bytes_archived":1572864000,` +
		`"files_duplicate":0,"bytes_duplicate":0,"files_quarantined":0,"bytes_quarantined":0,"files_released":0},` +
		`"subject_label":"sub-0001","session_label":"ses-0001","retry_of":""}`
)

// BenchmarkReadEventsPage measures reading a page of 1000 events from a
// feed of 10 million, from a cursor in its middle: every type, one type
// that is a tenth of the feed, and two types; and a read at the end of the
// feed, which answers none, as a waiting subscriber's read does. The feed
// is built once, straight through SQL, which takes a while: the benchmark
// is skipped under -short and under the race detector, and run by itself
// (see the package's Makefile line).
func BenchmarkReadEventsPage(b *testing.B) {
	if testing.Short() || raceDetector {
		b.Skip("builds a feed of 10 million events; run without -short and without the race detector")
	}
	const total = 10_000_000
	s, path := benchTenant(b)
	ctx := context.Background()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	built := time.Now()
	if _, err := db.Exec(`WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?1)
		INSERT INTO events (type, at, payload, key) SELECT CASE i % 10 WHEN 0 THEN 'file_quarantined' WHEN 1 THEN 'receiver_health'
		ELSE 'reception_step' END, i, CASE i % 10 WHEN 0 THEN ?2 WHEN 1 THEN ?3 ELSE ?4 END, NULL FROM n`,
		total, benchQuarantinedPayload, benchHealthPayload, benchEventPayload); err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`ANALYZE`); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	b.Logf("built a feed of %d events in %v", total, time.Since(built).Round(time.Second))
	history, err := s.EventHistory(ctx)
	if err != nil {
		b.Fatal(err)
	}
	middle := s.eventCursor(total/2, history)
	end, err := s.EventCursor(ctx)
	if err != nil {
		b.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		from  string
		types []tenant.EventType
		want  int
	}{
		{"all types", middle, nil, 1000},
		{"one type of a tenth", middle, []tenant.EventType{tenant.EventFileQuarantined}, 1000},
		{"two types", middle, []tenant.EventType{tenant.EventFileQuarantined, tenant.EventReceiverHealth}, 1000},
		{"at the end", end, nil, 0},
	} {
		b.Run(c.name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				p, err := s.ReadEvents(ctx, c.from, c.types, 1000)
				if err != nil {
					b.Fatal(err)
				}
				if len(p.Events) != c.want {
					b.Fatalf("a page read %d events, want %d", len(p.Events), c.want)
				}
			}
		})
	}
}
