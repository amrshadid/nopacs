package sqlkit

import (
	"context"
	"database/sql"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/query"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The tenant's search (tenant.Searching) as SQL both drivers run: the
// condition compiled by package query for the driver's dialect over the
// registry of package search, so every identifier in the statement is the
// registry's and every value a parameter; the rows of the page read by
// their ids from the catalog's own tables, with the columns and the
// reading the catalog's lists use.

// ListSearch is the name of the search's cursors.
const ListSearch = "tenant-search"

// SearchReader is the search on a SQL database.
type SearchReader[T any] struct {
	CatalogReader[T]
	// Dialect writes what differs between the databases in a compiled
	// search.
	Dialect query.Dialect
}

// SearchAttributes implements tenant.Searching.
func (r SearchReader[T]) SearchAttributes(ctx context.Context) ([]tenant.SearchAttributeState, error) {
	states, err := rows(ctx, r.Run, "the search attributes cannot be read", CatalogRead{SQL: `SELECT field, state FROM search_attributes ORDER BY field`},
		func(row Row, s *tenant.SearchAttributeState) error {
			var state string
			err := row.Scan(&s.Field, &state)
			s.Ready = state == "ready"
			return err
		})
	if err != nil {
		return nil, err
	}
	// What each attribute's last backfill could not read: the jobs in the
	// order they were made, the later one's count replacing the earlier's.
	type filled struct {
		fields  string
		skipped int64
	}
	jobs, err := rows(ctx, r.Run, "the search backfills cannot be read", CatalogRead{SQL: `SELECT b.fields, b.skipped FROM search_backfill_jobs b
		JOIN jobs j ON j.id = b.job_id WHERE j.state IN ('done', 'partial') ORDER BY j.created, j.id`},
		func(row Row, f *filled) error { return row.Scan(&f.fields, &f.skipped) })
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		for _, f := range strings.Split(j.fields, ",") {
			for i := range states {
				if states[i].Field == f {
					states[i].ScansSkipped = j.skipped
				}
			}
		}
	}
	return states, nil
}

