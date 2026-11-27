package tenanttest

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runSending runs the contract suite's groups of senders and jobs
// (tenant.Sending) against the stores open makes, each test on its own
// store, in parallel. Run runs it.
func runSending(t *testing.T, open OpenEvents) {
	tests := []struct {
		name string
		fn   func(*senv)
	}{
		{"Senders", testSenders},
		{"SenderSetMask", testSenderSetMask},
		{"SenderRemove", testSenderRemove},
		{"SenderChangesGuarded", testSenderChangesGuarded},
		{"CreateJob", testCreateJob},
		{"JobList", testJobList},
		{"JobRun", testJobRun},
		{"JobSnapshot", testJobSnapshot},
		{"JobFencing", testJobFencing},
		{"JobFailuresBounded", testJobFailuresBounded},
		{"JobCancel", testJobCancel},
		{"JobRetry", testJobRetry},
		{"JobRetryUnlisted", testJobRetryUnlisted},
		{"JobGivesUp", testJobGivesUp},
		{"JobProgressEvents", testJobProgressEvents},
		{"JobLateCommit", testJobLateCommit},
		{"JobRetryLateCommit", testJobRetryLateCommit},
		{"JobListingGivesUp", testJobListingGivesUp},
		{"JobReleased", testJobReleased},
		{"JobReleasedAfterItEnded", testJobReleasedAfterItEnded},
		{"JobOwed", testJobOwed},
		{"Forward", testForward},
		{"ForwardNothingArchived", testForwardNothingArchived},
		{"SenderRemovedWhileForwarding", testSenderRemovedWhileForwarding},
		{"Routing", testRouting},
		{"RoutingNamesWhatExists", testRoutingNamesWhatExists},
		{"ForwardByRoute", testForwardByRoute},
		{"ReleaseNamesStorage", testReleaseNamesStorage},
		{"SessionKeepsRoute", testSessionKeepsRoute},
		{"RetryKeepsRoute", testRetryKeepsRoute},
		{"ForwardAfterReceiverRemoved", testForwardAfterReceiverRemoved},
		{"ReleasedStorageKept", testReleasedStorageKept},
		{"JobUnitsAtOnce", testJobUnitsAtOnce},
		{"CancelRacesUnit", testCancelRacesUnit},
		{"CrashResumes", testCrashResumes},
		{"JobsInStatus", testJobsInStatus},
		{"DeidJobs", testDeidJobs},
		{"DeidJobTallies", testDeidJobTallies},
		{"SeenUIDs", testSeenUIDs},
		{"SenderMark", testSenderMark},
		{"InstancePixelRisk", testInstancePixelRisk},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			tt.fn(&senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s}})
		})
	}
}

// runSendingShared runs the tests of jobs that need two instances of the
// store on one database, as two processes would work one job.
func runSendingShared(t *testing.T, open OpenEventsPair) {
	t.Run("UnitsAcrossInstances", func(t *testing.T) {
		t.Parallel()
		first, second := open(t, SuiteTenant)
		e := &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: first, ctx: context.Background()}, p: first}, ev: first}}
		testUnitsShared(e, second)
	})
}

// senv is a sending test's store and its helpers; it has eenv's too.
type senv struct {
	*eenv
	// profile is the profile version archive records for each instance
	// it commits; zero archives as received.
	profile tenant.ProfileRef
	// pixelRisk says archive records each instance with profile as an image
	// whose accepted pixel risk a sender refuses unless marked.
	pixelRisk bool
}

// sealed is the stand-in for a credential sealed by the caller: the store
// keeps such bytes as they are, and the suite only checks they come back.
var sealed = []byte("sealed-credential-bytes")

// dimseSender returns a DIMSE sender with the defaults left to the store.
func dimseSender(name string) tenant.Sender {
	return tenant.Sender{Name: name, Type: tenant.SenderDIMSE, Enabled: true, DIMSE: tenant.DIMSESender{
		Host: "PACS.Lab.example", Port: 4242, CalledAETitle: " ORTHANC ", CallingAETitle: "NOPACS"}}
}

