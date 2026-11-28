package receive

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deidcoverage"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// coverageFile returns a public file written again with a Patient Name that
// would be a needle, a vendor's private block with two elements, and a
// private block whose creator holds a part of that name, with one element:
// synthetic data on a public file.
func coverageFile(t *testing.T, name string) []byte {
	t.Helper()
	return rewrite(t, corpus(t, name), "", func(ds *dataset.Dataset) {
		ds.Remove(tag.New(0x0010, 0x0010))
		add := func(g, e uint16, vr, v string) {
			if err := ds.Add(dataelem.NewDataElement(tag.New(g, e), dataelem.VR(vr), []byte(v))); err != nil {
				t.Fatal(err)
			}
		}
		add(0x0010, 0x0010, "PN", "ZZTEST^ALICE")
		add(0x0029, 0x0010, "LO", "ACME RESEARCH ")
		add(0x0029, 0x1001, "LO", "value-one-secret")
		add(0x0029, 0x1002, "LO", "value-two-secret")
		add(0x0031, 0x0010, "LO", "ZZTEST ALICE CLINIC ")
		add(0x0031, 0x1001, "LO", "withheld-secret-x")
	})
}

// fixedStorages answers every storage with one, counting its reads and
// calling cancelAt's function at the n-th, as a restart does between two
// pages of a job.
type fixedStorages struct {
	st    blob.Store
	reads *int
	n     int
	stop  context.CancelFunc
}

// Get returns the one storage, which counts the reads made of it.
func (f fixedStorages) Get(context.Context, string) (blob.Store, error) {
	return stopAfterReads{Store: f.st, f: f}, nil
}

// Close closes nothing: the storage is the test's own.
func (fixedStorages) Close() {}

// stopAfterReads is a storage that counts its reads and stops the pass at one.
type stopAfterReads struct {
	blob.Store
	f fixedStorages
}

// Read counts the read and, at the n-th, stops the pass.
func (s stopAfterReads) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	*s.f.reads++
	if s.f.n > 0 && *s.f.reads == s.f.n {
		s.f.stop()
	}
	return s.Store.Read(ctx, key, offset, length)
}

