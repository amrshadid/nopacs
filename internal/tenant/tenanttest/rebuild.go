package tenanttest

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Scratch makes the scratch schema a rebuild builds its shadow in on a
// driver that needs one (Postgres): empty, on no search path, gone when tb
// ends. Nil for a driver whose shadow is a file of its own (SQLite).
type Scratch func(tb testing.TB) string

// runRebuilding runs the tests of rebuild jobs and their shadow catalogs.
func runRebuilding(t *testing.T, open OpenEvents, scratch Scratch) {
	tests := []struct {
		name string
		fn   func(*renv)
	}{
		{"RebuildKeepsAETitles", testRebuildKeepsAETitles},
		{"RebuildCompareEqual", testRebuildCompareEqual},
		{"RebuildCompareDiffers", testRebuildCompareDiffers},
		{"RebuildActivates", testRebuildActivates},
		{"RebuildResumes", testRebuildResumes},
		{"RebuildScanRefusals", testRebuildScanRefusals},
		{"RebuildJobRefusals", testRebuildJobRefusals},
		{"RebuildCanceled", testRebuildCanceled},
		{"RebuildGivenUp", testRebuildGivenUp},
		{"RebuildMadeOnce", testRebuildMadeOnce},
		{"RebuildActivationRefused", testRebuildActivationRefused},
		{"RebuildTradesLabels", testRebuildTradesLabels},
		{"RebuildSessionReceiver", testRebuildSessionReceiver},
		{"RebuildPauseUntilActivated", testRebuildPauseUntilActivated},
		{"RebuildPauseEndsAnyWay", testRebuildPauseEndsAnyWay},
		{"RebuildHoldsCorrections", testRebuildHoldsCorrections},
		{"RebuildHoldsPurges", testRebuildHoldsPurges},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			e := &renv{senv: &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s}}}
			if scratch != nil {
				e.scratch = scratch(t)
				e.another = func() string { return scratch(t) }
			}
			tt.fn(e)
		})
	}
}

// renv is a test of rebuilds: the suite's sending environment and the
// scratch schema its rebuilds name.
type renv struct {
	*senv
	scratch string
	// another makes one more scratch schema; nil on a driver that builds in
	// none.
	another func() string
}

// rebuildSetup archives two sessions of subject sub-01 on the storage
// "store": one of two scans of three instances, with header values, and
// one of one scan of two instances.
func (e *renv) rebuildSetup() {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	a := e.archive("recv", 1, 2, 3, 0, at(-7200), "ses-1")
	e.finish(a)
	b := e.archive("recv", 2, 1, 2, 500, at(-3600), "ses-2")
	e.finish(b)
	e.ok("FillScanValues", e.p.FillScanValues(e.ctx, uidOf(2, 100), tenant.ScanValues{StudyDate: "20240131", Modality: "CT",
		SeriesDescription: "Axial", SeriesNumber: 3, HasSeriesNumber: true}))
	e.ok("FillScanValues", e.p.FillScanValues(e.ctx, uidOf(2, 101), tenant.ScanValues{Modality: "MR"}))
}

// createRebuild makes a rebuild job of the mode, failing the test on an
// error.
func (e *renv) createRebuild(id string, mode tenant.RebuildMode) tenant.Job {
	e.Helper()
	j, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: mode, Scratch: e.scratch},
		Created: at(0)}, e.audit(), nil)
	e.ok("CreateJob of a rebuild", err)
	return j
}

// claimRebuild claims the rebuild's unit at now and starts it.
func (e *renv) claimRebuild(now time.Time) tenant.Work {
	e.Helper()
	w, err := e.p.ClaimWork(e.ctx, tenant.RebuildWorkKind, "rebuilder", now, time.Minute)
	e.ok("ClaimWork of a rebuild", err)
	j, err := e.p.StartRebuildUnit(e.ctx, w.Fence(), now)
	e.ok("StartRebuildUnit", err)
	if j.State != tenant.JobRunning {
		e.Fatalf("the rebuild started is %s, want running", j.State)
	}
	return w
}

// liveScan is one scan of the catalog in use, as a storage would hold it:
// its rows and its instances.
type liveScan struct {
	key       string
	scan      tenant.RebuiltScan
	instances []tenant.RebuiltInstance
}

