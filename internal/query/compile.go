package query

import (
	"errors"
	"strings"
	"time"
)

// MinContains is the shortest run of text a trigram index serves: it holds
// runs of three characters, so a shorter term is either a full read of the
// index (pg_trgm) or, worse, a wrong empty answer (an FTS5 trigram MATCH of
// fewer than three characters returns no rows without an error). Each
// dialect says what counts toward the run (TrigramServes).
const MinContains = 3

// The bounds of a compiled search: the most bytes of SQL and the most values
// one makes. A condition within the grammar's bounds can still make more
// ("every" of "every" repeats its child at each level); one that would is
// refused as invalid before any SQL runs.
const (
	MaxSQLBytes = 64 << 10
	MaxParams   = 999
)

// Dialect is what differs between the databases a search compiles for:
// Postgres and SQLite, which a database driver passes to its Compiler.
// Everything else the compiler writes is SQL both read alike.
type Dialect interface {
	// Placeholder returns the text of the n-th bound parameter, from 1.
	Placeholder(n int) string
	// TextMatch returns the condition for one text match, binding each
	// value it needs through bind, which returns the value's placeholder.
	// It writes only the names m gives, the placeholders bind returns and
	// fixed text; never m.Text itself.
	TextMatch(m TextMatch, bind func(any) string) string
	// TrigramServes reports whether the database's trigram index serves a
	// run of literal text: a contains term, a word of words, or one run
	// of a pattern between its wildcards.
	TrigramServes(text string) bool
}

// TextMatch is a text match a dialect writes.
type TextMatch struct {
	// Op is OpPrefix (the text starts with Text, which is not empty),
	// OpLike (Text is a pattern of "*" and "?" that is not a prefix; on a
	// folded column a trigram index serves it, on an exact one it only
	// rechecks rows the folded match found) or OpContains (the text holds
	// Text; a trigram index serves it).
	Op Op
	// Column is the column compared, qualified by its table's alias.
	Column string
	// Name is the column's own name, unqualified.
	Name string
	// Folded says the column holds folded text, and Text is folded.
	Folded bool
	// Text is the prefix, the pattern or the term.
	Text string
	// Row is the row column a trigram structure is keyed by, qualified;
	// empty for a column of a values table, which has no trigram index.
	Row string
	// Trigram is the name of the column's trigram structure, "" for none.
	Trigram string
}

// NotServedError is a search no index serves. Field names the attribute to
// index, and Need the kind of index; Field is empty when the condition
// has nothing an index could serve (a "not" alone).
type NotServedError struct {
	// Field is the field, as FieldString writes it.
	Field string
	// Need says what would serve it.
	Need string
}

// Error returns the field and what would serve it.
func (e *NotServedError) Error() string {
	if e.Field == "" {
		return "query: no index serves the search: " + e.Need
	}
	return "query: no index serves the search on " + e.Field + ": " + e.Need
}

// ErrUnknownField is a field the registry does not have.
var ErrUnknownField = errors.New("query: the field is not searchable")

// SQL is a compiled search: the table and alias to select from, the
// condition, and its parameters in placeholder order.
type SQL struct {
	// From is the table and its alias: "scan_search t0".
	From string
	// Alias is the alias of the table in From.
	Alias string
	// Where is the condition.
	Where string
	// Args are the bound values.
	Args []any
}

// Compiler compiles conditions to SQL.
type Compiler struct {
	// Registry holds the tables and columns.
	Registry *Registry
	// Dialect writes what differs between databases.
	Dialect Dialect
	// Now is when the search runs: relative dates count back from its day.
	Now time.Time
	// Restrict, when not nil, returns a condition of fixed text, with no
	// parameter, that every row of a level must meet to be seen by the
	// search, written on the table under alias: it is added to the search's
	// own rows and to the rows of each "has" and "every", so a row it hides
	// takes no part in what matches. It is how a caller that may not learn
	// some rows' values is kept from learning them by what a condition
	// matches. "" for no restriction at that level.
	Restrict func(level Level, alias string) string
}

// restricted returns where with the restriction of the level's rows under
// alias added.
func (c Compiler) restricted(where string, level Level, alias string) string {
	if c.Restrict == nil {
		return where
	}
	if r := c.Restrict(level, alias); r != "" {
		return "(" + where + ") AND " + r
	}
	return where
}

