package sqlkit

import (
	"context"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The comparison of a shadow with the catalog in use, by the rows' names
// (tenant.Rebuilding, "Identical"): each table is described once here, by
// what names its rows and which of its columns names a row of another
// table by integer id, and every statement of the comparison and of the
// activation (rebuild_activate.go) is made from that.

// catalogTable is one table of the catalog as a comparison and an
// activation see it.
type catalogTable struct {
	// name is the table's.
	name string
	// key are the columns that name a row, but a reference.
	key []string
	// ref is the column that names a row of another table by integer id,
	// "" for none; refTable and refKey are that table and the column that
	// names its rows.
	ref, refTable, refKey string
	// refInKey says the reference is part of what names a row.
	refInKey bool
	// skip are the columns compared and copied by no one: the integer id,
	// and catalog_totals' one-row key.
	skip []string
	// merged says the activation keeps the catalog's rows that the shadow
	// holds, with their ids, and changes their values; the others' rows
	// are removed and copied whole.
	merged bool
}

// catalogTables are the catalog's tables, in the order an activation
// writes them: each after the table its reference names.
var catalogTables = []catalogTable{
	{name: "subjects", key: []string{"folded"}, skip: []string{"id"}, merged: true},
	{name: "sessions", key: []string{"study_uid"}, ref: "subject_id", refTable: "subjects", refKey: "folded", skip: []string{"id"}, merged: true},
	{name: "scans", key: []string{"series_uid"}, ref: "session_id", refTable: "sessions", refKey: "study_uid", skip: []string{"id"}, merged: true},
	{name: "instances", key: []string{"sop_instance_uid"}, ref: "scan_id", refTable: "scans", refKey: "series_uid"},
	{name: "session_modalities", key: []string{"modality"}, ref: "session_id", refTable: "sessions", refKey: "study_uid", refInKey: true},
	{name: "reception_scans", key: []string{"reception_id"}, ref: "scan_id", refTable: "scans", refKey: "series_uid", refInKey: true},
	{name: "storage_totals", key: []string{"storage"}},
	{name: "catalog_totals", skip: []string{"one", "only_row"}},
}

// refExpr is the name of the row alias's reference points at, read from
// the tables of prefix p ("" for the catalog in use): a lookup by the
// referenced table's integer key, one row each.
func (c catalogTable) refExpr(p, alias string) string {
	return "(SELECT r." + c.refKey + " FROM " + p + c.refTable + " AS r WHERE r.id = " + alias + "." + c.ref + ")"
}

// from is the table of prefix p under alias a; for a table whose
// reference is part of what names a row, joined to the row it references,
// under a+"r", so the name is read by a join, not by a lookup per pair.
func (c catalogTable) from(p, a string) string {
	f := p + c.name + " AS " + a
	if c.refInKey {
		f += " JOIN " + p + c.refTable + " AS " + a + "r ON " + a + "r.id = " + a + "." + c.ref
	}
	return f
}

// keys are what names a row of alias a, as from writes it.
func (c catalogTable) keys(a string) []string {
	var out []string
	for _, k := range c.key {
		out = append(out, a+"."+k)
	}
	if c.refInKey {
		out = append(out, a+"r."+c.refKey)
	}
	return out
}

// named returns the rows of prefix p under alias a that bear the name of
// the row of alias o: a FROM clause and its condition. A table whose
// reference names its rows is reached through the referenced table, by
// that table's unique name and then by the table's own key, which begins
// with the reference (session_modalities' (session_id, modality),
// reception_scans' (reception_id, scan_id)), so each row of o costs index
// lookups, never a scan of the other side.
func (c catalogTable) named(p, a, o string) (string, string) {
	f := p + c.name + " AS " + a
	if c.refInKey {
		f = p + c.refTable + " AS " + a + "r JOIN " + p + c.name + " AS " + a + " ON " + a + "." + c.ref + " = " + a + "r.id"
	}
	ka, ko := c.keys(a), c.keys(o)
	if len(ka) == 0 {
		return f, "1 = 1"
	}
	conds := make([]string, len(ka))
	for i := range ka {
		conds[i] = ka[i] + " = " + ko[i]
	}
	return f, strings.Join(conds, " AND ")
}

// match is the condition that s (of the shadow's prefix sp) and the row
// of the catalog in use the alias live names name the same row, for a
// table whose reference is not part of what names a row.
func (c catalogTable) match(sp, live string) string {
	if len(c.key) == 0 {
		return "1 = 1"
	}
	parts := make([]string, len(c.key))
	for i, k := range c.key {
		parts[i] = "s." + k + " = " + live + "." + k
	}
	return strings.Join(parts, " AND ")
}

// values are the columns of the table that are neither skipped nor the
// reference, of all its columns.
func (c catalogTable) values(all []string) []string {
	var out []string
	for _, col := range all {
		if col != c.ref && !slices.Contains(c.skip, col) {
			out = append(out, col)
		}
	}
	return out
}

// same is the condition that s (of the shadow's prefix sp) and the row of
// the catalog in use the alias live names, which name the same row, hold
// the same values: every column but those skipped, and the name the
// reference points at.
func (c catalogTable) same(sp, live string, cols []string) string {
	var same []string
	for _, col := range c.values(cols) {
		same = append(same, "s."+col+" IS NOT DISTINCT FROM "+live+"."+col)
	}
	if c.ref != "" && !c.refInKey {
		same = append(same, c.refExpr(sp, "s")+" IS NOT DISTINCT FROM "+c.refExpr("", live))
	}
	if len(same) == 0 {
		return "1 = 1"
	}
	return strings.Join(same, " AND ")
}

// CompareCatalog implements tenant.Rebuilding.
func (k Rebuild) CompareCatalog(ctx context.Context, fence tenant.WorkFence) (tenant.CatalogComparison, error) {
	if err := fence.Validate(); err != nil {
		return tenant.CatalogComparison{}, err
	}
	r, err := k.unitJob(ctx, fence)
	if err != nil {
		return tenant.CatalogComparison{}, err
	}
	scratch := r.rebuild.Scratch
	out := tenant.CatalogComparison{Tables: map[string]tenant.TableDifference{}}
	err = k.Shadow.Read(ctx, scratch, "the shadow cannot be compared with the catalog", func(tx SendingTx) error {
		out = tenant.CatalogComparison{Tables: map[string]tenant.TableDifference{}}
		st, err := k.stateIn(ctx, tx, scratch)
		if err != nil {
			return err
		}
		if st.Job != r.job.job.ID || !st.Complete {
			return tenant.Precondition("the build of the shadow is not complete")
		}
		scope := compareScope{}
		if from := r.rebuild.FromStorage; from != "" {
			// A replica is read for its primary, whose name the shadow's
			// scans carry; another storage for itself.
			var primary string
			if _, err := tx.QueryRow(ctx, `SELECT replica_of FROM storages WHERE name = ?`, []any{from}, &primary); err != nil {
				return err
			}
			scope.storage = from
			if primary != "" {
				scope.storage = primary
			}
		}
		return k.compareIn(ctx, tx, k.Shadow.Prefix(scratch), scope, &out)
	})
	return out, err
}

// compareScope limits a comparison to the catalog of one storage: a
// shadow built from one storage alone (tenant.RebuildJob.FromStorage)
// holds that storage's scans only, and compared with the whole catalog
// would answer every scan of the tenant's other storages as removed, so a
// switch's check that the compare is empty would never pass on a tenant
// with a second primary. Its zero value compares everything.
type compareScope struct {
	// storage is the storage whose catalog is compared; "" for every one.
	storage string
}

// in returns the condition that the row of alias a of table c, in the
// tables of prefix p, is of the scope's storage, and its arguments: a scan
// on it, and an instance, a reception's row and a session's modality of
// such a scan, a session or subject that holds one. "1 = 1" when the scope
// is every storage. catalog_totals counts the whole tenant and has no row
// of one storage: it is left out of a limited comparison (included is
// false), while storage_totals' row of the storage is compared.
func (s compareScope) in(c catalogTable, p, a string) (cond string, args []any, included bool) {
	if s.storage == "" {
		return "1 = 1", nil, true
	}
	one := []any{s.storage}
	switch c.name {
	case "subjects":
		return "EXISTS (SELECT 1 FROM " + p + "sessions AS xe JOIN " + p + "scans AS xs ON xs.session_id = xe.id WHERE xe.subject_id = " +
			a + ".id AND xs.storage = ?)", one, true
	case "sessions":
		return "EXISTS (SELECT 1 FROM " + p + "scans AS xs WHERE xs.session_id = " + a + ".id AND xs.storage = ?)", one, true
	case "scans", "storage_totals":
		return a + ".storage = ?", one, true
	case "instances", "reception_scans":
		return "EXISTS (SELECT 1 FROM " + p + "scans AS xs WHERE xs.id = " + a + ".scan_id AND xs.storage = ?)", one, true
	case "session_modalities":
		// A session's modality is its own scans': kept only when a scan of
		// the session on the storage has it.
		return "EXISTS (SELECT 1 FROM " + p + "scans AS xs WHERE xs.session_id = " + a + ".session_id AND xs.storage = ? AND xs.modality = " +
			a + ".modality)", one, true
	}
	return "", nil, false
}

// mixed returns the condition that the catalog's row of alias a of table c
// also holds a scan on another storage than the scope's, and its
// arguments: a subject or session whose counts and times are of every
// storage's scans, so its values cannot equal those of a shadow of one
// storage and are not compared (its presence still is). "1 = 0" for any
// other row, or when the scope is every storage.
func (s compareScope) mixed(c catalogTable, a string) (string, []any) {
	if s.storage == "" {
		return "1 = 0", nil
	}
	switch c.name {
	case "subjects":
		return "EXISTS (SELECT 1 FROM sessions AS ye JOIN scans AS ys ON ys.session_id = ye.id WHERE ye.subject_id = " + a +
			".id AND ys.storage <> ?)", []any{s.storage}
	case "sessions":
		return "EXISTS (SELECT 1 FROM scans AS ys WHERE ys.session_id = " + a + ".id AND ys.storage <> ?)", []any{s.storage}
	}
	return "1 = 0", nil
}

// compareIn counts the differences of each table of the scope into out,
// and names the first tenant.MaxCatalogDifferences of them.
func (k Rebuild) compareIn(ctx context.Context, tx SendingTx, sp string, scope compareScope, out *tenant.CatalogComparison) error {
	for _, c := range catalogTables {
		if _, _, included := scope.in(c, "", "l"); !included {
			continue
		}
		cols, err := k.Shadow.Columns(ctx, tx, c.name)
		if err != nil {
			return err
		}
		var d tenant.TableDifference
		missing := func(from, other, alias, otherAlias string) (string, []any) {
			f, cond := c.named(other, otherAlias, alias)
			in, args, _ := scope.in(c, from, alias)
			return "FROM " + c.from(from, alias) + " WHERE " + in + " AND NOT EXISTS (SELECT 1 FROM " + f + " WHERE " + cond + ")", args
		}
		added, addedArgs := missing(sp, "", "s", "l")
		removed, removedArgs := missing("", sp, "l", "s")
		lf, lcond := c.named("", "l", "s")
		in, inArgs, _ := scope.in(c, sp, "s")
		mixed, mixedArgs := scope.mixed(c, "l")
		changed := "FROM " + c.from(sp, "s") + ", " + lf + " WHERE " + lcond + " AND " + in + " AND NOT (" + mixed + ") AND NOT (" +
			c.same(sp, "l", cols) + ")"
		changedArgs := append(append([]any{}, inArgs...), mixedArgs...)
		for _, q := range []struct {
			from, kind, alias string
			args              []any
			n                 *int64
		}{{added, "added", "s", addedArgs, &d.Added}, {removed, "removed", "l", removedArgs, &d.Removed},
			{changed, "changed", "s", changedArgs, &d.Changed}} {
			if _, err := tx.QueryRow(ctx, "SELECT count(*) "+q.from, q.args, q.n); err != nil {
				return err
			}
			if *q.n == 0 || len(out.Differences) >= tenant.MaxCatalogDifferences {
				continue
			}
			keys := c.keys(q.alias)
			if len(keys) == 0 {
				out.Differences = append(out.Differences, tenant.CatalogDifference{Table: c.name, Kind: q.kind})
				continue
			}
			sel := make([]string, len(keys))
			for i, e := range keys {
				sel[i] = "coalesce(" + e + ", '')"
			}
			limit := tenant.MaxCatalogDifferences - len(out.Differences)
			args := append(append([]any{}, q.args...), limit)
			if err := tx.Query(ctx, "SELECT "+strings.Join(sel, ", ")+" "+q.from+" LIMIT ?", args, func(row Row) error {
				vals := make([]string, len(sel))
				ptrs := make([]any, len(sel))
				for i := range vals {
					ptrs[i] = &vals[i]
				}
				err := row.Scan(ptrs...)
				key := strings.Join(vals, "/")
				if len(key) > tenant.MaxDifferenceKeyLength {
					key = key[:tenant.MaxDifferenceKeyLength]
				}
				out.Differences = append(out.Differences, tenant.CatalogDifference{Table: c.name, Kind: q.kind, Key: key})
				return err
			}); err != nil {
				return err
			}
		}
		if d != (tenant.TableDifference{}) {
			out.Tables[c.name] = d
		}
	}
	return nil
}
