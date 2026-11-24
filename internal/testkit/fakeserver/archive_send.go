package fakeserver

import (
	"context"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// senderHandler implements SenderService and jobHandler JobService, over the
// tables of the archive.
type (
	senderHandler struct{ s *Server }
	jobHandler    struct{ s *Server }
)

// AddSender puts a test sender in a tenant, as AddSender would have, for a
// test that reads it rather than adds it.
func (s *Server) AddSender(tenant string, sd *v1.Sender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	s.arch.senders[key(tenant, sd.GetName())] = sd
}

// SenderCredential returns the credential AddSender or SetSender received for a
// sender, so a test can show it reached the server and nowhere else.
func (s *Server) SenderCredential(tenant, name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	return s.arch.senderSecrets[key(tenant, name)]
}

// SetSenderHealth makes a sender answer the given health from now on.
func (s *Server) SetSenderHealth(tenant, name string, h *v1.SenderHealth) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	s.arch.senderHealth[key(tenant, name)] = h
}

// SetSenderTest makes TestSender answer these results for a sender, whose
// passed is whether none failed.
func (s *Server) SetSenderTest(tenant, name string, results []*v1.SenderTestResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	s.arch.senderTests[key(tenant, name)] = results
}

// AddJob puts a job in a tenant, as the server's workers would have made and
// advanced it.
func (s *Server) AddJob(tenant string, j *v1.Job) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	s.arch.jobs[key(tenant, j.GetId())] = j
}

// AddJobFailure adds a failure to a job's list.
func (s *Server) AddJobFailure(tenant, job string, f *v1.JobFailure) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	s.arch.failures[key(tenant, job)] = append(s.arch.failures[key(tenant, job)], f)
}

// Job returns a copy of a job as the fake holds it now.
func (s *Server) Job(tenant, id string) *v1.Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	if j := s.arch.jobs[key(tenant, id)]; j != nil {
		return proto.Clone(j).(*v1.Job)
	}
	return nil
}

// SetJob replaces a job's state and counts, as a worker advancing it would.
func (s *Server) SetJob(tenant, id string, state v1.JobState, counts *v1.JobCounts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	if j := s.arch.jobs[key(tenant, id)]; j != nil {
		j.State, j.Counts = state, counts
	}
}

// SetJobTallies gives a deid job of the tenant the tallies GetJob answers.
func (s *Server) SetJobTallies(tenant, id string, tallies ...*v1.JobTally) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.arch.ensure()
	if j := s.arch.jobs[key(tenant, id)]; j != nil {
		j.Tallies = tallies
	}
}

// senderHealthOf is the health the fake answers for a sender: down while
// disabled, else unknown, unless a test set one.
func (s *Server) senderHealthOf(tenant string, sd *v1.Sender) *v1.SenderHealth {
	if h, ok := s.arch.senderHealth[key(tenant, sd.GetName())]; ok {
		return h
	}
	if !sd.GetEnabled() {
		return &v1.SenderHealth{State: v1.SenderState_SENDER_STATE_DOWN, DownReason: v1.SenderDownReason_SENDER_DOWN_REASON_DISABLED, Detail: "the sender is disabled", Since: sd.GetUpdateTime()}
	}
	return &v1.SenderHealth{State: v1.SenderState_SENDER_STATE_UNKNOWN, Since: sd.GetUpdateTime()}
}

// senderAnswer returns a sender as an answer carries it: with its health, and
// without its credential, whether one is held said apart.
func (s *Server) senderAnswer(tenant string, sd *v1.Sender) *v1.Sender {
	out := proto.Clone(sd).(*v1.Sender)
	out.Health = s.senderHealthOf(tenant, out)
	if st := out.GetStow(); st != nil {
		st.Credential = ""
	}
	return out
}