// coverageRunner makes a coverage runner of one-scan pages over the env's
// storage.
func coverageRunner(t *testing.T, e *env, reads *int, n int, stop context.CancelFunc) *deidcoverage.Runner {
	t.Helper()
	r, err := deidcoverage.New(deidcoverage.Config{Store: e.store, Page: 1, Now: e.clock.Now,
		Storages: func(context.Context) (deidcoverage.Storages, error) {
			return fixedStorages{st: e.storage, reads: reads, n: n, stop: stop}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// coverageReport returns the elements of the tenant's report, all pages, as
// the report holds them.
func coverageReport(t *testing.T, e *env) (tenant.DeidCoverageReport, []tenant.CoverageElement) {
	t.Helper()
	rep, found, err := e.store.DeidCoverageReport(e.ctx)
	if err != nil || !found {
		t.Fatalf("the report: found %v, %v", found, err)
	}
	var out []tenant.CoverageElement
	var after *tenant.CoverageKey
	for {
		page, err := e.store.DeidCoverageElements(e.ctx, rep.JobID, after, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page) == 0 {
			return rep, out
		}
		out = append(out, page...)
		k := page[len(page)-1].CoverageKey
		after = &k
	}
}

// TestCoverageReportsTheUnnamedElementsOfArchivedFiles guards the coverage
// report over files a receiver de-identified: each line of a scan's
// metadata records the elements no rule named, and the job reports exactly
// the private elements the files carry, by creator and element with their
// VRs and counts, and the public elements no rule of the profile named; a
// private creator that holds a part of the patient's name stays withheld,
// its element still counted; and no value of any file is in the report.
func TestCoverageReportsTheUnnamedElementsOfArchivedFiles(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	files := [][]byte{coverageFile(t, "ct5n-2062.dcm"), coverageFile(t, "MR_small.dcm")}
	for _, f := range files {
		e.sendBytes(f, Received)
	}
	e.settle(2)
	// The lines of the archived scans record what the engine found.
	lines := 0
	for _, path := range scanPaths(t, e, e.receptions()) {
		_, in := sidecarOf(t, e, path)
		for _, l := range in {
			lines++
			if l.Unnamed == nil {
				t.Errorf("a line of a de-identified instance records no unnamed list")
			}
		}
	}
	if lines != 2 {
		t.Fatalf("%d lines, want 2", lines)
	}

	j, err := e.store.QueueDeidCoverage(e.ctx, tenant.DeidCoverageQueue{ID: "cv-1", KeyID: "K1234567890a", Now: t0}, e.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	res, err := coverageRunner(t, e, &reads, 0, func() {}).Run(e.ctx, j.ID)
	if err != nil || !res.Ended || reads != 2 {
		t.Fatalf("the pass: ended %v, %d reads, %v; want ended after one read of each of the 2 scans", res.Ended, reads, err)
	}
	rep, elements := coverageReport(t, e)
	if rep.Partial || rep.Scans != 2 || rep.ScansSkipped != 0 || rep.Instances != 2 || rep.Recorded != 2 || rep.Capped != 0 {
		t.Errorf("the report is %+v, want 2 scans, 2 instances, both recorded", rep)
	}
	got := map[string]tenant.CoverageElement{}
	for _, el := range elements {
		got[fmt.Sprintf("%08X|%s|%v|%s", el.Tag, el.Creator, el.Withheld, el.VR)] = el
	}
	for key, want := range map[string]int64{
		"00290001|ACME RESEARCH|false|LO": 2,
		"00290002|ACME RESEARCH|false|LO": 2,
		"00310001||true|LO":               2,
	} {
		el, ok := got[key]
		if !ok || el.Instances != want || el.Occurrences != want {
			t.Errorf("%s: %+v (listed %v), want %d instances and occurrences", key, el, ok, want)
		}
	}
	// The public files carry vendor blocks of their own, which are reported
	// too, and no creator names a part of the patient.
	vendor := 0
	for _, el := range elements {
		if strings.HasPrefix(el.Creator, "GEMS_") {
			vendor++
		}
		if strings.Contains(strings.ToUpper(el.Creator), "ZZTEST") || strings.Contains(strings.ToUpper(el.Creator), "ALICE") {
			t.Errorf("a creator that holds the patient's name is reported: %+v", el)
		}
	}
	if vendor == 0 {
		t.Errorf("the vendor's own private elements of the public CT file are not reported: %d elements", len(elements))
	}
	// A public element no rule named is reported with its VR, from both files.
	if el, ok := got["00180050||false|DS"]; !ok || el.Instances < 1 {
		t.Errorf("Slice Thickness, which the basic profile does not name, is not reported: %v", elements)
	}
	// No value of a file is in the report.
	text := fmt.Sprintf("%+v %+v", rep, elements)
	for _, banned := range []string{"value-one-secret", "value-two-secret", "withheld-secret-x", "ZZTEST^ALICE", "ALICE", "CLINIC"} {
		if strings.Contains(text, banned) {
			t.Errorf("the report holds %q", banned)
		}
	}
}

// TestCoverageResumesAfterARestart guards a job stopped between two pages: it
// stands at the first scan counted, a runner made anew reads only the scans
// after it, and the report is the one an uninterrupted job makes, every
// scan counted once.
func TestCoverageResumesAfterARestart(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	for _, f := range [][]byte{coverageFile(t, "ct5n-2062.dcm"), coverageFile(t, "MR_small.dcm")} {
		e.sendBytes(f, Received)
	}
	e.settle(2)
	whole, err := e.store.QueueDeidCoverage(e.ctx, tenant.DeidCoverageQueue{ID: "cv-whole", Now: t0}, e.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	if res, err := coverageRunner(t, e, &reads, 0, func() {}).Run(e.ctx, whole.ID); err != nil || !res.Ended {
		t.Fatalf("the uninterrupted job: ended %v, %v", res.Ended, err)
	}
	wantRep, want := coverageReport(t, e)

	j, err := e.store.QueueDeidCoverage(e.ctx, tenant.DeidCoverageQueue{ID: "cv-cut", Now: t0}, e.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The pass stops as the second scan is read, after the first was counted.
	ctx, cancel := context.WithCancel(e.ctx)
	reads = 0
	res, err := coverageRunner(t, e, &reads, 2, cancel).Run(ctx, j.ID)
	cancel()
	if err == nil || res.Ended || res.Written != 1 {
		t.Fatalf("the stopped pass: wrote %d scans, ended %v, %v; want one scan, not ended, an error", res.Written, res.Ended, err)
	}
	mid, err := e.store.DeidCoverageJob(e.ctx, j.ID)
	if err != nil || mid.Job.State != tenant.JobRunning || mid.Job.Counts.InstancesSent != 1 || mid.Instances != 1 || mid.After == 0 {
		t.Fatalf("the stopped job is %+v (%v), want running with one scan counted", mid, err)
	}
	// A runner made anew goes on from the database's place: one read, the scan
	// not yet counted.
	reads = 0
	res, err = coverageRunner(t, e, &reads, 0, func() {}).Run(e.ctx, j.ID)
	if err != nil || !res.Ended || res.Written != 1 || reads != 1 {
		t.Fatalf("the resumed pass: wrote %d scans with %d reads, ended %v, %v; want one of each, ended", res.Written, reads, res.Ended, err)
	}
	gotRep, got := coverageReport(t, e)
	if gotRep.JobID != j.ID || gotRep.Instances != wantRep.Instances || gotRep.Recorded != wantRep.Recorded || gotRep.Scans != wantRep.Scans {
		t.Errorf("the resumed report is %+v, the uninterrupted one's %+v", gotRep, wantRep)
	}
	if !slices.Equal(got, want) {
		t.Errorf("the resumed report lists %d elements that differ from the uninterrupted job's %d", len(got), len(want))
	}
}

// TestCoverageCountsAFileArchivedAsReceivedButRecordsNothingOfIt guards a
// file a receiver archived as it is (no profile decided anything): its line
// records no unnamed list, and the job counts the instance as read and not as
// recorded, with no element.
func TestCoverageCountsAFileArchivedAsReceivedButRecordsNothingOfIt(t *testing.T) {
	e := newEnv(t, envOptions{})
	e.sendBytes(coverageFile(t, "ct5n-2062.dcm"), Received)
	e.settle(1)
	j, err := e.store.QueueDeidCoverage(e.ctx, tenant.DeidCoverageQueue{ID: "cv-1", Now: t0}, e.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	reads := 0
	if res, err := coverageRunner(t, e, &reads, 0, func() {}).Run(e.ctx, j.ID); err != nil || !res.Ended {
		t.Fatalf("the job: ended %v, %v", res.Ended, err)
	}
	rep, elements := coverageReport(t, e)
	if rep.Instances != 1 || rep.Recorded != 0 || len(elements) != 0 {
		t.Errorf("the report is %+v with %d elements, want one instance read, none recorded, no element", rep, len(elements))
	}
}

// TestSidecarUnnamedMatchesTheEngine guards that a line's list is the
// engine's record of the file without change: the same elements, VRs and
// counts, a withheld creator withheld, and the bound the line and the engine
// share.
func TestSidecarUnnamedMatchesTheEngine(t *testing.T) {
	if sidecar.MaxUnnamed != engine.MaxUnnamed {
		t.Errorf("a line lists at most %d elements and the engine records %d", sidecar.MaxUnnamed, engine.MaxUnnamed)
	}
	rec := &engine.Record{Unnamed: []engine.Unnamed{
		{Tag: 0x00180050, VR: "DS", Count: 3},
		{Tag: 0x00290001, Creator: "ACME", VR: "LO", Count: 1},
		{Tag: 0x00310001, CreatorUnreadable: true, VR: "UN", Count: 2},
	}, UnnamedOver: 5}
	got, over := unnamedOf(rec)
	want := []sidecar.UnnamedElement{
		{Tag: 0x00180050, VR: "DS", Count: 3},
		{Tag: 0x00290001, Creator: "ACME", VR: "LO", Count: 1},
		{Tag: 0x00310001, Withheld: true, VR: "UN", Count: 2},
	}
	if !slices.Equal(got, want) || over != 5 {
		t.Errorf("the line lists %+v and %d over, want %+v and 5", got, over, want)
	}
	if none, _ := unnamedOf(&engine.Record{}); none == nil || len(none) != 0 {
		t.Errorf("a record with none lists %v, want an empty list that is not nil", none)
	}
}
