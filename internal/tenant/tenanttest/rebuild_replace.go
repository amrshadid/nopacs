package tenanttest

import (
	"sort"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tests of rebuild jobs made at once, and of the activation's harder
// cases: a job that still names a scan, labels traded between rows, and
// the receiver of a session several receptions made.

// testRebuildMadeOnce guards that two rebuilds of one tenant made at the
// same moment, each in a scratch schema of its own, are never both made:
// the condition that no other rebuild is queued or running is on rows that
// do not exist yet, so the store serializes the making of rebuilds and the
// second is refused as a precondition.
func testRebuildMadeOnce(e *renv) {
	scratch := [2]string{e.scratch, e.scratch}
	if e.another != nil {
		scratch[1] = e.another()
	}
	for round := range 20 {
		var wg sync.WaitGroup
		var errs [2]error
		ids := [2]string{}
		for i := range 2 {
			ids[i] = "rb-" + string(rune('a'+round)) + string(rune('0'+i))
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				_, errs[i] = e.p.CreateJob(e.ctx, tenant.NewJob{ID: ids[i], Kind: tenant.JobRebuild,
					Rebuild: &tenant.RebuildJob{Mode: tenant.RebuildCompare, Scratch: scratch[i]}, Created: at(round)}, e.audit(), nil)
			}(i)
		}
		wg.Wait()
		made := 0
		for i, err := range errs {
			switch {
			case err == nil:
				made++
				_, cerr := e.p.CancelJob(e.ctx, ids[i], at(round), e.audit(), nil)
				e.ok("CancelJob", cerr)
				e.ok("DropShadow", e.p.DropShadow(e.ctx, ids[i]))
			case fault.KindOf(err) != fault.Precondition:
				e.Errorf("round %d: a rebuild made beside another: %v, want a precondition failure", round, err)
			}
		}
		if made != 1 {
			e.Fatalf("round %d: %d rebuilds of one tenant were made at once, want 1 (%v)", round, made, errs)
		}
	}
}

// testRebuildActivationRefused guards that a replace is not activated
// while a job not ended names a scan the shadow lacks: the job sends by
// the scan's integer id, so the activation would leave it sending what the
// catalog no longer holds. The refusal changes nothing and holds
// ErrRebuildRefused, FailRebuild ends the rebuild with it as its reason,
// and once the send job has ended the same shadow is activated.
func testRebuildActivationRefused(e *renv) {
	e.rebuildSetup()
	e.addSender("pacs")
	e.createJob("send-1", "pacs", tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, at(1))
	e.list("pacs", at(2))
	var shadow []liveScan
	for _, s := range e.liveScans() {
		if s.scan.SeriesUID != uidOf(2, 101) {
			shadow = append(shadow, s)
		}
	}
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	e.build(w, shadow, 1000)
	before := e.catalogText()
	_, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(30))
	e.wantIs("ActivateCatalog while a send job names a scan the shadow lacks", err, tenant.ErrRebuildRefused, fault.Precondition)
	if got := e.catalogText(); got != before {
		e.Errorf("a refused activation changed the catalog in use")
	}
	reason := fault.Message(err)
	j, err := e.p.FailRebuild(e.ctx, w.Fence(), reason, at(31))
	e.ok("FailRebuild", err)
	if j.State != tenant.JobFailed || j.Reason != reason {
		e.Fatalf("the rebuild refused ended %s (%q), want failed with %q", j.State, j.Reason, reason)
	}
	again, err := e.p.FailRebuild(e.ctx, w.Fence(), "another reason", at(32))
	e.ok("FailRebuild again", err)
	if again.Reason != reason || !again.Ended.Equal(j.Ended) {
		e.Errorf("FailRebuild of a job ended changed it: %+v", again)
	}
	e.ok("DropShadow", e.p.DropShadow(e.ctx, "rb-1"))
	e.ok("CompleteWork", e.p.CompleteWork(e.ctx, w.Fence(), at(33), nil))
	_, err = e.p.CancelJob(e.ctx, "send-1", at(34), e.audit(), nil)
	e.ok("CancelJob of the send job", err)
	e.createRebuild("rb-2", tenant.RebuildReplace)
	w = e.claimRebuild(at(40))
	e.build(w, shadow, 1000)
	j, err = e.p.ActivateCatalog(e.ctx, w.Fence(), at(50))
	e.ok("ActivateCatalog once the send job ended", err)
	if j.State != tenant.JobDone || !j.Rebuild.Activated {
		e.Errorf("the replace after the send job ended is %+v", j)
	}
}