// checkSender applies the rules of a sender the fake knows: a type with its
// required fields, and a credential when the authentication needs one.
func checkSender(sd *v1.Sender, credential string) error {
	switch {
	case sd.GetDimse() != nil:
		d := sd.GetDimse()
		if d.GetHost() == "" || d.GetPort() < 1 || d.GetPort() > 65535 || d.GetCalledAeTitle() == "" || d.GetCallingAeTitle() == "" {
			return fault.New(fault.Invalid, "sender.dimse: a host, a port, a called and a calling AE title are required")
		}
	case sd.GetStow() != nil:
		st := sd.GetStow()
		if st.GetUrl() == "" {
			return fault.New(fault.Invalid, "sender.stow.url: is required")
		}
		switch st.GetAuthentication() {
		case v1.StowAuthentication_STOW_AUTHENTICATION_UNSPECIFIED:
			return fault.New(fault.Invalid, "sender.stow.authentication: is required: none, basic, bearer or header, stated in so many words")
		case v1.StowAuthentication_STOW_AUTHENTICATION_NONE:
			if credential != "" {
				return fault.New(fault.Invalid, "sender.stow.credential: is empty with authentication none")
			}
		default:
			if credential == "" {
				return fault.New(fault.Invalid, "sender.stow.credential: is required and not empty")
			}
		}
	default:
		return fault.New(fault.Invalid, "sender: a type is required")
	}
	if n := sd.GetMaxAssociations(); n < 0 || n > 16 {
		return fault.New(fault.Invalid, "sender.max_associations: is 1 to 16")
	}
	return nil
}

// AddSender adds a sender, filling the defaults the contract names.
func (h senderHandler) AddSender(_ context.Context, req *v1.AddSenderRequest) (*v1.AddSenderResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	in := req.GetSender()
	if err := itemOK("sender.name", in.GetName()); err != nil {
		return nil, err
	}
	credential := in.GetStow().GetCredential()
	if err := checkSender(in, credential); err != nil {
		return nil, err
	}
	k := key(req.GetTenant(), in.GetName())
	if _, ok := s.arch.senders[k]; ok {
		return nil, fault.New(fault.Conflict, "the tenant has a sender with that name")
	}
	sd := proto.Clone(in).(*v1.Sender)
	sd.Health = nil
	if sd.GetStow() != nil {
		sd.GetStow().Credential = ""
		sd.GetStow().CredentialSet = credential != ""
		if sd.GetStow().GetMaxInstancesPerRequest() == 0 {
			sd.GetStow().MaxInstancesPerRequest = 64
		}
		if sd.GetStow().GetMaxRequestBytes() == 0 {
			sd.GetStow().MaxRequestBytes = 1 << 28
		}
	}
	if sd.GetMaxAssociations() == 0 {
		sd.MaxAssociations = 2
	}
	if sd.GetConnectTimeout() == nil {
		sd.ConnectTimeout = durationpb.New(30 * time.Second)
	}
	if sd.GetResponseTimeout() == nil {
		sd.ResponseTimeout = durationpb.New(5 * time.Minute)
	}
	sd.CreateTime, sd.UpdateTime = timestamppb.New(s.now()), timestamppb.New(s.now())
	s.arch.senders[k] = sd
	if credential != "" {
		s.arch.senderSecrets[k] = credential
	}
	return &v1.AddSenderResponse{Sender: s.senderAnswer(req.GetTenant(), sd)}, nil
}

// GetSender answers one sender with its health.
func (h senderHandler) GetSender(_ context.Context, req *v1.GetSenderRequest) (*v1.GetSenderResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	sd, ok := s.arch.senders[key(req.GetTenant(), req.GetName())]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no sender with that name")
	}
	return &v1.GetSenderResponse{Sender: s.senderAnswer(req.GetTenant(), sd)}, nil
}

