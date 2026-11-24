package receive

import (
	"bytes"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// setRouting gives the env's receiver its routing, with the storages and
// senders it names added first.
func (e *env) setRouting(r tenant.Routing) {
	e.t.Helper()
	for _, s := range r.Storages() {
		if _, err := e.store.AddStorage(e.ctx, tenant.Storage{Name: s, Type: tenant.StorageDirectory,
			Directory: tenant.DirectoryStorage{Root: "archive-" + s}}, t0, e.audit(), nil); err != nil && fault.KindOf(err) != fault.Conflict {
			e.t.Fatal(err)
		}
	}
	for _, s := range r.Senders() {
		e.addSender(s)
	}
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Routing: r},
		[]tenant.ReceiverField{tenant.FieldRouting}, t0, e.audit(), nil); err != nil {
		e.t.Fatal(err)
	}
}

// addSender adds a DIMSE sender, unless the tenant has it.
func (e *env) addSender(name string) {
	e.t.Helper()
	_, err := e.store.AddSender(e.ctx, tenant.Sender{Name: name, Type: tenant.SenderDIMSE, Enabled: true,
		DIMSE: tenant.DIMSESender{Host: "pacs.example", Port: 4242, CalledAETitle: "PACS", CallingAETitle: "NOPACS"}},
		t0, e.audit(), nil)
	if err != nil && fault.KindOf(err) != fault.Conflict {
		e.t.Fatal(err)
	}
}

// sendAs hands each named corpus file to the pipeline as received in an
// association with the calling AE title given.
func (e *env) sendAs(calling string, names ...string) {
	e.t.Helper()
	for _, n := range names {
		out := e.p.Accept(e.ctx, Arrival{Receiver: "dimse", Body: bytes.NewReader(corpus(e.t, n)),
			Caller: tenant.Caller{CallingAETitle: calling, CalledAETitle: "ARCHIVE", Address: "192.0.2.7:40000"}})
		if out.Status != Received {
			e.t.Fatalf("%s: outcome %v (%s)", n, out.Status, out.Reason)
		}
	}
}

// storeOf returns the env's storage of a name.
func (e *env) storeOf(name string) blob.Store {
	e.t.Helper()
	if name == "main" {
		return e.storage
	}
	s, err := e.storageNamed(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return s
}

// objectsOn returns the DICOM objects of a reception's scans that the named
// storage holds, by SOP Instance UID, read through each scan's manifest on
// it; none when the storage holds no manifest of them.
func objectsOn(t *testing.T, e *env, storage string, r tenant.Reception) map[string][]byte {
	t.Helper()
	s := e.storeOf(storage)
	scans, _, err := e.store.ReceptionScans(e.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]byte{}
	for _, sc := range scans {
		mk, _ := layout.ManifestKey(label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: sc.Label})
		if _, err := s.Stat(e.ctx, mk); err != nil {
			continue
		}
		m, err := manifest.Decode(bytes.NewReader(readObject(t, s, mk)))
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range m.Files {
			out[f.SOPInstanceUID] = readObject(t, s, f.Key)
		}
	}
	return out
}

// receptionOf returns the reception of the study and arrival given.
func receptionOf(t *testing.T, rs []tenant.Reception, study string, arrival int) tenant.Reception {
	t.Helper()
	for _, r := range rs {
		if r.StudyUID == study && r.Arrival == arrival {
			return r
		}
	}
	t.Fatalf("no arrival %d of study %s among %d receptions", arrival, study, len(rs))
	return tenant.Reception{}
}

