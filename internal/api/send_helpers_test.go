package api_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testDestinations is the installation's sender.destinations of the tests:
// a host with two ports, a host with every port, and a range with one.
var testDestinations = strings.Join([]string{"pacs.lab.example ports=104,11112", "orthanc.lab.example", "10.20.0.0/24 ports=104"}, "\n")

// senderFixture is the place fixture with the installation's destinations
// set, so a sender can be added toward them.
type senderFixture struct{ *placeFixture }

// newSenderFixture returns the sender fixture.
func newSenderFixture(t *testing.T) *senderFixture {
	t.Helper()
	f := newPlaceFixture(t)
	f.setSetting(installation(), settings.SenderDestinations, testDestinations)
	return &senderFixture{f}
}

// dimseTo is an enabled DIMSE sender as a request gives it.
func dimseTo(name, host string, port int32) *v1.Sender {
	return &v1.Sender{Name: name, Enabled: true, Type: &v1.Sender_Dimse{Dimse: &v1.DimseSender{
		Host: host, Port: port, CalledAeTitle: "PACS", CallingAeTitle: "NOPACS",
	}}}
}

// stowTo is an enabled stow sender as a request gives it.
func stowTo(name, url string, auth v1.StowAuthentication, user, credential string) *v1.Sender {
	return &v1.Sender{Name: name, Enabled: true, Type: &v1.Sender_Stow{Stow: &v1.StowSender{
		Url: url, Authentication: auth, Username: user, Credential: credential,
	}}}
}

// addSender is a call of AddSender.
func addSender(tenantName string, s *v1.Sender) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.senders.AddSender, &v1.AddSenderRequest{Tenant: tenantName, Sender: s})
	}
}

// getSender is a call of GetSender on lab1.
func getSender(name string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.senders.GetSender, &v1.GetSenderRequest{Tenant: "lab1", Name: name})
	}
}

// setSender is a call of SetSender on lab1.
func setSender(s *v1.Sender, paths ...string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.senders.SetSender, &v1.SetSenderRequest{Tenant: "lab1", Sender: s, UpdateMask: mask(paths...)})
	}
}

// removeSender is a call of RemoveSender on lab1.
func removeSender(name string, cancel bool) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.senders.RemoveSender, &v1.RemoveSenderRequest{Tenant: "lab1", Name: name, CancelJobs: cancel})
	}
}

// addedSenders adds the senders given to lab1 as the local operator,
// failing the test when one is refused.
func (f *senderFixture) addedSenders(t *testing.T, senders ...*v1.Sender) {
	t.Helper()
	for _, s := range senders {
		mustCall[*v1.AddSenderResponse](t, "add "+s.GetName(), addSender("lab1", s)(f.as(local)))
	}
}

// tenantID returns the id of the tenant named name.
func (e *env) tenantID(name string) string {
	e.t.Helper()
	row, err := e.master.TenantByName(context.Background(), name)
	if err != nil {
		e.t.Fatal(err)
	}
	return row.ID
}

// sealedCredential returns the sealed credential lab1's store holds for the
// stow sender named name.
func (e *env) sealedCredential(name string) ([]byte, error) {
	e.t.Helper()
	return e.pipelineOf("lab1").SenderCredential(context.Background(), name)
}

// fakeSending stands for the part of the server that sends: it answers a
// sender's health from a table a test sets, and a test of a peer from a
// function a test sets, recording what it was asked.
type fakeSending struct {
	mu      sync.Mutex
	health  map[string]api.SenderHealth
	results []api.SenderTestResult
	asked   []string
	changed []string
}

// SendersChanged records the notice.
func (f *fakeSending) SendersChanged(tenantName string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.changed = append(f.changed, tenantName)
}

// changes returns how many notices of changes it was given.
func (f *fakeSending) changes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.changed)
}

// SenderHealth answers the health set for the sender, or unknown.
func (f *fakeSending) SenderHealth(tenantName string, s tenant.Sender) api.SenderHealth {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h, ok := f.health[tenantName+"/"+s.Name]; ok {
		return h
	}
	return api.SenderHealth{State: tenant.SenderUnknown, Since: s.Updated}
}

// TestSender records the question and answers the results set.
func (f *fakeSending) TestSender(_ context.Context, tenantName string, s tenant.Sender) ([]api.SenderTestResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, tenantName+"/"+s.Name)
	if f.results == nil {
		return nil, fault.New(fault.Internal, "no results set")
	}
	return f.results, nil
}

// setHealth sets the health a sender is answered with.
func (f *fakeSending) setHealth(tenantName, sender string, h api.SenderHealth) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.health == nil {
		f.health = map[string]api.SenderHealth{}
	}
	f.health[tenantName+"/"+sender] = h
}

// sending wires a fakeSending into the services under test.
func (e *env) sending() *fakeSending {
	f := &fakeSending{}
	api.SetSenderHealths(e.handlers, f)
	api.SetSenderTests(e.handlers, f)
	return f
}

// jobData is a small archive (two instances of one scan of one subject,
// received through the pipeline) with lab1's receiver, and two senders,
// "pacs" (enabled) and "spare" (disabled), toward destinations the
// installation lists.
type jobData struct {
	f  *placeFixture
	sf *senderFixture
	// session and scan are the one session and scan the archive holds; the
	// subject is session.SubjectLabel.
	session *v1.Session
	scan    *v1.Scan
	// operator is a key with the operator role on lab1, and its id.
	operator   caller
	operatorID string
}

