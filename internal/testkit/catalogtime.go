package testkit

import (
	"context"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// CatalogProbe is one scan of a tenant the catalog's timings read, by its
// labels, with the SOP Instance UID of one instance it holds.
type CatalogProbe struct {
	Path label.Path
	UID  string
}

// TimeCatalogReads times each read of the catalog through the port, on a
// tenant made by GenerateTenant (its modalities MR and CT, its
// descriptions of research protocols, every reception through
// GenReceiver), rounds times each with a scan of probes drawn at random:
// every list in its main combinations of filter and order, its first page
// and the page after it, of 100 rows, the lists of a receiver and of a
// modality no row holds (whose plan, if it tested rows of another index,
// would read all of it), and the gets by labels and the reads of a scan's
// instances. It adds each read's
// spread (median, 99th percentile) to sum, and fails tb for a read that
// fails or whose 99th percentile is past bound.
func TimeCatalogReads(tb testing.TB, p tenant.CatalogReads, probes []CatalogProbe, rounds int, sum *Summary, bound time.Duration) {
	tb.Helper()
	if len(probes) == 0 {
		tb.Fatal("no scan to probe")
	}
	ctx := context.Background()
	r := rand.New(rand.NewPCG(5, 7)) // #nosec G404 -- a seeded generator, so a run probes the same rows as the last.
	pick := func() CatalogProbe { return probes[r.IntN(len(probes))] }
	page := master.Page{Limit: 100}
	// A read times its own call, after what it needs first; a negative
	// time is a probe with nothing to time (a list of one page has no page
	// after it).
	type read struct {
		what string
		run  func() (time.Duration, error)
	}
	timed := func(call func() error) func() (time.Duration, error) {
		return func() (time.Duration, error) {
			began := time.Now()
			err := call()
			return time.Since(began), err
		}
	}
	// lists times a list's first page, and the page after it, read with
	// the cursor of a first page read untimed; bind draws the list's
	// arguments once a probe, so both pages are of one list.
	lists := func(what string, bind func() func(master.Page) (string, error)) []read {
		return []read{
			{what, timed(func() error { _, err := bind()(page); return err })},
			{what + ", the page after the first", func() (time.Duration, error) {
				list := bind()
				next, err := list(page)
				if err != nil || next == "" {
					return -1, err
				}
				began := time.Now()
				_, err = list(master.Page{Limit: page.Limit, Cursor: next})
				return time.Since(began), err
			}},
		}
	}
	subjects := func(f func() tenant.SubjectFilter) func() func(master.Page) (string, error) {
		return func() func(master.Page) (string, error) {
			filter := f()
			return func(pg master.Page) (string, error) {
				got, err := p.ListSubjects(ctx, filter, pg)
				return got.Next, err
			}
		}
	}
	sessions := func(f func() tenant.SessionFilter) func() func(master.Page) (string, error) {
		return func() func(master.Page) (string, error) {
			filter := f()
			return func(pg master.Page) (string, error) {
				got, err := p.ListSessions(ctx, filter, pg)
				return got.Next, err
			}
		}
	}
	scans := func(f func() tenant.ScanFilter) func() func(master.Page) (string, error) {
		return func() func(master.Page) (string, error) {
			filter := f()
			return func(pg master.Page) (string, error) { got, err := p.ListScans(ctx, filter, pg); return got.Next, err }
		}
	}
	var reads []read
	reads = append(reads, lists("subjects newest first", subjects(func() tenant.SubjectFilter { return tenant.SubjectFilter{} }))...)
	reads = append(reads, lists("subjects by a label prefix", subjects(func() tenant.SubjectFilter {
		return tenant.SubjectFilter{Order: tenant.OrderLabel, LabelPrefix: pick().Path.Subject[:5]}
	}))...)
	reads = append(reads, lists("sessions newest first", sessions(func() tenant.SessionFilter { return tenant.SessionFilter{} }))...)
	reads = append(reads, lists("a subject's sessions", sessions(func() tenant.SessionFilter {
		return tenant.SessionFilter{SubjectLabel: pick().Path.Subject}
	}))...)
	reads = append(reads, lists("a modality's sessions", sessions(func() tenant.SessionFilter {
		return tenant.SessionFilter{Modality: []string{"MR", "CT"}[r.IntN(2)]}
	}))...)
	reads = append(reads, lists("a receiver's sessions", sessions(func() tenant.SessionFilter { return tenant.SessionFilter{Receiver: GenReceiver} }))...)
	reads = append(reads, lists("the sessions of a receiver no row holds", sessions(func() tenant.SessionFilter {
		return tenant.SessionFilter{Receiver: AbsentReceiver}
	}))...)
	reads = append(reads, lists("the sessions of a modality no scan holds", sessions(func() tenant.SessionFilter {
		return tenant.SessionFilter{Modality: AbsentModality}
	}))...)
	reads = append(reads, lists("sessions by label", sessions(func() tenant.SessionFilter { return tenant.SessionFilter{Order: tenant.OrderLabel} }))...)
	reads = append(reads, lists("sessions by study date", sessions(func() tenant.SessionFilter {
		return tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateEnd: "2015" + []string{"0101", "0601", "1231"}[r.IntN(3)]}
	}))...)
	reads = append(reads, lists("scans newest first", scans(func() tenant.ScanFilter { return tenant.ScanFilter{} }))...)
	reads = append(reads, lists("a session's scans", scans(func() tenant.ScanFilter {
		pr := pick()
		return tenant.ScanFilter{SubjectLabel: pr.Path.Subject, SessionLabel: pr.Path.Session}
	}))...)
	reads = append(reads, lists("a modality's scans", scans(func() tenant.ScanFilter {
		return tenant.ScanFilter{Modality: []string{"MR", "CT"}[r.IntN(2)]}
	}))...)
	reads = append(reads, lists("the scans of a modality no scan holds", scans(func() tenant.ScanFilter {
		return tenant.ScanFilter{Modality: AbsentModality}
	}))...)
	reads = append(reads, lists("a receiver's scans", scans(func() tenant.ScanFilter { return tenant.ScanFilter{Receiver: GenReceiver} }))...)
	reads = append(reads, lists("the scans of a receiver no row holds", scans(func() tenant.ScanFilter {
		return tenant.ScanFilter{Receiver: AbsentReceiver}
	}))...)
	reads = append(reads, lists("a subject's scans by label", scans(func() tenant.ScanFilter {
		return tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: pick().Path.Subject}
	}))...)
	reads = append(reads, lists("scans by label", scans(func() tenant.ScanFilter { return tenant.ScanFilter{Order: tenant.OrderLabel} }))...)
	reads = append(reads, lists("scans by a description prefix", scans(func() tenant.ScanFilter {
		return tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: []string{"T", "dwi", "Scout", "f"}[r.IntN(4)]}
	}))...)
	reads = append(reads,
		read{"a session by its labels", timed(func() error {
			pr := pick()
			_, err := p.CatalogSession(ctx, pr.Path.Subject, pr.Path.Session)
			return err
		})},
		read{"a scan by its labels", timed(func() error { _, err := p.CatalogScan(ctx, pick().Path); return err })},
		read{"a scan's instances, first page", timed(func() error { _, _, err := p.ListHeldInstances(ctx, pick().Path, page); return err })},
		read{"an instance by its scan's labels and UID", timed(func() error {
			pr := pick()
			_, err := p.HeldInstance(ctx, pr.Path, pr.UID)
			return err
		})},
	)
	for _, rd := range reads {
		d := make([]time.Duration, 0, rounds)
		for range rounds {
			took, err := rd.run()
			if err != nil {
				tb.Fatalf("%s: %v", rd.what, err)
			}
			if took >= 0 {
				d = append(d, took)
			}
		}
		if len(d) == 0 {
			sum.Add("port: "+rd.what, "none at this size")
			continue
		}
		sp := SpreadOf(d)
		sum.Add("port: "+rd.what, sp.String())
		if sp.P99 > bound {
			tb.Errorf("%s through the port: the 99th percentile %v is past %v", rd.what, sp.P99, bound)
		}
	}
}
