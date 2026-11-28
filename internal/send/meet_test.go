package send

import (
	"bytes"
	"crypto/sha256"
	"net/http"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/jobmeet"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// grace is the grace period the corrections of these tests publish with.
const grace = tenant.DefaultGrace

// deleteTarget deletes what target names (testkit.DeleteTarget).
func (f *fixture) deleteTarget(target tenant.CorrectionTarget) {
	f.t.Helper()
	testkit.DeleteTarget(f.t, f.p, f.audit(), target, f.clock.Now())
}

// replace replaces the archived file of the instance uid, of the scan
// label, by file (the same instance, other bytes): the new revision's
// object is put on the storage at its revision key, and the replace is
// prepared and published.
func (f *fixture) replace(scan, uid string, file []byte) {
	f.t.Helper()
	now := f.clock.Now()
	path := label.Path{Subject: "sub-01", Session: "ses-1", Scan: scan}
	key, err := layout.RevisionKey(path, uid, 2)
	if err != nil {
		f.t.Error("RevisionKey:", err)
		return
	}
	if _, err := f.store.Put(f.ctx, key, bytes.NewReader(file)); err != nil {
		f.t.Error("Put of the new revision:", err)
		return
	}
	sum := tenant.SHA256(sha256.Sum256(file))
	q := tenant.QuarantinedFile{ID: f.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: "recv", ReceptionID: f.reception,
		Size: int64(len(file)), SHA256: sum, KeptSHA256: tenant.SHA256(sha256.Sum256(f.files[uid])),
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", SOPInstanceUID: uid, StudyUID: f.study, SeriesUID: f.series[scan],
		TransferSyntaxUID: "1.2.840.10008.1.2.1", Incoming: "quarantine/" + uid, ContentKept: true, Quarantined: now}
	if _, err := f.p.HoldFiles(f.ctx, []tenant.QuarantinedFile{q}, nil); err != nil {
		f.t.Error("HoldFiles:", err)
		return
	}
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		f.t.Error("NewCorrectionID:", err)
		return
	}
	c, _, err := f.p.PrepareCorrection(f.ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindReplace, Quarantine: q.ID,
		NewSHA256: sum, NewSize: int64(len(file)), RestoreLine: []byte(`{"line":1}`), Reason: "corrected re-send", Now: now},
		f.audit(), nil)
	if err != nil {
		f.t.Error("PrepareCorrection of a replace:", err)
		return
	}
	manifest := tenant.SHA256(sha256.Sum256([]byte(uid)))
	// The new revision de-identified by the fixture's profile version, as the
	// one it replaces, so the default rule lets it out.
	replaced := tenant.InstanceProvenance{Profile: fixtureProfile}
	if _, err := f.p.CommitCorrection(f.ctx, tenant.CorrectionCommit{ID: c.ID, Now: now, Grace: grace,
		Manifests: map[string]tenant.SHA256{f.series[scan]: manifest}, Replaced: &replaced}); err != nil {
		f.t.Error("CommitCorrection of a replace:", err)
	}
}

// changed returns file with its last byte changed: the same instance, other
// bytes, still a file the sender can read.
func changed(file []byte) []byte {
	out := slices.Clone(file)
	out[len(out)-1] ^= 0xff
	return out
}

// sortedUIDs returns the SOP Instance UIDs of the files of a scan, in the
// order of the stream.
func sortedUIDs(t testing.TB, files [][]byte) []string {
	t.Helper()
	out := make([]string, len(files))
	for i, b := range files {
		out[i] = uidOf(t, b)
	}
	sort.Strings(out)
	return out
}

// meetScans are the four scans of three instances a job is sent over.
var meetScans = []string{"a", "b", "c", "d"}

