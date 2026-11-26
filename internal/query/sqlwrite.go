package query

import (
	"strconv"
	"strings"
)

// writer writes a checked, served condition as SQL.
type writer struct {
	c    Compiler
	args *[]any
}

// bind adds a value and returns its placeholder.
func (w *writer) bind(v any) string {
	*w.args = append(*w.args, v)
	return w.c.Dialect.Placeholder(len(*w.args))
}

// node writes n over the rows of level under alias; depth counts the
// subqueries around it, so each has its own alias.
func (w *writer) node(n Node, level Level, alias string, depth int) (string, error) {
	switch n := n.(type) {
	case *Leaf:
		return w.leaf(n, level, alias)
	case *And:
		return w.join(n.Children, " AND ", level, alias, depth)
	case *Or:
		return w.join(n.Children, " OR ", level, alias, depth)
	case *Not:
		s, err := w.node(n.Child, level, alias, depth)
		if err != nil {
			return "", err
		}
		// A comparison with NULL is NULL, and NOT NULL is NULL: the
		// negation of a leaf false for an absent attribute must be true.
		return "NOT COALESCE(" + s + ", FALSE)", nil
	case *Has:
		return w.has(n, level, alias, depth)
	}
	return "", invalid("a node is missing")
}

// join writes children joined by op, in parentheses.
func (w *writer) join(children []Node, op string, level Level, alias string, depth int) (string, error) {
	parts := make([]string, len(children))
	for i, ch := range children {
		s, err := w.node(ch, level, alias, depth)
		if err != nil {
			return "", err
		}
		parts[i] = s
	}
	return "(" + strings.Join(parts, op) + ")", nil
}

// has writes "has" as the row's join column among the join column of the
// other level's rows for which the child holds. A correlated EXISTS would
// mean the same, but a database plans it as a read of every row of this
// level, each probing the other; the IN is answered from the child's index
// and then probes this level's.
//
// "every" is that IN, which says there is a row and finds the rows of this
// level, and a correlated NOT EXISTS of a row for which the child does not
// hold, which each row the IN found answers by probing the other level's
// join column. The negated child is served by no index, so the
// uncorrelated "NOT IN (rows for which the child does not hold)" would
// read the other level's whole table; the NOT EXISTS reads only the rows
// that belong with the rows found.
func (w *writer) has(h *Has, level Level, alias string, depth int) (string, error) {
	outer, innerCol, err := w.c.join(level, h.Level)
	if err != nil {
		return "", err
	}
	t, _ := w.c.Registry.Table(h.Level)
	inner := "t" + strconv.Itoa(depth+1)
	some, err := w.node(h.Child, h.Level, inner, depth+1)
	if err != nil {
		return "", err
	}
	in := alias + "." + outer + " IN (SELECT " + inner + "." + innerCol + " FROM " + t.Name + " " + inner + " WHERE " + some + ")"
	if !h.Every {
		return in, nil
	}
	none, err := w.node(h.Child, h.Level, inner, depth+1)
	if err != nil {
		return "", err
	}
	return "(" + in + " AND NOT EXISTS (SELECT 1 FROM " + t.Name + " " + inner + " WHERE " + inner + "." + innerCol + " = " +
		alias + "." + outer + " AND NOT COALESCE(" + none + ", FALSE)))", nil
}

// leaf writes one comparison. On a multi-valued attribute it holds when
// one of the values in the column's values table does: the row's key among
// the keys of the values that match, as routing holds a leaf true when one
// value of the attribute matches.
func (w *writer) leaf(l *Leaf, level Level, alias string) (string, error) {
	col, err := w.c.column(l, level)
	if err != nil {
		return "", err
	}
	t, _ := w.c.Registry.Table(level)
	if col.Values == "" {
		return w.compare(l, col, alias, alias+"."+t.Row)
	}
	v := alias + "v"
	if l.Op == OpAbsent {
		// The values table holds a row for each value that is not empty,
		// so a row has none where its key is not among the table's keys.
		return alias + "." + t.Key + " NOT IN (SELECT " + v + "." + t.Key + " FROM " + col.Values + " " + v + " WHERE " + v + "." + t.Key + " IS NOT NULL)", nil
	}
	s, err := w.compare(l, col, v, "")
	if err != nil {
		return "", err
	}
	return alias + "." + t.Key + " IN (SELECT " + v + "." + t.Key + " FROM " + col.Values + " " + v + " WHERE " + s + ")", nil
}