// liveScans reads every scan of the catalog in use through the catalog's
// reads, in order of their manifest's key, as a rebuild would read them
// from the storages.
func (e *renv) liveScans() []liveScan {
	e.Helper()
	var out []liveScan
	cursor := ""
	for {
		page, err := e.p.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderNewest}, master.Page{Limit: 1000, Cursor: cursor})
		e.ok("ListScans", err)
		for _, sc := range page.Items {
			path := label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
			se, err := e.p.CatalogSession(e.ctx, sc.SubjectLabel, sc.SessionLabel)
			e.ok("CatalogSession", err)
			labels, err := e.p.CatalogLabels(e.ctx, se.StudyUID, nil)
			e.ok("CatalogLabels", err)
			key, err := layout.ManifestKey(path)
			e.ok("ManifestKey", err)
			v := sc.Values
			v.StudyDate = se.StudyDate
			ls := liveScan{key: key, scan: tenant.RebuiltScan{Storage: sc.Storage, Path: path, StudyUID: se.StudyUID, SeriesUID: sc.SeriesUID,
				SessionReceiver: se.Receiver, Receiver: sc.Receiver, Route: labels.Session.Route, Values: v, Created: sc.Created,
				Updated: sc.Updated, ManifestSHA256: sc.ManifestSHA256}}
			ins, _, err := e.p.ListHeldInstances(e.ctx, path, master.Page{Limit: 1000})
			e.ok("ListHeldInstances", err)
			for _, in := range ins {
				ls.instances = append(ls.instances, tenant.RebuiltInstance{SOPInstanceUID: in.SOPInstanceUID, Key: in.Key, Offset: in.Offset,
					Length: in.Length, Size: in.Size, ReceivedSHA256: in.SHA256, ArchivedSHA256: in.ArchivedSHA256, Profile: in.Profile,
					Reception: in.ReceptionID, HeldAt: in.Held})
			}
			out = append(out, ls)
		}
		if cursor = page.Next; cursor == "" {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].key < out[j].key })
	return out
}

// build writes scans into the shadow of the unit, storage by storage in
// order of name, each scan in batches of at most batch instances, from the
// progress the shadow answers (so a build goes on where it stopped), and
// finishes it.
func (e *renv) build(w tenant.Work, scans []liveScan, batch int) tenant.RebuildProgress {
	e.Helper()
	prog, err := e.p.PrepareShadow(e.ctx, w.Fence())
	e.ok("PrepareShadow", err)
	storages := map[string]bool{}
	for _, s := range scans {
		storages[s.scan.Storage] = true
	}
	names := make([]string, 0, len(storages))
	for n := range storages {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		if name < prog.Cursor.Storage {
			continue
		}
		if name != prog.Cursor.Storage {
			prog, err = e.p.FinishRebuildStorage(e.ctx, w.Fence(), prog.Cursor.Storage, name, nil)
			e.ok("FinishRebuildStorage", err)
		}
		for _, s := range scans {
			if s.scan.Storage != name || s.key <= prog.Cursor.After {
				continue
			}
			ins := s.instances
			if prog.Cursor.Partial == s.key {
				for len(ins) > 0 && ins[0].SOPInstanceUID <= prog.Cursor.PartialUID {
					ins = ins[1:]
				}
			}
			for {
				n := min(batch, len(ins))
				sc := s.scan
				prog, err = e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: prog.Cursor, Key: s.key, Scan: &sc,
					Instances: ins[:n], Done: n == len(ins)})
				e.ok("RebuildScan", err)
				if ins = ins[n:]; len(ins) == 0 {
					break
				}
			}
		}
	}
	prog, err = e.p.FinishRebuildStorage(e.ctx, w.Fence(), prog.Cursor.Storage, "", nil)
	e.ok("FinishRebuildStorage at the end", err)
	if !prog.Complete {
		e.Fatalf("the build is not complete after its last storage: %+v", prog)
	}
	return prog
}

