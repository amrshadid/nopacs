package search

import (
	"strings"

	"github.com/amrshadid/nopacs/internal/query"
)

// Dialect names a database the search tables are made in.
type Dialect int

// The databases.
const (
	// Postgres is PostgreSQL with pg_trgm.
	Postgres Dialect = iota + 1
	// SQLite is SQLite with FTS5 and its trigram tokenizer.
	SQLite
)

// HiddenPrefix begins the name of the table a delete moves a search table's
// rows to, under the correction's id (the catalog's own hidden_ tables).
const HiddenPrefix = "hidden_"

// Tables returns the names of the tables that hold a scan's search values:
// the scan's row table, then the values table of each multi-valued
// attribute. A delete hides, and a purge removes, the rows of each of them
// by scan_id.
func Tables() []string {
	out := []string{ScanTable}
	for _, a := range attributes {
		if a.Multi {
			out = append(out, a.Values())
		}
	}
	return out
}

// sqlType returns the column type of an attribute's exact column.
func sqlType(d Dialect, t query.ColumnType) string {
	switch {
	case t == query.TypeNumber && d == Postgres:
		return "double precision"
	case t == query.TypeNumber:
		return "REAL"
	case d == Postgres:
		return `text COLLATE "C"`
	}
	return "TEXT"
}

// columnDefs returns the definitions of the scan row's columns after its
// key: the source and each single-valued attribute's columns.
func columnDefs(d Dialect) []string {
	src := "source_uid TEXT NOT NULL"
	if d == Postgres {
		src = `source_uid text COLLATE "C" NOT NULL`
	}
	defs := []string{src}
	for _, a := range attributes {
		if a.Multi {
			continue
		}
		defs = append(defs, a.Column()+" "+sqlType(d, a.Type))
		if a.Folded {
			defs = append(defs, a.FoldedColumn()+" "+sqlType(d, query.TypeText))
		}
	}
	return defs
}