// stowSender returns a stow sender with bearer authentication and a sealed
// credential.
func stowSender(name string) tenant.Sender {
	return tenant.Sender{Name: name, Type: tenant.SenderSTOW, STOW: tenant.STOWSender{
		URL: "https://pacs2.lab.example/dicom-web", Authentication: tenant.StowAuthBearer, SealedCredential: sealed}}
}

// addSender adds a DIMSE sender, failing the test on an error.
func (e *senv) addSender(name string) tenant.Sender {
	e.Helper()
	s, err := e.p.AddSender(e.ctx, dimseSender(name), T0, e.audit(), nil)
	e.ok("AddSender", err)
	return s
}

// archived is what archive put in the catalog: the reception and its files
// by scan, in order of SOP Instance UID.
type archived struct {
	r     tenant.Reception
	files []tenant.ReceivedFile
}

// archive receives scans scans of per instances each of study study at the
// receiver, identifies the reception with the labels given (or the suite's
// when session is empty), and archives them, held at the reception's step
// time; it leaves the reception archived, not yet done. Instance n of scan
// s is uidOf(3, base+1000*s+n), so base keeps two calls apart.
func (e *senv) archive(receiver string, study, scans, per, base int, when time.Time, session string) archived {
	e.Helper()
	return e.archiveRouted(receiver, study, scans, per, base, when, session, tenant.ReceptionRoute{})
}

// archiveRouted is archive with what the routing chose kept with the
// identify step.
func (e *senv) archiveRouted(receiver string, study, scans, per, base int, when time.Time, session string, route tenant.ReceptionRoute) archived {
	e.Helper()
	r, _, err := e.p.OpenReception(e.ctx, receiver, uidOf(1, study), e.id("rc"), when)
	e.ok("OpenReception", err)
	var files []tenant.ReceivedFile
	for s := range scans {
		for i := range per {
			f := file(r, study, base+1000*s+i)
			f.SeriesUID = uidOf(2, study*100+s)
			files = append(files, f)
		}
	}
	_, err = e.p.AddReceivedFiles(e.ctx, files, when)
	e.ok("AddReceivedFiles", err)
	if session == "" {
		session = "ses-" + r.ID
	}
	step := func(from reception.State, ev reception.Event) {
		e.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Minute)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage, st.Route = "sub-01", session, "store", route
		}
		r, err = e.p.StepReception(e.ctx, st, nil)
		e.ok("StepReception "+string(ev), err)
	}
	step(reception.Receiving, reception.Quiet)
	step(reception.Complete, reception.Identify)
	claims := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	}
	_, err = e.p.ClaimInstances(e.ctx, r.ID, claims, r.StepTime, nil)
	e.ok("ClaimInstances", err)
	for s := range scans {
		series := uidOf(2, study*100+s)
		p := label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: "scan-" + series}
		c := tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: series, SubjectLabel: p.Subject,
			SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "store", ManifestSHA256: sum(s), At: r.StepTime}
		for _, f := range files[s*per : (s+1)*per] {
			key, err := layout.InstanceKey(p, f.SOPInstanceUID)
			e.ok("InstanceKey", err)
			c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key,
				Length: f.Size, Size: f.Size, ArchivedSHA256: f.SHA256, Profile: e.profile, PixelRiskAccepted: e.pixelRisk})
		}
		_, err := e.p.CommitScan(e.ctx, c)
		e.ok("CommitScan", err)
	}
	step(reception.Identified, reception.Archive)
	return archived{r: r, files: files}
}

// finish takes an archived reception to done.
func (e *senv) finish(a archived) tenant.Reception {
	e.Helper()
	r := a.r
	for _, s := range []struct {
		from reception.State
		ev   reception.Event
	}{{reception.Archived, reception.Verify}, {reception.Verified, reception.Finish}} {
		var err error
		r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: s.from, Event: s.ev, At: r.StepTime.Add(time.Minute)}, nil)
		e.ok("StepReception "+string(s.ev), err)
	}
	return r
}

