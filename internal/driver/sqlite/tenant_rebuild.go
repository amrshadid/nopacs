package sqlite

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io/fs"
	"os"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's rebuilds (tenant.Rebuilding): package sqlkit's one
// implementation, with the shadow catalog in a SQLite file of its own
// beside the tenant's (its path and ShadowSuffix), attached to the
// tenant's one write connection under the name shadowSchema while a
// rebuild writes it (sqlite.org/lang_attach.html). A file of its own, and
// not tables in the tenant's file, so dropping a shadow of millions of
// rows is removing a file, and nothing of a build ever grows the tenant's
// database or its write-ahead log.

// ShadowSuffix ends the name of a rebuild's shadow file: the tenant's
// database file's name with this added.
const ShadowSuffix = ".rebuild"

// shadowSchema is the name the shadow is attached under.
const shadowSchema = "rebuild_shadow"

// shadowDB names the shadow in the messages of its errors.
const shadowDB dbName = "rebuild's shadow database"

// shadowOwnTables are the build's own tables, made with every shadow.
var shadowOwnTables = []string{
	`CREATE TABLE rebuild_state (
		one            INTEGER NOT NULL PRIMARY KEY DEFAULT 1 CHECK (one = 1),
		job_id         TEXT    NOT NULL,
		made           INTEGER NOT NULL CHECK (made IN (0, 1)),
		storage        TEXT    NOT NULL,
		after_key      TEXT    NOT NULL,
		partial_key    TEXT    NOT NULL,
		partial_uid    TEXT    NOT NULL,
		complete       INTEGER NOT NULL CHECK (complete IN (0, 1)),
		scans          INTEGER NOT NULL,
		left_out       INTEGER NOT NULL,
		left_out_bytes INTEGER NOT NULL,
		instances      INTEGER NOT NULL,
		bytes          INTEGER NOT NULL
	) STRICT`,
	`CREATE TABLE rebuild_tallies (
		tally TEXT    NOT NULL PRIMARY KEY,
		count INTEGER NOT NULL
	) STRICT, WITHOUT ROWID`,
	`INSERT INTO catalog_totals (one, subjects, sessions, scans, instances) VALUES (1, 0, 0, 0, 0)`,
	// What the search table holds of each scan the build found, as JSON, by
	// the scan's series: the activation writes it under the scan's id in
	// the catalog in use.
	`CREATE TABLE search_shadow (
		series_uid TEXT NOT NULL PRIMARY KEY,
		content    TEXT NOT NULL
	) STRICT, WITHOUT ROWID`,
}

// rebuild returns the rebuilds' implementation on t.
func (t *Tenant) rebuild() sqlkit.Rebuild { return sqlkit.Rebuild{Sending: t.sending()} }

// shadowPath is the path of the shadow's file.
func (t *Tenant) shadowPath() string { return t.path + ShadowSuffix }

// shadowHost is sqlkit.ShadowHost on a tenant's SQLite file.
type shadowHost struct{ t *Tenant }

var _ sqlkit.ShadowHost = shadowHost{}

// Prefix implements sqlkit.ShadowHost.
func (shadowHost) Prefix(string) string { return shadowSchema + "." }

// Claim implements sqlkit.ShadowHost: a SQLite tenant names no scratch
// schema, which CreateJob refuses before it asks; its shadow is a file of
// its own, marked when Create makes it.
func (shadowHost) Claim(context.Context, sqlkit.SendingTx, string, string) error { return nil }

// attached reports whether the shadow is attached to the write connection.
func (h shadowHost) attached(ctx context.Context) (bool, error) {
	var n int
	err := h.t.p.write.QueryRowContext(ctx, `SELECT count(*) FROM pragma_database_list WHERE name = ?`, shadowSchema).Scan(&n)
	return n > 0, err
}