// DDL returns the statements that make the search tables, their indexes and
// what keeps them whole, for one database: the scan's row table and the
// values table of each multi-valued attribute, a btree index on each exact
// and each folded column, the trigram index of each declared attribute, the
// view searches read, the tables a delete hides rows into, and the list of
// attributes with whether each is ready to search. The text is what the
// tenant schema's migration holds; a test makes the tables from it and
// compares them with the registry, so an attribute added to the registry
// without its migration fails there.
func DDL(d Dialect) string {
	var b strings.Builder
	w := func(format string, args ...string) {
		s := format
		for _, a := range args {
			s = strings.Replace(s, "%s", a, 1)
		}
		b.WriteString(s)
		b.WriteString("\n")
	}
	w("-- The search tables: what a search over a tenant's scans reads, kept in step with the")
	w("-- catalog by the transactions that write it. A scan's row holds one typed column for each")
	w("-- attribute a search can name (its exact value and, for text, its folded value, made in Go")
	w("-- by the function a condition's values are folded with), a btree index on each, and a")
	w("-- trigram index on the text a search may look inside. An attribute with several values")
	w("-- holds a row for each in a table of its own. Sessions and subjects are searched in the")
	w("-- catalog's own tables.")
	w("--")
	w("-- Adding an attribute means: ADD COLUMN (and its folded column), its index, a row in")
	w("-- search_attributes in state 'building', the backfill job that fills the column for the")
	w("-- scans already archived (which sets the row 'ready'), then ANALYZE.")
	w("")
	key, ints, strict, keyCol := "scan_id INTEGER NOT NULL PRIMARY KEY", "INTEGER", ") STRICT;", "scan_id INTEGER NOT NULL"
	if d == Postgres {
		key, ints, strict, keyCol = "scan_id bigint NOT NULL PRIMARY KEY", "bigint", ");", "scan_id bigint NOT NULL"
	}
	w("CREATE TABLE %s (", ScanTable)
	w("    %s,", key)
	defs := columnDefs(d)
	for i, def := range defs {
		sep := ","
		if i == len(defs)-1 {
			sep = ""
		}
		w("    %s%s", def, sep)
	}
	w("%s", strict)
	w("")
	for _, a := range attributes {
		if a.Multi {
			continue
		}
		w("CREATE INDEX %s ON %s (%s);", ScanTable+"_"+a.Column(), ScanTable, a.Column())
		if a.Folded {
			w("CREATE INDEX %s ON %s (%s);", ScanTable+"_"+a.FoldedColumn(), ScanTable, a.FoldedColumn())
		}
	}
	w("")
	for _, a := range attributes {
		if !a.Multi {
			continue
		}
		w("CREATE TABLE %s (", a.Values())
		w("    %s,", keyCol)
		w("    %s %s,", a.Column(), sqlType(d, query.TypeText))
		w("    %s %s", a.FoldedColumn(), sqlType(d, query.TypeText))
		w("%s", strict)
		w("CREATE INDEX %s ON %s (%s);", a.Values()+"_by_scan", a.Values(), "scan_id")
		w("CREATE INDEX %s ON %s (%s);", a.Values()+"_by_value", a.Values(), a.Column())
		w("CREATE INDEX %s ON %s (%s);", a.Values()+"_by_folded", a.Values(), a.FoldedColumn())
		w("")
	}
	switch d {
	case Postgres:
		w("DO $$")
		w("DECLARE ns text;")
		w("BEGIN")
		w("    CREATE EXTENSION IF NOT EXISTS pg_trgm;")
		w("    SELECT n.nspname INTO ns FROM pg_extension e JOIN pg_namespace n ON n.oid = e.extnamespace WHERE e.extname = 'pg_trgm';")
		for _, a := range attributes {
			if a.Trigram {
				w("    EXECUTE format('CREATE INDEX %s ON %s USING gin (%s %I.gin_trgm_ops)', ns);",
					a.TrigramName(), ScanTable, a.FoldedColumn())
			}
		}
		w("END $$;")
	case SQLite:
		var ins, del, upd []string
		for _, a := range attributes {
			if !a.Trigram {
				continue
			}
			t, c := a.TrigramName(), a.FoldedColumn()
			w("CREATE VIRTUAL TABLE %s USING fts5(%s, content='%s', content_rowid='scan_id', tokenize='trigram');", t, c, ScanTable)
			ins = append(ins, "    INSERT INTO "+t+"(rowid, "+c+") VALUES (new.scan_id, new."+c+");")
			del = append(del, "    INSERT INTO "+t+"("+t+", rowid, "+c+") VALUES ('delete', old.scan_id, old."+c+");")
		}
		upd = append(upd, del...)
		upd = append(upd, ins...)
		w("")
		w("-- The trigram tables are external-content tables over the scan's row: a trigger keeps")
		w("-- each in step with every write, whoever makes it.")
		w("CREATE TRIGGER %s_after_insert AFTER INSERT ON %s BEGIN", ScanTable, ScanTable)
		w("%s", strings.Join(ins, "\n"))
		w("END;")
		w("CREATE TRIGGER %s_after_delete AFTER DELETE ON %s BEGIN", ScanTable, ScanTable)
		w("%s", strings.Join(del, "\n"))
		w("END;")
		w("CREATE TRIGGER %s_after_update AFTER UPDATE ON %s BEGIN", ScanTable, ScanTable)
		w("%s", strings.Join(upd, "\n"))
		w("END;")
	}
	w("")
	var cols []string
	for _, def := range defs {
		cols = append(cols, "ss."+strings.Fields(def)[0])
	}
	w("-- What a search over scans reads: the scan's row with the session and subject the")
	w("-- catalog holds it under now, so a move or a merge changes no row of the search table.")
	w("-- Both joins are on a primary key and a search that names neither column does not")
	w("-- make them.")
	w("CREATE VIEW %s AS SELECT ss.scan_id, s.session_id AS session_id, se.subject_id AS subject_id,", ScanView)
	w("    %s", strings.Join(cols, ", "))
	w("    FROM %s AS ss LEFT JOIN scans AS s ON s.id = ss.scan_id LEFT JOIN sessions AS se ON se.id = s.session_id;", ScanTable)
	w("")
	w("CREATE INDEX sessions_by_calling_ae ON sessions (calling_ae, id);")
	w("CREATE INDEX sessions_by_called_ae ON sessions (called_ae, id);")
	w("")
	hidden := func(table string, def []string) {
		w("CREATE TABLE %s (", HiddenPrefix+table)
		if d == Postgres {
			w(`    correction_id text COLLATE "C" NOT NULL,`)
		} else {
			w("    correction_id TEXT NOT NULL,")
		}
		for i, c := range def {
			sep := ","
			if i == len(def)-1 {
				sep = ""
			}
			w("    %s%s", c, sep)
		}
		w("%s", strict)
		w("CREATE INDEX %s ON %s (correction_id, scan_id);", HiddenPrefix+table+"_by_correction", HiddenPrefix+table)
	}
	hidden(ScanTable, append([]string{"scan_id " + ints + " NOT NULL"}, defs...))
	for _, a := range attributes {
		if a.Multi {
			hidden(a.Values(), []string{"scan_id " + ints + " NOT NULL", a.Column() + " " + sqlType(d, query.TypeText),
				a.FoldedColumn() + " " + sqlType(d, query.TypeText)})
		}
	}
	w("")
	w("-- The edits a correction made to a scan's row, as the row was before, so an undo")
	w("-- gives the values back.")
	if d == Postgres {
		w("CREATE TABLE correction_search (correction_id text COLLATE \"C\" NOT NULL, series_uid text COLLATE \"C\" NOT NULL, prior text NOT NULL, PRIMARY KEY (correction_id, series_uid));")
	} else {
		w("CREATE TABLE correction_search (correction_id TEXT NOT NULL, series_uid TEXT NOT NULL, prior TEXT NOT NULL, PRIMARY KEY (correction_id, series_uid)) STRICT, WITHOUT ROWID;")
	}
	w("")
	w("-- Each attribute a search can name, and whether its column is filled for every scan: a")
	w("-- search on an attribute still being filled is refused, since it would answer from a")
	w("-- part of the scans. A tenant with no scan has every attribute ready.")
	if d == Postgres {
		w(`CREATE TABLE search_attributes (field text COLLATE "C" NOT NULL PRIMARY KEY, state text COLLATE "C" NOT NULL CHECK (state IN ('ready', 'building')));`)
	} else {
		w("CREATE TABLE search_attributes (field TEXT NOT NULL PRIMARY KEY, state TEXT NOT NULL CHECK (state IN ('ready', 'building'))) STRICT, WITHOUT ROWID;")
	}
	for _, a := range attributes {
		w("INSERT INTO search_attributes (field, state) SELECT '%s', CASE WHEN EXISTS (SELECT 1 FROM scans) THEN 'building' ELSE 'ready' END;", a.Keyword)
	}
	return b.String()
}
