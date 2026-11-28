package postgres

import (
	"context"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's rebuilds (tenant.Rebuilding): package sqlkit's one
// implementation, with the shadow catalog in the scratch schema the job
// names, a schema of the tenant's own database, so one transaction reads
// the shadow and writes the catalog in use (the activation's INSERT ...
// SELECT) and every reader keeps its snapshot of the old catalog until
// that transaction commits. The shadow's tables are made LIKE the
// catalog's own (INCLUDING ALL: columns, defaults, checks, indexes and
// identities), so they have the shape of the catalog in use whatever
// migrations made it.

// rebuildStateTable is the build's state and the scratch schema's mark of
// whose it is, made by CreateJob (Claim) with made false; %s is the
// schema's prefix.
const rebuildStateTable = `CREATE TABLE %srebuild_state (
	only_row       boolean     NOT NULL DEFAULT true PRIMARY KEY CHECK (only_row),
	job_id         text        COLLATE "C" NOT NULL,
	made           boolean     NOT NULL,
	storage        text        COLLATE "C" NOT NULL,
	after_key      text        COLLATE "C" NOT NULL,
	partial_key    text        COLLATE "C" NOT NULL,
	partial_uid    text        COLLATE "C" NOT NULL,
	complete       boolean     NOT NULL,
	scans          bigint      NOT NULL,
	left_out       bigint      NOT NULL,
	left_out_bytes bigint      NOT NULL,
	instances      bigint      NOT NULL,
	bytes          bigint      NOT NULL
)`

// shadowOwnTables are the build's own tables but its state, made with the
// shadow's catalog tables; %s is the schema's prefix.
var shadowOwnTables = []string{
	`CREATE TABLE %srebuild_tallies (
		tally text   COLLATE "C" NOT NULL PRIMARY KEY,
		count bigint NOT NULL
	)`,
	`INSERT INTO %scatalog_totals (only_row, subjects, sessions, scans, instances) VALUES (true, 0, 0, 0, 0)`,
	// What the search table holds of each scan the build found, as JSON, by
	// the scan's series: the activation writes it under the scan's id in
	// the catalog in use.
	`CREATE TABLE %ssearch_shadow (
		series_uid text COLLATE "C" NOT NULL PRIMARY KEY,
		content    text NOT NULL
	)`,
}

// rebuild returns the rebuilds' implementation on t.
func (t *Tenant) rebuild() sqlkit.Rebuild { return sqlkit.Rebuild{Sending: t.sending()} }

// shadowHost is sqlkit.ShadowHost on a tenant's Postgres database.
type shadowHost struct{ t *Tenant }

var _ sqlkit.ShadowHost = shadowHost{}

// Prefix implements sqlkit.ShadowHost. The scratch schema's name is a
// Postgres identifier that needs no quoting (tenant.ValidateScratch), and
// is quoted all the same.
func (shadowHost) Prefix(scratch string) string { return `"` + scratch + `".` }

// Claim implements sqlkit.ShadowHost: the schema exists, holds no table,
// is neither the tenant's own schema nor on its connection's search path,
// so the shadow can never be taken for the catalog in use, and lets no
// other role in (scratchShared), since the shadow is the whole catalog's
// UIDs and labels. Then the job's mark is made in it, in CreateJob's
// transaction (DDL is transactional in Postgres), so from the job's first
// moment the schema says whose it is, and a shadow table is never made,
// nor dropped, for a job that does not hold the mark.
func (h shadowHost) Claim(ctx context.Context, tx sqlkit.SendingTx, scratch, job string) error {
	if err := tenant.ValidateScratch(scratch); err != nil {
		return err
	}
	var there, own, onPath bool
	var tables int64
	if _, err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = ?), ? = current_schema(),
		? = ANY (current_schemas(false)) OR ? = ANY (string_to_array(replace(current_setting('search_path'), ' ', ''), ',')),
		(SELECT count(*) FROM pg_tables WHERE schemaname = ?)`, []any{scratch, scratch, scratch, scratch, scratch},
		&there, &own, &onPath, &tables); err != nil {
		return err
	}
	switch {
	case !there:
		return tenant.Precondition("the scratch schema does not exist; the database's administrator makes it")
	case own:
		return tenant.Precondition("the scratch schema is the tenant's own schema")
	case onPath:
		return tenant.Precondition("the scratch schema is on the tenant's search path")
	case tables > 0:
		return tenant.Precondition("the scratch schema holds tables; a rebuild builds in an empty one")
	}
	shared, err := scratchShared(ctx, tx, scratch)
	if err != nil {
		return err
	}
	if shared {
		return tenant.Precondition(sharedMessage)
	}
	p := h.Prefix(scratch)
	if _, err := tx.Exec(ctx, strings.Replace(rebuildStateTable, "%s", p, 1)); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+p+`rebuild_state (job_id, made, storage, after_key, partial_key, partial_uid, complete, scans,
		left_out, left_out_bytes, instances, bytes) VALUES (?, false, '', '', '', '', false, 0, 0, 0, 0, 0)`, job); err != nil {
		return err
	}
	return revokeOthers(ctx, tx, scratch)
}

// sharedMessage is the refusal of a scratch schema another role may read.
const sharedMessage = "the scratch schema grants access to a role other than the tenant's, or gives its tables default privileges; " +
	"a rebuild builds where only the tenant reads"

// scratchShared reports whether the schema grants a privilege to a role
// other than its owner and the tenant's own role (PUBLIC included), or
// holds default privileges of its own (pg_default_acl), which would give
// another role the shadow's tables as they are made.
func scratchShared(ctx context.Context, tx sqlkit.SendingTx, scratch string) (bool, error) {
	var shared bool
	_, err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace AS n, aclexplode(n.nspacl) AS a WHERE n.nspname = ?
			AND a.grantee <> n.nspowner AND a.grantee <> (SELECT oid FROM pg_roles WHERE rolname = current_user))
		OR EXISTS (SELECT 1 FROM pg_default_acl AS d JOIN pg_namespace AS n ON n.oid = d.defaclnamespace WHERE n.nspname = ?)`,
		[]any{scratch, scratch}, &shared)
	return shared, err
}

