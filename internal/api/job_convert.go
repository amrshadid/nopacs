package api

import (
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The phrases of a job held back, from the fixed set the contract has
// (job.proto, Job.reason); the store writes those of a job that ended.
const (
	reasonSenderDisabled = "sender disabled"
	reasonSenderDown     = "sender down"
)

// jobCountsMessage returns a job's counts as the contract writes them.
func jobCountsMessage(c tenant.JobCounts) *v1.JobCounts {
	return &v1.JobCounts{
		Instances: c.Instances, Bytes: c.Bytes, InstancesSent: c.InstancesSent, BytesSent: c.BytesSent,
		InstancesWarned: c.InstancesWarned, BytesWarned: c.BytesWarned, InstancesRefused: c.InstancesRefused,
		BytesRefused: c.BytesRefused, InstancesRemaining: c.InstancesRemaining(), BytesRemaining: c.BytesRemaining(),
		Scans: c.Scans, ScansDone: c.ScansDone, Listed: c.Listed,
	}
}

// jobMessage returns a job as the contract writes it. held is the phrase of
// why a job in progress is not moving, which the server knows from its view
// of the sender, empty for none; it is used only when the store wrote no
// reason of its own.
func jobMessage(j tenant.Job, held string) *v1.Job {
	m := &v1.Job{
		Id:     j.ID,
		State:  enumOf[v1.JobState](v1.JobState_value, "JOB_STATE_", string(j.State)),
		Counts: jobCountsMessage(j.Counts), Reason: j.Reason, CreatedByKeyId: j.CreatedByKeyID,
		CreateTime: timestamp(j.Created), StartTime: timestamp(j.Started), StateTime: timestamp(j.StateTime),
		EndTime: timestamp(j.Ended), RetryOf: j.RetryOf, RetryJob: j.RetryJob, FailuresNotListed: j.FailuresNotListed,
	}
	if m.Reason == "" && !j.State.Ended() {
		m.Reason = held
	}
	switch j.Kind {
	case tenant.JobSend:
		m.Kind = &v1.Job_Send{Send: &v1.SendJob{
			Sender: j.Sender, SubjectLabel: j.Target.SubjectLabel, SessionLabel: j.Target.SessionLabel,
			ScanLabel: j.Target.ScanLabel, Reception: j.ReceptionID, Receiver: j.Receiver,
		}}
	case tenant.JobDeid:
		m.Kind = &v1.Job_Deid{Deid: &v1.DeidJob{
			Mode:    enumOf[v1.DeidJobMode](v1.DeidJobMode_value, "DEID_JOB_MODE_", string(j.Mode)),
			Profile: j.Profile.Name, ProfileVersion: j.Profile.Version, SubjectLabel: j.Target.SubjectLabel,
			SessionLabel: j.Target.SessionLabel, ScanLabel: j.Target.ScanLabel,
		}}
	case tenant.JobScrub:
		m.Kind = &v1.Job_Scrub{Scrub: &v1.ScrubJob{Storage: j.Storage, Scheduled: j.Scheduled}}
	case tenant.JobRebuild:
		m.Kind = &v1.Job_Rebuild{Rebuild: rebuildMessage(j.Rebuild)}
	case tenant.JobImport:
		// Its paths are answered here; GetJob and ListJobs take them out
		// for a reader (hideImportPaths).
		m.Kind = &v1.Job_ImportJob{ImportJob: importJobMessage(j)}
	case tenant.JobRepair:
		r := &v1.RepairJob{}
		if j.Repair != nil {
			r = &v1.RepairJob{Storage: j.Repair.Storage, From: j.Repair.From, Findings: j.Repair.Findings, All: j.Repair.All, Automatic: j.Repair.Automatic}
		}
		m.Kind = &v1.Job_Repair{Repair: r}
	case tenant.JobReplicate:
		r := &v1.ReplicateJob{Mode: v1.ReplicateJobMode_REPLICATE_JOB_MODE_BACKFILL}
		if j.Replicate != nil {
			r.Replica, r.Primary, r.Scheduled = j.Replicate.Replica, j.Replicate.Primary, j.Replicate.Scheduled
		}
		m.Kind = &v1.Job_Replicate{Replicate: r}
	case tenant.JobDispose:
		// Its rule and reason are the dispose job's own rows
		// (disposeJobMessage), which a view reads.
		m.Kind = &v1.Job_Dispose{Dispose: &v1.DisposeJob{}}
	case tenant.JobDeleteBatch:
		// Its reason and whether it is paused are the batch job's own rows,
		// which a view reads.
		m.Kind = &v1.Job_DeleteBatch{DeleteBatch: &v1.DeleteBatchJob{}}
	case tenant.JobSearchBackfill:
		// Its attributes are the job's own row, which a view reads.
		m.Kind = &v1.Job_SearchBackfill{SearchBackfill: &v1.SearchBackfillJob{}}
	}
	return m
}

// disposeJobMessage returns a dispose job's own part as the contract writes
// it.
func disposeJobMessage(dj tenant.DisposeJob) *v1.DisposeJob {
	return &v1.DisposeJob{Rule: dj.Rule, Reason: dj.Reason, Sessions: int64(len(dj.Sessions)), Scheduled: dj.Scheduled}
}

// jobFailureMessage returns a failure of a job as the contract writes it.
func jobFailureMessage(f tenant.JobFailure) *v1.JobFailure {
	return &v1.JobFailure{
		InstanceId: f.SOPInstanceUID, SubjectLabel: f.Path.Subject, SessionLabel: f.Path.Session, ScanLabel: f.Path.Scan,
		Code:       enumOf[v1.JobFailureCode](v1.JobFailureCode_value, "JOB_FAILURE_CODE_", string(f.Code)),
		PeerStatus: f.PeerStatus, PeerStatusHttp: f.HTTPStatus(), SopClassUid: f.SOPClassUID, TransferSyntaxUid: f.TransferSyntaxUID,
		Detail: f.Detail, Time: timestamp(f.Time),
	}
}

// jobStateOf returns the state a request's filter names, and false for
// UNSPECIFIED or a number the contract does not define.
func jobStateOf(s v1.JobState) (tenant.JobState, bool) {
	name, ok := strings.CutPrefix(s.String(), "JOB_STATE_")
	if !ok || s == v1.JobState_JOB_STATE_UNSPECIFIED {
		return "", false
	}
	return tenant.JobState(strings.ToLower(name)), true
}