// ListSenders answers a page of a tenant's senders by name.
func (h senderHandler) ListSenders(_ context.Context, req *v1.ListSendersRequest) (*v1.ListSendersResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	page, next, err := pageOfKeys(forTenant(s.arch.senders, req.GetTenant()), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	out := &v1.ListSendersResponse{NextPageToken: next}
	for _, k := range page {
		out.Senders = append(out.Senders, s.senderAnswer(req.GetTenant(), s.arch.senders[k]))
	}
	return out, nil
}

// SetSender changes the fields the mask names, as the contract's paths, and
// keeps the credential unless the mask names it.
func (h senderHandler) SetSender(_ context.Context, req *v1.SetSenderRequest) (*v1.SetSenderResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	paths := req.GetUpdateMask().GetPaths()
	if len(paths) == 0 {
		return nil, fault.New(fault.Invalid, "update_mask: is required and not empty")
	}
	k := key(req.GetTenant(), req.GetSender().GetName())
	cur, ok := s.arch.senders[k]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no sender with that name")
	}
	in := req.GetSender()
	next := proto.Clone(cur).(*v1.Sender)
	secret := s.arch.senderSecrets[k]
	for _, p := range paths {
		if typ, _, typed := strings.Cut(p, "."); typed && ((typ == "dimse") != (cur.GetDimse() != nil) || (typ == "stow") != (cur.GetStow() != nil)) && (typ == "dimse" || typ == "stow") {
			return nil, fault.New(fault.Invalid, "update_mask: "+p+" names a field of another type")
		}
		switch p {
		case "enabled":
			next.Enabled = in.GetEnabled()
		case "max_associations":
			next.MaxAssociations = in.GetMaxAssociations()
		case "max_bytes_per_second":
			next.MaxBytesPerSecond = in.GetMaxBytesPerSecond()
		case "connect_timeout":
			next.ConnectTimeout = in.GetConnectTimeout()
		case "response_timeout":
			next.ResponseTimeout = in.GetResponseTimeout()
		case "dimse.host":
			next.GetDimse().Host = in.GetDimse().GetHost()
		case "dimse.port":
			next.GetDimse().Port = in.GetDimse().GetPort()
		case "dimse.called_ae_title":
			next.GetDimse().CalledAeTitle = in.GetDimse().GetCalledAeTitle()
		case "dimse.calling_ae_title":
			next.GetDimse().CallingAeTitle = in.GetDimse().GetCallingAeTitle()
		case "dimse.tls":
			next.GetDimse().Tls = in.GetDimse().GetTls()
		case "dimse.tls.ca_file", "dimse.tls.certificate_file", "dimse.tls.private_key_file", "dimse.tls.server_name":
			if next.GetDimse().GetTls() == nil {
				next.GetDimse().Tls = &v1.SenderTls{}
			}
			setTLSField(next.GetDimse().GetTls(), in.GetDimse().GetTls(), p)
		case "stow.url":
			next.GetStow().Url = in.GetStow().GetUrl()
		case "stow.authentication":
			next.GetStow().Authentication = in.GetStow().GetAuthentication()
		case "stow.username":
			next.GetStow().Username = in.GetStow().GetUsername()
		case "stow.header_name":
			next.GetStow().HeaderName = in.GetStow().GetHeaderName()
		case "stow.credential":
			secret = in.GetStow().GetCredential()
		case "stow.tls":
			next.GetStow().Tls = in.GetStow().GetTls()
		case "stow.tls.ca_file", "stow.tls.certificate_file", "stow.tls.private_key_file", "stow.tls.server_name":
			if next.GetStow().GetTls() == nil {
				next.GetStow().Tls = &v1.SenderTls{}
			}
			setTLSField(next.GetStow().GetTls(), in.GetStow().GetTls(), p)
		case "stow.max_instances_per_request":
			next.GetStow().MaxInstancesPerRequest = in.GetStow().GetMaxInstancesPerRequest()
		case "stow.max_request_bytes":
			next.GetStow().MaxRequestBytes = in.GetStow().GetMaxRequestBytes()
		default:
			return nil, fault.New(fault.Invalid, "update_mask: "+p+" names no field that can change")
		}
	}
	if (slices.Contains(paths, "stow.url") || slices.Contains(paths, "stow.authentication")) && !slices.Contains(paths, "stow.credential") &&
		next.GetStow().GetAuthentication() != v1.StowAuthentication_STOW_AUTHENTICATION_NONE {
		return nil, fault.New(fault.Invalid, "update_mask: stow.url and stow.authentication are named with stow.credential")
	}
	if err := checkSender(next, secret); err != nil {
		return nil, err
	}
	next.UpdateTime = timestamppb.New(s.now())
	if next.GetStow() != nil {
		next.GetStow().CredentialSet = secret != ""
	}
	s.arch.senders[k] = next
	if secret != "" {
		s.arch.senderSecrets[k] = secret
	} else {
		delete(s.arch.senderSecrets, k)
	}
	return &v1.SetSenderResponse{Sender: s.senderAnswer(req.GetTenant(), next)}, nil
}

