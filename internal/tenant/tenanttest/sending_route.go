package tenanttest

import (
	"reflect"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// suiteRuleSet is a rule set of receiver recv with archive rules (one with
// a profile), its default, and forward rules (one filtered, one with a
// destination profile).
func suiteRuleSet() tenant.RuleSet {
	return tenant.RuleSet{Receiver: "recv", QuarantineUnmatched: true, Comment: "split by site",
		Archive: []tenant.ArchiveRule{
			{Name: "lab-a", When: `calling_ae == "LAB A" and Modality in ("CT", "MR")`, Storage: "other",
				Profile: tenant.ProfileRef{Name: "strict", Version: 2}},
			{Name: "rest", Storage: "store"},
		},
		Forward: []tenant.ForwardRule{
			{Name: "t1", When: `Modality = "MR"`, Series: `SeriesDescription contains "t1"`, Senders: []string{"pacs"}},
			{Name: "all", Senders: []string{"pacs"}, Profile: tenant.ProfileRef{Name: "strict", Version: 3}},
		}}
}

// deidReceiver sets the receiver to de-identify with a profile version,
// which an archive rule that names a profile needs; the store does not ask
// whether the version exists (the caller does).
func (e *senv) deidReceiver(name string) {
	e.Helper()
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: name, Profile: tenant.ProfileRef{Name: "basic", Version: 1}},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, T0, e.audit(), nil)
	e.ok("SetReceiver deidentification", err)
}

// sameRules reports whether two rule sets hold the same rules and version,
// whatever their times.
func sameRules(a, b tenant.RuleSet) bool {
	a.Created, b.Created, a.CreatedBy, b.CreatedBy = a.Created.UTC(), a.Created.UTC(), "", ""
	return reflect.DeepEqual(a, b)
}