// TestRouteChoosesInOrder guards a receiver's routes over a real pipeline:
// the first route whose conditions hold chooses the storage of a new
// session (an AE title before a header pattern that holds too); a later
// arrival of that session goes by what was chosen for the session (its
// storage and route), whatever route would hold for it; a study no route
// holds for goes to the receiver's storage.
func TestRouteChoosesInOrder(t *testing.T) {
	e := newEnv(t, envOptions{routing: &tenant.Routing{Routes: []tenant.Route{
		{Name: "lab-a", Storage: "lab-a", Conditions: []tenant.RouteCondition{
			{Subject: tenant.SubjectCallingAETitle, Test: tenant.TestEquals, Value: "LAB-A"}}},
		{Name: "ct", Storage: "lab-b", Conditions: []tenant.RouteCondition{
			{Subject: tenant.SubjectModality, Test: tenant.TestPattern, Value: "C."},
			{Subject: tenant.SubjectSOPClassUID, Test: tenant.TestPrefix, Value: "1.2.840.10008.5.1.4.1.1."},
			{Subject: tenant.SubjectCalledAETitle, Test: tenant.TestPresent, Present: true}}},
	}}})
	ct := studyOf(t, corpus(t, studyFiles[0]))
	e.sendAs("LAB-A", studyFiles[:3]...)
	first := receptionOf(t, e.settle(1), ct, 1)
	if !done(first) || first.Storage != "lab-a" || first.Route.Route != "lab-a" {
		t.Fatalf("the first arrival: %s on %s by %+v, want done on lab-a by route lab-a", first.State, first.Storage, first.Route)
	}
	if n := len(objectsOn(t, e, "lab-a", first)); n != 3 {
		t.Errorf("lab-a holds %d of the first arrival's files, want 3", n)
	}
	if n := len(objectsOn(t, e, "main", first)); n != 0 {
		t.Errorf("the receiver's storage holds %d of a routed study's files", n)
	}
	e.sendAs("OTHER", studyFiles[3:]...)
	late := receptionOf(t, e.settle(2), ct, 2)
	if !done(late) || late.Storage != "lab-a" || late.Route.Route != "lab-a" {
		t.Errorf("the late arrival: %s on %s by %+v, want done on its session's storage, lab-a, by the session's route", late.State, late.Storage, late.Route)
	}
	if n := len(objectsOn(t, e, "lab-b", late)); n != 0 {
		t.Errorf("lab-b holds %d files of a session that is on lab-a", n)
	}
	e.sendAs("OTHER", "MR_small.dcm")
	mr := receptionOf(t, e.settle(3), studyOf(t, corpus(t, "MR_small.dcm")), 1)
	if !done(mr) || mr.Storage != "main" || !mr.Route.IsZero() {
		t.Errorf("a study no route holds for: %s on %s by %+v, want done on main by none", mr.State, mr.Storage, mr.Route)
	}
}

// strictRef is the profile version a route names in place of the
// receiver's.
var strictRef = tenant.ProfileRef{Name: "strict", Version: 1}

// strictProfile is basicProfile that also removes KVP (0018,0060), which
// basicProfile keeps, so a file shows which of the two made it.
func strictProfile() *v1.ProfileDocument {
	doc := basicProfile()
	doc.Rules = append(doc.Rules, &v1.ProfileRule{Name: "kvp",
		Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "KVP"}},
		Action:   &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}})
	return doc
}