// setTLSField copies the one TLS field a mask path names.
func setTLSField(dst, src *v1.SenderTls, path string) {
	switch {
	case strings.HasSuffix(path, "ca_file"):
		dst.CaFile = src.GetCaFile()
	case strings.HasSuffix(path, "certificate_file"):
		dst.CertificateFile = src.GetCertificateFile()
	case strings.HasSuffix(path, "private_key_file"):
		dst.PrivateKeyFile = src.GetPrivateKeyFile()
	case strings.HasSuffix(path, "server_name"):
		dst.ServerName = src.GetServerName()
	}
}

// RemoveSender removes a sender, refusing while a receiver forwards to it or a
// job to it is in progress unless cancel_jobs is set.
func (h senderHandler) RemoveSender(_ context.Context, req *v1.RemoveSenderRequest) (*v1.RemoveSenderResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	k := key(req.GetTenant(), req.GetName())
	sd, ok := s.arch.senders[k]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no sender with that name")
	}
	for _, rk := range forTenant(s.arch.receivers, req.GetTenant()) {
		if slices.Contains(s.arch.receivers[rk].GetForwardTo(), req.GetName()) {
			return nil, fault.New(fault.Precondition, "the receiver "+s.arch.receivers[rk].GetName()+" forwards to this sender")
		}
	}
	var live []*v1.Job
	for _, jk := range forTenant(s.arch.jobs, req.GetTenant()) {
		if j := s.arch.jobs[jk]; j.GetSend().GetSender() == req.GetName() && !jobEnded(j.GetState()) {
			live = append(live, j)
		}
	}
	if len(live) > 0 && !req.GetCancelJobs() {
		return nil, fault.New(fault.Precondition, "a job to this sender is queued or running; cancel it, or remove the sender with cancel_jobs")
	}
	for _, j := range live {
		j.State, j.EndTime = v1.JobState_JOB_STATE_CANCELED, timestamppb.New(s.now())
	}
	delete(s.arch.senders, k)
	delete(s.arch.senderSecrets, k)
	out := s.senderAnswer(req.GetTenant(), sd)
	out.Health = nil
	return &v1.RemoveSenderResponse{Sender: out, JobsCanceled: int64(len(live))}, nil
}

// TestSender answers the results a test set, or a passing test of each step
// of the sender's type.
func (h senderHandler) TestSender(_ context.Context, req *v1.TestSenderRequest) (*v1.TestSenderResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	k := key(req.GetTenant(), req.GetName())
	sd, ok := s.arch.senders[k]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no sender with that name")
	}
	results := s.arch.senderTests[k]
	if results == nil {
		took := durationpb.New(3 * time.Millisecond)
		step := func(st v1.SenderTestStep, detail string) *v1.SenderTestResult {
			return &v1.SenderTestResult{Step: st, Outcome: v1.SenderTestOutcome_SENDER_TEST_OUTCOME_PASSED, Duration: took, Detail: detail}
		}
		results = []*v1.SenderTestResult{step(v1.SenderTestStep_SENDER_TEST_STEP_RESOLVE, ""), step(v1.SenderTestStep_SENDER_TEST_STEP_CONNECT, "")}
		if sd.GetDimse() != nil {
			results = append(results, step(v1.SenderTestStep_SENDER_TEST_STEP_ASSOCIATE, ""), step(v1.SenderTestStep_SENDER_TEST_STEP_REQUEST, "0000"), step(v1.SenderTestStep_SENDER_TEST_STEP_RELEASE, ""))
		} else {
			results = append(results, step(v1.SenderTestStep_SENDER_TEST_STEP_REQUEST, "HTTP 200"))
		}
	}
	passed := true
	for _, r := range results {
		passed = passed && r.GetOutcome() != v1.SenderTestOutcome_SENDER_TEST_OUTCOME_FAILED
	}
	return &v1.TestSenderResponse{Passed: passed, Results: results}, nil
}