// testRuleSets guards a receiver's rule set versions: each creation stores
// the next version, read back exactly, with when and by which key it was
// made; a receiver runs the version it is pointed at, never a newer one
// made without activating it; activating an older version runs it again,
// and 0 runs none; versions list newest first in pages; and a version never
// changes, whatever is made or activated after it.
func testRuleSets(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addStorage("other")
	e.addSender("pacs")
	e.deidReceiver("recv")
	audit := e.audit()
	audit.KeyID = "key-1"
	first, err := e.p.CreateRuleSet(e.ctx, suiteRuleSet(), true, at(10), audit, nil)
	e.ok("CreateRuleSet", err)
	want := suiteRuleSet()
	want.Version = 1
	if !sameRules(first, want) || !first.Created.Equal(at(10)) || first.CreatedBy != "key-1" {
		e.Errorf("the version made reads %+v, want %+v made at %v by key-1", first, want, at(10))
	}
	stored, err := e.p.RuleSet(e.ctx, "recv", 1)
	e.ok("RuleSet", err)
	if !sameRules(stored, want) || !stored.Created.Equal(at(10)) || stored.CreatedBy != "key-1" {
		e.Errorf("version 1 reads %+v, want %+v", stored, want)
	}
	rcv, err := e.p.Receiver(e.ctx, "recv")
	e.ok("Receiver", err)
	if !sameRules(rcv.Routing, want) {
		e.Errorf("the receiver runs %+v, want version 1", rcv.Routing)
	}
	second := suiteRuleSet()
	second.Archive = second.Archive[1:]
	second.Comment = "one storage"
	made, err := e.p.CreateRuleSet(e.ctx, second, false, at(20), e.audit(), nil)
	e.ok("CreateRuleSet", err)
	if made.Version != 2 {
		e.Errorf("the second version made is %d, want 2", made.Version)
	}
	if active, err := e.p.RuleSet(e.ctx, "recv", 0); err != nil || active.Version != 1 {
		e.Errorf("a version made without activating it: the receiver runs %d (%v), want 1", active.Version, err)
	}
	page, next, err := e.p.ListRuleSets(e.ctx, "recv", master.Page{Limit: 1})
	e.ok("ListRuleSets", err)
	if len(page) != 1 || page[0].Version != 2 || next == "" {
		e.Fatalf("the first page lists %d versions (next %q), want version 2 and a next page", len(page), next)
	}
	page, next, err = e.p.ListRuleSets(e.ctx, "recv", master.Page{Limit: 1, Cursor: next})
	e.ok("ListRuleSets", err)
	if len(page) != 1 || page[0].Version != 1 || next != "" {
		e.Errorf("the second page lists %+v (next %q), want version 1 alone", page, next)
	}
	if rs, err := e.p.ActivateRuleSet(e.ctx, "recv", 2, at(30), e.audit(), nil); err != nil || rs.Version != 2 {
		e.Errorf("ActivateRuleSet 2: version %d (%v)", rs.Version, err)
	}
	if rs, err := e.p.ActivateRuleSet(e.ctx, "recv", 1, at(40), e.audit(), nil); err != nil || rs.Version != 1 {
		e.Errorf("ActivateRuleSet 1, back to the first: version %d (%v)", rs.Version, err)
	}
	if again, err := e.p.RuleSet(e.ctx, "recv", 1); err != nil || !sameRules(again, want) || !again.Created.Equal(at(10)) {
		e.Errorf("version 1 after later versions and activations reads %+v (%v), want it unchanged", again, err)
	}
	if rs, err := e.p.ActivateRuleSet(e.ctx, "recv", 0, at(50), e.audit(), nil); err != nil || rs.Version != 0 || !rs.IsZero() {
		e.Errorf("ActivateRuleSet 0: %+v (%v), want none", rs, err)
	}
	if _, err := e.p.RuleSet(e.ctx, "recv", 0); fault.KindOf(err) != fault.NotFound {
		e.Errorf("the rule set of a receiver that runs none: %v, want NotFound", err)
	}
	if _, err := e.p.RuleSet(e.ctx, "recv", 9); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a version the receiver does not have: %v, want NotFound", err)
	}
	if _, err := e.p.ActivateRuleSet(e.ctx, "recv", 9, at(60), e.audit(), nil); fault.KindOf(err) != fault.NotFound {
		e.Errorf("activating a version the receiver does not have: %v, want NotFound", err)
	}
	other := suiteRuleSet()
	other.Receiver = "nobody"
	if _, err := e.p.CreateRuleSet(e.ctx, other, false, at(70), e.audit(), nil); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a rule set of a receiver the tenant does not have: %v, want NotFound", err)
	}
	bad := suiteRuleSet()
	bad.Archive[0].When = `PatientName = "DOE"`
	if _, err := e.p.CreateRuleSet(e.ctx, bad, false, at(80), e.audit(), nil); fault.KindOf(err) != fault.Invalid {
		e.Errorf("a rule set whose condition reads the patient's name: %v, want Invalid", err)
	}
}