// meetScript is what happens to the archive while a job sends over four
// scans, one unit at a time, in the order the store hands the units out:
// it runs as instances arrive at the peer, and records what it did.
//
// As the first instance of the first scan arrives, the second instance of
// another scan is deleted, before its unit starts; as the last instance of
// that first scan arrives, it is replaced, while it is being sent. As the
// first instance of the second scan arrives, the second of its instances is
// deleted, after its unit read its stream. As the first instance of the
// third scan arrives, the second of its instances is replaced, before the
// unit reaches it. A scan holds one correction, so each of the four scans
// takes one part.
type meetScript struct {
	f     *fixture
	files map[string][][]byte
	// order is each scan's UIDs in the stream's order.
	order map[string][]string

	mu sync.Mutex
	// started are the scans (but gone) whose first instance has arrived, in
	// order; gone is the scan an instance of which is deleted before its
	// unit starts.
	started []string
	gone    string
	// during and before are the replacements of the instance replaced as it
	// was sent and of the one replaced before its turn.
	during, before []byte
	ran            []bool
}

// newMeetScript archives the four scans and returns the script over them.
func newMeetScript(t *testing.T, f *fixture) *meetScript {
	t.Helper()
	all := generated(t, 12, 1500)
	s := &meetScript{f: f, files: map[string][][]byte{}, order: map[string][]string{}}
	var scans []scanFiles
	for i, l := range meetScans {
		s.files[l] = all[3*i : 3*i+3]
		s.order[l] = sortedUIDs(t, s.files[l])
		scans = append(scans, scanFiles{l, s.files[l]})
	}
	f.archive("ses-1", scans...)
	return s
}

// onArrival runs the script as the instance uid has arrived whole at the
// peer, before the peer answers.
func (s *meetScript) onArrival(uid string) {
	f := s.f
	scan := f.scanOf[uid]
	s.mu.Lock()
	if scan == s.gone {
		s.mu.Unlock()
		return
	}
	step := slices.Index(s.started, scan)
	if step < 0 {
		s.started = append(s.started, scan)
		step = len(s.started) - 1
		if step == 0 {
			for _, l := range meetScans {
				if l != scan {
					s.gone = l
				}
			}
		}
	}
	first := step == len(s.started)-1 && s.started[step] == scan && !s.doneStep(step)
	s.mu.Unlock()
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	switch {
	case first && step == 0:
		target.Scan, target.Instance = s.gone, s.order[s.gone][1]
		f.deleteTarget(target)
	case first && step == 1:
		target.Scan, target.Instance = scan, s.order[scan][1]
		f.deleteTarget(target)
	case first && step == 2:
		s.mu.Lock()
		s.before = changed(f.files[s.order[scan][1]])
		s.mu.Unlock()
		f.replace(scan, s.order[scan][1], s.before)
	}
	if step == 0 && uid == s.order[scan][2] {
		s.mu.Lock()
		s.during = changed(f.files[uid])
		s.mu.Unlock()
		f.replace(scan, uid, s.during)
	}
}

// doneSteps are the steps of the script already run.
func (s *meetScript) doneStep(step int) bool {
	for len(s.ran) <= step {
		s.ran = append(s.ran, false)
	}
	r := s.ran[step]
	s.ran[step] = true
	return r
}