// TestRouteProfileAndSenders guards what a route chooses besides the
// storage: its profile version de-identifies the study in place of the
// receiver's, and each instance records it; its senders replace the
// receiver's forward_to. A study no route holds for keeps the receiver's
// profile and senders.
func TestRouteProfileAndSenders(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}, routing: &tenant.Routing{Routes: []tenant.Route{
		{Name: "strict", Storage: "main", Profile: strictRef, ReplacesForwardTo: true, ForwardTo: []string{"pacs"},
			Conditions: []tenant.RouteCondition{{Subject: tenant.SubjectCallingAETitle, Test: tenant.TestEquals, Value: "LAB-A"}}},
	}}})
	e.storeProfile(strictRef, strictProfile())
	e.addSender("orthanc")
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", ForwardTo: []string{"orthanc"}},
		[]tenant.ReceiverField{tenant.FieldForwardTo}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.sendAs("LAB-A", studyFiles[:2]...)
	routed := e.settle(1)[0]
	if !done(routed) || routed.Route.Profile != strictRef {
		t.Fatalf("the routed study: %s by %+v", routed.State, routed.Route)
	}
	objects := objectsOn(t, e, "main", routed)
	if len(objects) != 2 {
		t.Fatalf("the routed study archived %d files, want 2", len(objects))
	}
	kvp := naming.NewTag(0x0018, 0x0060)
	for uid, b := range objects {
		ref, err := e.store.(tenant.Profiles).InstanceProfile(e.ctx, uid)
		if err != nil || ref != strictRef {
			t.Errorf("%s records %v (%v), want the route's %v", uid, ref, err, strictRef)
		}
		h, err := header.Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := h.Lookup(kvp); ok {
			t.Errorf("%s keeps KVP, which the route's profile removes", uid)
		}
	}
	e.sendAs("OTHER", "MR_small.dcm")
	rs := e.settle(2)
	other := receptionOf(t, rs, remapped(studyOf(t, corpus(t, "MR_small.dcm"))), 1)
	for uid := range objectsOn(t, e, "main", other) {
		if ref, err := e.store.(tenant.Profiles).InstanceProfile(e.ctx, uid); err != nil || ref != testProfile {
			t.Errorf("a study no route holds for records %v (%v), want the receiver's %v", ref, err, testProfile)
		}
	}
	jobs, _, err := e.store.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{routed.ID: "pacs", other.ID: "orthanc"}
	if len(jobs) != len(want) {
		t.Errorf("%d forwards, want one per reception", len(jobs))
	}
	for _, j := range jobs {
		if want[j.ReceptionID] != j.Sender {
			t.Errorf("reception %s forwarded to %s, want %s", j.ReceptionID, j.Sender, want[j.ReceptionID])
		}
	}
}

// TestRouteUnroutedHeldAndReleased guards a routing that holds what no
// route chooses: every file of the study is held as unrouted, with the
// cause, its bytes kept; a release naming a storage the tenant does not
// have is refused; a release naming one it has archives the study there,
// with no route evaluated again.
func TestRouteUnroutedHeldAndReleased(t *testing.T) {
	e := newEnv(t, envOptions{routing: &tenant.Routing{QuarantineUnmatched: true, Routes: []tenant.Route{
		{Name: "nobody", Storage: "lab-a", Conditions: []tenant.RouteCondition{
			{Subject: tenant.SubjectCallingAETitle, Test: tenant.TestEquals, Value: "NOBODY"}}},
	}}})
	e.sendAs("SCANNER", studyFiles[:2]...)
	r := e.settle(1)[0]
	if r.State != "quarantined" || r.Counts.FilesQuarantined != 2 {
		t.Fatalf("an unrouted study: %s (%s) with %+v", r.State, r.Failure, r.Counts)
	}
	held := e.held()
	for _, q := range held {
		if q.Reason != tenant.QuarantineUnrouted || q.Details["cause"] != "no_match" || !q.ContentKept {
			t.Errorf("a held file: %s %v kept %v", q.Reason, q.Details, q.ContentKept)
		}
	}
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: held[0].ID, Mode: tenant.ReleaseAsIs, Storage: "nowhere"}, e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		t.Errorf("a release naming a storage the tenant does not have: %v, want a precondition failure", err)
	}
	if _, err := e.store.AddStorage(e.ctx, tenant.Storage{Name: "lab-c", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive-lab-c"}}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	for _, q := range held {
		if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, Storage: "lab-c"}, e.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	released := receptionOf(t, e.settle(2), r.StudyUID, 2)
	if !done(released) || released.Storage != "lab-c" || released.Route.ReleasedStorage != "lab-c" || released.Route.Route != "" {
		t.Errorf("the released study: %s on %s by %+v, want done on lab-c as released", released.State, released.Storage, released.Route)
	}
	if n := len(objectsOn(t, e, "lab-c", released)); n != 2 {
		t.Errorf("lab-c holds %d of the released files, want 2", n)
	}
}