// jobEnded says whether a state is one a job never leaves.
func jobEnded(s v1.JobState) bool {
	switch s {
	case v1.JobState_JOB_STATE_DONE, v1.JobState_JOB_STATE_PARTIAL, v1.JobState_JOB_STATE_FAILED, v1.JobState_JOB_STATE_CANCELED:
		return true
	}
	return false
}

// CreateJob makes a send job, queued, with the counts of nothing yet listed.
func (h jobHandler) CreateJob(_ context.Context, req *v1.CreateJobRequest) (*v1.CreateJobResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if dj := req.GetDeid(); dj != nil {
		if dj.GetMode() == v1.DeidJobMode_DEID_JOB_MODE_UNSPECIFIED {
			return nil, fault.New(fault.Invalid, "deid.mode: is required")
		}
		d := proto.Clone(dj).(*v1.DeidJob)
		if d.GetProfile() != "" && d.GetProfileVersion() == 0 {
			d.ProfileVersion = 1
		}
		j := &v1.Job{Id: s.newJobID(req.GetTenant()), Kind: &v1.Job_Deid{Deid: d}, State: v1.JobState_JOB_STATE_QUEUED,
			Counts: &v1.JobCounts{}, CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now())}
		s.arch.jobs[key(req.GetTenant(), j.GetId())] = j
		return &v1.CreateJobResponse{Job: proto.Clone(j).(*v1.Job)}, nil
	}
	if ij := req.GetImportJob(); ij != nil {
		return s.createImportJob(req.GetTenant(), ij)
	}
	in := req.GetSend()
	if in == nil {
		return nil, fault.New(fault.Invalid, "send: a kind is required")
	}
	if in.GetSubjectLabel() == "" || (in.GetSessionLabel() == "" && in.GetScanLabel() != "") || in.GetReception() != "" || in.GetReceiver() != "" {
		return nil, fault.New(fault.Invalid, "send: a subject is required, a scan needs its session, and a reception is the server's to set")
	}
	if _, ok := s.arch.senders[key(req.GetTenant(), in.GetSender())]; !ok {
		return nil, fault.New(fault.Precondition, "the tenant has no sender with that name")
	}
	j := &v1.Job{
		Id: s.newJobID(req.GetTenant()), Kind: &v1.Job_Send{Send: proto.Clone(in).(*v1.SendJob)},
		State: v1.JobState_JOB_STATE_QUEUED, Counts: &v1.JobCounts{}, CreatedByKeyId: "",
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now()),
	}
	s.arch.jobs[key(req.GetTenant(), j.GetId())] = j
	return &v1.CreateJobResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// newJobID makes the id of the next job: job_1, job_2 and so on, past any a
// test added under one of those names.
func (s *Server) newJobID(tenant string) string {
	for {
		s.arch.nextJob++
		id := "job_" + strconv.Itoa(s.arch.nextJob)
		if _, taken := s.arch.jobs[key(tenant, id)]; !taken {
			return id
		}
	}
}

// job returns a stored job, or the not found error.
func (s *Server) job(tenant, id string) (*v1.Job, error) {
	j, ok := s.arch.jobs[key(tenant, id)]
	if !ok {
		return nil, fault.New(fault.NotFound, "the tenant has no job with that id")
	}
	return j, nil
}