// check asserts what a job did while the script changed the archive: the
// deleted instances were not sent and are listed as deleted in the archive,
// the instance replaced before its turn was sent at its new revision, the
// one replaced as it was sent was sent as it was and tallied, and the
// counts add up to the instances the job counted.
func (s *meetScript) check(t *testing.T, j tenant.Job, arrived func(uid string) ([]byte, bool), same func(got, file []byte) bool) {
	t.Helper()
	f := s.f
	if len(s.started) != 3 {
		t.Fatalf("%v started sending, want three scans besides the one an instance of which was deleted first", s.started)
	}
	first, second, third := s.started[0], s.started[1], s.started[2]
	want := map[string][]byte{}
	for _, uid := range s.order[first] {
		want[uid] = f.files[uid]
	}
	want[s.order[second][0]], want[s.order[second][2]] = f.files[s.order[second][0]], f.files[s.order[second][2]]
	want[s.order[s.gone][0]], want[s.order[s.gone][2]] = f.files[s.order[s.gone][0]], f.files[s.order[s.gone][2]]
	want[s.order[third][0]], want[s.order[third][1]], want[s.order[third][2]] = f.files[s.order[third][0]], s.before, f.files[s.order[third][2]]
	for uid, file := range want {
		got, ok := arrived(uid)
		if !ok {
			t.Errorf("instance %s never arrived at the peer", uid)
			continue
		}
		if !same(got, file) {
			t.Errorf("instance %s arrived as %d bytes that are not the revision current when its transfer started (%d bytes)", uid, len(got), len(file))
		}
	}
	deleted := []string{s.order[second][1], s.order[s.gone][1]}
	for _, uid := range deleted {
		if _, ok := arrived(uid); ok {
			t.Errorf("instance %s was deleted and still arrived at the peer", uid)
		}
	}
	var listed []string
	for _, fl := range f.failures(j.ID) {
		if fl.Code != tenant.FailureDeletedInArchive {
			t.Errorf("failure %+v, want only deleted_in_archive", fl)
		}
		listed = append(listed, fl.SOPInstanceUID)
	}
	sort.Strings(listed)
	sort.Strings(deleted)
	if !slices.Equal(listed, deleted) {
		t.Errorf("failures list %v, want the deleted instances %v", listed, deleted)
	}
	c := j.Counts
	if c.Instances != 12 || c.InstancesSent != 10 || c.InstancesRefused != 2 || c.InstancesWarned != 0 || c.InstancesRemaining() != 0 {
		t.Errorf("counts %+v, want 12 instances: 10 sent, 2 refused, none remaining", c)
	}
	tallies, err := f.p.JobTallies(f.ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tallies) != 1 || tallies[0].Key != jobmeet.TallySupersededDuringSend || tallies[0].Count != 1 {
		t.Errorf("tallies %+v, want superseded_during_send once, for the instance replaced as it was sent", tallies)
	}
}

// TestDIMSEMeetsTheArchiveAsItIsNow guards a send job over an archive that
// changes while it runs: an instance deleted before its unit started, and
// one deleted after its unit read its stream, are not sent and are listed
// as deleted in the archive; an instance replaced before its turn is
// sent as its current revision; one replaced while it is sent is sent as it
// was and tallied as superseded during the send; the counts add up to the
// instances counted.
func TestDIMSEMeetsTheArchiveAsItIsNow(t *testing.T) {
	f := newFixture(t)
	script := newMeetScript(t, f)
	peer := &dimsePeer{}
	peer.answer = func(uid string, _ int) uint16 {
		script.onArrival(uid)
		return 0
	}
	addr := startPeer(t, peer)
	f.addDIMSE("peer", addr, func(s *tenant.Sender) { s.MaxAssociations = 1 })
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 60*time.Second, ended)
	script.check(t, j, func(uid string) ([]byte, bool) {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		if _, ok := peer.count[uid]; !ok {
			return nil, false
		}
		return peer.data[uid], true
	}, func(got, file []byte) bool {
		want, _ := dataSetOfFile(t, file)
		return bytes.Equal(got, want)
	})
}

// TestStowMeetsTheArchiveAsItIsNow is the same guard for a STOW-RS sender,
// a request of one instance at a time: what is deleted before its part
// starts is not read or sent and is listed as deleted in the archive; what
// is replaced before its part starts is sent as its current revision; what
// is replaced during its request is tallied.
func TestStowMeetsTheArchiveAsItIsNow(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	script := newMeetScript(t, f)
	peer := &webPeer{auth: "Bearer token-xyz"}
	peer.answer = func(uids []string, _ map[string]int) (int, []byte) {
		for _, u := range uids {
			script.onArrival(u)
		}
		return http.StatusOK, storeAnswer(uids, nil, nil)
	}
	url := startWebPeer(t, peer)
	f.addStow("pacs", url, func(s *tenant.Sender) { s.STOW.MaxInstancesPerRequest = 1; s.MaxAssociations = 1 })
	job := f.createJob("pacs", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 60*time.Second, ended)
	script.check(t, j, func(uid string) ([]byte, bool) {
		peer.mu.Lock()
		defer peer.mu.Unlock()
		got, ok := peer.parts[uid]
		return got, ok
	}, bytes.Equal)
}