// setup adds a storage, a receiver and a sender, and archives a session of
// scans × per instances, returning what it archived and its session's
// labels as a target.
func (e *senv) setup(sender string, scans, per int) (archived, tenant.JobTarget) {
	e.Helper()
	e.addReceiver("recv", "store", 11112)
	e.addSender(sender)
	a := e.archive("recv", 1, scans, per, 0, at(0), "ses-1")
	return a, tenant.JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}
}

// createJob makes a send job at the time given, failing the test on an
// error.
func (e *senv) createJob(id, sender string, t tenant.JobTarget, when time.Time) tenant.Job {
	e.Helper()
	j, err := e.p.CreateJob(e.ctx, tenant.NewJob{ID: id, Sender: sender, Target: t, Created: when}, e.audit(), nil)
	e.ok("CreateJob", err)
	return j
}

// claim claims the next unit of the sender's jobs, failing the test when
// none is due.
func (e *senv) claim(sender, owner string, now time.Time) tenant.Work {
	e.Helper()
	w, err := e.p.ClaimWork(e.ctx, tenant.SendKind(sender), owner, now, time.Minute)
	e.ok("ClaimWork "+sender, err)
	return w
}

// list claims a job's listing unit and lists every scan, returning the job
// as the last page left it.
func (e *senv) list(sender string, now time.Time) tenant.Job {
	e.Helper()
	w := e.claim(sender, "lister", now)
	u, err := e.p.StartJobUnit(e.ctx, w.Fence(), now)
	e.ok("StartJobUnit", err)
	if !u.Listing {
		e.Fatalf("the first unit of a job is %+v, want its listing", u)
	}
	for range 1000 {
		j, more, err := e.p.ListJobScans(e.ctx, w.Fence(), now)
		e.ok("ListJobScans", err)
		if !more {
			return j
		}
	}
	e.Fatal("a listing did not end")
	return tenant.Job{}
}

// instances reads a scan unit's whole stream after the cursor given.
func (e *senv) instances(w tenant.Work, after string) []tenant.ScanInstance {
	e.Helper()
	var out []tenant.ScanInstance
	for range 1000 {
		page, err := e.p.JobScanInstances(e.ctx, w.Fence(), after, 7)
		e.ok("JobScanInstances", err)
		out = append(out, page...)
		if len(page) < 7 {
			return out
		}
		after = page[len(page)-1].SOPInstanceUID
	}
	e.Fatal("a stream did not end")
	return nil
}

// outcome says what a fake peer answers for instance i of a stream: sent,
// a warning, or a refusal.
type outcome func(i int, in tenant.ScanInstance) tenant.JobFailureCode

// allSent is a peer that takes every instance.
func allSent(int, tenant.ScanInstance) tenant.JobFailureCode { return "" }

// progress returns the progress of the instances given, after from, as
// the peer answered them.
func progress(w tenant.Work, from string, ins []tenant.ScanInstance, start int, peer outcome, now time.Time) tenant.JobProgress {
	p := tenant.JobProgress{Fence: w.Fence(), From: from, To: from, FailuresKept: tenant.DefaultFailuresKept, Now: now}
	for i, in := range ins {
		p.To = in.SOPInstanceUID
		switch code := peer(start+i, in); code {
		case "":
			p.Sent, p.SentBytes = p.Sent+1, p.SentBytes+in.Size
		default:
			if code.Refused() {
				p.Refused, p.RefusedBytes = p.Refused+1, p.RefusedBytes+in.Size
			} else {
				p.Warned, p.WarnedBytes = p.Warned+1, p.WarnedBytes+in.Size
			}
			p.Failures = append(p.Failures, tenant.JobFailure{SOPInstanceUID: in.SOPInstanceUID, Code: code, PeerStatus: 0xC000,
				SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Detail: "refused by the peer", Time: now})
		}
	}
	return p
}

