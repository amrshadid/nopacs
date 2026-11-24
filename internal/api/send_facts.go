package api

import (
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The keys of the details the sender and job methods put in audit entries,
// beside those pipeline.go and requirement.go name.
const (
	detailKind       = "kind"
	detailSender     = "sender"
	detailTarget     = "target"
	detailStates     = "states"
	detailCancelJobs = "cancel_jobs"
	detailProfile    = "profile"
)

// maxSenderName is the longest name a sender may have: the rule of a
// tenant's name less the six bytes of "send-", since the server names the
// work of a sender's jobs "send-<name>" within the 63 bytes a kind of work
// may have (sender.proto, Sender.name).
const maxSenderName = 57

// maxStateFilter bounds the states a job listing may filter by, the
// contract's 16 (job.proto, ListJobsRequest.states).
const maxStateFilter = 16

// senderObject returns a sender's name as an audit entry's object: as given
// when it passes the rule of a sender's name, and "invalid" otherwise, since
// what a caller sends in its place can be anything.
func senderObject(name string) string {
	if len(name) > maxSenderName || master.ValidateTenantName(name) != nil {
		return invalidValue
	}
	return name
}

// senderType returns the type of the sender a request carries, as an audit
// entry's details name it.
func senderType(s *v1.Sender) string {
	switch s.GetType().(type) {
	case *v1.Sender_Dimse:
		return "dimse"
	case *v1.Sender_Stow:
		return "stow"
	default:
		return "none"
	}
}

// jobObject returns a job's id as an audit entry's object: as given when it
// has the form of an id the server makes, 1 to 64 bytes of A-Z, a-z, 0-9,
// ".", "_" and "-", and "invalid" otherwise.
func jobObject(id string) string {
	if id == "" || len(id) > MaxServerIDLength || strings.Trim(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789._-") != "" {
		return invalidValue
	}
	return id
}

// createJobDetails returns the details of a CreateJob entry: the job's
// kind, its sender and the path of labels of its target, each value that
// does not pass its rule recorded as "invalid". The new job's id, its
// object, is the handler's to record once it is made.
func createJobDetails(r *v1.CreateJobRequest) map[string]string {
	if dj := r.GetDeid(); dj != nil {
		d := map[string]string{detailKind: "deid", detailMode: strings.ToLower(strings.TrimPrefix(dj.GetMode().String(), "DEID_JOB_MODE_")),
			detailTarget: labelPath(dj.GetSubjectLabel(), dj.GetSessionLabel(), dj.GetScanLabel())}
		if dj.GetProfile() != "" {
			name := dj.GetProfile()
			if !profile.ValidName(name) {
				name = invalidValue
			}
			d[detailProfile] = name
		}
		return d
	}
	if sj := r.GetScrub(); sj != nil {
		storage := sj.GetStorage()
		if storage != "" && tenant.ValidateName("storage", storage) != nil {
			storage = invalidValue
		}
		return map[string]string{detailKind: "scrub", detailStorage: storage}
	}
	if ij := r.GetImportJob(); ij != nil {
		// The receiver by name, never the path or the map file; the
		// handler adds the receiver's root.
		receiver := ij.GetReceiver()
		if tenant.ValidateName("receiver", receiver) != nil {
			receiver = invalidValue
		}
		return map[string]string{detailKind: "import", detailReceiver: receiver}
	}
	send := r.GetSend()
	if send == nil {
		return map[string]string{detailKind: "none"}
	}
	return map[string]string{
		detailKind:   "send",
		detailSender: senderObject(send.GetSender()),
		detailTarget: labelPath(send.GetSubjectLabel(), send.GetSessionLabel(), send.GetScanLabel()),
	}
}

// removeSenderDetails returns the details of a RemoveSender entry: whether
// the request cancels the sender's jobs. The number canceled is the
// handler's to record.
func removeSenderDetails(r *v1.RemoveSenderRequest) map[string]string {
	if !r.GetCancelJobs() {
		return nil
	}
	return map[string]string{detailCancelJobs: "true"}
}

// jobFilterDetails returns the details of a ListJobs entry: the filters
// given, a sender's name recorded only when it passes the rule of one.
func jobFilterDetails(r *v1.ListJobsRequest) map[string]string {
	d := map[string]string{}
	if len(r.GetStates()) > 0 {
		names := make([]string, 0, len(r.GetStates()))
		for i, s := range r.GetStates() {
			if i == maxStateFilter {
				break
			}
			names = append(names, s.String())
		}
		d[detailStates] = clip(joinDetail(names), master.MaxAuditFieldLength-8)
	}
	if r.GetInProgress() {
		d[detailInProgress] = "true"
	}
	if r.GetSender() != "" {
		d[detailSender] = senderObject(r.GetSender())
	}
	timeDetails(d, r.GetStartTime().AsTime(), r.GetStartTime() != nil, r.GetEndTime().AsTime(), r.GetEndTime() != nil)
	return d
}
