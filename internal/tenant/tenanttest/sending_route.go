package tenanttest

import (
	"reflect"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// suiteRouting is a routing with every kind of condition, a profile, and
// senders that replace the receiver's.
func suiteRouting() tenant.Routing {
	return tenant.Routing{QuarantineUnmatched: true, Routes: []tenant.Route{
		{Name: "lab-a", Storage: "other", Profile: tenant.ProfileRef{Name: "strict", Version: 2},
			ReplacesForwardTo: true, ForwardTo: []string{"pacs"},
			Conditions: []tenant.RouteCondition{
				{Subject: tenant.SubjectCallingAETitle, Test: tenant.TestEquals, Value: "LAB A"},
				{Subject: tenant.SubjectModality, Test: tenant.TestPattern, Value: "CT|MR"},
				{Subject: tenant.SubjectManufacturer, Test: tenant.TestPrefix, Value: "SIE", Negate: true},
				{Subject: tenant.SubjectSoftwareVersions, Test: tenant.TestPresent, Present: true},
			}},
		{Name: "none", Storage: "store", ReplacesForwardTo: true},
	}}
}

// deidReceiver sets the receiver to de-identify with a profile version,
// which a route that names a profile needs; the store does not ask whether
// the version exists (the caller does).
func (e *senv) deidReceiver(name string) {
	e.Helper()
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: name, Profile: tenant.ProfileRef{Name: "basic", Version: 1}},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, T0, e.audit(), nil)
	e.ok("SetReceiver deidentification", err)
}

// testRouting guards a receiver's routing: it is stored and read back
// exactly, with its order, every kind of condition and its default; it is
// replaced whole; a change made on a receiver read before another change
// is refused as a precondition failure, as a stale version is, and made on
// the one read now.
func testRouting(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addStorage("other")
	e.addSender("pacs")
	e.deidReceiver("recv")
	read, err := e.p.Receiver(e.ctx, "recv")
	e.ok("Receiver", err)
	set, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", Routing: suiteRouting(), Updated: read.Updated},
		[]tenant.ReceiverField{tenant.FieldRouting}, at(10), e.audit(), nil)
	e.ok("SetReceiver routing", err)
	got, err := e.p.Receiver(e.ctx, "recv")
	e.ok("Receiver", err)
	if !reflect.DeepEqual(got.Routing, suiteRouting()) || !reflect.DeepEqual(set.Routing, got.Routing) {
		e.Errorf("the routing read back:\n%+v\nwant\n%+v", got.Routing, suiteRouting())
	}
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", Routing: tenant.Routing{}, Updated: read.Updated},
		[]tenant.ReceiverField{tenant.FieldRouting}, at(20), e.audit(), nil)
	if fault.KindOf(err) != fault.Precondition {
		e.Errorf("a change on a receiver read before another change: %v, want a precondition failure", err)
	}
	cleared, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", Updated: got.Updated},
		[]tenant.ReceiverField{tenant.FieldRouting}, at(30), e.audit(), nil)
	e.ok("SetReceiver routing cleared", err)
	if !cleared.Routing.IsZero() {
		e.Errorf("a routing set to none reads %+v", cleared.Routing)
	}
}

// testRoutingNamesWhatExists guards what a routing may name: a storage and
// senders the tenant has, a profile only on a receiver that
// de-identifies; and that what a route names cannot be removed while it
// names it, nor the receiver changed to archive as received while a route
// names a profile.
func testRoutingNamesWhatExists(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	set := func(r tenant.Routing) error {
		_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", Routing: r},
			[]tenant.ReceiverField{tenant.FieldRouting}, at(10), e.audit(), nil)
		return err
	}
	routing := suiteRouting()
	if err := set(routing); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a route with a profile on a receiver that archives as received: %v, want a precondition failure", err)
	}
	e.deidReceiver("recv")
	if err := set(routing); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a route to a storage the tenant does not have: %v, want a precondition failure", err)
	}
	e.addStorage("other")
	routing.Routes[0].ForwardTo = []string{"nowhere"}
	if err := set(routing); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a route to a sender the tenant does not have: %v, want a precondition failure", err)
	}
	e.ok("SetReceiver routing", set(suiteRouting()))
	if _, err := e.p.RemoveStorage(e.ctx, "other", e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a storage a route names removed: %v, want a precondition failure", err)
	}
	if _, _, err := e.p.RemoveSender(e.ctx, "pacs", true, at(20), e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a sender a route names removed: %v, want a precondition failure", err)
	}
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv"}, []tenant.ReceiverField{tenant.FieldDeidentification},
		at(30), e.audit(), nil)
	if fault.KindOf(err) != fault.Precondition {
		e.Errorf("a receiver whose route names a profile changed to archive as received: %v, want a precondition failure", err)
	}
	e.ok("SetReceiver routing cleared", set(tenant.Routing{}))
	if _, err := e.p.RemoveStorage(e.ctx, "other", e.audit(), nil); err != nil {
		e.Errorf("a storage no route names any more removed: %v", err)
	}
}