// catalogText is the catalog in use as its reads answer it, one line a
// row, for comparing two moments.
func (e *renv) catalogText() string {
	e.Helper()
	var lines []string
	subs, err := e.p.ListSubjects(e.ctx, tenant.SubjectFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	e.ok("ListSubjects", err)
	for _, s := range subs.Items {
		lines = append(lines, fmt.Sprintf("subject %+v", s))
	}
	for _, s := range e.liveScans() {
		se, err := e.p.CatalogSession(e.ctx, s.scan.Path.Subject, s.scan.Path.Session)
		e.ok("CatalogSession", err)
		sc, err := e.p.CatalogScan(e.ctx, s.scan.Path)
		e.ok("CatalogScan", err)
		lines = append(lines, fmt.Sprintf("session %+v", se), fmt.Sprintf("scan %+v", sc))
		for _, in := range s.instances {
			lines = append(lines, fmt.Sprintf("instance %+v", in))
		}
	}
	totals, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	lines = append(lines, fmt.Sprintf("totals %+v", totals))
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

// testRebuildCompareEqual guards the round trip: a shadow built from what
// the catalog holds compares equal to it, table by table, and a compare
// job ends done with its counts, its shadow dropped after.
func testRebuildCompareEqual(e *renv) {
	e.rebuildSetup()
	before := e.catalogText()
	j := e.createRebuild("rb-1", tenant.RebuildCompare)
	if j.Rebuild == nil || j.Rebuild.Mode != tenant.RebuildCompare || j.Rebuild.Scratch != e.scratch || j.State != tenant.JobQueued {
		e.Fatalf("the rebuild job made is %+v", j)
	}
	w := e.claimRebuild(at(10))
	prog := e.build(w, e.liveScans(), 2)
	if prog.Scans != 3 || prog.Instances != 8 || prog.LeftOut != 0 {
		e.Fatalf("the build counts %+v, want 3 scans and 8 instances", prog)
	}
	cmp, err := e.p.CompareCatalog(e.ctx, w.Fence())
	e.ok("CompareCatalog", err)
	if !cmp.Equal() {
		e.Fatalf("a shadow built from the catalog differs from it: %+v %+v", cmp.Tables, cmp.Differences)
	}
	if got := e.catalogText(); got != before {
		e.Fatalf("building a shadow changed the catalog in use:\n%s\nwant\n%s", got, before)
	}
	end, err := e.p.EndRebuild(e.ctx, w.Fence(), cmp, at(20))
	e.ok("EndRebuild", err)
	e.wantCounts("the compare job", end, 8, 8, 0, 0, 3, 3, true)
	if end.State != tenant.JobDone {
		e.Fatalf("the compare job ended %s, want done", end.State)
	}
	_, err = e.p.PrepareShadow(e.ctx, w.Fence())
	e.wantPrecondition("PrepareShadow of a job ended", err)
	e.ok("DropShadow", e.p.DropShadow(e.ctx, "rb-1"))
	e.ok("DropShadow again", e.p.DropShadow(e.ctx, "rb-1"))
	e.ok("CompleteWork", e.p.CompleteWork(e.ctx, w.Fence(), at(21), nil))
}

// testRebuildCompareDiffers guards that a compare names what differs: a
// scan the storage no longer holds, and a value that changed, counted by
// table and kind and kept as the job's tallies.
func testRebuildCompareDiffers(e *renv) {
	e.rebuildSetup()
	e.createRebuild("rb-1", tenant.RebuildCompare)
	w := e.claimRebuild(at(10))
	scans := e.liveScans()
	var kept []liveScan
	for _, s := range scans {
		switch s.scan.SeriesUID {
		case uidOf(2, 101):
			continue // the storage lost it
		case uidOf(2, 100):
			s.scan.Values.SeriesDescription = "Coronal"
		}
		kept = append(kept, s)
	}
	e.build(w, kept, 1000)
	cmp, err := e.p.CompareCatalog(e.ctx, w.Fence())
	e.ok("CompareCatalog", err)
	want := map[string]tenant.TableDifference{
		"scans":     {Removed: 1, Changed: 1},
		"instances": {Removed: 3},
	}
	for table, d := range want {
		if cmp.Tables[table] != d {
			e.Errorf("table %s differs by %+v, want %+v (all: %+v)", table, cmp.Tables[table], d, cmp.Tables)
		}
	}
	if cmp.Tables["catalog_totals"].Changed != 1 || cmp.Tables["reception_scans"].Removed != 1 || cmp.Tables["sessions"].Changed != 1 {
		e.Errorf("the derived tables' differences are %+v", cmp.Tables)
	}
	found := false
	for _, d := range cmp.Differences {
		if d.Table == "scans" && d.Kind == "removed" && d.Key == uidOf(2, 101) {
			found = true
		}
	}
	if !found {
		e.Errorf("the differences do not name the scan removed by its Series Instance UID: %+v", cmp.Differences)
	}
	end, err := e.p.EndRebuild(e.ctx, w.Fence(), cmp, at(20))
	e.ok("EndRebuild", err)
	tallies, err := e.p.JobTallies(e.ctx, end.ID)
	e.ok("JobTallies", err)
	got := map[string]int64{}
	for _, t := range tallies {
		got[t.Key] = t.Count
	}
	if got[tenant.DifferTally("scans", "removed")] != 1 || got[tenant.DifferTally("instances", "removed")] != 3 {
		e.Errorf("the job's tallies are %v", got)
	}
	// The differences by name outlive the shadow, kept with the job in the
	// order the comparison found them.
	e.ok("DropShadow", e.p.DropShadow(e.ctx, end.ID))
	named, err := e.p.RebuildDifferences(e.ctx, end.ID)
	e.ok("RebuildDifferences", err)
	if fmt.Sprint(named) != fmt.Sprint(cmp.Differences) || len(named) == 0 {
		e.Errorf("the job kept the differences %+v, the comparison named %+v", named, cmp.Differences)
	}
}

// testRebuildActivates guards the replace: the catalog in use is untouched
// while the shadow is built, then becomes the shadow in one transaction,
// with a new event history whose first event is catalog_rebuilt, the job
// ended with the tally activated; an activation sent again changes
// nothing.
func testRebuildActivates(e *renv) {
	e.rebuildSetup()
	history, err := e.p.EventHistory(e.ctx)
	e.ok("EventHistory", err)
	if tenant.ValidateEventHistory(history) != nil {
		e.Fatalf("the event history id %q is not 32 lowercase hexadecimal digits", history)
	}
	start := e.now()
	before := e.catalogText()
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	var shadow []liveScan
	for _, s := range e.liveScans() {
		if s.scan.SeriesUID == uidOf(2, 101) {
			continue // the storage lost it
		}
		shadow = append(shadow, s)
	}
	// A scan only the storages hold, of a subject the catalog does not
	// have.
	extra := shadow[0]
	extra.scan.Path = label.Path{Subject: "sub-02", Session: "ses-9", Scan: "scan-9"}
	extra.scan.StudyUID, extra.scan.SeriesUID = uidOf(1, 9), uidOf(2, 900)
	extra.key, _ = layout.ManifestKey(extra.scan.Path)
	extra.instances = nil
	for i := range 2 {
		uid := uidOf(3, 9000+i)
		key, err := layout.InstanceKey(extra.scan.Path, uid)
		e.ok("InstanceKey", err)
		extra.instances = append(extra.instances, tenant.RebuiltInstance{SOPInstanceUID: uid, Key: key, Length: 10, Size: 10,
			ReceivedSHA256: sum(90 + i), ArchivedSHA256: sum(90 + i), Reception: "rc-gone", HeldAt: at(-60)})
	}
	shadow = append(shadow, extra)
	sort.Slice(shadow, func(i, j int) bool { return shadow[i].key < shadow[j].key })
	e.build(w, shadow, 1000)
	if got := e.catalogText(); got != before {
		e.Fatalf("building the shadow changed the catalog in use")
	}
	_, err = e.p.EndRebuild(e.ctx, w.Fence(), tenant.CatalogComparison{}, at(19))
	e.wantPrecondition("EndRebuild of a replace job", err)
	// A reader during the activation sees the old catalog's totals or the
	// new one's, never a mixture.
	old, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	stop, seen := make(chan struct{}), make(chan []tenant.CatalogCounts, 1)
	go func() {
		var odd []tenant.CatalogCounts
		for {
			select {
			case <-stop:
				seen <- odd
				return
			default:
			}
			c, err := e.p.CatalogTotals(e.ctx)
			if err == nil && c != old && c != (tenant.CatalogCounts{Subjects: 2, Sessions: 3, Scans: 3, Instances: 7}) {
				odd = append(odd, c)
			}
		}
	}()
	j, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(30))
	close(stop)
	if odd := <-seen; len(odd) > 0 {
		e.Errorf("a reader during the activation saw totals of neither catalog: %+v", odd)
	}
	e.ok("ActivateCatalog", err)
	if j.State != tenant.JobDone || j.Rebuild == nil || !j.Rebuild.Activated {
		e.Fatalf("the activated job is %+v", j)
	}
	e.wantCounts("the replace job", j, 7, 7, 0, 0, 3, 3, true)
	if _, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 101)}); fault.KindOf(err) != fault.NotFound {
		e.Errorf("the scan the shadow lacked is still in the catalog: %v", err)
	}
	sc, err := e.p.CatalogScan(e.ctx, extra.scan.Path)
	e.ok("CatalogScan of the scan only the shadow held", err)
	if sc.Instances != 2 || sc.Bytes != 20 || sc.SeriesUID != uidOf(2, 900) {
		e.Errorf("the scan only the shadow held is %+v", sc)
	}
	kept, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 100)})
	e.ok("CatalogScan of a scan both held", err)
	if kept.Values.SeriesDescription != "Axial" || kept.Values.Modality != "CT" || kept.Instances != 3 {
		e.Errorf("a scan both held is %+v", kept)
	}
	totals, err := e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	if totals != (tenant.CatalogCounts{Subjects: 2, Sessions: 3, Scans: 3, Instances: 7}) {
		e.Errorf("the catalog's totals after activation are %+v", totals)
	}
	after, err := e.p.EventHistory(e.ctx)
	e.ok("EventHistory", err)
	if after == history || tenant.ValidateEventHistory(after) != nil {
		e.Errorf("the event history after activation is %q, before %q", after, history)
	}
	e.checkNewHistory(start, history, after, "rb-1")
	tallies, err := e.p.JobTallies(e.ctx, "rb-1")
	e.ok("JobTallies", err)
	if len(tallies) != 1 || tallies[0].Key != tenant.TallyActivated || tallies[0].Count != 1 {
		e.Errorf("the replace job's tallies are %+v", tallies)
	}
	again, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(31))
	e.ok("ActivateCatalog sent again", err)
	if again.State != j.State || !again.Ended.Equal(j.Ended) {
		e.Errorf("an activation sent again changed the job: %+v", again)
	}
	if events, _ := e.readAll("", 100, tenant.EventCatalogRebuilt); len(events) != 1 {
		e.Errorf("an activation sent again wrote another event")
	}
	e.ok("DropShadow", e.p.DropShadow(e.ctx, "rb-1"))
	// A catalog rebuilt takes new commits as the one before did.
	b := e.archive("recv", 3, 1, 1, 700, at(40), "ses-3")
	e.finish(b)
	totals, err = e.p.CatalogTotals(e.ctx)
	e.ok("CatalogTotals", err)
	if totals.Instances != 8 || totals.Scans != 4 {
		e.Errorf("the totals after a commit into the rebuilt catalog are %+v", totals)
	}
}