// Compile compiles a condition into the WHERE clause of a search over the
// rows of one level. The condition is checked (Check); every field must be
// in the registry at the level it is compared at (a field of another
// level is reached through "has"); its values must be of the column's
// type. A condition no index serves is refused with a *NotServedError:
// a leaf is served by an index of the kind its operator needs, an "and" by
// one served child, an "or" by every child, a "has" by its child; a "not"
// alone never, "absent" of a multi-valued attribute never (it holds for
// the rows a values table lacks), and "matches" never (it is for routing).
// A nil condition is every row ("1 = 1"): a search without a condition
// pages by id.
//
// The text holds only fixed fragments, registry names and placeholders;
// every value is in Args. A leaf is false where its column is NULL (the
// attribute absent), and "not" negates the leaf's truth, never SQL's NULL:
// "not Modality == "CT"" holds for a row without a modality, as it does in
// routing. A leaf on a multi-valued attribute holds when one of its values
// does, as in routing: the row's key is in the values table's rows that
// match. A "has" is the row's key, or its parent's, in the keys of the
// other level's rows that match (IN, which a database answers from the
// other level's index, never row by row of this one).
func (c Compiler) Compile(n Node, level Level) (SQL, error) {
	t, ok := c.Registry.Table(level)
	if !ok {
		return SQL{}, errors.New("query: no search table at level " + string(level))
	}
	out := SQL{From: t.Name + " t0", Alias: "t0"}
	if n == nil {
		out.Where = c.restricted("1 = 1", level, "t0")
		return out, nil
	}
	if err := Check(n); err != nil {
		return SQL{}, err
	}
	served, why, err := c.served(n, level)
	if err != nil {
		return SQL{}, err
	}
	if !served {
		return SQL{}, why
	}
	w := &writer{c: c, args: &out.Args}
	where, err := w.node(n, level, "t0", 0)
	if err != nil {
		return SQL{}, err
	}
	out.Where = c.restricted(where, level, "t0")
	if err := w.checkSize(len(out.Where)); err != nil {
		return SQL{}, err
	}
	return out, nil
}

// served reports whether an index serves n at level, why not when not,
// and an error for a field or value the search cannot compare.
func (c Compiler) served(n Node, level Level) (bool, *NotServedError, error) {
	switch n := n.(type) {
	case *Leaf:
		col, err := c.column(n, level)
		if err != nil {
			return false, nil, err
		}
		why := leafServed(n, col, c.Dialect)
		return why == nil, why, nil
	case *And:
		var first *NotServedError
		one := false
		for _, ch := range n.Children {
			ok, why, err := c.served(ch, level)
			if err != nil {
				return false, nil, err
			}
			one = one || ok
			if !ok && first == nil {
				first = why
			}
		}
		return one, first, nil
	case *Or:
		for _, ch := range n.Children {
			ok, why, err := c.served(ch, level)
			if err != nil || !ok {
				return false, why, err
			}
		}
		return true, nil, nil
	case *Not:
		if _, _, err := c.served(n.Child, level); err != nil {
			return false, nil, err
		}
		return false, &NotServedError{Need: "a negation alone reads every row; add a condition an index serves"}, nil
	case *Has:
		if _, _, err := c.join(level, n.Level); err != nil {
			return false, nil, err
		}
		return c.served(n.Child, n.Level)
	}
	return false, nil, invalid("a node is missing")
}

// column returns a leaf's column, refusing a field the registry does not
// have, one held at another level, and values of another type.
func (c Compiler) column(l *Leaf, level Level) (Column, error) {
	name := FieldString(l.Field)
	col, ok := c.Registry.Column(l.Field)
	if !ok {
		return Column{}, &fieldError{field: name, why: "it is not a search attribute; it must be added to the search table and indexed", unknown: true}
	}
	if col.Level != level {
		return Column{}, &fieldError{field: name, why: "it is held at level " + string(col.Level) + "; compare it inside has " + string(col.Level) + " (...)"}
	}
	if l.Op == OpMatches {
		return col, nil
	}
	if len(l.Values) == 0 {
		return col, nil
	}
	want := map[ColumnType]Kind{TypeText: KindText, TypeNumber: KindNumber, TypeDate: KindDate, TypeTime: KindTime, TypeDateTime: KindDateTime}[col.Type]
	if sortOf(l.Values[0].Kind) != want {
		return Column{}, &fieldError{field: name, why: "its values are of another kind than the condition's"}
	}
	if l.Fold && col.Folded == "" {
		return Column{}, &fieldError{field: name, why: "it has no folded column; compare it exactly"}
	}
	return col, nil
}