// TestScanDeletedWholeBeforeItsUnit guards a send job one of whose scans
// is deleted whole after the job listed it and before its unit started:
// the unit starts all the same (it is not refused as no job's and tried
// again on each attempt), sends nothing of the scan, and refuses each of
// its instances as deleted in the archive, so the job ends partial with
// every instance answered and none remaining.
func TestScanDeletedWholeBeforeItsUnit(t *testing.T) {
	f := newFixture(t)
	all := generated(t, 6, 1500)
	f.archive("ses-1", scanFiles{"a", all[:3]}, scanFiles{"b", all[3:]})
	var mu sync.Mutex
	var first, gone string
	peer := &dimsePeer{}
	peer.answer = func(uid string, _ int) uint16 {
		mu.Lock()
		defer mu.Unlock()
		if first == "" {
			first = f.scanOf[uid]
			gone = "a"
			if first == "a" {
				gone = "b"
			}
			f.deleteTarget(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: gone})
		}
		return 0
	}
	addr := startPeer(t, peer)
	f.addDIMSE("peer", addr, func(s *tenant.Sender) { s.MaxAssociations = 1 })
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	j := f.waitJob(s, job.ID, 30*time.Second, ended)
	mu.Lock()
	defer mu.Unlock()
	c := j.Counts
	if j.State != tenant.JobPartial || c.Instances != 6 || c.InstancesSent != 3 || c.InstancesRefused != 3 || c.InstancesRemaining() != 0 {
		t.Fatalf("job %s %+v, want partial: 6 instances, 3 sent, 3 refused, none remaining", j.State, c)
	}
	for uid, scan := range f.scanOf {
		peer.mu.Lock()
		n := peer.count[uid]
		peer.mu.Unlock()
		if scan == gone && n != 0 {
			t.Errorf("instance %s of the scan deleted whole arrived %d times, want never", uid, n)
		}
	}
	failures := f.failures(j.ID)
	if len(failures) != 3 {
		t.Fatalf("the job lists %d failures, want the deleted scan's three", len(failures))
	}
	for _, fl := range failures {
		if fl.Code != tenant.FailureDeletedInArchive || f.scanOf[fl.SOPInstanceUID] != gone {
			t.Errorf("failure %+v, want deleted_in_archive of an instance of scan %s", fl, gone)
		}
	}
}

// TestUnitStopsWhenItsTenantIsNoLongerServed guards a unit whose tenant
// stops being ready (moved, fenced, restoring) while it sends: it stops
// before its next instance, is given back with the reason the registry
// states and without an attempt spent, keeps its place, and when the tenant
// is served again sends the rest, none of the instances sent twice.
func TestUnitStopsWhenItsTenantIsNoLongerServed(t *testing.T) {
	f := newFixture(t)
	rec := f.record()
	f.archive("ses-1", scanFiles{"a", generated(t, 4, 1500)})
	var once sync.Once
	peer := &dimsePeer{}
	peer.answer = func(string, int) uint16 {
		once.Do(func() { f.ten.notReady.Store(true) })
		return 0
	}
	addr := startPeer(t, peer)
	f.addDIMSE("peer", addr, func(s *tenant.Sender) { s.MaxAssociations = 1 })
	job := f.createJob("peer", "ses-1")
	s := f.start(f.config())
	waitFor(t, 20*time.Second, "the unit to be given back", func() bool {
		released, _ := rec.counts()
		return released > 0
	})
	rec.mu.Lock()
	reason := rec.reasons[0]
	rec.mu.Unlock()
	if reason != "the tenant is not ready" {
		t.Errorf("the unit was given back with %q, want the reason the registry states", reason)
	}
	if _, failed := rec.counts(); failed != 0 {
		t.Errorf("%d attempts were spent on a tenant that was not served, want none", failed)
	}
	if n := peer.arrivals(); n != 1 {
		t.Errorf("%d instances arrived while the tenant was not served, want the one in flight", n)
	}
	f.ten.notReady.Store(false)
	j := f.waitJob(s, job.ID, 60*time.Second, ended)
	if j.State != tenant.JobDone || j.Counts.InstancesSent != 4 {
		t.Fatalf("job %s %+v, want done with 4 sent", j.State, j.Counts)
	}
	for uid := range f.files {
		peer.mu.Lock()
		n := peer.count[uid]
		peer.mu.Unlock()
		if n != 1 {
			t.Errorf("instance %s arrived %d times, want once", uid, n)
		}
	}
}
