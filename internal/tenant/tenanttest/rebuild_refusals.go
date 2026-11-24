package tenanttest

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests of what a rebuild refuses, and of a rebuild that ends without
// its worker: canceled, or its attempts run out.

// testRebuildScanRefusals guards that a scan the shadow cannot hold is
// refused whole, changing nothing: a profile version the tenant does not
// have, a series another scan holds, an instance another scan holds (of
// the same storage, or of another, which the refusal tells apart), a
// label another scan holds; that the shadow answers which storage holds an
// instance; and that a compare job is not activated.
func testRebuildScanRefusals(e *renv) {
	e.rebuildSetup()
	e.createRebuild("rb-1", tenant.RebuildCompare)
	w := e.claimRebuild(at(10))
	scans := e.liveScans()
	_, err := e.p.PrepareShadow(e.ctx, w.Fence())
	e.ok("PrepareShadow", err)
	_, err = e.p.ActivateCatalog(e.ctx, w.Fence(), at(11))
	e.wantPrecondition("ActivateCatalog of a compare job", err)
	_, err = e.p.EndRebuild(e.ctx, w.Fence(), tenant.CatalogComparison{}, at(11))
	e.wantPrecondition("EndRebuild before the build is complete", err)
	_, err = e.p.CompareCatalog(e.ctx, w.Fence())
	e.wantPrecondition("CompareCatalog before the build is complete", err)
	_, err = e.p.FinishRebuildStorage(e.ctx, w.Fence(), "other", "store", nil)
	e.wantIs("FinishRebuildStorage of another storage", err, tenant.ErrRebuildMoved, fault.Precondition)
	prog, err := e.p.FinishRebuildStorage(e.ctx, w.Fence(), "", "store", nil)
	e.ok("FinishRebuildStorage", err)
	write := func(s liveScan, ins []tenant.RebuiltInstance) error {
		sc := s.scan
		_, err := e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: prog.Cursor, Key: s.key, Scan: &sc,
			Instances: ins, Done: true})
		return err
	}
	unknown := scans[0]
	ins := append([]tenant.RebuiltInstance(nil), unknown.instances...)
	ins[1].Profile = tenant.ProfileRef{Name: "gone", Version: 3}
	e.wantIs("RebuildScan of an unknown profile version", write(unknown, ins), tenant.ErrProfileUnknown, fault.Precondition)
	prog, err = e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: prog.Cursor, Key: scans[0].key, Scan: &scans[0].scan,
		Instances: scans[0].instances, Done: true})
	e.ok("RebuildScan after a refusal", err)
	twin := scans[1]
	twin.scan.SeriesUID = scans[0].scan.SeriesUID
	e.wantIs("RebuildScan of a series another scan holds", write(twin, twin.instances), tenant.ErrSeriesElsewhere, fault.Conflict)
	shared := scans[1]
	shared.instances = append([]tenant.RebuiltInstance(nil), scans[1].instances...)
	shared.instances[0].SOPInstanceUID = scans[0].instances[0].SOPInstanceUID
	shared.instances[0].Key, _ = layout.InstanceKey(shared.scan.Path, shared.instances[0].SOPInstanceUID)
	sort.Slice(shared.instances, func(i, j int) bool { return shared.instances[i].SOPInstanceUID < shared.instances[j].SOPInstanceUID })
	err = write(shared, shared.instances)
	e.wantIs("RebuildScan of an instance another scan holds", err, tenant.ErrInstanceElsewhere, fault.Conflict)
	if errors.Is(err, tenant.ErrOtherStorage) {
		e.Errorf("an instance held by a scan of the same storage is refused as another storage's: %v", err)
	}
	held, err := e.p.ShadowHolder(e.ctx, w.Fence(), []string{uidOf(3, 99999), scans[0].instances[1].SOPInstanceUID})
	e.ok("ShadowHolder", err)
	if held != "store" {
		e.Errorf("the shadow says %q holds an instance a scan of store holds", held)
	}
	if held, err = e.p.ShadowHolder(e.ctx, w.Fence(), []string{uidOf(3, 99999)}); err != nil || held != "" {
		e.Errorf("the shadow says %q holds an instance none holds (%v)", held, err)
	}
	clash := scans[1]
	clash.scan.SeriesUID = uidOf(2, 999)
	clash.scan.Path.Scan = scans[0].scan.Path.Scan
	clash.scan.Path.Session = scans[0].scan.Path.Session
	clash.scan.StudyUID = scans[0].scan.StudyUID
	clash.instances = nil
	var lc *tenant.LabelConflict
	if err := write(clash, nil); !errors.As(err, &lc) {
		e.Errorf("RebuildScan of a label another scan holds: %v, want a label conflict", err)
	}
	// A scan of another storage that holds an instance a scan of store
	// holds: two storages' series.
	prog, err = e.p.FinishRebuildStorage(e.ctx, w.Fence(), "store", "zz", nil)
	e.ok("FinishRebuildStorage to another storage", err)
	far := shared
	far.scan.Storage = "zz"
	far.scan.Path.Subject, far.scan.StudyUID, far.scan.SeriesUID = "sub-zz", uidOf(1, 77), uidOf(2, 7700)
	far.key, _ = layout.ManifestKey(far.scan.Path)
	far.instances = append([]tenant.RebuiltInstance(nil), shared.instances...)
	for i := range far.instances {
		far.instances[i].Key, _ = layout.InstanceKey(far.scan.Path, far.instances[i].SOPInstanceUID)
	}
	err = write(far, far.instances)
	e.wantIs("RebuildScan of an instance a scan of another storage holds", err, tenant.ErrInstanceElsewhere, fault.Conflict)
	if !errors.Is(err, tenant.ErrOtherStorage) {
		e.Errorf("an instance held by a scan of another storage is not refused as another storage's: %v", err)
	}
	// None of the refusals wrote anything: the build counts one scan.
	prog, err = e.p.FinishRebuildStorage(e.ctx, w.Fence(), "zz", "", map[string]int64{tenant.TallyUnresolvedCorrection: 2})
	e.ok("FinishRebuildStorage", err)
	if prog.Scans != 1 || prog.Instances != 3 || prog.Tallies[tenant.TallyUnresolvedCorrection] != 2 {
		e.Errorf("after the refusals the build counts %+v, want one scan of three instances and the storage's tally", prog)
	}
	_, err = e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: prog.Cursor, Key: scans[1].key, Done: true,
		LeftOut: 2, Tallies: map[string]int64{tenant.TallyLeftOutUnreadable: 2}})
	e.wantPrecondition("RebuildScan once the build is complete", err)
}

