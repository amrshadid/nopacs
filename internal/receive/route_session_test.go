package receive

import (
	"bytes"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// byAE is a route of the name and storage given for the calling AE title.
func byAE(name, storage, ae string) tenant.Route {
	return tenant.Route{Name: name, Storage: storage,
		Conditions: []tenant.RouteCondition{{Subject: tenant.SubjectCallingAETitle, Test: tenant.TestEquals, Value: ae}}}
}

// sendTo hands named corpus files, or bytes, to a receiver of the env as
// from the calling AE title given.
func (e *env) sendTo(receiver, calling string, bodies ...[]byte) {
	e.t.Helper()
	for _, b := range bodies {
		out := e.p.Accept(e.ctx, Arrival{Receiver: receiver, Body: bytes.NewReader(b),
			Caller: tenant.Caller{CallingAETitle: calling, CalledAETitle: "ARCHIVE", Address: "192.0.2.7:40000"}})
		if out.Status != Received {
			e.t.Fatalf("outcome %v (%s)", out.Status, out.Reason)
		}
	}
}

// corpora reads named corpus files.
func corpora(t *testing.T, names ...string) [][]byte {
	t.Helper()
	out := make([][]byte, len(names))
	for i, n := range names {
		out[i] = corpus(t, n)
	}
	return out
}

// dataKeys returns the keys of the DICOM objects a storage holds.
func (e *env) dataKeys(storage string) []string {
	e.t.Helper()
	l, err := e.storeOf(storage).List(e.ctx, blob.ListOptions{Prefix: "data/"})
	if err != nil {
		e.t.Fatal(err)
	}
	return l.Keys
}

// waitReal waits, moving the clock by step, for cond, while a step may be
// held in a hook (which env.waitFor would wait out before moving the
// clock).
func (e *env) waitReal(what string, step time.Duration, cond func() bool) {
	e.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			e.t.Fatalf("timed out waiting for %s", what)
		}
		e.clock.Advance(step)
		time.Sleep(20 * time.Millisecond)
	}
}

// receptionState returns the state of the reception of study and arrival,
// or "" when there is none yet.
func (e *env) receptionState(study string, arrival int) reception.State {
	for _, r := range e.receptions() {
		if r.StudyUID == study && r.Arrival == arrival {
			return r.State
		}
	}
	return ""
}

// TestRouteLateArrivalTakesSessionChoice guards that a study is routed once,
// for its session: a later arrival from another AE title, which no route
// holds for, goes by what was chosen for the session, so every instance of
// the session is de-identified with the route's profile version.
func TestRouteLateArrivalTakesSessionChoice(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}, routing: &tenant.Routing{Routes: []tenant.Route{
		{Name: "strict", Storage: "main", Profile: strictRef, Conditions: byAE("", "", "LAB-A").Conditions},
	}}})
	e.storeProfile(strictRef, strictProfile())
	e.sendAs("LAB-A", studyFiles[:2]...)
	first := e.settle(1)[0]
	e.sendAs("OTHER", studyFiles[2:4]...)
	rs := e.settle(2)
	late := receptionOf(t, rs, first.StudyUID, 2)
	if !done(late) || late.Route.Profile != strictRef {
		t.Fatalf("the late arrival: %s by %+v, want done by the session's choice", late.State, late.Route)
	}
	n := 0
	for _, r := range rs {
		for uid := range objectsOn(t, e, "main", r) {
			n++
			if ref, err := e.store.(tenant.Profiles).InstanceProfile(e.ctx, uid); err != nil || ref != strictRef {
				t.Errorf("%s records %v (%v), want the session's %v", uid, ref, err, strictRef)
			}
		}
	}
	if n != 4 {
		t.Errorf("%d instances archived, want 4", n)
	}
}

