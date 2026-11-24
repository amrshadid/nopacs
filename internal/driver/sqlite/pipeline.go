package sqlite

import (
	"context"
	"database/sql"
	"errors"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The pipeline's operations on a tenant's database (tenant.Pipeline), in
// pipeline_*.go, on the tables of migrations/tenant/0002_pipeline.sql.
//
// Every change goes through Tenant.write, so it is grouped with the changes
// waiting at the same moment like every other (write.go), and like every
// other it only reads, writes and assigns its results to variables it sets
// afresh on each run: a change may run again in a new transaction.
//
// The statements run per file or per claim, where their parsing would show,
// are prepared once (pipelineStatements); the others, run when a person
// changes a configuration or a step is taken, are written in their
// functions and parsed when they run.

var _ tenant.Pipeline = (*Tenant)(nil)

// The names of the pipeline's lists in their cursors, apart from each other
// and from the settings and audit lists.
const (
	listStorages      = "tenant-storages"
	listReceivers     = "tenant-receivers"
	listReceptions    = "tenant-receptions"
	listReceivedFiles = "tenant-received-files"
	listQuarantine    = "tenant-quarantine"
	listDiscarded     = "tenant-discarded-files"
	listWithoutWork   = "tenant-receptions-without-work"
	listScanInstances = "tenant-scan-instances"
)

// pipelineStatements are the pipeline's prepared statements.
type pipelineStatements struct {
	// Write pool.
	insertReceived, insertAssociation, receptionForBatch, addReceived *sql.Stmt
	claimNext, leaseUnit                                              *sql.Stmt
	instanceRow, insertClaim, receivedFile                            *sql.Stmt
	// Read pool.
	duplicates *sql.Stmt
}

// pipelineEntries returns the pipeline's statements to prepare into s.
func pipelineEntries(s *pipelineStatements, p *pools) []stmtEntry {
	r, w := p.read, p.write
	return []stmtEntry{
		{&s.insertReceived, w, `INSERT INTO received_files (` + receivedInsertColumns + `)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`},
		{&s.insertAssociation, w, `INSERT INTO reception_associations (reception_id, association) VALUES (?, ?) ON CONFLICT DO NOTHING`},
		// The reception's row, locked by an update whose condition is that
		// it is receiving: on a database with several writers a step and a
		// batch of one reception are then ordered, and what the batch read
		// stays true until it commits.
		{&s.receptionForBatch, w, `UPDATE receptions SET last_file = last_file WHERE id = ? AND state = 'receiving'
			RETURNING study_uid, calling_ae_titles, calling_truncated`},
		{&s.addReceived, w, `UPDATE receptions SET files_received = files_received + ?, bytes_received = bytes_received + ?,
			files_released = files_released + ?, associations = associations + ?, calling_ae_titles = ?, calling_truncated = ?,
			last_file = max(last_file, ?) WHERE id = ? AND state = 'receiving'`},
		{&s.claimNext, w, claimNextSQL},
		{&s.leaseUnit, w, `UPDATE work SET state = 'leased', attempt = attempt + 1, owner = ?, due = ?, updated = ?
			WHERE id = ? AND attempt = ? AND state IN ('queued', 'leased')`},
		{&s.duplicates, r, duplicatesSQL},
		{&s.instanceRow, w, instanceRowSQL},
		// A new claim; ON CONFLICT DO NOTHING so that a claim that meets
		// another's (committed meanwhile, on a database with several
		// writers) reads that row and decides again.
		{&s.insertClaim, w, `INSERT INTO instance_claims (sop_instance_uid, study_uid, series_uid, sha256, reception_id, claimed_at)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`},
		// The reception's received file a claim names: the received files'
		// primary key.
		{&s.receivedFile, w, `SELECT study_uid, series_uid FROM received_files WHERE reception_id = ? AND sop_instance_uid = ? AND sha256 = ?`},
	}
}

// write runs fn in one write transaction (base.writeTx) with the tenant's
// prepared statements. fn may run more than once and must only assign its
// results.
func (t *Tenant) write(ctx context.Context, message string, fn func(tx *sql.Tx, s *tenantStatements) error) error {
	s, err := t.ready(ctx)
	if err != nil {
		return err
	}
	return t.writeTx(ctx, message, func(tx *sql.Tx) error { return fn(tx, s) })
}

// audited ends a change a person or a key asked for: its audit entry is
// written in its transaction, then its Guard runs, last, as Store says.
func audited(ctx context.Context, tx *sql.Tx, s *tenantStatements, audit master.AuditEntry, guard tenant.Guard) error {
	if err := execAudit(ctx, tx.StmtContext(ctx, s.insertAudit), audit); err != nil {
		return err
	}
	return runLast(ctx, tx, guarded(guard))
}

// exists reports whether query, run in tx with args, returns a row.
func exists(ctx context.Context, tx *sql.Tx, query string, args ...any) (bool, error) {
	var one int
	err := tx.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// readRows runs query on the read pool and scans every row.
func readRows[T any](ctx context.Context, t *Tenant, message, query string, args []any, scan func(interface{ Scan(...any) error }) (T, error)) ([]T, error) {
	if _, err := t.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := t.p.read.QueryContext(ctx, query, args...)
	out, err := collect(rows, err, scan)
	if err != nil {
		return nil, t.fail(ctx, err, message)
	}
	return out, nil
}

// readOne runs query on the read pool and scans its one row, or returns
// NotFound with what.
func readOne[T any](ctx context.Context, t *Tenant, what, message, query string, args []any, scan func(interface{ Scan(...any) error }) (T, error)) (T, error) {
	var zero T
	if _, err := t.ready(ctx); err != nil {
		return zero, err
	}
	v, err := scan(t.p.read.QueryRowContext(ctx, query, args...))
	if err != nil {
		return zero, t.notFound(ctx, err, what, message)
	}
	return v, nil
}

// binding is what a pipeline list's cursor is bound to: the tenant and the
// list's arguments.
func (t *Tenant) binding(parts ...string) string {
	return sqlkit.Binding(append([]string{t.lists.owner}, parts...)...)
}

// pageAfterName decodes a list's cursor that holds one name, or returns ""
// for the first page.
func (t *Tenant) pageAfterName(page master.Page, list string) (string, error) {
	if err := page.Validate(); err != nil {
		return "", err
	}
	if page.Cursor == "" {
		return "", nil
	}
	f, err := sqlkit.DecodeCursor(page.Cursor, list, t.binding(), 1)
	if err != nil {
		return "", err
	}
	return f[0], nil
}

// afterArgs returns a newest-first list's keyset arguments, the (time, id)
// of the last row returned, or nil for the first page.
func afterArgs(pos *sqlkit.TimePosition) []any {
	if pos == nil {
		return nil
	}
	return []any{micros(pos.At), pos.ID}
}

// nullBlob returns NULL for a zero checksum, its bytes otherwise.
func nullBlob(s tenant.SHA256) any {
	if s.IsZero() {
		return nil
	}
	return s[:]
}

// boolInt stores a bool as SQLite's 0 or 1.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
