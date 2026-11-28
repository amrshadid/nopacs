package sqlkit

import (
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The statements of jobs' work that read tables which grow, named so the
// plan tests of both drivers read the very text that runs (Plans). Each is a
// range or a lookup of the index named beside it.
const (
	// jobScanByWorkSQL reads the scan a unit sends: job_scans_by_work,
	// then the scan's key. The scan is joined outer, so a scan a delete
	// hid whole is still found its unit's (jobScanOf).
	jobScanByWorkSQL = `SELECT ` + jobScanColumns + ` FROM job_scans AS js LEFT JOIN scans AS s ON s.id = js.scan_id
		WHERE js.work_id = ?`
	// hiddenScanSQL reads the labels, storage and series of a scan a
	// delete hid whole, by its id, from the correction that hid it last:
	// a read of hidden_scans, which holds only the scans of deletes not
	// yet purged.
	hiddenScanSQL = `SELECT subject_label, session_label, label, storage, series_uid FROM hidden_scans WHERE id = ?
		ORDER BY correction_id DESC LIMIT 1`
	// deletedStreamSQL reads a page of the instances of a scan's stream a
	// committed delete hid, before its condition (SendingDialect.deleted):
	// the corrections of the scan by correction_scans_by_subject, then
	// hidden_instances' key of each. Of an instance hidden twice under the
	// scan, the row of the first correction is the one.
	deletedStreamSQL = `SELECT h.sop_instance_uid, h.reception_id, h.size, h.held_at FROM hidden_instances AS h
		WHERE h.correction_id IN (SELECT c.correction_id FROM correction_scans AS c WHERE lower(c.subject_label) = ?
			AND c.series_uid = ?)
		AND NOT EXISTS (SELECT 1 FROM hidden_instances AS x WHERE x.sop_instance_uid = h.sop_instance_uid
			AND x.scan_id = h.scan_id AND x.correction_id < h.correction_id) AND `
	// forwardScansSQL lists a reception's scans after one: reception_scans'
	// key.
	forwardScansSQL = `SELECT scan_id, instances, bytes FROM reception_scans WHERE reception_id = ? AND scan_id > ?
		ORDER BY scan_id LIMIT ?`
	// forwardScanSQL lists the one scan of a reception a forward rule's
	// series filter chose, by its labels, after one.
	forwardScanSQL = `SELECT r.scan_id, r.instances, r.bytes FROM reception_scans AS r JOIN scans AS s ON s.id = r.scan_id
		WHERE r.reception_id = ? AND s.subject_folded = ? AND s.session_folded = ? AND s.folded = ? AND r.scan_id > ?
		ORDER BY r.scan_id LIMIT ?`
	// targetScansColumns are what a listing reads of a scan of the catalog.
	targetScansColumns = `SELECT id, instances, bytes, session_folded, folded, updated FROM scans WHERE `
	// tenantScansSQL lists the tenant's scans after one, for a deid job of
	// the whole tenant: a range of the scans' key.
	tenantScansSQL = targetScansColumns + `id > ? ORDER BY id LIMIT ?`
	// storageScansSQL lists one storage's scans after one, for a scrub of
	// that storage: a range of the scans' index on (storage, id), which
	// SQLite's scans_by_storage is with its row id.
	storageScansSQL = targetScansColumns + `storage = ? AND id > ? ORDER BY id LIMIT ?`
	// targetScanSQL reads one scan by its labels: scans_by_label.
	targetScanSQL = targetScansColumns + `subject_folded = ? AND session_folded = ? AND folded = ? AND subject_label = ?
		AND session_label = ? AND label = ?`
	// sessionScansSQL lists a session's scans after one: a range of
	// scans_by_label.
	sessionScansSQL = targetScansColumns + `subject_folded = ? AND session_folded = ? AND subject_label = ? AND session_label = ?
		AND folded > ? ORDER BY folded LIMIT ?`
	// subjectScansSQL lists a subject's scans after one: a range of
	// scans_by_label.
	subjectScansSQL = targetScansColumns + `subject_folded = ? AND subject_label = ? AND (session_folded, folded) > (?, ?)
		ORDER BY session_folded, folded LIMIT ?`
	// heldByCountSQL counts a scan's instances held by a time: a range of
	// the instances' key.
	heldByCountSQL = `SELECT count(*), CAST(coalesce(sum(size), 0) AS BIGINT) FROM instances WHERE scan_id = ? AND held_at <= ?`
	// retriedScansSQL lists the ended job's scans a retry reads, after
	// one: job_scans' key.
	retriedScansSQL = `SELECT ` + jobScanColumns + ` FROM job_scans AS js JOIN scans AS s ON s.id = js.scan_id
		WHERE js.job_id = ? AND js.scan_id > ? ORDER BY js.scan_id LIMIT ?`
	// streamCountSQL and streamPageSQL count and read a scan's stream,
	// before its condition (SendingDialect.stream): the instances' key.
	streamCountSQL = `SELECT count(*), CAST(coalesce(sum(i.size), 0) AS BIGINT) FROM instances AS i WHERE `
	// The profile version an instance records is read by its key, and its
	// scrub's mark by instance_damage's (damageColumn); whether a pixel risk
	// was accepted is the row's own, which every send asks of each instance
	// (tenant.SendRefusal).
	streamPageSQL = `SELECT i.sop_instance_uid, i.reception_id, i.storage_key, i.byte_offset, i.byte_length, i.size,
		i.sha256, coalesce(i.archived_sha256, i.sha256), i.held_at, coalesce(pv.name, ''), coalesce(pv.version, 0),
		coalesce((SELECT d.finding_id FROM instance_damage AS d WHERE d.sop_instance_uid = i.sop_instance_uid), ''),
		i.pixel_risk_accepted
		FROM instances AS i LEFT JOIN profile_versions AS pv ON pv.id = i.profile_version_id WHERE `
	// scanRefusedSQL reads the instances an ended job's scan refused, by
	// the scan's key: job_failures_by_scan.
	scanRefusedSQL = `SELECT scan_id, sop_instance_uid FROM job_failures WHERE job_id = ? AND scan_id = ? AND warning = ?`
	// owedCountSQL counts the instances a scan's unit owes: job_owed's key.
	owedCountSQL = `SELECT count(*) FROM job_owed WHERE job_id = ? AND scan_id = ?`
	// owedInstancesSQL reads the instances a scan's unit owes, as a page of
	// its stream: job_owed's key, then the instances' key.
	owedInstancesSQL = streamPageSQL + `i.sop_instance_uid IN (SELECT sop_instance_uid FROM job_owed WHERE job_id = ?
		AND scan_id = ?) AND i.scan_id = ? ORDER BY i.sop_instance_uid LIMIT ?`
	// receptionArchivedSQL asks whether a reception archived an instance:
	// reception_scans' key.
	receptionArchivedSQL = `SELECT 1 FROM reception_scans WHERE reception_id = ? AND instances > 0 LIMIT 1`
	// queuedUnitsSQL reads a job's units that are queued: the work's key,
	// for its listing and each scan of job_scans' key.
	queuedUnitsSQL = `SELECT id FROM work WHERE state = 'queued' AND (id = ? OR id IN (SELECT work_id FROM job_scans
		WHERE job_id = ?))`
)

// SenderJobsInProgressSQL reads a sender's jobs not ended:
// jobs_of_sender_in_progress.
func (d SendingDialect) SenderJobsInProgressSQL() string {
	return `SELECT id FROM jobs WHERE sender = ? AND ` + d.NotEnded + ` ORDER BY created, id`
}

// SendingPlan is one statement of senders' and jobs' work with arguments a
// plan test runs it with, and the index it must be answered from.
type SendingPlan struct {
	// Name names the read.
	Name string
	// Index is the index the plan must use, or a part of its name; empty
	// for the table's primary key, whose name each database gives its own
	// way.
	Index string
	// SQL is the statement, with "?" for each parameter.
	SQL string
	// Args are its parameters' values.
	Args []any
	// Ordered says the plan must read the rows in the statement's order
	// from its index, sorting none: a queue whose backlog may be long, read
	// a few rows at a time.
	Ordered bool
}

// Plans returns every statement of senders' and jobs' work that reads a
// table that grows, with arguments a plan test fills the tables to match:
// jobs "j<n>" made at n seconds, few of them of sender "s1" or failed;
// job_scans of job "j1" keyed by scan; failures of job "j1"; owed
// instances of job "j1".
func (d SendingDialect) Plans() []SendingPlan {
	at := time.Unix(5000, 0).UTC()
	after := &TimePosition{At: at, ID: "j5000"}
	jobs := func(f tenant.JobFilter) (string, []any) { return d.JobsQuery(f, after, 101) }
	q := func(name, index string, sql string, args []any) SendingPlan {
		return SendingPlan{Name: name, Index: index, SQL: sql, Args: args}
	}
	var out []SendingPlan
	add := func(name, index string, f tenant.JobFilter) {
		sql, args := jobs(f)
		out = append(out, q(name, index, sql, args))
	}
	add("ListJobs", "jobs_by_created", tenant.JobFilter{})
	add("ListJobs of a state", "jobs_by_state", tenant.JobFilter{States: []tenant.JobState{tenant.JobFailed}})
	add("ListJobs of states", "jobs_by_state", tenant.JobFilter{States: []tenant.JobState{tenant.JobFailed, tenant.JobQueued}})
	add("ListJobs of a sender", "jobs_by_sender", tenant.JobFilter{Sender: "s1"})
	add("ListJobs in progress", "jobs_in_progress", tenant.JobFilter{InProgress: true})
	fsql, fargs := d.FailuresQuery("j1", false, []string{"sub-1", "ses-1", "scan-1", "2.25.3.5"}, 101)
	// The refused alone: job_failures_by_kind, or the key with the warnings
	// filtered out where most of a job's failures are refusals; both are a
	// range of the one job's.
	out = append(out, q("ListJobFailures refused", "job_failures_", fsql, fargs))
	fsql, fargs = d.FailuresQuery("j1", true, []string{"sub-1", "ses-1", "scan-1", "2.25.3.5"}, 101)
	out = append(out, q("ListJobFailures", "", fsql, fargs))
	j := tenant.Job{ID: "j1"}
	ssql, sargs := d.StreamPage(j, at, 77, "", "2.25.3.1", 64)
	out = append(out, q("a scan's stream", "", ssql, sargs))
	dsql, dargs := d.DeletedPage(j, at, 77, "sub-1", "2.25.2.77", "", "2.25.3.1", 64)
	out = append(out, q("a scan's deleted instances", "correction_scans_by_subject", dsql, dargs))
	return append(out,
		q("the scan a unit sends", "job_scans_by_work", jobScanByWorkSQL, []any{"u77"}),
		q("a forward's scans", "", forwardScansSQL, []any{"rc5", int64(0), tenant.JobListPage}),
		q("the tenant's scans", "", tenantScansSQL, []any{int64(0), tenant.JobListPage}),
		// A storage that holds a few scans of many: every scan of the fill
		// is on "store".
		q("a storage's scans", "scans_by_storage", storageScansSQL, []any{"cold", int64(0), tenant.JobListPage}),
		q("a check's series", "", seenSeriesSQL+"?)", []any{"2.25.2.7"}),
		q("a check's studies", "scans_by_study", seenStudySQL+"?)", []any{"2.25.1.7"}),
		q("a check's instances", "instances_", seenInstanceSQL+"?)", []any{"2.25.3.7"}),
		q("a check's instances as received", "instances_", plainInstanceSQL+"?)", []any{"2.25.3.7"}),
		q("a check's series as received", "", plainSeriesSQL+"?)", []any{"2.25.2.7"}),
		q("a check's studies as received", "scans_by_study", plainStudySQL+"?)", []any{"2.25.1.7"}),
		q("a check's instances de-identified", "instances_", deidInstanceSQL+"?)", []any{"2.25.3.7"}),
		q("a check's series de-identified", "", deidSeriesSQL+"?)", []any{"2.25.2.7"}),
		q("a check's studies de-identified", "scans_by_study", deidStudySQL+"?)", []any{"2.25.1.7"}),
		q("a job's tallies", "", jobTalliesSQL, []any{"j1", tenant.MaxJobTallies + 1}),
		q("a session's scans", "scans_by_label", sessionScansSQL, []any{"sub-1", "ses-1", "sub-1", "ses-1", "", tenant.JobListPage}),
		q("a subject's scans", "scans_by_label", subjectScansSQL, []any{"sub-1", "sub-1", "", "", tenant.JobListPage}),
		q("a scan's instances by a time", "", heldByCountSQL, []any{int64(77), d.Time(at)}),
		q("a retry's scans", "", retriedScansSQL, []any{"j1", int64(0), tenant.JobListPage}),
		q("a job's queued units", "", queuedUnitsSQL, []any{"u1", "j1"}),
		q("a retried scan's refused", "job_failures_by_scan", scanRefusedSQL, []any{"j1", int64(7), d.Bool(false)}),
		q("a unit's owed count", "", owedCountSQL, []any{"j1", int64(7)}),
		q("a unit's owed instances", "", owedInstancesSQL, []any{"j1", int64(7), int64(7), tenant.MaxJobOwed}),
		q("whether a reception archived", "", receptionArchivedSQL, []any{"rc5"}),
		q("a sender's jobs in progress", "jobs_of_sender_in_progress", d.SenderJobsInProgressSQL(), []any{"s1"}),
		// Either partial index of the jobs not ended bounds the count.
		q("the jobs in progress by state", "_in_progress", d.JobsInProgressSQL(), nil),
		// The replicas' queue: rows of replica "copy" for series
		// "2.25.2.<n>", due at n seconds, a few pending, most in step,
		// some copying with their leases ended.
		SendingPlan{Name: "the replicas' rows due", Index: "replica_scans_due", SQL: replicaDueSQL + d.SkipLocked,
			Args: []any{d.Time(at), tenant.MaxReplicaClaim}, Ordered: true},
		SendingPlan{Name: "the replicas' leases ended", Index: "replica_scans_leased", SQL: replicaLeaseEndedSQL + d.SkipLocked,
			Args: []any{d.Time(at), tenant.MaxReplicaClaim}, Ordered: true},
		// The sessions due: rows of study "2.25.1.<n>" under rule
		// "r<n % 4>", due at n % 1000 seconds, one in ten due under none;
		// a page after a position.
		SendingPlan{Name: "the sessions due under a rule", Index: "retention_sessions_due", SQL: dueByRuleSQL,
			Args: []any{"r3", d.Time(at), d.Time(dueBefore), "", tenant.MaxDisposeSessions}, Ordered: true},
		SendingPlan{Name: "the sessions due under any rule", Index: "retention_sessions_next", SQL: dueAllSQL,
			Args: []any{d.Time(at), d.Time(dueBefore), "", tenant.MaxDisposeSessions}, Ordered: true},
	)
}
