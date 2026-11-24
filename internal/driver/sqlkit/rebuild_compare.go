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
		return k.compareIn(ctx, tx, k.Shadow.Prefix(scratch), &out)
	})
	return out, err
}

// compareIn counts the differences of each table into out, and names the
// first tenant.MaxCatalogDifferences of them.
func (k Rebuild) compareIn(ctx context.Context, tx SendingTx, sp string, out *tenant.CatalogComparison) error {
	for _, c := range catalogTables {
		cols, err := k.Shadow.Columns(ctx, tx, c.name)
		if err != nil {
			return err
		}
		var d tenant.TableDifference
		missing := func(from, other, alias, otherAlias string) string {
			f, cond := c.named(other, otherAlias, alias)
			return "FROM " + c.from(from, alias) + " WHERE NOT EXISTS (SELECT 1 FROM " + f + " WHERE " + cond + ")"
		}
		added, removed := missing(sp, "", "s", "l"), missing("", sp, "l", "s")
		lf, lcond := c.named("", "l", "s")
		changed := "FROM " + c.from(sp, "s") + ", " + lf + " WHERE " + lcond + " AND NOT (" + c.same(sp, "l", cols) + ")"
		for _, q := range []struct {
			from, kind, alias string
			n                 *int64
		}{{added, "added", "s", &d.Added}, {removed, "removed", "l", &d.Removed}, {changed, "changed", "s", &d.Changed}} {
			if _, err := tx.QueryRow(ctx, "SELECT count(*) "+q.from, nil, q.n); err != nil {
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
			if err := tx.Query(ctx, "SELECT "+strings.Join(sel, ", ")+" "+q.from+" LIMIT ?", []any{limit}, func(row Row) error {
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