// sendScan works a scan unit to its end: it reads its stream, records it in
// batches of batch, and ends it; it returns the job as the end left it.
func (e *senv) sendScan(w tenant.Work, batch int, peer outcome, now time.Time) tenant.Job {
	e.Helper()
	u, err := e.p.StartJobUnit(e.ctx, w.Fence(), now)
	e.ok("StartJobUnit", err)
	ins := e.instances(w, u.Scan.Cursor)
	cursor := u.Scan.Cursor
	for i := 0; i < len(ins); i += batch {
		chunk := ins[i:min(i+batch, len(ins))]
		p := progress(w, cursor, chunk, i, peer, now)
		_, err := e.p.RecordJobProgress(e.ctx, p)
		e.ok("RecordJobProgress", err)
		cursor = p.To
	}
	j, err := e.p.EndJobScan(e.ctx, tenant.JobProgress{Fence: w.Fence(), From: cursor, To: cursor,
		FailuresKept: tenant.DefaultFailuresKept, Now: now})
	e.ok("EndJobScan", err)
	return j
}

// runJob lists the job of the sender and works every scan unit, returning
// the job as the last unit left it.
func (e *senv) runJob(sender string, peer outcome, now time.Time) tenant.Job {
	e.Helper()
	j := e.list(sender, now)
	for {
		w, err := e.p.ClaimWork(e.ctx, tenant.SendKind(sender), "sender", now, time.Minute)
		if fault.KindOf(err) == fault.NotFound {
			return j
		}
		e.ok("ClaimWork", err)
		j = e.sendScan(w, 3, peer, now)
	}
}

// wantCounts fails unless the job's counts are those given and each
// instance is counted once.
func (e *senv) wantCounts(what string, j tenant.Job, instances, sent, warned, refused, scans, done int64, listed bool) {
	e.Helper()
	c := j.Counts
	if c.Instances != instances || c.InstancesSent != sent || c.InstancesWarned != warned || c.InstancesRefused != refused ||
		c.Scans != scans || c.ScansDone != done || c.Listed != listed || c.InstancesRemaining() < 0 || c.BytesRemaining() < 0 {
		e.Errorf("%s: counts %+v, want %d instances, %d sent, %d warned, %d refused, %d of %d scans done, listed %v",
			what, c, instances, sent, warned, refused, done, scans, listed)
	}
}

// jobEvents returns the job events of the feed after the cursor.
func (e *senv) jobEvents(after string) []tenant.Event {
	e.Helper()
	got, _ := e.readAll(after, 100, tenant.EventJobStep, tenant.EventJobProgress)
	return got
}

// steps returns the states the job_step events of job reached, in order.
func steps(events []tenant.Event, job string) []tenant.JobState {
	var out []tenant.JobState
	for _, ev := range events {
		if p, ok := ev.Payload.(*tenant.JobStepReached); ok && p.Job == job {
			out = append(out, p.State)
		}
	}
	return out
}