// testRebuildJobRefusals guards the making of rebuild jobs: one mode, a
// scratch schema where the driver needs one and none where it does not,
// one rebuild at a time, no retry, and a shadow dropped only once its job
// has ended.
func testRebuildJobRefusals(e *renv) {
	bad := []tenant.NewJob{
		{ID: "rb-x", Kind: tenant.JobRebuild, Created: at(0)},
		{ID: "rb-x", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: "both"}, Created: at(0)},
		{ID: "rb-x", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: "Bad-Name"}, Created: at(0)},
		{ID: "rb-x", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}, Storage: "store", Created: at(0)},
		{ID: "rb-x", Kind: tenant.JobScrub, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare}, Created: at(0)},
	}
	if e.scratch == "" {
		bad = append(bad, tenant.NewJob{ID: "rb-x", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare,
			Scratch: "scratch"}, Created: at(0)})
	} else {
		bad = append(bad, tenant.NewJob{ID: "rb-x", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare},
			Created: at(0)})
		_, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: "rb-x", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare,
			Scratch: "no_such_schema_here"}, Created: at(0)}, e.audit(), nil)
		e.wantPrecondition("CreateJob of a rebuild in a schema that does not exist", err)
	}
	for i, j := range bad {
		_, err := e.p.CreateJob(e.ctx, j, e.audit(), nil)
		e.wantKind(fmt.Sprintf("CreateJob of a rebuild %d", i), err, fault.Invalid)
	}
	e.createRebuild("rb-1", tenant.RebuildReplace)
	replacing, err := e.p.RebuildReplacing(e.ctx)
	e.ok("RebuildReplacing", err)
	if !replacing {
		e.Errorf("a replace rebuild queued is not reported replacing")
	}
	_, err = e.p.CreateJob(e.ctx, tenant.NewJob{ID: "rb-2", Kind: tenant.JobRebuild, Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare,
		Scratch: e.scratch}, Created: at(1)}, e.audit(), nil)
	e.wantPrecondition("CreateJob of a second rebuild", err)
	e.wantPrecondition("DropShadow of a job not ended", e.p.DropShadow(e.ctx, "rb-1"))
	_, err = e.p.CancelJob(e.ctx, "rb-1", at(2), e.audit(), nil)
	e.ok("CancelJob", err)
	_, _, err = e.p.RetryJob(e.ctx, "rb-1", "rb-3", at(3), e.audit(), nil)
	e.wantPrecondition("RetryJob of a rebuild", err)
	if replacing, err = e.p.RebuildReplacing(e.ctx); err != nil || replacing {
		e.Errorf("a replace rebuild canceled is reported replacing: %v %v", replacing, err)
	}
}