// GetJob answers one job.
func (h jobHandler) GetJob(_ context.Context, req *v1.GetJobRequest) (*v1.GetJobResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	j, err := s.job(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	return &v1.GetJobResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// ListJobs answers a page of a tenant's jobs, newest first, with the
// contract's filters.
func (h jobHandler) ListJobs(_ context.Context, req *v1.ListJobsRequest) (*v1.ListJobsResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	var all []*v1.Job
	for _, k := range forTenant(s.arch.jobs, req.GetTenant()) {
		j := s.arch.jobs[k]
		switch {
		case req.GetSender() != "" && j.GetSend().GetSender() != req.GetSender(),
			len(req.GetStates()) > 0 && !slices.Contains(req.GetStates(), j.GetState()),
			req.GetInProgress() && jobEnded(j.GetState()),
			req.GetStartTime() != nil && j.GetCreateTime().AsTime().Before(req.GetStartTime().AsTime()),
			req.GetEndTime() != nil && !j.GetCreateTime().AsTime().Before(req.GetEndTime().AsTime()):
			continue
		}
		all = append(all, j)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].GetCreateTime().AsTime().After(all[j].GetCreateTime().AsTime()) })
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListJobsResponse{Jobs: all[from:to], NextPageToken: next}, nil
}

// CancelJob ends a queued or running job as canceled; a canceled one is
// answered as it stands, and one that ended otherwise is a state that forbids.
func (h jobHandler) CancelJob(_ context.Context, req *v1.CancelJobRequest) (*v1.CancelJobResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	j, err := s.job(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	switch {
	case j.GetState() == v1.JobState_JOB_STATE_CANCELED:
	case jobEnded(j.GetState()):
		return nil, fault.New(fault.Precondition, "the job has ended "+strings.ToLower(strings.TrimPrefix(j.GetState().String(), "JOB_STATE_"))+" and cannot be canceled")
	default:
		j.State, j.EndTime, j.StateTime = v1.JobState_JOB_STATE_CANCELED, timestamppb.New(s.now()), timestamppb.New(s.now())
	}
	return &v1.CancelJobResponse{Job: proto.Clone(j).(*v1.Job)}, nil
}

// RetryJob makes a job that sends what an ended job did not send; a job
// already retried is answered as it stands.
func (h jobHandler) RetryJob(_ context.Context, req *v1.RetryJobRequest) (*v1.RetryJobResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	j, err := s.job(req.GetTenant(), req.GetId())
	if err != nil {
		return nil, err
	}
	if j.GetRetryJob() != "" {
		r := s.arch.jobs[key(req.GetTenant(), j.GetRetryJob())]
		return &v1.RetryJobResponse{Job: proto.Clone(j).(*v1.Job), Retry: proto.Clone(r).(*v1.Job)}, nil
	}
	if !jobEnded(j.GetState()) || j.GetState() == v1.JobState_JOB_STATE_DONE {
		return nil, fault.New(fault.Precondition, "the job has not ended, or sent everything, so it has nothing to retry")
	}
	r := &v1.Job{
		Id: s.newJobID(req.GetTenant()), Kind: &v1.Job_Send{Send: proto.Clone(j.GetSend()).(*v1.SendJob)},
		State: v1.JobState_JOB_STATE_QUEUED, Counts: &v1.JobCounts{}, RetryOf: j.GetId(),
		CreateTime: timestamppb.New(s.now()), StateTime: timestamppb.New(s.now()),
	}
	j.RetryJob = r.GetId()
	s.arch.jobs[key(req.GetTenant(), r.GetId())] = r
	return &v1.RetryJobResponse{Job: proto.Clone(j).(*v1.Job), Retry: proto.Clone(r).(*v1.Job)}, nil
}

// ListJobFailures answers a page of a job's failures, those sent with a
// warning only when asked for.
func (h jobHandler) ListJobFailures(_ context.Context, req *v1.ListJobFailuresRequest) (*v1.ListJobFailuresResponse, error) {
	s := h.s
	err := s.begin(req.GetTenant())
	defer s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if _, err := s.job(req.GetTenant(), req.GetId()); err != nil {
		return nil, err
	}
	var all []*v1.JobFailure
	for _, f := range s.arch.failures[key(req.GetTenant(), req.GetId())] {
		if f.GetCode() == v1.JobFailureCode_JOB_FAILURE_CODE_PEER_WARNING && !req.GetIncludeWarnings() {
			continue
		}
		all = append(all, f)
	}
	from, to, next, err := pageOf(len(all), req.GetPageSize(), req.GetPageToken())
	if err != nil {
		return nil, err
	}
	return &v1.ListJobFailuresResponse{Failures: all[from:to], NextPageToken: next}, nil
}