// newJobData returns the job data.
func newJobData(t *testing.T) *jobData {
	t.Helper()
	f := newPlaceFixture(t)
	receiveArchive(t, f, [][]byte{corpusFile(t, "MR_small.dcm"), reUID(t, corpusFile(t, "MR_small_RLE.dcm"), '8')})
	// The archive was held on the day the reception pipeline's clock says;
	// a job sends what was held before it was made.
	f.advance(72 * time.Hour)
	f.setSetting(installation(), settings.SenderDestinations, testDestinations)
	cat := f.as(f.lab1Reader).catalog
	label := v1.CatalogOrder_CATALOG_ORDER_LABEL
	sessions := mustCall[*v1.ListSessionsResponse](t, "sessions", call(cat.ListSessions, &v1.ListSessionsRequest{Tenant: "lab1", Order: label, PageSize: 1000})).GetSessions()
	scans := mustCall[*v1.ListScansResponse](t, "scans", call(cat.ListScans, &v1.ListScansRequest{Tenant: "lab1", Order: label, PageSize: 1000})).GetScans()
	if len(sessions) != 1 || len(scans) != 1 || scans[0].GetInstanceCount() != 2 {
		t.Fatalf("the archive holds %d sessions and %d scans, want 1 and 1 of 2 instances: %v", len(sessions), len(scans), scans)
	}
	sf := &senderFixture{f}
	spare := dimseTo("spare", "pacs.lab.example", 104)
	spare.Enabled = false
	sf.addedSenders(t, dimseTo("pacs", "pacs.lab.example", 104), spare)
	op, id := f.key(f.onTenant("lab1", master.RoleOperator))
	return &jobData{f: f, sf: sf, session: sessions[0], scan: scans[0], operator: op, operatorID: id}
}

// target is a request's send job to sender of the scan, session or subject
// named by labels (a subject alone, or with its session, or with its scan).
func target(sender string, labels ...string) *v1.SendJob {
	j := &v1.SendJob{Sender: sender}
	for i, l := range labels {
		switch i {
		case 0:
			j.SubjectLabel = l
		case 1:
			j.SessionLabel = l
		case 2:
			j.ScanLabel = l
		}
	}
	return j
}

// createJob is a call of CreateJob on lab1.
func createJob(j *v1.SendJob) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.jobs.CreateJob, &v1.CreateJobRequest{Tenant: "lab1", Kind: &v1.CreateJobRequest_Send{Send: j}})
	}
}

// makeJob makes a job of the subject, as the local operator, and returns
// it.
func (d *jobData) makeJob(t *testing.T, sender string, labels ...string) *v1.Job {
	t.Helper()
	return mustCall[*v1.CreateJobResponse](t, "create a job", createJob(target(sender, labels...))(d.f.as(local))).GetJob()
}

// cancelJob, getJob and retryJob are calls on lab1.
func cancelJob(id string) func(clients) outcome {
	return func(c clients) outcome { return call(c.jobs.CancelJob, &v1.CancelJobRequest{Tenant: "lab1", Id: id}) }
}

func getJob(id string) func(clients) outcome {
	return func(c clients) outcome { return call(c.jobs.GetJob, &v1.GetJobRequest{Tenant: "lab1", Id: id}) }
}

func retryJob(id string) func(clients) outcome {
	return func(c clients) outcome { return call(c.jobs.RetryJob, &v1.RetryJobRequest{Tenant: "lab1", Id: id}) }
}

// work does what the part of the server that sends does, through the
// store: it lists the job of the sender and sends every scan, the peer
// answering a warning for the first instance when warn is set and refusing
// every other when refuse is set, and accepting the rest.
func (d *jobData) work(t *testing.T, sender string, warn, refuse bool) {
	t.Helper()
	p := d.f.pipelineOf("lab1")
	now := d.f.now()
	first := true
	for range 100 {
		w, err := p.ClaimWork(ctx, tenant.SendKind(sender), "test", now, time.Minute)
		if fault.KindOf(err) == fault.NotFound {
			return
		}
		if err != nil {
			t.Fatal(err)
		}
		unit, err := p.StartJobUnit(ctx, w.Fence(), now)
		if err != nil {
			t.Fatal(err)
		}
		if unit.Listing {
			for more := true; more; {
				if _, more, err = p.ListJobScans(ctx, w.Fence(), now); err != nil {
					t.Fatal(err)
				}
			}
			continue
		}
		ins, err := p.JobScanInstances(ctx, w.Fence(), "", tenant.MaxJobInstancePage)
		if err != nil {
			t.Fatal(err)
		}
		prog := tenant.JobProgress{Fence: w.Fence(), FailuresKept: tenant.DefaultFailuresKept, Now: now}
		for _, in := range ins {
			prog.To = in.SOPInstanceUID
			code := tenant.JobFailureCode("")
			switch {
			case warn && first:
				code = tenant.FailurePeerWarning
			case refuse:
				code = tenant.FailurePeerRefused
			}
			first = false
			switch {
			case code == "":
				prog.Sent, prog.SentBytes = prog.Sent+1, prog.SentBytes+in.Size
				continue
			case code.Refused():
				prog.Refused, prog.RefusedBytes = prog.Refused+1, prog.RefusedBytes+in.Size
			default:
				prog.Warned, prog.WarnedBytes = prog.Warned+1, prog.WarnedBytes+in.Size
			}
			prog.Failures = append(prog.Failures, tenant.JobFailure{SOPInstanceUID: in.SOPInstanceUID, Code: code, PeerStatus: 0xC000,
				SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Detail: "refused by the peer", Time: now})
		}
		if _, err := p.RecordJobProgress(ctx, prog); err != nil {
			t.Fatal(err)
		}
		end := tenant.JobProgress{Fence: w.Fence(), From: prog.To, To: prog.To, FailuresKept: tenant.DefaultFailuresKept, Now: now}
		if _, err := p.EndJobScan(ctx, end); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("the job's units did not end")
}