// testSenders guards adding and reading senders: the defaults the contract
// fills are stored; AE titles lose their spaces and a host its case; a
// stow sender's credential is kept sealed, never answered, and handed back
// as given by SenderCredential alone; a name taken is a Conflict; a list
// pages by name.
func testSenders(e *senv) {
	d, err := e.p.AddSender(e.ctx, dimseSender("orthanc"), T0, e.audit(), nil)
	e.ok("AddSender dimse", err)
	if d.MaxAssociations != tenant.DefaultSenderAssociations || d.ConnectTimeout != tenant.DefaultConnectTimeout ||
		d.ResponseTimeout != tenant.DefaultResponseTimeout || d.DIMSE.CalledAETitle != "ORTHANC" || d.DIMSE.Host != "pacs.lab.example" ||
		!d.Created.Equal(T0) || !d.Updated.Equal(T0) || !d.Enabled {
		e.Errorf("a DIMSE sender reads %+v", d)
	}
	s, err := e.p.AddSender(e.ctx, stowSender("pacs2"), T0, e.audit(), nil)
	e.ok("AddSender stow", err)
	if !s.STOW.CredentialSet || len(s.STOW.SealedCredential) != 0 || s.STOW.MaxInstancesPerRequest != tenant.DefaultStowInstances ||
		s.STOW.MaxRequestBytes != tenant.DefaultStowRequest || s.Enabled {
		e.Errorf("a stow sender reads %+v: its credential is held, never answered, and its defaults filled", s)
	}
	got, err := e.p.Sender(e.ctx, "pacs2")
	e.ok("Sender", err)
	if len(got.STOW.SealedCredential) != 0 || !got.STOW.CredentialSet || got.STOW.URL != s.STOW.URL {
		e.Errorf("Sender answers %+v", got)
	}
	c, err := e.p.SenderCredential(e.ctx, "pacs2")
	if err != nil || !bytes.Equal(c, sealed) {
		e.Errorf("SenderCredential = %q, %v; want the sealed bytes as given", c, err)
	}
	_, err = e.p.SenderCredential(e.ctx, "orthanc")
	e.wantKind("SenderCredential of a sender holding none", err, fault.NotFound)
	_, err = e.p.AddSender(e.ctx, dimseSender("orthanc"), T0, e.audit(), nil)
	e.wantKind("AddSender of a name taken", err, fault.Conflict)
	bad := stowSender("nocred")
	bad.STOW.SealedCredential = nil
	_, err = e.p.AddSender(e.ctx, bad, T0, e.audit(), nil)
	e.wantKind("AddSender of bearer authentication without a credential", err, fault.Invalid)
	long := dimseSender("s" + string(bytes.Repeat([]byte("x"), tenant.MaxSenderNameLength)))
	_, err = e.p.AddSender(e.ctx, long, T0, e.audit(), nil)
	e.wantKind("AddSender of a name longer than 57", err, fault.Invalid)
	for i := range 5 {
		e.addSender(fmt.Sprintf("more-%d", i))
	}
	all := allPages(e.penv, 2, func(p master.Page) ([]tenant.Sender, string, error) { return e.p.ListSenders(e.ctx, p) })
	var names []string
	for _, x := range all {
		names = append(names, x.Name)
		if len(x.STOW.SealedCredential) != 0 {
			e.Errorf("a list answers a credential of %s", x.Name)
		}
	}
	if fmt.Sprint(names) != "[more-0 more-1 more-2 more-3 more-4 orthanc pacs2]" {
		e.Errorf("ListSenders pages %v, want every sender once in order of name", names)
	}
	_, err = e.p.Sender(e.ctx, "missing")
	e.wantKind("Sender not there", err, fault.NotFound)
	destinations := map[string]bool{}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if a.Details[tenant.DetailSenderType] != "" {
			destinations[a.Details[tenant.DetailSenderType]+" "+a.Details[tenant.DetailSenderDestination]] = true
		}
		for k, v := range a.Details {
			if bytes.Contains([]byte(v), sealed) || bytes.Contains([]byte(k), sealed) {
				e.Errorf("an audit entry carries the credential in %s", k)
			}
		}
	}
	if len(destinations) != 2 || !destinations["dimse pacs.lab.example:4242"] || !destinations["stow pacs2.lab.example"] {
		e.Errorf("the additions' audit details by type and destination: %v", destinations)
	}
}