// testRebuildGivenUp guards a rebuild whose unit's attempts run out: the
// job ends failed with its reason, and its shadow, which no worker is left
// to drop, is listed as left until DropShadow removes it.
func testRebuildGivenUp(e *renv) {
	e.rebuildSetup()
	e.createRebuild("rb-1", tenant.RebuildCompare)
	w := e.claimRebuild(at(10))
	_, err := e.p.PrepareShadow(e.ctx, w.Fence())
	e.ok("PrepareShadow", err)
	left, err := e.p.ShadowsLeft(e.ctx)
	e.ok("ShadowsLeft", err)
	if len(left) != 0 {
		e.Fatalf("a running rebuild's shadow is listed as left: %v", left)
	}
	now := at(10)
	for i := 0; ; i++ {
		got, err := e.p.FailWork(e.ctx, w.Fence(), "a storage did not answer", now)
		e.ok("FailWork", err)
		if got.State == tenant.WorkFailed {
			break
		}
		if i > tenant.JobUnitAttempts {
			e.Fatalf("the unit is not held failed after %d attempts", i)
		}
		now = now.Add(48 * time.Hour)
		w, err = e.p.ClaimWork(e.ctx, tenant.RebuildWorkKind, "rebuilder", now, time.Minute)
		e.ok("ClaimWork", err)
	}
	j, err := e.p.Job(e.ctx, "rb-1")
	e.ok("Job", err)
	if j.State != tenant.JobFailed || j.Reason != tenant.JobReasonAttemptsRanOut {
		e.Fatalf("the job whose unit gave up is %s (%q), want failed: %s", j.State, j.Reason, tenant.JobReasonAttemptsRanOut)
	}
	left, err = e.p.ShadowsLeft(e.ctx)
	e.ok("ShadowsLeft", err)
	if len(left) != 1 || left[0] != "rb-1" {
		e.Fatalf("the shadows left are %v, want the failed job's", left)
	}
	e.ok("DropShadow", e.p.DropShadow(e.ctx, "rb-1"))
	if left, err = e.p.ShadowsLeft(e.ctx); err != nil || len(left) != 0 {
		e.Errorf("after the drop the shadows left are %v (%v)", left, err)
	}
}

// testRebuildCanceled guards a cancel: the unit is left for its worker,
// which meets the job ended at its next write and at its next start, so it
// drops the shadow and completes the unit rather than writing on.
func testRebuildCanceled(e *renv) {
	e.rebuildSetup()
	e.createRebuild("rb-1", tenant.RebuildCompare)
	w := e.claimRebuild(at(10))
	prog, err := e.p.PrepareShadow(e.ctx, w.Fence())
	e.ok("PrepareShadow", err)
	j, err := e.p.CancelJob(e.ctx, "rb-1", at(11), e.audit(), nil)
	e.ok("CancelJob", err)
	if j.State != tenant.JobCanceled {
		e.Fatalf("the job canceled is %s", j.State)
	}
	scans := e.liveScans()
	_, err = e.p.RebuildScan(e.ctx, tenant.RebuildScanWrite{Fence: w.Fence(), From: prog.Cursor, Key: scans[0].key})
	e.wantPrecondition("RebuildScan of a job canceled", err)
	j, err = e.p.StartRebuildUnit(e.ctx, w.Fence(), at(12))
	e.ok("StartRebuildUnit of a job canceled", err)
	if j.State != tenant.JobCanceled {
		e.Fatalf("the unit's job is %s, want canceled", j.State)
	}
	if left, err := e.p.ShadowsLeft(e.ctx); err != nil || len(left) != 1 {
		e.Errorf("a canceled rebuild's shadow is not listed as left: %v %v", left, err)
	}
	e.ok("DropShadow", e.p.DropShadow(e.ctx, "rb-1"))
	e.ok("CompleteWork", e.p.CompleteWork(e.ctx, w.Fence(), at(13), nil))
	if _, err := e.p.ClaimWork(e.ctx, tenant.RebuildWorkKind, "again", at(500), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a unit of a canceled rebuild is claimed again: %v", err)
	}
}