// revokeOthers takes back every privilege on the schema's tables and
// sequences held by a role other than their owner: what default
// privileges of the whole database (pg_default_acl of no schema) gave them
// as they were made.
func revokeOthers(ctx context.Context, tx sqlkit.SendingTx, scratch string) error {
	var stmts []string
	if err := tx.Query(ctx, `SELECT DISTINCT CASE WHEN c.relkind = 'S' THEN 'SEQUENCE ' ELSE 'TABLE ' END || quote_ident(c.relname),
			CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE quote_ident(r.rolname) END
		FROM pg_class AS c JOIN pg_namespace AS n ON n.oid = c.relnamespace CROSS JOIN LATERAL aclexplode(c.relacl) AS a
			LEFT JOIN pg_roles AS r ON r.oid = a.grantee
		WHERE n.nspname = ? AND a.grantee <> c.relowner`, []any{scratch}, func(row sqlkit.Row) error {
		var on, role string
		err := row.Scan(&on, &role)
		kind, name, _ := strings.Cut(on, " ")
		stmts = append(stmts, `REVOKE ALL ON `+kind+` "`+scratch+`".`+name+` FROM `+role)
		return err
	}); err != nil {
		return err
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

// mark reads the scratch schema's mark in the transaction, locking it:
// the job it names and whether its tables are made; found is false when
// the schema holds no mark.
func (h shadowHost) mark(ctx context.Context, tx sqlkit.SendingTx, scratch string) (job string, made, found bool, err error) {
	if _, err = tx.QueryRow(ctx, `SELECT to_regclass(?) IS NOT NULL`, []any{h.Prefix(scratch) + "rebuild_state"}, &found); err != nil || !found {
		return "", false, false, err
	}
	found, err = tx.QueryRow(ctx, `SELECT job_id, made FROM `+h.Prefix(scratch)+`rebuild_state FOR UPDATE`, nil, &job, &made)
	return job, made, found, err
}

// Open implements sqlkit.ShadowHost: the schema must be there.
func (h shadowHost) Open(ctx context.Context, scratch string) error {
	return h.t.rebuild().Host.Read(ctx, "the scratch schema cannot be read", func(tx sqlkit.SendingTx) error {
		var there bool
		if _, err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = ?)`, []any{scratch}, &there); err != nil {
			return err
		}
		if !there {
			return tenant.Precondition("the scratch schema does not exist")
		}
		return nil
	})
}

// Tables implements sqlkit.ShadowHost.
func (h shadowHost) Tables(ctx context.Context, scratch string) ([]string, error) {
	var out []string
	err := h.t.rebuild().Host.Read(ctx, "the scratch schema cannot be read", func(tx sqlkit.SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT tablename FROM pg_tables WHERE schemaname = ? ORDER BY tablename`, []any{scratch}, func(r sqlkit.Row) error {
			var name string
			err := r.Scan(&name)
			out = append(out, name)
			return err
		})
	})
	return out, err
}