// testRuleSetNamesWhatExists guards what a rule set a receiver runs may
// name: a storage and senders the tenant has, a profile only on a receiver
// that de-identifies; that a refused activation stores no version; and
// that what a rule names cannot be removed while the receiver runs it, nor
// the receiver changed to archive as received while an archive rule names
// a profile.
func testRuleSetNamesWhatExists(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	create := func(rs tenant.RuleSet) error {
		_, err := e.p.CreateRuleSet(e.ctx, rs, true, at(10), e.audit(), nil)
		return err
	}
	rules := suiteRuleSet()
	if err := create(rules); fault.KindOf(err) != fault.Precondition {
		e.Errorf("an archive rule with a profile on a receiver that archives as received: %v, want a precondition failure", err)
	}
	e.deidReceiver("recv")
	if err := create(rules); fault.KindOf(err) != fault.Precondition {
		e.Errorf("an archive rule to a storage the tenant does not have: %v, want a precondition failure", err)
	}
	e.addStorage("other")
	rules.Forward[0].Senders = []string{"nowhere"}
	if err := create(rules); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a forward rule to a sender the tenant does not have: %v, want a precondition failure", err)
	}
	if list, _, err := e.p.ListRuleSets(e.ctx, "recv", master.Page{Limit: 10}); err != nil || len(list) != 0 {
		e.Errorf("after refused activations the receiver has %d versions (%v), want none: a refused activation stores nothing", len(list), err)
	}
	e.ok("CreateRuleSet", create(suiteRuleSet()))
	if _, err := e.p.RemoveStorage(e.ctx, "other", e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a storage an archive rule names removed: %v, want a precondition failure", err)
	}
	if _, _, err := e.p.RemoveSender(e.ctx, "pacs", true, at(20), e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		e.Errorf("a sender a forward rule names removed: %v, want a precondition failure", err)
	}
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv"}, []tenant.ReceiverField{tenant.FieldDeidentification},
		at(30), e.audit(), nil)
	if fault.KindOf(err) != fault.Precondition {
		e.Errorf("a receiver whose archive rule names a profile changed to archive as received: %v, want a precondition failure", err)
	}
	if _, err := e.p.ActivateRuleSet(e.ctx, "recv", 0, at(40), e.audit(), nil); err != nil {
		e.Errorf("ActivateRuleSet 0: %v", err)
	}
	if _, err := e.p.RemoveStorage(e.ctx, "other", e.audit(), nil); err != nil {
		e.Errorf("a storage no rule the receiver runs names any more removed: %v", err)
	}
	if _, err := e.p.ActivateRuleSet(e.ctx, "recv", 1, at(50), e.audit(), nil); fault.KindOf(err) != fault.Precondition {
		e.Errorf("activating a version whose storage was removed: %v, want a precondition failure", err)
	}
}

// testForwardByRule guards the forwards a reception's rules chose: they
// are kept with the identify step and read back with the reception; when it
// reaches done each forward rule makes a job per sender beside the
// receiver's own senders, one per series the reception brought for a rule
// with no series filter, one per series its filter chose for a rule with one
// (each naming that series' scan, never a series it did not choose or an
// earlier arrival's), each with the
// rule's destination profile; two rules to one sender make a job each.
func testForwardByRule(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("orthanc")
	e.addSender("pacs")
	_, err := e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "recv", ForwardTo: []string{"orthanc"}},
		[]tenant.ReceiverField{tenant.FieldForwardTo}, T0, e.audit(), nil)
	e.ok("SetReceiver forward_to", err)
	chosen := uidOf(2, 1*100+1)
	dest := tenant.ProfileRef{Name: "strict", Version: 3}
	route := tenant.ReceptionRoute{RuleSet: 1, Route: "lab-a", ForwardRuleSet: 1, CallingAE: "SCANNER1", CalledAE: "ARCHIVE",
		Forwards: []tenant.ForwardChoice{
			{Rule: "t1", Senders: []string{"pacs"}, Filtered: true, Series: []string{chosen}},
			{Rule: "all", Senders: []string{"pacs"}, Profile: dest},
		}}
	a := e.archiveRouted("recv", 1, 2, 2, 0, at(0), "ses-1", route)
	if !reflect.DeepEqual(a.r.Route, route) {
		e.Errorf("the step answered the route %+v, want %+v", a.r.Route, route)
	}
	r := e.finish(a)
	stored, _, err := e.p.Reception(e.ctx, r.ID)
	e.ok("Reception", err)
	if !reflect.DeepEqual(stored.Route, route) {
		e.Errorf("the reception reads the route %+v, want %+v", stored.Route, route)
	}
	if _, err := e.p.Job(e.ctx, tenant.ForwardJobID(r.ID, "orthanc")); err != nil {
		e.Errorf("the receiver's own sender has no forward: %v", err)
	}
	for _, uid := range []string{uidOf(2, 100), chosen} {
		all, err := e.p.Job(e.ctx, tenant.RuleForwardJobID(r.ID, "all", "pacs", uid))
		e.ok("Job all", err)
		if all.Target.ScanLabel != "scan-"+uid || all.Target.SessionLabel != "ses-1" || all.Profile != dest {
			e.Errorf("the forward of a rule with no filter targets %+v with profile %v, want the scan of series %s with %v", all.Target, all.Profile, uid, dest)
		}
	}
	t1, err := e.p.Job(e.ctx, tenant.RuleForwardJobID(r.ID, "t1", "pacs", chosen))
	e.ok("Job t1", err)
	if t1.Target.ScanLabel != "scan-"+chosen || !t1.Profile.IsZero() {
		e.Errorf("the forward of the filtered rule targets %+v with profile %v, want the scan of the series it chose", t1.Target, t1.Profile)
	}
	if _, err := e.p.Job(e.ctx, tenant.RuleForwardJobID(r.ID, "t1", "pacs", uidOf(2, 100))); fault.KindOf(err) != fault.NotFound {
		e.Errorf("a series the filter did not choose has a forward (%v)", err)
	}
	jobs, _, err := e.p.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 20})
	e.ok("ListJobs", err)
	n := 0
	for _, j := range jobs {
		if j.ReceptionID == r.ID {
			n++
		}
	}
	if n != 4 {
		e.Errorf("the reception made %d forwards, want 4 (the receiver's sender, the rule with no filter for each of two series, the one series chosen)", n)
	}
}