// TestRouteReceptionsInFlightShareARoute guards a second reception of a
// study that arrives while the first, routed to lab-a, is still archiving
// (held before its commit): the second goes by the first's choice, not by
// the route its own AE title holds for, so every file of the study ends on
// lab-a, and nothing is put on another storage.
func TestRouteReceptionsInFlightShareARoute(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	var held atomic.Bool
	e := newEnv(t, envOptions{routing: &tenant.Routing{Routes: []tenant.Route{byAE("lab-a", "lab-a", "LAB-A"), byAE("lab-b", "lab-b", "OTHER")}},
		hook: func(p string) {
			if p == "archive.before-commit" {
				// Only the first comes here held; sync.Once would hold
				// every later caller too, until the first returns.
				if held.CompareAndSwap(false, true) {
					close(blocked)
					<-release
				}
			}
		}})
	t.Cleanup(free)
	study := studyOf(t, corpus(t, studyFiles[0]))
	e.sendAs("LAB-A", studyFiles[:3]...)
	e.waitReal("the first reception to reach its commit", time.Second, func() bool {
		select {
		case <-blocked:
			return true
		default:
			return false
		}
	})
	e.sendAs("OTHER", studyFiles[3:]...)
	e.waitReal("the second reception to be identified", time.Second, func() bool {
		s := e.receptionState(study, 2)
		return s != "" && s != reception.Receiving && s != reception.Complete
	})
	free()
	rs := e.settle(2)
	for _, r := range rs {
		if !done(r) || r.Storage != "lab-a" || r.Route.Route != "lab-a" {
			t.Errorf("reception %d: %s on %s by %+v, want done on lab-a by the first's route", r.Arrival, r.State, r.Storage, r.Route)
		}
	}
	if n := len(e.dataKeys("lab-a")); n != 7 {
		t.Errorf("lab-a holds %d objects, want the study's 7", n)
	}
	for _, s := range []string{"lab-b", "main"} {
		if keys := e.dataKeys(s); len(keys) != 0 {
			t.Errorf("%s holds %v", s, keys)
		}
	}
}

