package query

import (
	"errors"
	"regexp"
	"strconv"

	"github.com/amrshadid/go-dicom/tag"
)

// ColumnType is the type of the values a search column holds.
type ColumnType int

// The column types. A date is held as DICOM writes a DA value (YYYYMMDD), a
// time as HHMMSS and a date-time as YYYYMMDDHHMMSS, so their order as text is
// their order in time on every database.
const (
	// TypeText holds text, its folded form in a second column when one is
	// declared.
	TypeText ColumnType = iota + 1
	// TypeNumber holds a number (double precision).
	TypeNumber
	// TypeDate holds YYYYMMDD.
	TypeDate
	// TypeTime holds HHMMSS.
	TypeTime
	// TypeDateTime holds YYYYMMDDHHMMSS.
	TypeDateTime
)

// Index is the set of indexes a column has, which decides the comparisons
// a search can serve on it.
type Index uint8

// The indexes.
const (
	// IndexExact is a btree index on the exact column: equality, lists,
	// ranges and prefixes of the exact value.
	IndexExact Index = 1 << iota
	// IndexFolded is a btree index on the folded column: equality, lists
	// and prefixes of the folded value.
	IndexFolded
	// IndexTrigram is a trigram index on the folded column (pg_trgm GIN on
	// Postgres, an FTS5 table with the trigram tokenizer on SQLite):
	// contains, words, and patterns with three characters in a row.
	IndexTrigram
)

// Column is one searchable field of the search tables.
type Column struct {
	// Field is the field as Parse reads it ("SeriesDescription",
	// "session.date", "(0018,0050)"); the registry keys the column by its
	// canonical spelling (FieldString).
	Field string
	// Level is the level whose table holds the column.
	Level Level
	// Type is the type of its values.
	Type ColumnType
	// Exact is the name of the column holding the value as written.
	Exact string
	// Folded is the name of the column holding the folded text (Fold), ""
	// for none. Text only.
	Folded string
	// Index is the set of indexes it has.
	Index Index
	// Trigram is the name of the trigram structure over the folded column
	// a dialect needs to name (SQLite's FTS5 table), "" for none.
	Trigram string
	// Values is the name of the table holding the values of a
	// multi-valued attribute, "" for one held in its level's table. It has
	// a row per value that is not empty: a column named as its level
	// table's Key holding the row's key, and the columns Exact and Folded
	// holding the value. A leaf holds when one value matches, as routing
	// holds it. A person's name (PN) is always multi-valued, each value
	// held once per component group that is not empty (alphabetic,
	// ideographic, phonetic, PS3.5 6.2), so a leaf matches a name by any
	// of its groups, as routing matches it; a registry refuses a PN
	// attribute without a values table.
	Values string
}

// Table is the search table of one level.
type Table struct {
	// Level is the level whose rows it holds.
	Level Level
	// Name is the table's name.
	Name string
	// Key is the column naming the row's catalog id at its level.
	Key string
	// Row is the column a trigram structure is keyed by (SQLite's rowid);
	// Key when empty.
	Row string
	// Parents names, for each level above, the column holding the id of
	// the row's ancestor at that level.
	Parents map[Level]string
}

// Registry is the search tables and their columns: the only source of the
// identifiers a compiled query holds.
type Registry struct {
	tables map[Level]Table
	cols   map[string]Column
}