// testRouteMatches guards the records of what a reception was routed by:
// its identify step writes its archive rule (the default when none held)
// and each forward, by version; hit counts group them by rule, of one
// version or every version, counting receptions and the series forwards
// sent; and a session's receptions' records come with it in RecentSessions,
// beside the AE titles its first file came with.
func testRouteMatches(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	first := tenant.ReceptionRoute{RuleSet: 3, Route: "lab-a", ForwardRuleSet: 3, CallingAE: "SCANNER1", CalledAE: "ARCHIVE",
		Forwards: []tenant.ForwardChoice{{Rule: "t1", Senders: []string{"pacs"}, Filtered: true, Series: []string{uidOf(2, 100)}}}}
	e.finish(e.archiveRouted("recv", 1, 1, 1, 0, at(0), "ses-1", first))
	e.finish(e.archiveRouted("recv", 2, 1, 1, 100, at(600), "ses-2", tenant.ReceptionRoute{RuleSet: 3, CallingAE: "OTHER"}))
	e.finish(e.archiveRouted("recv", 3, 1, 1, 200, at(1200), "ses-3", tenant.ReceptionRoute{RuleSet: 4, Route: "lab-a"}))
	hits, err := e.p.RouteHits(e.ctx, "recv", 3)
	e.ok("RouteHits", err)
	want := []tenant.RouteHit{
		{Version: 3, Phase: tenant.PhaseArchive, Rule: "", Receptions: 1},
		{Version: 3, Phase: tenant.PhaseArchive, Rule: "lab-a", Receptions: 1},
		{Version: 3, Phase: tenant.PhaseForward, Rule: "t1", Receptions: 1, Series: 1},
	}
	if len(hits) != len(want) {
		e.Fatalf("version 3's hits: %+v, want %+v", hits, want)
	}
	for i := range want {
		h := hits[i]
		h.Last = want[i].Last
		if h != want[i] || hits[i].Last.IsZero() {
			e.Errorf("hit %d: %+v, want %+v with its last time", i, hits[i], want[i])
		}
	}
	every, err := e.p.RouteHits(e.ctx, "recv", 0)
	e.ok("RouteHits every version", err)
	if len(every) != 4 || every[3].Version != 4 || every[3].Rule != "lab-a" {
		e.Errorf("every version's hits: %+v, want version 3's and version 4's lab-a", every)
	}
	none, err := e.p.RouteHits(e.ctx, "recv", 9)
	e.ok("RouteHits", err)
	if len(none) != 0 {
		e.Errorf("a version that routed nothing has hits %+v", none)
	}
	sessions, err := e.p.RecentSessions(e.ctx, "recv", 2)
	e.ok("RecentSessions", err)
	if len(sessions) != 2 || sessions[0].SessionLabel != "ses-3" || sessions[1].SessionLabel != "ses-2" {
		e.Fatalf("the latest two sessions: %+v, want ses-3 then ses-2", sessions)
	}
	all, err := e.p.RecentSessions(e.ctx, "recv", 10)
	e.ok("RecentSessions", err)
	s1 := all[len(all)-1]
	if s1.SessionLabel != "ses-1" || s1.Route.Route != "lab-a" || s1.Route.RuleSet != 3 || s1.Route.CallingAE != "SCANNER1" ||
		s1.Route.CalledAE != "ARCHIVE" || len(s1.Route.Forwards) != 0 {
		e.Errorf("the first session reads %+v, want lab-a of version 3 with its AE titles and no forwards", s1)
	}
	if len(s1.Matches) != 2 || s1.Matches[0].Phase != tenant.PhaseArchive || s1.Matches[1].Rule != "t1" || s1.Matches[1].Series != 1 {
		e.Errorf("the first session's records: %+v, want lab-a and t1", s1.Matches)
	}
	if _, err := e.p.RecentSessions(e.ctx, "recv", 0); fault.KindOf(err) != fault.Invalid {
		e.Errorf("a dry run of no sessions: %v, want Invalid", err)
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
// commit that makes the session keeps the archive choice and AE titles of
// the reception that made it, never that reception's forwards, and
// CatalogLabels answers it, so a later arrival goes by it.
func testSessionKeepsRoute(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	chosen := tenant.ReceptionRoute{RuleSet: 2, Route: "lab-a", Profile: tenant.ProfileRef{Name: "strict", Version: 2},
		ForwardRuleSet: 2, Forwards: []tenant.ForwardChoice{{Rule: "all", Senders: []string{"pacs"}}}, CallingAE: "SCANNER1", CalledAE: "ARCHIVE"}
	a := e.archiveRouted("recv", 1, 1, 1, 0, at(0), "ses-1", chosen)
	c, err := e.p.CatalogLabels(e.ctx, a.r.StudyUID, nil)
	e.ok("CatalogLabels", err)
	if c.Session == nil || !reflect.DeepEqual(c.Session.Route, chosen.Archived()) {
		e.Errorf("the session reads %+v, want the archive choice of the reception that made it, %+v", c.Session, chosen.Archived())
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
	chosen := tenant.ReceptionRoute{RuleSet: 1, Route: "lab-a"}
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

// testForwardAfterReceiverRemoved guards a reception's forwards when the
// receiver was removed before it reached done: it is forwarded as its
// forward rules chose; one no forward rule chose is forwarded to none.
func testForwardAfterReceiverRemoved(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	routed := e.archiveRouted("recv", 1, 1, 1, 0, at(0), "ses-1", tenant.ReceptionRoute{ForwardRuleSet: 1,
		Forwards: []tenant.ForwardChoice{{Rule: "all", Senders: []string{"pacs"}}}})
	plain := e.archive("recv", 2, 1, 1, 100, at(10), "ses-2")
	_, err := e.p.RemoveReceiver(e.ctx, "recv", e.audit(), nil)
	e.ok("RemoveReceiver", err)
	r := e.finish(routed)
	if _, err := e.p.Job(e.ctx, tenant.RuleForwardJobID(r.ID, "all", "pacs", uidOf(2, 100))); err != nil {
		e.Errorf("a reception whose forward rule chose pacs, its receiver removed, has no forward: %v", err)
	}
	p := e.finish(plain)
	jobs, _, err := e.p.ListJobs(e.ctx, tenant.JobFilter{}, master.Page{Limit: 10})
	e.ok("ListJobs", err)
	for _, j := range jobs {
		if j.ReceptionID == p.ID {
			e.Errorf("a reception of a removed receiver with no forward rule made a forward to %s", j.Sender)
		}
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
