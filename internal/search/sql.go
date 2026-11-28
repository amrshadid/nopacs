package search

import (
	"database/sql"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/query"
)

// The statements that write a scan's row, in the "?" placeholders every
// SQL driver of this repository's transactions takes. They hold only the
// names of the attributes' columns, which are constants of this package,
// and placeholders; every value is a parameter.

// singles are the attributes held in the scan's row, in column order.
func singles() []Attribute {
	var out []Attribute
	for _, a := range attributes {
		if !a.Multi {
			out = append(out, a)
		}
	}
	return out
}

// rowColumns returns the columns the row statements write after scan_id,
// in the order Args gives their values.
func rowColumns() []string {
	cols := []string{"source_uid"}
	for _, a := range singles() {
		cols = append(cols, a.Column())
		if a.Folded {
			cols = append(cols, a.FoldedColumn())
		}
	}
	return cols
}

// Mode says when a row the scan already has is changed.
type Mode int

// The modes of a write.
const (
	// Lowest changes the row only when it was read from a file with a
	// higher SOP Instance UID than the one given (Row.Source): files
	// archived in any order, in any number of commits, leave the row of
	// the lowest UID, which is what a rebuild reads from the metadata
	// file.
	Lowest Mode = iota
	// LowestOrSame is Lowest, and also changes a row read from the same
	// file: an edit or a replace of that file gives the row its new values,
	// and one of a file with a higher UID leaves it.
	LowestOrSame
	// Replace changes the row whatever its source: an undo gives back the
	// row an edit changed, a backfill fills a row.
	Replace
)

// UpsertSQL returns the statement that writes a scan's row: the values
// given by Args. It changes one row when it wrote, none when the mode left
// the row as it was.
func UpsertSQL(m Mode) string { return upserts[m] }

// The forms of the upsert, made once.
var upserts = [...]string{Lowest: buildUpsert(Lowest), LowestOrSame: buildUpsert(LowestOrSame), Replace: buildUpsert(Replace)}

// buildUpsert writes UpsertSQL's statement.
func buildUpsert(m Mode) string {
	cols := rowColumns()
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(cols)+1), ", ")
	set := make([]string, len(cols))
	for i, c := range cols {
		set[i] = c + " = excluded." + c
	}
	q := "INSERT INTO " + ScanTable + " (scan_id, " + strings.Join(cols, ", ") + ") VALUES (" + marks + ") ON CONFLICT (scan_id) DO UPDATE SET " +
		strings.Join(set, ", ")
	switch m {
	case Lowest:
		q += " WHERE " + ScanTable + ".source_uid > excluded.source_uid"
	case LowestOrSame:
		q += " WHERE " + ScanTable + ".source_uid >= excluded.source_uid"
	}
	return q
}

// Args returns the values UpsertSQL takes for a scan's row: a missing
// value is NULL, a text value is followed by its folded form when the
// attribute has one.
func Args(scanID int64, r Row) []any {
	r = r.Normal()
	source := r.Source
	if source == "" {
		source = NoSource
	}
	args := []any{scanID, source}
	for i, a := range attributes {
		if a.Multi {
			continue
		}
		c := r.Cells[i]
		switch {
		case a.Type == query.TypeNumber:
			if c.Number == nil {
				args = append(args, nil)
			} else {
				args = append(args, *c.Number)
			}
		case len(c.Texts) == 0:
			args = append(args, nil)
			if a.Folded {
				args = append(args, nil)
			}
		default:
			args = append(args, c.Texts[0])
			if a.Folded {
				args = append(args, query.Fold(c.Texts[0]))
			}
		}
	}
	return args
}

// Value is one row of an attribute's values table.
type Value struct {
	// Table is the values table, Column its exact column and Folded its
	// folded one.
	Table, Column, Folded string
	// Text and FoldedText are the value as written and folded.
	Text, FoldedText string
}

// Values returns the rows the values tables hold for a scan's row, one for
// each value of each multi-valued attribute.
func Values(r Row) []Value {
	r = r.Normal()
	var out []Value
	for i, a := range attributes {
		if !a.Multi {
			continue
		}
		for _, t := range r.Cells[i].Texts {
			out = append(out, Value{Table: a.Values(), Column: a.Column(), Folded: a.FoldedColumn(), Text: t, FoldedText: query.Fold(t)})
		}
	}
	return out
}

// InsertValueSQL returns the statement that writes one row of a values
// table, taking the scan's id, the value and its folded form.
func InsertValueSQL(v Value) string {
	return "INSERT INTO " + v.Table + " (scan_id, " + v.Column + ", " + v.Folded + ") VALUES (?, ?, ?)"
}

// DeleteValuesSQL returns one statement for each values table, each
// removing the rows of one scan, taking its id.
func DeleteValuesSQL() []string { return deleteValues }

// deleteValues are DeleteValuesSQL's statements, made once.
var deleteValues = func() []string {
	var out []string
	for _, a := range attributes {
		if a.Multi {
			out = append(out, "DELETE FROM "+a.Values()+" WHERE scan_id = ?")
		}
	}
	return out
}()

// SelectSQL returns the statement that reads a scan's row, taking its id;
// Dest and Finish read it.
func SelectSQL() string {
	return "SELECT " + strings.Join(rowColumns(), ", ") + " FROM " + ScanTable + " WHERE scan_id = ?"
}

// Reading is the destinations a row of SelectSQL is scanned into, and
// what it made of them.
type Reading struct {
	source  string
	texts   []sql.NullString
	folded  []sql.NullString
	numbers []sql.NullFloat64
}

// Dest returns the values to scan a row of SelectSQL into, in its column
// order.
func (g *Reading) Dest() []any {
	g.texts = make([]sql.NullString, len(attributes))
	g.folded = make([]sql.NullString, len(attributes))
	g.numbers = make([]sql.NullFloat64, len(attributes))
	dest := []any{&g.source}
	for i, a := range attributes {
		if a.Multi {
			continue
		}
		if a.Type == query.TypeNumber {
			dest = append(dest, &g.numbers[i])
			continue
		}
		dest = append(dest, &g.texts[i])
		if a.Folded {
			dest = append(dest, &g.folded[i])
		}
	}
	return dest
}

// SelectValuesSQL returns the statement that reads the values of one
// multi-valued attribute of a scan, taking its id, in no order.
func SelectValuesSQL(a Attribute) string {
	return "SELECT " + a.Column() + " FROM " + a.Values() + " WHERE scan_id = ?"
}

// Finish returns the row a scan of Dest read, with the values the caller
// read of each multi-valued attribute (by keyword).
func (g *Reading) Finish(multi map[string][]string) Row {
	r := Row{Source: g.source, Cells: make([]Cell, len(attributes))}
	for i, a := range attributes {
		switch {
		case a.Multi:
			r.Cells[i].Texts = append([]string(nil), multi[a.Keyword]...)
			slices.Sort(r.Cells[i].Texts)
		case a.Type == query.TypeNumber:
			if g.numbers[i].Valid {
				v := g.numbers[i].Float64
				r.Cells[i].Number = &v
			}
		case g.texts[i].Valid:
			r.Cells[i].Texts = []string{g.texts[i].String}
		}
	}
	return r
}