// identifier is the form of every table and column name the registry
// takes: never quoted, never from a query's text.
var identifier = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// NewRegistry checks the tables and columns and returns their registry.
// Every name must be a plain identifier (lower-case letters, digits and
// "_", at most 63 bytes, which Postgres keeps whole); every column's field
// must parse and appear once; its level must have a table; a folded column
// or a folded index is text only; a trigram index needs a folded column.
func NewRegistry(tables []Table, cols []Column) (*Registry, error) {
	r := &Registry{tables: map[Level]Table{}, cols: map[string]Column{}}
	for _, t := range tables {
		if t.Level.depth() < 0 {
			return nil, errors.New("query: a table's level is unknown")
		}
		if _, dup := r.tables[t.Level]; dup {
			return nil, errors.New("query: two tables at level " + string(t.Level))
		}
		if t.Row == "" {
			t.Row = t.Key
		}
		names := []string{t.Name, t.Key, t.Row}
		for lv, p := range t.Parents {
			if lv.depth() < 0 || lv.depth() >= t.Level.depth() {
				return nil, errors.New("query: table " + t.Name + " names a parent below or at its level")
			}
			names = append(names, p)
		}
		for _, n := range names {
			if !identifier.MatchString(n) {
				return nil, errors.New("query: " + strconv.Quote(n) + " is not a plain identifier")
			}
		}
		r.tables[t.Level] = t
	}
	for _, c := range cols {
		f, err := ParseField(c.Field)
		if err != nil {
			return nil, err
		}
		key := FieldString(f)
		if _, dup := r.cols[key]; dup {
			return nil, errors.New("query: field " + key + " is registered twice")
		}
		if _, ok := r.tables[c.Level]; !ok {
			return nil, errors.New("query: field " + key + " is at a level with no table")
		}
		if c.Type < TypeText || c.Type > TypeDateTime {
			return nil, errors.New("query: field " + key + " has no type")
		}
		names := []string{c.Exact}
		if c.Folded != "" {
			names = append(names, c.Folded)
		}
		if c.Trigram != "" {
			names = append(names, c.Trigram)
		}
		if c.Values != "" {
			names = append(names, c.Values)
		}
		for _, n := range names {
			if !identifier.MatchString(n) {
				return nil, errors.New("query: " + strconv.Quote(n) + " is not a plain identifier")
			}
		}
		if c.Type != TypeText && (c.Folded != "" || c.Index&(IndexFolded|IndexTrigram) != 0) {
			return nil, errors.New("query: field " + key + " is folded but holds no text")
		}
		if c.Index&(IndexFolded|IndexTrigram) != 0 && c.Folded == "" {
			return nil, errors.New("query: field " + key + " has a folded index but no folded column")
		}
		if c.Values != "" && c.Index&IndexTrigram != 0 {
			// A trigram structure is keyed by its table's row; the values
			// table's rows are not the level's.
			return nil, errors.New("query: field " + key + " is multi-valued and has no trigram index")
		}
		if c.Values == "" && isPersonName(f) {
			return nil, errors.New("query: field " + key + " is a person's name, matched by each component group; it needs a values table")
		}
		r.cols[key] = c
	}
	return r, nil
}

// isPersonName reports whether a field names a public attribute whose VR
// is PN by the dictionary.
func isPersonName(f Field) bool {
	if f.Catalog != "" || len(f.Path) == 0 {
		return false
	}
	last := f.Path[len(f.Path)-1]
	return last.Creator == "" && (last.Tag>>16)%2 == 0 && tag.Tag(last.Tag).GetVR() == "PN"
}

// Column returns the column of a field, and whether the registry has one.
func (r *Registry) Column(f Field) (Column, bool) {
	c, ok := r.cols[FieldString(f)]
	return c, ok
}

// Table returns the table of a level, and whether the registry has one.
func (r *Registry) Table(l Level) (Table, bool) {
	t, ok := r.tables[l]
	return t, ok
}

// ParseField reads one field as a condition writes it: a catalog field's
// name or an attribute's path.
func ParseField(text string) (Field, error) {
	if len(text) > MaxLength {
		return Field{}, fail(MaxLength, "a field is longer than "+strconv.Itoa(MaxLength)+" bytes")
	}
	p := &parser{lex: lexer{src: text}}
	f, err := p.field()
	if err != nil {
		return Field{}, err
	}
	if p.tok.kind != tokEOF {
		return Field{}, fail(p.tok.pos, "expected the end of the field")
	}
	if err := checkField(f); err != nil {
		return Field{}, err
	}
	return f, nil
}