// notReady returns the attributes whose column is still being filled.
func (r SearchReader[T]) notReady(ctx context.Context) (map[string]bool, error) {
	states, err := r.SearchAttributes(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, s := range states {
		if !s.Ready {
			out[s.Field] = true
		}
	}
	return out, nil
}

// Hidden is the condition, in fixed text, that a row at a level was not
// archived as received, written for the table under alias: a scan with no
// instance that records no profile version, a session with no such scan
// (the catalog's own rule of the rows archived as received, read from the
// rows' as_received columns, never from the instances). A subject is
// archived under a label the archive gave, never a value read from a
// file, and is never hidden.
func Hidden(level query.Level, alias string) string {
	switch level {
	case query.LevelScan:
		return "NOT " + receivedOf(scanReceivedSQL, alias+".scan_id")
	case query.LevelSession:
		return "NOT " + receivedOf(sessionReceivedSQL, alias+".id")
	}
	return ""
}

// Search implements tenant.Searching.
func (r SearchReader[T]) Search(ctx context.Context, q tenant.SearchQuery) (tenant.SearchResult, error) {
	var out tenant.SearchResult
	if err := q.Page.Validate(); err != nil {
		return out, err
	}
	switch q.Level {
	case query.LevelSubject, query.LevelSession, query.LevelScan:
	default:
		return out, fault.New(fault.Invalid, "a search lists subjects, sessions or scans")
	}
	if q.Condition != nil {
		used := map[string]bool{}
		for _, f := range query.Fields(q.Condition) {
			used[f] = true
		}
		if q.Facet != nil {
			used[query.FieldString(*q.Facet)] = true
		}
		building, err := r.notReady(ctx)
		if err != nil {
			return out, err
		}
		for _, f := range slices.Sorted(maps.Keys(used)) {
			if building[f] {
				return out, fault.New(fault.Precondition, f+" is still being filled for the scans already archived; search it when the fill ends")
			}
		}
	}
	c := query.Compiler{Registry: search.Registry(), Dialect: r.Dialect, Now: q.Now}
	if q.Hide {
		c.Restrict = Hidden
	}
	sq, err := c.Compile(q.Condition, q.Level)
	if err != nil {
		return out, searchError(err)
	}
	if q.StudyUID != "" {
		if q.Level != query.LevelScan {
			return out, fault.New(fault.Invalid, "only a search of scans is kept to one study")
		}
		// The session is found by its unique UID; the scans' view holds its id.
		sq.Args = append(slices.Clone(sq.Args), q.StudyUID)
		sq.Where = "(" + sq.Where + ") AND " + sq.Alias + ".session_id IN (SELECT id FROM sessions WHERE study_uid = " +
			r.Dialect.Placeholder(len(sq.Args)) + ")"
	}
	reg, _ := search.Registry().Table(q.Level)
	key := sq.Alias + "." + reg.Key
	binding := r.Run.Binding(string(q.Level), query.Print(q.Condition), strconv.FormatBool(q.Hide), q.StudyUID)
	after := int64(0)
	if q.Page.Cursor != "" {
		f, err := DecodeCursor(q.Page.Cursor, ListSearch, binding, 1)
		if err != nil {
			return out, err
		}
		if after, err = DecodeCursorInt(f[0]); err != nil {
			return out, err
		}
	}
	args := slices.Clone(sq.Args)
	where := sq.Where
	if after > 0 {
		args = append(args, after)
		where = "(" + where + ") AND " + key + " > " + r.Dialect.Placeholder(len(args))
	}
	text := "SELECT " + key + " FROM " + sq.From + " WHERE " + where + " ORDER BY " + key + " LIMIT " + strconv.Itoa(q.Page.Limit+1) // #nosec G202 -- the compiler's text: registry names and placeholders; the limit a validated int.
	ids, err := rows(ctx, r.Run, "the search cannot be run", CatalogRead{SQL: text, Args: args}, func(row Row, id *int64) error { return row.Scan(id) })
	if err != nil {
		return out, err
	}
	ids, _, more := Cut(ids, q.Page.Limit)
	if more {
		out.Next = EncodeCursor(ListSearch, binding, strconv.FormatInt(ids[len(ids)-1], 10))
	}
	if err := r.load(ctx, q.Level, ids, &out); err != nil {
		return out, err
	}
	if q.Count || q.Facet != nil {
		bound := max(q.CountRows, 1)
		n, err := r.countUpTo(ctx, sq, bound)
		if err != nil {
			return out, err
		}
		if q.Count {
			out.Total, out.Counted, out.AtLeast = min(n, bound), true, n > bound
		}
		if q.Facet != nil {
			if n > bound {
				return out, fault.New(fault.Precondition, "the search matches more than "+strconv.FormatInt(bound, 10)+
					" rows, which a facet is not made of; narrow the search, or ask the tenant's admin to raise search.facet-rows")
			}
			if out.Facet, err = r.facet(ctx, q, sq); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

// searchError returns the error of a refused condition as the contract's
// Invalid, with the compiler's own words.
func searchError(err error) error {
	return fault.Wrap(fault.Invalid, err, err.Error())
}

// countUpTo counts the rows a compiled search matches, reading at most
// bound+1 of them.
func (r SearchReader[T]) countUpTo(ctx context.Context, sq query.SQL, bound int64) (int64, error) {
	text := "SELECT count(*) FROM (SELECT 1 FROM " + sq.From + " WHERE " + sq.Where + " LIMIT " + strconv.FormatInt(bound+1, 10) + ") AS counted" // #nosec G202 -- as in Search.
	var n int64
	err := r.Run.One(ctx, "the search", "the search cannot be counted", CatalogRead{SQL: text, Args: sq.Args}, func(row Row) error { return row.Scan(&n) })
	return n, err
}

// facet counts the rows of a compiled search by the values of one field.
func (r SearchReader[T]) facet(ctx context.Context, q tenant.SearchQuery, sq query.SQL) ([]tenant.FacetCount, error) {
	col, ok := search.Registry().Column(*q.Facet)
	if !ok {
		return nil, fault.New(fault.Invalid, "facet: "+query.FieldString(*q.Facet)+" is not a search attribute")
	}
	if col.Level != q.Level {
		return nil, fault.New(fault.Invalid, "facet: "+query.FieldString(*q.Facet)+" is held at level "+string(col.Level)+", not "+string(q.Level))
	}
	reg, _ := search.Registry().Table(q.Level)
	from, value, count := sq.From, sq.Alias+"."+col.Exact, "count(*)"
	if col.Values != "" {
		from += " JOIN " + col.Values + " fv ON fv." + reg.Key + " = " + sq.Alias + "." + reg.Key
		value, count = "fv."+col.Exact, "count(DISTINCT "+sq.Alias+"."+reg.Key+")"
	}
	text := "SELECT " + value + ", " + count + " FROM " + from + " WHERE (" + sq.Where + ") AND " + value + " IS NOT NULL GROUP BY " + value + // #nosec G202 -- as in Search.
		" ORDER BY " + count + " DESC, " + value + " LIMIT " + strconv.Itoa(tenant.MaxFacetValues)
	var out []tenant.FacetCount
	err := r.Run.Rows(ctx, "the facet cannot be counted", CatalogRead{SQL: text, Args: sq.Args}, func(row Row) error {
		var f tenant.FacetCount
		if col.Type == query.TypeNumber {
			var v sql.NullFloat64
			if err := row.Scan(&v, &f.Count); err != nil {
				return err
			}
			f.Value = strconv.FormatFloat(v.Float64, 'g', -1, 64)
		} else {
			var v sql.NullString
			if err := row.Scan(&v, &f.Count); err != nil {
				return err
			}
			f.Value = v.String
		}
		out = append(out, f)
		return nil
	})
	return out, err
}

// load reads the rows of the ids from the catalog's tables, in the order
// of the ids, into out.
func (r SearchReader[T]) load(ctx context.Context, level query.Level, ids []int64, out *tenant.SearchResult) error {
	if len(ids) == 0 {
		return nil
	}
	list, err := r.SQL.IDs(ids)
	if err != nil {
		return err
	}
	b := &builder{c: r.SQL}
	b.where(r.SQL.InIDs("id", "%s"), list)
	order := map[int64]int{}
	for i, id := range ids {
		order[id] = i
	}
	switch level {
	case query.LevelScan:
		got, err := rows(ctx, r.Run, "the scans cannot be read", b.one(ScanColumns, "scans"), r.scanScan)
		if err != nil {
			return err
		}
		slices.SortFunc(got, func(a, b scanRow) int { return order[a.key.ID] - order[b.key.ID] })
		for _, g := range got {
			out.Scans = append(out.Scans, g.s)
		}
	case query.LevelSession:
		got, err := rows(ctx, r.Run, "the sessions cannot be read", b.one(SessionColumns, "sessions"), r.scanSession)
		if err != nil {
			return err
		}
		if err := r.addModalities(ctx, got); err != nil {
			return err
		}
		slices.SortFunc(got, func(a, b sessionRow) int { return order[a.key.ID] - order[b.key.ID] })
		for _, g := range got {
			out.Sessions = append(out.Sessions, g.s)
		}
	default:
		got, err := rows(ctx, r.Run, "the subjects cannot be read", b.one(SubjectColumns, "subjects"), r.scanSubject)
		if err != nil {
			return err
		}
		slices.SortFunc(got, func(a, b subjectRow) int { return order[a.key.ID] - order[b.key.ID] })
		for _, g := range got {
			out.Subjects = append(out.Subjects, g.s)
		}
	}
	return nil
}