// Drop implements sqlkit.ShadowHost: the shadow's tables, by their names,
// never the schema, which is the operator's, and only when the schema's
// mark names the job: its catalog tables when it made them, its mark
// always. A table of the same name another job, or anyone, made in the
// schema is never the job's to drop.
func (h shadowHost) Drop(ctx context.Context, scratch, job string) error {
	if err := tenant.ValidateScratch(scratch); err != nil {
		return err
	}
	return h.t.rebuild().Host.Write(ctx, "the rebuild's shadow cannot be dropped", func(tx sqlkit.SendingTx) error {
		var there bool
		if _, err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = ?)`, []any{scratch}, &there); err != nil || !there {
			return err
		}
		holder, made, found, err := h.mark(ctx, tx, scratch)
		if err != nil || !found || holder != job {
			return err
		}
		names := []string{"rebuild_state"}
		if made {
			names = sqlkit.ShadowTables()
		}
		for i, n := range names {
			names[i] = h.Prefix(scratch) + n
		}
		_, err = tx.Exec(ctx, `DROP TABLE IF EXISTS `+strings.Join(names, ", "))
		return err
	})
}

// Create implements sqlkit.ShadowHost: the shadow's tables made in one
// transaction with the fence's check, beside the mark CreateJob left. The
// schema must still hold the job's mark and nothing else, and let no other
// role in; otherwise the refusal ends the job, since no later attempt
// would find it otherwise. The tables are made private to the tenant's
// role (revokeOthers) whatever default privileges the database has.
func (h shadowHost) Create(ctx context.Context, scratch, job string, fenced func(sqlkit.SendingTx) error) error {
	if err := tenant.ValidateScratch(scratch); err != nil {
		return err
	}
	p := h.Prefix(scratch)
	return h.t.rebuild().Host.Write(ctx, "the rebuild's shadow cannot be made", func(tx sqlkit.SendingTx) error {
		if err := fenced(tx); err != nil {
			return err
		}
		holder, made, found, err := h.mark(ctx, tx, scratch)
		switch {
		case err != nil:
			return err
		case !found:
			return tenant.RebuildRefused("the scratch schema no longer holds this rebuild's mark")
		case holder != job:
			return tenant.RebuildRefused("the scratch schema holds another rebuild's shadow")
		case made:
			return nil
		}
		var others int64
		if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM pg_class AS c JOIN pg_namespace AS n ON n.oid = c.relnamespace
			WHERE n.nspname = ? AND c.relkind IN ('r', 'p', 'v', 'm', 'f') AND c.relname <> 'rebuild_state'`, []any{scratch}, &others); err != nil {
			return err
		}
		if others > 0 {
			return tenant.RebuildRefused("the scratch schema holds tables that are not this rebuild's shadow")
		}
		shared, err := scratchShared(ctx, tx, scratch)
		if err != nil {
			return err
		}
		if shared {
			return tenant.RebuildRefused(sharedMessage)
		}
		for _, table := range tenant.CatalogTables() {
			if _, err := tx.Exec(ctx, `CREATE TABLE `+p+table+` (LIKE `+table+` INCLUDING ALL)`); err != nil {
				return err
			}
		}
		for _, stmt := range shadowOwnTables {
			if _, err := tx.Exec(ctx, strings.Replace(stmt, "%s", p, 1)); err != nil {
				return err
			}
		}
		if err := revokeOthers(ctx, tx, scratch); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE `+p+`rebuild_state SET made = true, storage = '', after_key = '', partial_key = '', partial_uid = '',
			complete = false, scans = 0, left_out = 0, left_out_bytes = 0, instances = 0, bytes = 0`)
		return err
	})
}

// Read implements sqlkit.ShadowHost: the tenant's own read transaction,
// the scratch schema being in the same database.
func (h shadowHost) Read(ctx context.Context, _, message string, fn func(sqlkit.SendingTx) error) error {
	return h.t.rebuild().Host.Read(ctx, message, fn)
}

// Columns implements sqlkit.ShadowHost.
func (shadowHost) Columns(ctx context.Context, tx sqlkit.SendingTx, table string) ([]string, error) {
	var out []string
	err := tx.Query(ctx, `SELECT column_name FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ?
		ORDER BY ordinal_position`, []any{table}, func(r sqlkit.Row) error {
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