// testSenderSetMask guards a partial change of a sender by the mask's
// rules: only the fields named change; a credential not named is kept, one
// named replaces it, and none removes it with the authentication; a stow
// URL or authentication is never changed without the credential; a field
// of the other type, an empty mask and a bound named with no value are
// Invalid.
func testSenderSetMask(e *senv) {
	_, err := e.p.AddSender(e.ctx, stowSender("pacs2"), T0, e.audit(), nil)
	e.ok("AddSender", err)
	set := func(s tenant.Sender, fields ...tenant.SenderField) (tenant.Sender, error) {
		s.Name = "pacs2"
		return e.p.SetSender(e.ctx, s, fields, at(10), e.audit(), nil)
	}
	got, err := set(tenant.Sender{MaxAssociations: 5}, tenant.FieldSenderAssociations)
	e.ok("SetSender max_associations", err)
	if got.MaxAssociations != 5 || !got.STOW.CredentialSet || !got.Updated.Equal(at(10)) || got.STOW.URL == "" {
		e.Errorf("a change of max_associations reads %+v", got)
	}
	if c, err := e.p.SenderCredential(e.ctx, "pacs2"); err != nil || !bytes.Equal(c, sealed) {
		e.Errorf("a change that does not name the credential changed it: %q, %v", c, err)
	}
	_, err = set(tenant.Sender{STOW: tenant.STOWSender{URL: "https://elsewhere.example/dicom-web"}}, tenant.FieldSTOWURL)
	e.wantKind("SetSender of the URL without the credential", err, fault.Invalid)
	_, err = set(tenant.Sender{STOW: tenant.STOWSender{URL: "https://elsewhere.example/dicom-web", SealedCredential: []byte("other")}},
		tenant.FieldSTOWURL, tenant.FieldSTOWCredential)
	e.ok("SetSender of the URL with a credential", err)
	if c, _ := e.p.SenderCredential(e.ctx, "pacs2"); string(c) != "other" {
		e.Errorf("the credential named was not stored: %q", c)
	}
	_, err = set(tenant.Sender{STOW: tenant.STOWSender{Authentication: tenant.StowAuthNone}}, tenant.FieldSTOWAuthentication)
	e.ok("SetSender of no authentication", err)
	if c, err := e.p.SenderCredential(e.ctx, "pacs2"); fault.KindOf(err) != fault.NotFound {
		e.Errorf("authentication none kept a credential: %q, %v", c, err)
	}
	if got, _ := e.p.Sender(e.ctx, "pacs2"); got.STOW.CredentialSet {
		e.Error("a sender without authentication says it holds a credential")
	}
	_, err = set(tenant.Sender{DIMSE: tenant.DIMSESender{Port: 104}}, tenant.FieldDIMSEPeerPort)
	e.wantKind("SetSender of a field of the other type", err, fault.Invalid)
	_, err = set(tenant.Sender{})
	e.wantKind("SetSender of no field", err, fault.Invalid)
	_, err = set(tenant.Sender{}, tenant.FieldSenderConnectTimeout)
	e.wantKind("SetSender of a timeout with no value", err, fault.Invalid)
	_, err = e.p.SetSender(e.ctx, tenant.Sender{Name: "missing"}, []tenant.SenderField{tenant.FieldSenderEnabled}, at(10), e.audit(), nil)
	e.wantKind("SetSender of a sender not there", err, fault.NotFound)
}