// Open implements sqlkit.ShadowHost: the shadow's file, made private when
// it is not there, attached to the write connection unless it is.
func (h shadowHost) Open(ctx context.Context, _ string) error {
	t := h.t
	if err := t.open(); err != nil {
		return err
	}
	t.shadowMu.Lock()
	defer t.shadowMu.Unlock()
	on, err := h.attached(ctx)
	if err != nil {
		return t.fail(ctx, err, "the rebuild's shadow cannot be reached")
	}
	if on {
		return nil
	}
	if err := checkPrivate(t.shadowPath(), shadowDB); err != nil {
		return err
	}
	if err := createPrivate(t.shadowPath(), shadowDB); err != nil {
		return err
	}
	if _, err := t.p.write.ExecContext(ctx, `ATTACH DATABASE ? AS `+shadowSchema, t.shadowPath()); err != nil {
		return t.fail(ctx, err, "the rebuild's shadow cannot be attached")
	}
	return nil
}

// Tables implements sqlkit.ShadowHost.
func (h shadowHost) Tables(ctx context.Context, scratch string) ([]string, error) {
	if err := h.Open(ctx, scratch); err != nil {
		return nil, err
	}
	// SQLite's own tables (sqlite_sequence, which AUTOINCREMENT makes) are
	// not the shadow's.
	rows, err := h.t.p.write.QueryContext(ctx, `SELECT name FROM `+shadowSchema+`.sqlite_master WHERE type = 'table'
		AND name NOT LIKE 'sqlite\_%' ESCAPE '\' ORDER BY name`)
	if err != nil {
		return nil, h.t.fail(ctx, err, "the rebuild's shadow cannot be read")
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, h.t.fail(ctx, err, "the rebuild's shadow cannot be read")
		}
		out = append(out, name)
	}
	return out, h.t.fail(ctx, rows.Err(), "the rebuild's shadow cannot be read")
}

// detach detaches the shadow from the write connection when it is
// attached; the caller holds shadowMu.
func (h shadowHost) detach(ctx context.Context) error {
	on, err := h.attached(ctx)
	if err == nil && on {
		_, err = h.t.p.write.ExecContext(ctx, `DETACH DATABASE `+shadowSchema)
	}
	return h.t.fail(ctx, err, "the rebuild's shadow cannot be detached")
}

// remove removes the shadow's file and those SQLite keeps beside it; the
// caller holds shadowMu and has detached it.
func (h shadowHost) remove() error {
	for _, suffix := range []string{"", "-journal", "-wal", "-shm"} {
		if err := os.Remove(h.t.shadowPath() + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fault.New(fault.Unavailable, "the rebuild's shadow file cannot be removed")
		}
	}
	return nil
}