// testForwardByRoute guards what a reception's routing chose: it is kept
// with the identify step and read back with the reception, and a route
// that names its senders replaces the receiver's when the reception
// reaches done, an empty list forwarding to none.
func testForwardByRoute(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("orthanc")
	e.addSender("pacs")
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", ForwardTo: []string{"orthanc"}},
		[]tenant.ReceiverField{tenant.FieldForwardTo}, T0, e.audit(), nil)
	e.ok("SetReceiver forward_to", err)
	chosen := tenant.ReceptionRoute{Route: "lab-a", Profile: tenant.ProfileRef{Name: "strict", Version: 2},
		ReplacesForwardTo: true, ForwardTo: []string{"pacs"}}
	a := e.archiveRouted("recv", 1, 1, 2, 0, at(0), "ses-1", chosen)
	if !reflect.DeepEqual(a.r.Route, chosen) {
		e.Errorf("the step answered the route %+v, want %+v", a.r.Route, chosen)
	}
	r := e.finish(a)
	stored, _, err := e.p.Reception(e.ctx, r.ID)
	e.ok("Reception", err)
	if !reflect.DeepEqual(stored.Route, chosen) {
		e.Errorf("the reception reads the route %+v, want %+v", stored.Route, chosen)
	}
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(r.ID, "pacs")); err != nil {
		e.Errorf("the route's sender has no forward: %v", err)
	}
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(r.ID, "orthanc")); fault.KindOf(err) != fault.NotFound {
		e.Errorf("the receiver's sender, which the route replaced, has a forward (%v)", err)
	}
	none := e.finish(e.archiveRouted("recv", 2, 1, 1, 100, at(600), "ses-2", tenant.ReceptionRoute{Route: "none", ReplacesForwardTo: true}))
	jobs, _, err := e.p.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 10})
	e.ok("ListJobs", err)
	for _, j := range jobs {
		if j.ReceptionID == none.ID {
			e.Errorf("a route that forwards to none made a forward to %s", j.Sender)
		}
	}
	kept := e.finish(e.archive("recv", 3, 1, 1, 200, at(1200), "ses-3"))
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(kept.ID, "orthanc")); err != nil {
		e.Errorf("a reception no route chose for is not forwarded to the receiver's sender: %v", err)
	}
}

// testReleaseNamesStorage guards a release that names a storage: one the
// tenant does not have is refused and releases nothing; one it has is kept
// with the reception the file joins and with the record, and stays with the
// reception through the identify step; a second release into that
// reception naming another storage is refused, one naming the same taken.
func testReleaseNamesStorage(e *senv) {
	e.addReceiver("a", "store", 11112)
	e.addStorage("other")
	r := e.openReception("a", 1, at(1))
	f := file(r, 1, 1)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(2))
	e.ok("AddReceivedFiles", err)
	q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineUnrouted,
		Details: map[string]string{"cause": "no_match"}, Receiver: "a", ReceptionID: r.ID,
		SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
		TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: at(3)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q})
	e.ok("HoldFiles", err)
	r = e.stepTo(r, reception.Complete, "store")
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Quarantine,
		At: at(5)}, nil)
	e.ok("StepReception quarantine", err)
	rel := tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, ReceptionID: e.id("rc"), Storage: "nowhere", Now: at(6)}
	if _, err := e.p.ReleaseFile(e.ctx, rel, e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a release naming a storage the tenant does not have: %v, want a precondition failure", err)
	}
	rel.Storage = "other"
	out, err := e.p.ReleaseFile(e.ctx, rel, e.audit(), nil)
	e.ok("ReleaseFile", err)
	joined, _, err := e.p.Reception(e.ctx, out.ReleaseReception)
	e.ok("Reception", err)
	if joined.Route.ReleasedStorage != "other" {
		e.Fatalf("the reception a release joined reads %+v, want the storage it named", joined.Route)
	}
	second := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineUnrouted,
		Details: map[string]string{"cause": "no_match"}, Receiver: "a", ReceptionID: r.ID,
		SOPInstanceUID: uidOf(3, 2), StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
		TransferSyntaxUID: f.TransferSyntaxUID, SHA256: sum(2), Size: f.Size, Incoming: f.Incoming + "2", ContentKept: true, Quarantined: at(3)}
	_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{second})
	e.ok("HoldFiles", err)
	other := tenant.Release{ID: second.ID, Mode: tenant.ReleaseAsIs, ReceptionID: e.id("rc"), Storage: "store", Now: at(7)}
	if _, err := e.p.ReleaseFile(e.ctx, other, e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a second release into the reception naming another storage: %v, want a precondition failure", err)
	}
	other.Storage = "other"
	if _, err := e.p.ReleaseFile(e.ctx, other, e.audit(), nil); err != nil {
		e.Errorf("a second release naming the same storage: %v", err)
	}
	if q, err := e.p.QuarantinedFile(e.ctx, second.ID); err != nil || q.ReleaseStorage != "other" {
		e.Errorf("the released record reads the storage %q (%v), want the one its release named", q.ReleaseStorage, err)
	}
	joined = e.stepTo(joined, reception.Complete, "other")
	identified, err := e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: joined.ID, From: reception.Complete, Event: reception.Identify,
		At: at(9), SubjectLabel: "sub-01", SessionLabel: "ses-1", Storage: "other"}, nil)
	e.ok("StepReception identify", err)
	if identified.Route.ReleasedStorage != "other" || identified.Storage != "other" {
		e.Errorf("after the identify step the reception reads %+v on %s", identified.Route, identified.Storage)
	}
}