// fieldError is a field a search cannot compare. It wraps ErrUnknownField
// for a field the registry does not have, ErrInvalid otherwise.
type fieldError struct {
	field, why string
	unknown    bool
}

func (e *fieldError) Error() string { return "query: " + e.field + ": " + e.why }
func (e *fieldError) Unwrap() error {
	if e.unknown {
		return ErrUnknownField
	}
	return ErrInvalid
}

// leafServed returns why no index serves a leaf on col, nil when one does.
func leafServed(l *Leaf, col Column, d Dialect) *NotServedError {
	field := FieldString(l.Field)
	need := func(what string) *NotServedError { return &NotServedError{Field: field, Need: what} }
	has := func(i Index) bool { return col.Index&i != 0 }
	btree, btreeName := IndexExact, "an exact index"
	if l.Fold {
		btree, btreeName = IndexFolded, "a folded index"
	}
	text := func() string {
		if l.Fold {
			return Fold(l.Values[0].Text)
		}
		return l.Values[0].Text
	}
	switch l.Op {
	case OpPresent:
		if !has(IndexExact) && !has(IndexFolded) {
			return need("index it")
		}
	case OpAbsent:
		if !has(IndexExact) && !has(IndexFolded) {
			return need("index it")
		}
		if col.Values != "" {
			return need("absent of a multi-valued attribute reads every row; add a condition an index serves")
		}
	case OpPrefix:
		if text() == "" {
			return need("a prefix of at least one character")
		}
		if !has(btree) {
			return need(btreeName)
		}
	case OpEqual, OpIn, OpLess, OpLessEqual, OpGreater, OpGreaterEqual, OpBetween:
		if !has(btree) {
			return need(btreeName)
		}
	case OpLike:
		if lit, ok := likePrefix(text()); ok {
			if lit == "" {
				return need("a pattern that does not start with a wildcard, or a trigram index")
			}
			if !has(btree) {
				return need(btreeName)
			}
			return nil
		}
		if !has(IndexTrigram) {
			return need("a trigram index")
		}
		if !literalServes(Fold(l.Values[0].Text), d) {
			return need("a pattern with three characters in a row, between its wildcards, the trigram index can use")
		}
	case OpContains:
		if !has(IndexTrigram) {
			return need("a trigram index")
		}
		if !d.TrigramServes(Fold(l.Values[0].Text)) {
			return need("a term with three characters in a row the trigram index can use")
		}
	case OpWords:
		if !has(IndexTrigram) {
			return need("a trigram index")
		}
		for _, w := range strings.Fields(Fold(l.Values[0].Text)) {
			if !d.TrigramServes(w) {
				return need("words each with three characters in a row the trigram index can use")
			}
		}
	case OpMatches:
		return need("matches is for routing; no index serves a regular expression, use like or contains")
	}
	return nil
}

// likePrefix reports whether a pattern is a literal followed by "*" and no
// other wildcard, or a literal alone, and returns the literal.
func likePrefix(p string) (string, bool) {
	lit := strings.TrimSuffix(p, "*")
	if strings.ContainsAny(lit, "*?") {
		return "", false
	}
	return lit, true
}

// literalServes reports whether one run of a pattern's characters between
// its wildcards is one the dialect's trigram index serves: the index
// finds the rows holding that run, and the pattern rechecks them.
func literalServes(p string, d Dialect) bool {
	for _, run := range strings.FieldsFunc(p, func(r rune) bool { return r == '*' || r == '?' }) {
		if d.TrigramServes(run) {
			return true
		}
	}
	return false
}

// join returns how a row at level from is joined to the rows at level to:
// the column of from's table whose value is among the values of the column
// of to's table that the rows matching a "has" give. Down the levels, from's
// key among to's parent column; up, from's parent column among to's key.
func (c Compiler) join(from, to Level) (outer, inner string, err error) {
	ft, ok1 := c.Registry.Table(from)
	tt, ok2 := c.Registry.Table(to)
	if !ok1 || !ok2 {
		return "", "", invalid("no search table at level " + string(to))
	}
	switch {
	case to.depth() > from.depth():
		if p, ok := tt.Parents[from]; ok {
			return ft.Key, p, nil
		}
	case to.depth() < from.depth():
		if p, ok := ft.Parents[to]; ok {
			return p, tt.Key, nil
		}
	}
	return "", "", invalid("has " + string(to) + " cannot be compared from level " + string(from))
}