// testRebuildResumes guards that a build stopped anywhere goes on from the
// last scan written: a lease taken over fences the old worker out, the new
// one reads the shadow's cursor, writes a scan's rest, and the result
// equals the catalog.
func testRebuildResumes(e *renv) {
	e.rebuildSetup()
	e.createRebuild("rb-1", tenant.RebuildCompare)
	w := e.claimRebuild(at(10))
	scans := e.liveScans()
	_, err := e.p.PrepareShadow(e.ctx, w.Fence())
	e.ok("PrepareShadow", err)
	prog, err := e.p.FinishRebuildStorage(e.ctx, w.Fence(), "", "store", nil)
	e.ok("FinishRebuildStorage", err)
	first := scans[0]
	sc := first.scan
	prog, err = e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: prog.Cursor, Key: first.key, Scan: &sc,
		Instances: first.instances[:1]})
	e.ok("RebuildScan of a part", err)
	if prog.Cursor.Partial != first.key || prog.Cursor.PartialUID != first.instances[0].SOPInstanceUID {
		e.Fatalf("the cursor after a part is %+v", prog.Cursor)
	}
	stale := prog.Cursor
	stale.PartialUID = ""
	_, err = e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: stale, Key: first.key, Scan: &sc,
		Instances: first.instances[1:2]})
	e.wantIs("RebuildScan from a cursor left", err, tenant.ErrRebuildMoved, fault.Precondition)
	// The lease runs out; another worker takes the unit.
	w2, err := e.p.ClaimWork(e.ctx, tenant.RebuildWorkKind, "second", at(200), time.Minute)
	e.ok("ClaimWork after the lease ran out", err)
	_, err = e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: prog.Cursor, Key: first.key, Scan: &sc,
		Instances: first.instances[1:]})
	e.wantIs("RebuildScan of the lease taken over", err, tenant.ErrLeaseLost, fault.Conflict)
	j, err := e.p.StartRebuildUnit(e.ctx, w2.Fence(), at(200))
	e.ok("StartRebuildUnit again", err)
	if j.State != tenant.JobRunning {
		e.Fatalf("the job is %s on its second start", j.State)
	}
	again, err := e.p.PrepareShadow(e.ctx, w2.Fence())
	e.ok("PrepareShadow again", err)
	if again.Cursor != prog.Cursor || again.Scans != 1 {
		e.Fatalf("the shadow found again is at %+v, want %+v", again, prog)
	}
	e.build(w2, scans, 1)
	cmp, err := e.p.CompareCatalog(e.ctx, w2.Fence())
	e.ok("CompareCatalog", err)
	if !cmp.Equal() {
		e.Fatalf("a build resumed differs from the catalog: %+v %+v", cmp.Tables, cmp.Differences)
	}
}