// testSenderRemove guards removing a sender: refused while a receiver
// forwards to it and while a job to it is queued or running, unless its
// jobs are canceled with it in the same change; its jobs stay, canceled,
// naming it; its name stays held while they do; a receiver cannot forward
// to a sender the tenant does not have.
func testSenderRemove(e *senv) {
	_, target := e.setup("orthanc", 1, 2)
	r := receiverOf("fwd", "store", 11113)
	r.ForwardTo = []string{"missing"}
	_, err := e.p.AddReceiver(e.ctx, r, T0, e.audit(), nil)
	e.wantPrecondition("AddReceiver forwarding to a sender not there", err)
	r.ForwardTo = []string{"orthanc"}
	_, err = e.p.AddReceiver(e.ctx, r, T0, e.audit(), nil)
	e.ok("AddReceiver forwarding", err)
	if got, _ := e.p.Receiver(e.ctx, "fwd"); fmt.Sprint(got.ForwardTo) != "[orthanc]" {
		e.Errorf("a receiver's forward_to reads %v", got.ForwardTo)
	}
	_, _, err = e.p.RemoveSender(e.ctx, "orthanc", true, at(10), e.audit(), nil)
	e.wantPrecondition("RemoveSender while a receiver forwards to it", err)
	_, err = e.p.SetReceiver(e.ctx, tenant.Receiver{Name: "fwd"}, []tenant.ReceiverField{tenant.FieldForwardTo}, at(5), e.audit(), nil)
	e.ok("SetReceiver forward_to none", err)
	j := e.createJob("job1", "orthanc", target, at(20))
	_, _, err = e.p.RemoveSender(e.ctx, "orthanc", false, at(30), e.audit(), nil)
	e.wantPrecondition("RemoveSender with a job queued", err)
	start := e.now()
	gone, n, err := e.p.RemoveSender(e.ctx, "orthanc", true, at(30), e.audit(), nil)
	e.ok("RemoveSender canceling", err)
	if gone.Name != "orthanc" || n != 1 {
		e.Errorf("RemoveSender = %s, %d canceled; want orthanc, 1", gone.Name, n)
	}
	got, err := e.p.Job(e.ctx, j.ID)
	e.ok("Job", err)
	if got.State != tenant.JobCanceled || got.Reason != tenant.JobReasonSenderRemoved || got.Sender != "orthanc" || !got.Ended.Equal(at(30)) {
		e.Errorf("a job of a removed sender reads %+v", got)
	}
	if s := steps(e.jobEvents(start), j.ID); fmt.Sprint(s) != "[canceled]" {
		e.Errorf("the removal's events of the job: %v", s)
	}
	_, err = e.p.AddSender(e.ctx, dimseSender("orthanc"), at(40), e.audit(), nil)
	e.wantKind("AddSender of a name a job still names", err, fault.Conflict)
	_, _, err = e.p.RetryJob(e.ctx, j.ID, "job2", at(50), e.audit(), nil)
	e.wantPrecondition("RetryJob of a job whose sender was removed", err)
	_, _, err = e.p.RemoveSender(e.ctx, "orthanc", false, at(60), e.audit(), nil)
	e.wantKind("RemoveSender again", err, fault.NotFound)
	found := false
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if a.Details[tenant.DetailJobsCanceled] == "1" && a.Details[tenant.DetailSenderType] == "dimse" {
			found = true
		}
	}
	if !found {
		e.Error("the removal's audit entry does not say the sender's type and how many jobs it canceled")
	}
}

// testSenderChangesGuarded guards that a change of senders or jobs whose
// Guard refuses writes nothing: neither the change, nor its audit entry,
// nor its event.
func testSenderChangesGuarded(e *senv) {
	_, target := e.setup("orthanc", 1, 1)
	refuse := func(context.Context) error { return fault.New(fault.Denied, "revoked") }
	before := len(e.allAudit(master.AuditFilter{}))
	start := e.now()
	_, err := e.p.AddSender(e.ctx, dimseSender("other"), T0, e.audit(), refuse)
	e.wantKind("AddSender refused", err, fault.Denied)
	_, err = e.p.CreateJob(e.ctx, tenant.NewJob{ID: "job1", Sender: "orthanc", Target: target, Created: at(10)}, e.audit(), refuse)
	e.wantKind("CreateJob refused", err, fault.Denied)
	if _, err := e.p.Sender(e.ctx, "other"); fault.KindOf(err) != fault.NotFound {
		e.Error("a refused AddSender left its sender")
	}
	if _, err := e.p.Job(e.ctx, "job1"); fault.KindOf(err) != fault.NotFound {
		e.Error("a refused CreateJob left its job")
	}
	if _, err := e.p.ClaimWork(e.ctx, tenant.SendKind("orthanc"), "w", at(20), time.Minute); fault.KindOf(err) != fault.NotFound {
		e.Error("a refused CreateJob left its listing unit")
	}
	if after := len(e.allAudit(master.AuditFilter{})); after != before {
		e.Errorf("refused changes wrote %d audit entries", after-before)
	}
	if got := e.jobEvents(start); len(got) != 0 {
		e.Errorf("refused changes wrote events: %v", types(got))
	}
	j := e.createJob("job2", "orthanc", target, at(30))
	_, err = e.p.CancelJob(e.ctx, j.ID, at(40), e.audit(), refuse)
	e.wantKind("CancelJob refused", err, fault.Denied)
	if got, _ := e.p.Job(e.ctx, j.ID); got.State != tenant.JobQueued {
		e.Errorf("a refused cancel left the job %s", got.State)
	}
}