// holder reads the job the shadow's file names, through a connection of
// its own: "" when there is no file, or it holds no mark (a file a crash
// left before its first transaction ended); the caller holds shadowMu.
func (h shadowHost) holder(ctx context.Context) (string, error) {
	if _, err := os.Stat(h.t.shadowPath()); errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}
	db, err := sql.Open("sqlite", "file:"+h.t.shadowPath()+"?mode=ro&_pragma=busy_timeout(10000)")
	if err != nil {
		return "", fault.Wrap(fault.Internal, err, "the SQLite driver cannot be set up")
	}
	defer func() { _ = db.Close() }()
	var marked int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'rebuild_state'`).Scan(&marked); err != nil {
		return "", classify(err, shadowDB, "the rebuild's shadow cannot be read")
	}
	if marked == 0 {
		return "", nil
	}
	var job string
	err = db.QueryRowContext(ctx, `SELECT job_id FROM rebuild_state`).Scan(&job)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return job, classify(err, shadowDB, "the rebuild's shadow cannot be read")
}

// Drop implements sqlkit.ShadowHost: the file is removed when it names the
// job, or no job; one that names another job is that job's, made since (a
// tenant has one shadow file, and a job that ended without its worker is
// dropped later, when another may have made the file again), and is left.
func (h shadowHost) Drop(ctx context.Context, _, job string) error {
	if err := h.t.open(); err != nil {
		return err
	}
	h.t.shadowMu.Lock()
	defer h.t.shadowMu.Unlock()
	holder, err := h.holder(ctx)
	if err != nil {
		return err
	}
	if holder != "" && holder != job {
		return nil
	}
	if err := h.detach(ctx); err != nil {
		return err
	}
	return h.remove()
}

// Create implements sqlkit.ShadowHost: the shadow's file made anew, and in
// it every table and index of the catalog as the tenant's own file's
// schema states them (sqlite_master), so the shadow has the shape of the
// catalog in use whatever migrations made it, then the build's own tables
// and the row of rebuild_state that names the job, in one transaction, so
// a file whose tables are made always says whose it is. It is written
// through a connection of its own, before it is attached; the fence is
// checked in a write transaction of the tenant's just before, under the
// lock of the shadow, since a transaction cannot span the two files'
// connections (the tenant runs one worker of its rebuild at a time).
func (h shadowHost) Create(ctx context.Context, _, job string, fenced func(sqlkit.SendingTx) error) error {
	t := h.t
	if err := t.open(); err != nil {
		return err
	}
	ddl, err := h.catalogSchema(ctx)
	if err != nil {
		return err
	}
	t.shadowMu.Lock()
	defer t.shadowMu.Unlock()
	if err := t.rebuild().Host.Write(ctx, "the rebuild's shadow cannot be made", fenced); err != nil {
		return err
	}
	if err := h.detach(ctx); err != nil {
		return err
	}
	if err := h.remove(); err != nil {
		return err
	}
	if err := createPrivate(t.shadowPath(), shadowDB); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", t.shadowPath())
	if err != nil {
		return fault.Wrap(fault.Internal, err, "the SQLite driver cannot be set up")
	}
	defer func() { _ = db.Close() }()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return classify(err, shadowDB, "the rebuild's shadow cannot be made")
	}
	defer func() { _ = tx.Rollback() }() // After the commit, a no-op.
	for _, stmt := range append(ddl, shadowOwnTables...) {
		if _, err := tx.ExecContext(ctx, stmt); err != nil {
			return classify(err, shadowDB, "the rebuild's shadow cannot be made")
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rebuild_state (job_id, made, storage, after_key, partial_key, partial_uid, complete, scans,
		left_out, left_out_bytes, instances, bytes) VALUES (?, 1, '', '', '', '', 0, 0, 0, 0, 0, 0)`, job); err != nil {
		return classify(err, shadowDB, "the rebuild's shadow cannot be made")
	}
	if err := tx.Commit(); err != nil {
		return classify(err, shadowDB, "the rebuild's shadow cannot be made")
	}
	return nil
}

