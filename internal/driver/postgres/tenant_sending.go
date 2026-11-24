package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's senders and jobs (tenant.Sending), on the tables of
// migrations/tenant/0011_senders_and_jobs.sql: package sqlkit's one
// implementation, run here in Postgres's transactions at read committed,
// each change through Tenant.pipe, so a deadlock or a serialization failure
// runs it again. The rows are locked in the package's order (doc.go): a
// unit's row (its fence), a sender's, a job's, then its scan's; a cancel
// skips a unit's row a transaction holds rather than wait for it, since a
// worker locks its unit's row before its job's.

// sendingDialect is how Postgres keeps what the senders' and jobs'
// statements write, and the row locks they take.
var sendingDialect = sqlkit.SendingDialect{
	Time:       func(t time.Time) any { return micros(t) },
	Bool:       func(b bool) any { return b },
	Lock:       " FOR NO KEY UPDATE",
	Share:      " FOR KEY SHARE",
	LockDelete: " FOR UPDATE",
	SkipLocked: " FOR UPDATE SKIP LOCKED",
	NotEnded:   "NOT ended",
	// A rebuild's shadow is tables in a scratch schema of the tenant's
	// database, which the job names.
	ScratchRequired: true,
	// The catalog's ids are identities always generated (0002_pipeline.sql).
	Overriding: " OVERRIDING SYSTEM VALUE",
}

// sending returns the senders' and jobs' implementation on t.
func (t *Tenant) sending() sqlkit.Sending {
	return sqlkit.Sending{Host: sendingHost{t}, D: sendingDialect, Shadow: shadowHost{t}}
}

// sendingHost runs the senders' and jobs' transactions on a tenant.
type sendingHost struct{ t *Tenant }

// Write implements sqlkit.SendingHost.
func (h sendingHost) Write(ctx context.Context, message string, fn func(sqlkit.SendingTx) error) error {
	return h.t.pipe(ctx, message, func(tx pgx.Tx) error { return fn(sendingTx{tx: tx, t: h.t}) })
}

// Read implements sqlkit.SendingHost: one snapshot, read only.
func (h sendingHost) Read(ctx context.Context, message string, fn func(sqlkit.SendingTx) error) error {
	return h.t.readFeed(ctx, message, func(tx pgx.Tx) error { return fn(sendingTx{tx: tx, t: h.t}) })
}

// Binding implements sqlkit.SendingHost.
func (h sendingHost) Binding(parts ...string) string { return h.t.binding(parts...) }

// sendingTx is one of the tenant's transactions as sqlkit.Sending runs its
// statements in it, each rewritten with Postgres's parameters.
type sendingTx struct {
	tx pgx.Tx
	t  *Tenant
}

// Exec implements sqlkit.SendingTx.
func (x sendingTx) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	return rowsChanged(x.tx.Exec(ctx, sqlkit.Rebind(query), args...))
}