// relabeled returns s with its labels at path p: its manifest's key and
// its instances' keys follow.
func (e *renv) relabeled(s liveScan, p label.Path) liveScan {
	e.Helper()
	s.scan.Path = p
	var err error
	s.key, err = layout.ManifestKey(p)
	e.ok("ManifestKey", err)
	s.instances = append([]tenant.RebuiltInstance(nil), s.instances...)
	for i := range s.instances {
		s.instances[i].Key, err = layout.InstanceKey(p, s.instances[i].SOPInstanceUID)
		e.ok("InstanceKey", err)
	}
	return s
}

// testRebuildTradesLabels guards an activation over rows that trade
// labels: two scans of one session whose labels the storages hold the
// other way round, and two sessions of one subject likewise. Each row is
// named by its UID and keeps its id, so its label changes in place, and
// the unique indexes of labels are met by no row between the activation's
// writes: it activates, and the catalog says what the shadow says.
func testRebuildTradesLabels(e *renv) {
	e.rebuildSetup()
	first, second := "scan-"+uidOf(2, 100), "scan-"+uidOf(2, 101)
	var shadow []liveScan
	studies := map[string]string{}
	for _, s := range e.liveScans() {
		p := s.scan.Path
		studies[p.Session] = s.scan.StudyUID
		switch p.Session {
		case "ses-1":
			p.Session = "ses-2"
		case "ses-2":
			p.Session = "ses-1"
		}
		switch p.Scan {
		case first:
			p.Scan = second
		case second:
			p.Scan = first
		}
		shadow = append(shadow, e.relabeled(s, p))
	}
	sort.Slice(shadow, func(i, j int) bool { return shadow[i].key < shadow[j].key })
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	e.build(w, shadow, 1000)
	j, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(30))
	e.ok("ActivateCatalog over labels traded", err)
	if j.State != tenant.JobDone {
		e.Fatalf("the replace ended %+v", j)
	}
	sc, err := e.p.CatalogScan(e.ctx, label.Path{Subject: "sub-01", Session: "ses-2", Scan: second})
	e.ok("CatalogScan of a scan relabeled", err)
	if sc.SeriesUID != uidOf(2, 100) || sc.Instances != 3 {
		e.Errorf("the scan at its new labels is %+v, want the series %s", sc, uidOf(2, 100))
	}
	for label, study := range map[string]string{"ses-1": studies["ses-2"], "ses-2": studies["ses-1"]} {
		se, err := e.p.CatalogSession(e.ctx, "sub-01", label)
		e.ok("CatalogSession", err)
		if se.StudyUID != study {
			e.Errorf("the session %s is of the study %s, want %s", label, se.StudyUID, study)
		}
	}
	if got := e.liveScans(); len(got) != len(shadow) {
		e.Errorf("the catalog after the activation holds %d scans, the shadow %d", len(got), len(shadow))
	}
}

// testRebuildSessionReceiver guards the receiver a rebuilt session takes
// when its scans' metadata say other things of it, as two receptions into
// one new study at once may leave them: the session's receiver and route
// are those of the scan created first, the reception that made the session
// in the catalog in use, not those of the scan the build met first.
func testRebuildSessionReceiver(e *renv) {
	e.rebuildSetup()
	var shadow []liveScan
	for _, s := range e.liveScans() {
		if s.scan.SeriesUID == uidOf(2, 100) {
			// Met first (its key sorts first), created after the other.
			s.scan.SessionReceiver = "later"
			s.scan.Route = tenant.ReceptionRoute{Route: "other-route"}
			s.scan.Created = s.scan.Created.Add(time.Hour)
		}
		shadow = append(shadow, s)
	}
	if shadow[0].scan.SeriesUID != uidOf(2, 100) {
		e.Fatalf("the scan of the later receiver is not met first: %s", shadow[0].scan.SeriesUID)
	}
	e.createRebuild("rb-1", tenant.RebuildReplace)
	w := e.claimRebuild(at(10))
	e.build(w, shadow, 1000)
	_, err := e.p.ActivateCatalog(e.ctx, w.Fence(), at(30))
	e.ok("ActivateCatalog", err)
	se, err := e.p.CatalogSession(e.ctx, "sub-01", "ses-1")
	e.ok("CatalogSession", err)
	if se.Receiver != "recv" {
		e.Errorf("the rebuilt session's receiver is %q, want recv, that of its scan created first", se.Receiver)
	}
	labels, err := e.p.CatalogLabels(e.ctx, se.StudyUID, nil)
	e.ok("CatalogLabels", err)
	if labels.Session.Route.Route != "" {
		e.Errorf("the rebuilt session's route is %+v, want that of its scan created first", labels.Session.Route)
	}
}