// catalogSchema returns the statements that made the catalog's tables and
// their indexes in the tenant's file, tables first.
func (h shadowHost) catalogSchema(ctx context.Context) ([]string, error) {
	var out []string
	err := h.t.readTx(ctx, "the catalog's schema cannot be read", func(tx *sql.Tx) error {
		out = nil
		for _, table := range tenant.CatalogTables() {
			rows, err := tx.QueryContext(ctx, `SELECT sql FROM sqlite_master WHERE tbl_name = ? AND sql IS NOT NULL
				ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, name`, table)
			if err != nil {
				return err
			}
			for rows.Next() {
				var stmt string
				if err := rows.Scan(&stmt); err != nil {
					_ = rows.Close()
					return err
				}
				out = append(out, stmt)
			}
			if err := rows.Close(); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// Read implements sqlkit.ShadowHost: a read connection of its own, the
// shadow attached to it for the transaction and detached after; a
// connection that cannot be detached is let go rather than given back to
// the pool with the shadow on it.
func (h shadowHost) Read(ctx context.Context, _, message string, fn func(sqlkit.SendingTx) error) error {
	t := h.t
	if _, err := t.ready(ctx); err != nil {
		return err
	}
	conn, err := t.p.read.Conn(ctx)
	if err != nil {
		return t.fail(ctx, err, message)
	}
	defer func() { _ = conn.Close() }()
	if _, err := conn.ExecContext(ctx, `ATTACH DATABASE ? AS `+shadowSchema, t.shadowPath()); err != nil {
		return t.fail(ctx, err, message)
	}
	err = func() error {
		tx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }() // A read changes nothing.
		return fn(sendingTx{tx: tx})
	}()
	if _, derr := conn.ExecContext(context.WithoutCancel(ctx), `DETACH DATABASE `+shadowSchema); derr != nil {
		_ = conn.Raw(func(any) error { return driver.ErrBadConn })
	}
	return t.fail(ctx, err, message)
}

// Columns implements sqlkit.ShadowHost.
func (shadowHost) Columns(ctx context.Context, tx sqlkit.SendingTx, table string) ([]string, error) {
	var out []string
	err := tx.Query(ctx, `SELECT name FROM pragma_table_info(?, 'main') ORDER BY cid`, []any{table}, func(r sqlkit.Row) error {
		var name string
		err := r.Scan(&name)
		out = append(out, name)
		return err
	})
	if err == nil && len(out) == 0 {
		err = fault.New(fault.Internal, "the catalog has no table "+table)
	}
	return out, err
}

// StartRebuildUnit implements tenant.Rebuilding.
func (t *Tenant) StartRebuildUnit(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	return t.rebuild().StartRebuildUnit(ctx, fence, now)
}

// PrepareShadow implements tenant.Rebuilding.
func (t *Tenant) PrepareShadow(ctx context.Context, fence tenant.WorkFence) (tenant.RebuildProgress, error) {
	return t.rebuild().PrepareShadow(ctx, fence)
}

// RebuildScan implements tenant.Rebuilding.
func (t *Tenant) RebuildScan(ctx context.Context, w tenant.RebuildScanWrite) (tenant.RebuildProgress, error) {
	return t.rebuild().RebuildScan(ctx, w)
}

// FinishRebuildStorage implements tenant.Rebuilding.
func (t *Tenant) FinishRebuildStorage(ctx context.Context, fence tenant.WorkFence, storage, next string,
	tallies map[string]int64) (tenant.RebuildProgress, error) {
	return t.rebuild().FinishRebuildStorage(ctx, fence, storage, next, tallies)
}

// CompareCatalog implements tenant.Rebuilding.
func (t *Tenant) CompareCatalog(ctx context.Context, fence tenant.WorkFence) (tenant.CatalogComparison, error) {
	return t.rebuild().CompareCatalog(ctx, fence)
}

// EndRebuild implements tenant.Rebuilding.
func (t *Tenant) EndRebuild(ctx context.Context, fence tenant.WorkFence, cmp tenant.CatalogComparison, now time.Time) (tenant.Job, error) {
	return t.rebuild().EndRebuild(ctx, fence, cmp, now)
}

// ShadowHolder implements tenant.Rebuilding.
func (t *Tenant) ShadowHolder(ctx context.Context, fence tenant.WorkFence, uids []string) (string, error) {
	return t.rebuild().ShadowHolder(ctx, fence, uids)
}

// FailRebuild implements tenant.Rebuilding.
func (t *Tenant) FailRebuild(ctx context.Context, fence tenant.WorkFence, reason string, now time.Time) (tenant.Job, error) {
	return t.rebuild().FailRebuild(ctx, fence, reason, now)
}

// RebuildDifferences implements tenant.Rebuilding.
func (t *Tenant) RebuildDifferences(ctx context.Context, jobID string) ([]tenant.CatalogDifference, error) {
	return t.rebuild().RebuildDifferences(ctx, jobID)
}

// ActivateCatalog implements tenant.Rebuilding.
func (t *Tenant) ActivateCatalog(ctx context.Context, fence tenant.WorkFence, now time.Time) (tenant.Job, error) {
	return t.rebuild().ActivateCatalog(ctx, fence, now)
}

// DropShadow implements tenant.Rebuilding.
func (t *Tenant) DropShadow(ctx context.Context, jobID string) error {
	return t.rebuild().DropShadow(ctx, jobID)
}

// EventHistory implements tenant.Rebuilding.
func (t *Tenant) EventHistory(ctx context.Context) (string, error) {
	return t.rebuild().EventHistory(ctx)
}

// ShadowsLeft implements tenant.Rebuilding.
func (t *Tenant) ShadowsLeft(ctx context.Context) ([]string, error) {
	return t.rebuild().ShadowsLeft(ctx)
}

// RebuildReplacing implements tenant.Rebuilding.
func (t *Tenant) RebuildReplacing(ctx context.Context) (bool, error) {
	return t.rebuild().RebuildReplacing(ctx)
}

// RebuildRunning implements tenant.Rebuilding.
func (t *Tenant) RebuildRunning(ctx context.Context) (bool, error) {
	return t.rebuild().RebuildRunning(ctx)
}