// testSessionKeepsRoute guards what the routing chose for a session: the
// commit that makes the session keeps the route of the reception that made
// it, and CatalogLabels answers it, so a later arrival goes by it.
func testSessionKeepsRoute(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	chosen := tenant.ReceptionRoute{Route: "lab-a", Profile: tenant.ProfileRef{Name: "strict", Version: 2},
		ReplacesForwardTo: true, ForwardTo: []string{"pacs"}}
	a := e.archiveRouted("recv", 1, 1, 1, 0, at(0), "ses-1", chosen)
	c, err := e.p.CatalogLabels(e.ctx, a.r.StudyUID, nil)
	e.ok("CatalogLabels", err)
	if c.Session == nil || !reflect.DeepEqual(c.Session.Route, chosen) {
		e.Errorf("the session reads %+v, want the route of the reception that made it, %+v", c.Session, chosen)
	}
}

// testRetryKeepsRoute guards a retry of a failed reception: the new
// reception keeps what the failed one's routing chose and the storage a
// release into it named.
func testRetryKeepsRoute(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addStorage("other")
	r := e.openReception("recv", 1, at(1))
	f := file(r, 1, 1)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(2))
	e.ok("AddReceivedFiles", err)
	r = e.stepTo(r, reception.Complete, "store")
	chosen := tenant.ReceptionRoute{Route: "lab-a"}
	r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify,
		At: at(4), SubjectLabel: "sub-01", SessionLabel: "ses-1", Storage: "other", Route: chosen}, nil)
	e.ok("StepReception identify", err)
	r = e.failReception(r)
	_, retry, err := e.p.RetryReception(e.ctx, r.ID, e.id("rr"), "", at(7200), e.audit(), nil)
	e.ok("RetryReception", err)
	if !reflect.DeepEqual(retry.Route, chosen) {
		e.Errorf("the retry reads the route %+v, want the failed reception's %+v", retry.Route, chosen)
	}
}

// testForwardAfterReceiverRemoved guards a route's senders when the
// receiver was removed before its reception reached done: the reception is
// forwarded to the senders its route chose; one that kept the receiver's
// senders is forwarded to none.
func testForwardAfterReceiverRemoved(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	routed := e.archiveRouted("recv", 1, 1, 1, 0, at(0), "ses-1", tenant.ReceptionRoute{Route: "lab-a", ReplacesForwardTo: true, ForwardTo: []string{"pacs"}})
	plain := e.archive("recv", 2, 1, 1, 100, at(10), "ses-2")
	_, err := e.p.RemoveReceiver(e.ctx, "recv", e.audit(), nil)
	e.ok("RemoveReceiver", err)
	r := e.finish(routed)
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(r.ID, "pacs")); err != nil {
		e.Errorf("a reception whose route chose pacs, its receiver removed, has no forward: %v", err)
	}
	p := e.finish(plain)
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(p.ID, "pacs")); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a reception of a removed receiver with no route made a forward (%v)", err)
	}
}

// testReleasedStorageKept guards the storage a release named when the file
// joins no reception and lands again: the reception the landed file joins
// keeps it, and keeps the first one a second file names.
func testReleasedStorageKept(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addStorage("other")
	r := e.openReception("recv", 1, at(1))
	f := file(r, 1, 1)
	f.Released, f.ReleasedStorage = true, "other"
	g := file(r, 1, 2)
	g.Released, g.ReleasedStorage = true, "store"
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(2))
	e.ok("AddReceivedFiles", err)
	_, err = e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{g}, at(3))
	e.ok("AddReceivedFiles", err)
	got, _, err := e.p.Reception(e.ctx, r.ID)
	e.ok("Reception", err)
	if got.Route.ReleasedStorage != "other" {
		e.Errorf("the reception reads %+v, want the storage the first landed release named", got.Route)
	}
}