// compare writes one comparison on col of the table under alias; row is
// the row column a trigram structure is keyed by, qualified.
func (w *writer) compare(l *Leaf, col Column, alias, row string) (string, error) {
	name := col.Exact
	if l.Fold {
		name = col.Folded
	}
	q := alias + "." + name
	match := func(op Op, text string, folded bool) string {
		n := col.Exact
		if folded {
			n = col.Folded
		}
		return w.c.Dialect.TextMatch(TextMatch{Op: op, Column: alias + "." + n, Name: n, Folded: folded, Text: text,
			Row: row, Trigram: col.Trigram}, w.bind)
	}
	// A column indexed only in its folded form is present where its folded
	// form is: the folded value of a value that is not empty is not empty.
	presence := q
	if col.Index&IndexExact == 0 && col.Folded != "" {
		presence = alias + "." + col.Folded
	}
	switch l.Op {
	case OpPresent:
		return presence + " IS NOT NULL", nil
	case OpAbsent:
		return presence + " IS NULL", nil
	case OpEqual:
		return q + " = " + w.bind(w.value(l, l.Values[0])), nil
	case OpIn:
		ph := make([]string, len(l.Values))
		for i, v := range l.Values {
			ph[i] = w.bind(w.value(l, v))
		}
		return q + " IN (" + strings.Join(ph, ", ") + ")", nil
	case OpLess, OpLessEqual, OpGreater, OpGreaterEqual:
		return q + " " + l.Op.String() + " " + w.bind(w.value(l, l.Values[0])), nil
	case OpBetween:
		return q + " BETWEEN " + w.bind(w.value(l, l.Values[0])) + " AND " + w.bind(w.value(l, l.Values[1])), nil
	case OpPrefix:
		return match(OpPrefix, w.text(l, l.Values[0].Text), l.Fold), nil
	case OpLike:
		p := w.text(l, l.Values[0].Text)
		if lit, ok := likePrefix(p); ok {
			if lit == p {
				return q + " = " + w.bind(lit), nil
			}
			return match(OpPrefix, lit, l.Fold), nil
		}
		if l.Fold {
			return match(OpLike, p, true), nil
		}
		// No trigram index is kept over the exact text: the folded one
		// finds the rows, and the exact pattern keeps those that match it.
		// In the folded pattern each "?" is a "*": folding may change how
		// many characters a value has ("ß" folds to "ss", a decomposed
		// "ü" loses its combining mark), and a row the exact pattern keeps
		// must not be lost by the one that finds it.
		return "(" + match(OpLike, strings.ReplaceAll(Fold(p), "?", "*"), true) + " AND " + match(OpLike, p, false) + ")", nil
	case OpContains:
		return match(OpContains, Fold(l.Values[0].Text), true), nil
	case OpWords:
		words := strings.Fields(Fold(l.Values[0].Text))
		parts := make([]string, len(words))
		for i, word := range words {
			parts[i] = match(OpContains, word, true)
		}
		return "(" + strings.Join(parts, " AND ") + ")", nil
	}
	return "", &NotServedError{Field: FieldString(l.Field), Need: "no index serves " + l.Op.String()}
}

// text returns a text value as the column holds it: folded when the leaf
// folds.
func (w *writer) text(l *Leaf, s string) string {
	if l.Fold {
		return Fold(s)
	}
	return s
}

// value returns the parameter of one value: text (folded when the leaf
// folds), a number, or a date, time or date-time in the column's form.
func (w *writer) value(l *Leaf, v Value) any {
	switch v.Kind {
	case KindText:
		return w.text(l, v.Text)
	case KindNumber:
		return v.Number
	case KindRelative:
		return RelativeDate(v, w.c.Now)
	}
	return v.Text
}

// EscapeLike returns s with the characters special to SQL's LIKE ("%",
// "_") and the escape character itself ("\") escaped by "\", for a pattern
// written with ESCAPE '\': a literal never becomes a wildcard, so "_" in a
// series description is an underscore.
func EscapeLike(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '%' || r == '_' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// GlobToLike returns a pattern of "*" and "?" (PS3.4 C.2.2.2.4) as a LIKE
// pattern for ESCAPE '\': "*" as "%", "?" as "_", every other character
// literal (EscapeLike).
func GlobToLike(p string) string {
	var b strings.Builder
	for _, r := range p {
		switch r {
		case '*':
			b.WriteByte('%')
		case '?':
			b.WriteByte('_')
		default:
			b.WriteString(EscapeLike(string(r)))
		}
	}
	return b.String()
}