// TestRouteSessionElsewhereHeld guards the last line against two
// receptions of a study identified at once at two receivers, each by its
// own routes: the second to commit finds the session on another storage
// and holds its files (unrouted, session_elsewhere) before any is put,
// never failing in a loop or leaving an object nothing lists.
func TestRouteSessionElsewhereHeld(t *testing.T) {
	blocked, release := make(chan struct{}), make(chan struct{})
	free := sync.OnceFunc(func() { close(release) })
	var held atomic.Bool
	e := newEnv(t, envOptions{routing: &tenant.Routing{Routes: []tenant.Route{byAE("lab-a", "lab-a", "LAB-A")}},
		hook: func(p string) {
			if p == "identify.before-step" {
				// Only the first comes here held; sync.Once would hold
				// every later caller too, until the first returns.
				if held.CompareAndSwap(false, true) {
					close(blocked)
					<-release
				}
			}
		}})
	t.Cleanup(free)
	if _, err := e.store.AddStorage(e.ctx, tenant.Storage{Name: "lab-b", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive-lab-b"}}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	// The first receiver is quiet for longer, so the second's reception,
	// which opens later, is identified first.
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", QuietPeriod: 30 * time.Second},
		[]tenant.ReceiverField{tenant.FieldQuietPeriod}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.AddReceiver(e.ctx, tenant.Receiver{Name: "second", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE:   tenant.DIMSEReceiver{Port: 11113, AETitle: "SECOND", AnyCaller: true},
		Routing: tenant.Routing{Routes: []tenant.Route{byAE("lab-b", "lab-b", "OTHER")}}}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	study := studyOf(t, corpus(t, studyFiles[0]))
	e.sendTo("dimse", "LAB-A", corpora(t, studyFiles[:3]...)...)
	e.sendTo("second", "OTHER", corpora(t, studyFiles[3:5]...)...)
	e.waitReal("the second reception's identify", time.Second, func() bool {
		select {
		case <-blocked:
			return true
		default:
			return false
		}
	})
	e.waitReal("the first reception to end", 2*time.Second, func() bool { return e.receptionState(study, 1) == reception.Done })
	free()
	rs := e.settle(2)
	first, second := receptionOf(t, rs, study, 1), receptionOf(t, rs, study, 2)
	if !done(first) || first.Storage != "lab-a" {
		t.Errorf("the first reception: %s on %s, want done on lab-a", first.State, first.Storage)
	}
	if second.State != reception.Quarantined || second.Counts.FilesQuarantined != 2 {
		t.Errorf("the second reception: %s with %+v, want its 2 files held", second.State, second.Counts)
	}
	for _, q := range e.held() {
		if q.ReceptionID == second.ID && (q.Reason != tenant.QuarantineUnrouted || q.Details["cause"] != "session_elsewhere") {
			t.Errorf("a file of the second reception held as %s %v", q.Reason, q.Details)
		}
	}
	for _, s := range []string{"lab-b", "main"} {
		if keys := e.dataKeys(s); len(keys) != 0 {
			t.Errorf("%s holds %v", s, keys)
		}
	}
}

// TestRouteReleaseChoosesOnlyStorage guards a release naming a storage: it
// chooses the storage only; the routes still choose the profile version
// (here, a route mended after the files were held), so every released
// file is de-identified with the route's version and archived on the
// storage the release named.
func TestRouteReleaseChoosesOnlyStorage(t *testing.T) {
	strictOnLabA := func(extra ...tenant.RouteCondition) tenant.Routing {
		rt := byAE("strict", "lab-a", "LAB-A")
		rt.Profile, rt.Conditions = strictRef, append(rt.Conditions, extra...)
		return tenant.Routing{QuarantineUnmatched: true, Routes: []tenant.Route{rt}}
	}
	routing := strictOnLabA(tenant.RouteCondition{Subject: tenant.SubjectModality, Test: tenant.TestEquals, Value: "XX"})
	e := newEnv(t, envOptions{deid: &deidOptions{}, routing: &routing})
	e.storeProfile(strictRef, strictProfile())
	e.sendAs("LAB-A", studyFiles[:3]...)
	held := e.settle(1)[0]
	if held.State != reception.Quarantined || held.Counts.FilesQuarantined != 3 {
		t.Fatalf("the study no route held for: %s with %+v", held.State, held.Counts)
	}
	e.setRouting(strictOnLabA())
	if _, err := e.store.AddStorage(e.ctx, tenant.Storage{Name: "lab-c", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive-lab-c"}}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	for _, q := range e.held() {
		if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, Storage: "lab-c"}, e.audit(), nil); err != nil {
			t.Fatal(err)
		}
	}
	released := receptionOf(t, e.settle(2), held.StudyUID, 2)
	if !done(released) || released.Storage != "lab-c" || released.Route.Route != "strict" || released.Route.Profile != strictRef {
		t.Fatalf("the released study: %s on %s by %+v, want done on lab-c by route strict", released.State, released.Storage, released.Route)
	}
	objects := objectsOn(t, e, "lab-c", released)
	if len(objects) != 3 {
		t.Errorf("lab-c holds %d of the released files, want 3", len(objects))
	}
	for uid := range objects {
		if ref, err := e.store.(tenant.Profiles).InstanceProfile(e.ctx, uid); err != nil || ref != strictRef {
			t.Errorf("%s records %v (%v), want the route's %v", uid, ref, err, strictRef)
		}
	}
}

// TestRouteValueTooLongIsUnusable guards the bound of a value a route
// reads: a file whose Manufacturer is 4 MiB makes the route that reads it
// unusable, so the study is held as unrouted at once, not tested by a
// pattern over the whole value.
func TestRouteValueTooLongIsUnusable(t *testing.T) {
	e := newEnv(t, envOptions{routing: &tenant.Routing{Routes: []tenant.Route{{Name: "maker", Storage: "lab-a",
		Conditions: []tenant.RouteCondition{{Subject: tenant.SubjectManufacturer, Test: tenant.TestPattern, Value: `(a|b|c|d)*x`}}}}}})
	long := rewrite(t, corpus(t, studyFiles[0]), "", func(ds *dataset.Dataset) {
		ds.Remove(tag.New(0x0008, 0x0070))
		if err := ds.Add(dataelem.NewDataElement(tag.New(0x0008, 0x0070), dataelem.VR("UN"), []byte(strings.Repeat("abcd", 1<<20)))); err != nil {
			t.Fatal(err)
		}
	})
	start := time.Now()
	e.sendTo("dimse", "SCANNER", long)
	r := e.settle(1)[0]
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("the study took %v", d)
	}
	qs := e.held()
	if r.State != reception.Quarantined || len(qs) != 1 || qs[0].Reason != tenant.QuarantineUnrouted ||
		qs[0].Details["cause"] != "route_unusable" || qs[0].Details["route"] != "maker" {
		t.Errorf("a 4 MiB Manufacturer: %s, held %+v", r.State, qs)
	}
}

// TestRouteReleaseLandsAgainWithStorage guards a release naming a storage
// of a file that joins no reception (recorded as received at a receiver
// that de-identifies now): the file lands again, de-identified, and the
// reception it joins keeps the storage, so it is archived there.
func TestRouteReleaseLandsAgainWithStorage(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	e.send(studyFiles[0])
	e.waitFor("the file to be recorded", time.Second, func() bool { return e.p.stats.Recorded.Load() == 1 })
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Profile: testProfile},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.settle(1)
	qs := e.held()
	if len(qs) != 1 || qs[0].Details["cause"] != "recorded_as_received" {
		t.Fatalf("held %+v", qs)
	}
	if _, err := e.store.AddStorage(e.ctx, tenant.Storage{Name: "lab-c", Type: tenant.StorageDirectory,
		Directory: tenant.DirectoryStorage{Root: "archive-lab-c"}}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: qs[0].ID, Mode: tenant.ReleaseAsIs, Storage: "lab-c"}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	for _, r := range e.settle(2) {
		if done(r) {
			if r.Storage != "lab-c" || r.Route.ReleasedStorage != "lab-c" {
				t.Errorf("the landed file's reception: on %s by %+v, want lab-c, as the release named", r.Storage, r.Route)
			}
			if n := len(e.dataKeys("lab-c")); n != 1 {
				t.Errorf("lab-c holds %d objects, want 1", n)
			}
			return
		}
	}
	t.Fatal("no reception archived the released file")
}