// Query implements sqlkit.SendingTx.
func (x sendingTx) Query(ctx context.Context, query string, args []any, scan func(sqlkit.Row) error) error {
	rows, err := x.tx.Query(ctx, sqlkit.Rebind(query), args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// QueryRow implements sqlkit.SendingTx.
func (x sendingTx) QueryRow(ctx context.Context, query string, args []any, dest ...any) (bool, error) {
	err := x.tx.QueryRow(ctx, sqlkit.Rebind(query), args...).Scan(dest...)
	if isNoRow(err) {
		return false, nil
	}
	return err == nil, err
}

// Enqueue implements sqlkit.SendingTx.
func (x sendingTx) Enqueue(ctx context.Context, w tenant.Work, now time.Time) (tenant.Work, bool, error) {
	if err := w.Validate(); err != nil {
		return tenant.Work{}, false, err
	}
	return enqueueInTx(ctx, x.tx, w, now)
}

// CheckFence implements sqlkit.SendingTx.
func (x sendingTx) CheckFence(ctx context.Context, f tenant.WorkFence) error {
	return checkFence(ctx, x.tx, f)
}

// LockConfig implements sqlkit.SendingTx.
func (x sendingTx) LockConfig(ctx context.Context) error { return lockConfig(ctx, x.tx) }

// WriteEvents implements sqlkit.SendingTx: the feed's lock is taken here,
// last in the change but for its audit entry, and held to the commit.
func (x sendingTx) WriteEvents(ctx context.Context, events ...tenant.EventRecord) error {
	return writeEvents(ctx, x.tx, events...)
}

// Audited implements sqlkit.SendingTx.
func (x sendingTx) Audited(ctx context.Context, audit master.AuditEntry, guard tenant.Guard) error {
	return x.t.audited(ctx, x.tx, audit, guard)
}

// AddSender implements tenant.Senders.
func (t *Tenant) AddSender(ctx context.Context, s tenant.Sender, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Sender, error) {
	return t.sending().AddSender(ctx, s, now, audit, guard)
}

// Sender implements tenant.Senders.
func (t *Tenant) Sender(ctx context.Context, name string) (tenant.Sender, error) {
	return t.sending().Sender(ctx, name)
}

// ListSenders implements tenant.Senders.
func (t *Tenant) ListSenders(ctx context.Context, page master.Page) ([]tenant.Sender, string, error) {
	return t.sending().ListSenders(ctx, page)
}

// SetSender implements tenant.Senders.
func (t *Tenant) SetSender(ctx context.Context, s tenant.Sender, fields []tenant.SenderField, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Sender, error) {
	return t.sending().SetSender(ctx, s, fields, now, audit, guard)
}

// RemoveSender implements tenant.Senders.
func (t *Tenant) RemoveSender(ctx context.Context, name string, cancelJobs bool, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Sender, int64, error) {
	return t.sending().RemoveSender(ctx, name, cancelJobs, now, audit, guard)
}

// SenderCredential implements tenant.Senders.
func (t *Tenant) SenderCredential(ctx context.Context, name string) ([]byte, error) {
	return t.sending().SenderCredential(ctx, name)
}

// CreateJob implements tenant.Jobs.
func (t *Tenant) CreateJob(ctx context.Context, j tenant.NewJob, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	return t.sending().CreateJob(ctx, j, audit, guard)
}

// Job implements tenant.Jobs.
func (t *Tenant) Job(ctx context.Context, id string) (tenant.Job, error) {
	return t.sending().Job(ctx, id)
}

// ListJobs implements tenant.Jobs.
func (t *Tenant) ListJobs(ctx context.Context, filter tenant.JobFilter, page master.Page) ([]tenant.Job, string, error) {
	return t.sending().ListJobs(ctx, filter, page)
}

// CancelJob implements tenant.Jobs.
func (t *Tenant) CancelJob(ctx context.Context, id string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, error) {
	return t.sending().CancelJob(ctx, id, now, audit, guard)
}

// RetryJob implements tenant.Jobs.
func (t *Tenant) RetryJob(ctx context.Context, id, newID string, now time.Time, audit master.AuditEntry, guard tenant.Guard) (tenant.Job, tenant.Job, error) {
	return t.sending().RetryJob(ctx, id, newID, now, audit, guard)
}

// ListJobFailures implements tenant.Jobs.
func (t *Tenant) ListJobFailures(ctx context.Context, id string, includeWarnings bool, page master.Page) ([]tenant.JobFailure, string, error) {
	return t.sending().ListJobFailures(ctx, id, includeWarnings, page)
}

// JobTallies implements tenant.Jobs.
func (t *Tenant) JobTallies(ctx context.Context, id string) ([]tenant.JobTally, error) {
	return t.sending().JobTallies(ctx, id)
}

// SeenUIDs implements tenant.JobWork.
func (t *Tenant) SeenUIDs(ctx context.Context, keyed, plain []string) (tenant.UIDsSeen, error) {
	return t.sending().SeenUIDs(ctx, keyed, plain)
}

// StartJobUnit implements tenant.JobWork.
func (t *Tenant) StartJobUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.JobUnit, error) {
	return t.sending().StartJobUnit(ctx, fence, now)
}

// ListJobScans implements tenant.JobWork.
func (t *Tenant) ListJobScans(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, bool, error) {
	return t.sending().ListJobScans(ctx, fence, now)
}

// JobScanInstances implements tenant.JobWork.
func (t *Tenant) JobScanInstances(ctx context.Context, fence tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error) {
	return t.sending().JobScanInstances(ctx, fence, after, limit)
}

// DeletedJobInstances implements tenant.JobWork.
func (t *Tenant) DeletedJobInstances(ctx context.Context, fence tenant.WorkFence, after string, limit int) ([]tenant.ScanInstance, error) {
	return t.sending().DeletedJobInstances(ctx, fence, after, limit)
}

// RecordJobProgress implements tenant.JobWork.
func (t *Tenant) RecordJobProgress(ctx context.Context, p tenant.JobProgress) (tenant.Job, error) {
	return t.sending().RecordJobProgress(ctx, p)
}

// EndJobScan implements tenant.JobWork.
func (t *Tenant) EndJobScan(ctx context.Context, p tenant.JobProgress) (tenant.Job, error) {
	return t.sending().EndJobScan(ctx, p)
}

// ReleaseJobUnit implements tenant.JobWork.
func (t *Tenant) ReleaseJobUnit(ctx context.Context, fence tenant.WorkFence, due time.Time, reason string, now time.Time) error {
	return t.sending().ReleaseJobUnit(ctx, fence, due, reason, now)
}

// giveUpJobUnit ends what a unit held as failed carried for a job, in the
// transaction that holds it, and returns the events, which the caller
// writes last.
func giveUpJobUnit(ctx context.Context, tx pgx.Tx, workID string, now time.Time) ([]tenant.EventRecord, error) {
	return sqlkit.Sending{D: sendingDialect}.GiveUpJobUnit(ctx, sendingTx{tx: tx}, workID, now)
}

// forwardInTx makes the jobs of a step that reached done, in the step's
// transaction, and returns their events, which the step writes with its
// own.
func forwardInTx(ctx context.Context, tx pgx.Tx, r tenant.Reception) ([]tenant.EventRecord, error) {
	return sqlkit.Sending{D: sendingDialect}.Forward(ctx, sendingTx{tx: tx}, r)
}

// checkForwardTo refuses a receiver that forwards to a sender the tenant
// does not have, holding each sender's row from removal to the commit.
func checkForwardTo(ctx context.Context, tx pgx.Tx, senders []string) error {
	return sqlkit.CheckForwardTo(ctx, sendingTx{tx: tx}, sendingDialect, senders)
}

// jobsInProgress counts the jobs not ended by state, in tx's snapshot.
func jobsInProgress(ctx context.Context, tx pgx.Tx) ([]tenant.JobStateCount, error) {
	counts := map[tenant.JobState]int64{}
	err := sendingTx{tx: tx}.Query(ctx, sendingDialect.JobsInProgressSQL(), nil, func(r sqlkit.Row) error {
		var state string
		var n int64
		err := r.Scan(&state, &n)
		counts[tenant.JobState(state)] = n
		return err
	})
	return tenant.InProgressJobs(counts), err
}
