package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's senders and jobs (tenant.Sending), on the tables of
// migrations/tenant/0001_baseline.sql: package sqlkit's one
// implementation, run here in SQLite's transactions. One writer runs at a
// time, so no row lock is taken and the clauses that would take one are
// empty.

// sendingDialect is how SQLite keeps what the senders' and jobs' statements
// write: times as microseconds, bools as 0 or 1, and the partial indexes of
// jobs not ended under the condition they are written with.
var sendingDialect = sqlkit.SendingDialect{
	Time:     func(t time.Time) any { return micros(t) },
	Bool:     func(b bool) any { return boolInt(b) },
	NotEnded: "ended = 0",
}

// sending returns the senders' and jobs' implementation on t.
func (t *Tenant) sending() sqlkit.Sending {
	return sqlkit.Sending{Host: sendingHost{t}, D: sendingDialect, Shadow: shadowHost{t}}
}

// sendingHost runs the senders' and jobs' transactions on a tenant.
type sendingHost struct{ t *Tenant }

// Write implements sqlkit.SendingHost.
func (h sendingHost) Write(ctx context.Context, message string, fn func(sqlkit.SendingTx) error) error {
	return h.t.write(ctx, message, func(tx *sql.Tx, s *tenantStatements) error { return fn(sendingTx{tx: tx, s: s}) })
}

// Read implements sqlkit.SendingHost.
func (h sendingHost) Read(ctx context.Context, message string, fn func(sqlkit.SendingTx) error) error {
	return h.t.readTx(ctx, message, func(tx *sql.Tx) error { return fn(sendingTx{tx: tx}) })
}

// Binding implements sqlkit.SendingHost.
func (h sendingHost) Binding(parts ...string) string { return h.t.binding(parts...) }

// sendingTx is one of the tenant's transactions as sqlkit.Sending runs its
// statements in it; s is nil in a read.
type sendingTx struct {
	tx *sql.Tx
	s  *tenantStatements
}

// Exec implements sqlkit.SendingTx.
func (x sendingTx) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	res, err := x.tx.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Query implements sqlkit.SendingTx.
func (x sendingTx) Query(ctx context.Context, query string, args []any, scan func(sqlkit.Row) error) error {
	rows, err := x.tx.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }() // rows.Err below reports what matters.
	for rows.Next() {
		if err := scan(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

// QueryRow implements sqlkit.SendingTx.
func (x sendingTx) QueryRow(ctx context.Context, query string, args []any, dest ...any) (bool, error) {
	err := x.tx.QueryRowContext(ctx, query, args...).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
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

// LockConfig implements sqlkit.SendingTx: one writer runs at a time, so a
// change of configuration has no other in flight.
func (sendingTx) LockConfig(context.Context) error { return nil }

// WriteEvents implements sqlkit.SendingTx.
func (x sendingTx) WriteEvents(ctx context.Context, events ...tenant.EventRecord) error {
	for _, e := range events {
		if _, err := writeEvent(ctx, x.tx, e, nil); err != nil {
			return err
		}
	}
	return nil
}

// Audited implements sqlkit.SendingTx.
func (x sendingTx) Audited(ctx context.Context, audit master.AuditEntry, guard tenant.Guard) error {
	return audited(ctx, x.tx, x.s, audit, guard)
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

// giveUpJobUnit ends what a unit held as failed carried for a job, and
// writes the events, in the transaction that holds it.
func giveUpJobUnit(ctx context.Context, tx *sql.Tx, workID string, now time.Time) error {
	x := sendingTx{tx: tx}
	events, err := sqlkit.Sending{D: sendingDialect}.GiveUpJobUnit(ctx, x, workID, now)
	if err != nil {
		return err
	}
	return x.WriteEvents(ctx, events...)
}

// forwardInTx makes the jobs of a step that reached done and writes their
// events, in the step's transaction.
func forwardInTx(ctx context.Context, tx *sql.Tx, r tenant.Reception) error {
	x := sendingTx{tx: tx}
	events, err := sqlkit.Sending{D: sendingDialect}.Forward(ctx, x, r)
	if err != nil {
		return err
	}
	return x.WriteEvents(ctx, events...)
}

// checkForwardTo refuses a receiver that forwards to a sender the tenant
// does not have.
func checkForwardTo(ctx context.Context, tx *sql.Tx, senders []string) error {
	return sqlkit.CheckForwardTo(ctx, sendingTx{tx: tx}, sendingDialect, senders)
}

// jobsInProgress counts the jobs not ended by state, in tx's snapshot.
func jobsInProgress(ctx context.Context, tx *sql.Tx) ([]tenant.JobStateCount, error) {
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
