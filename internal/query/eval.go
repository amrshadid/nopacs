package query

import (
	"errors"
	"strings"
	"time"

	"github.com/amrshadid/go-dicom/dataset"
)

// MaxReadLength is the most bytes of one attribute's value routing reads:
// routing decides on short identifying values (a modality, a maker, a
// protocol's name), and a longer one, a value the reader leaves in the
// file among them, makes the condition one that cannot be evaluated
// (*ReadError), as a receiver's routing has always treated it, rather than
// a value read in part.
const MaxReadLength = 1024

// ErrSearchOnly is a condition routing cannot evaluate: "has" and "every"
// look at rows of another level of the catalog, and routing reads one file.
var ErrSearchOnly = errors.New("query: has and every are search conditions; routing reads one file")

// ReadError is a field routing could not read: a value longer than
// MaxReadLength or not held in memory, or a catalog field the caller does
// not give. Its text names the field, never a value.
type ReadError struct {
	// Field is the field, as FieldString writes it.
	Field string
	// Why says what was wrong.
	Why string
}

// Error returns the field and why.
func (e *ReadError) Error() string { return "query: " + e.Field + ": " + e.Why }

// Subject is what a condition is evaluated over: one file's data set, and
// the facts about it that are not in the file.
type Subject struct {
	// Dataset is the file's data set as go-dicom reads it, text decoded
	// into UTF-8.
	Dataset *dataset.Dataset
	// BigEndian says the binary values of the data set are big endian, as
	// a file in the retired Explicit VR Big Endian transfer syntax holds
	// them (PS3.5 A.3); they are little endian otherwise.
	BigEndian bool
	// Catalog gives a catalog field's value: present is false when the
	// field has none, and err refuses a field the caller does not know.
	// Nil refuses every catalog field.
	Catalog func(name string) (value string, present bool, err error)
	// Removed, in a dry run over files already de-identified, reports
	// whether the profile removed or replaced the attribute a field names:
	// the file kept does not hold the value received, so a leaf on it is
	// Unknown. Nil when the file is the one received.
	Removed func(Field) bool
	// Now is when the condition is evaluated: relative dates count back
	// from its day, in its location.
	Now time.Time
}

// Result is a condition's value over a subject.
type Result struct {
	// Truth is the condition's value.
	Truth Truth
	// Decided is the leaf that decided it: the first false child of a
	// false "and", the first true child of a true "or", the first unknown
	// one when the result is unknown, the last child otherwise. It points
	// into the tree evaluated.
	Decided *Leaf
}

// Eval evaluates a condition over one file. An attribute the file does not
// hold makes a comparison false; one the subject says was removed makes a
// leaf Unknown, and Unknown goes up the tree by Kleene's logic: false wins
// an "and", true wins an "or", and "not" leaves it unknown. A field that
// cannot be read is a *ReadError; a "has" or "every" is ErrSearchOnly. A
// multi-valued attribute, or one inside a sequence, holds when any of its
// values does, in any item of the sequences on its path (sequence matching
// holds for an item that matches, PS3.4 C.2.2.2.6).
func Eval(n Node, s Subject) (Result, error) {
	if s.Dataset == nil {
		return Result{}, errors.New("query: no data set to evaluate over")
	}
	e := &evaluator{s: s}
	return e.eval(n)
}

// evaluator holds one evaluation's state.
type evaluator struct {
	s Subject
}

// eval evaluates n.
func (e *evaluator) eval(n Node) (Result, error) {
	switch n := n.(type) {
	case *Leaf:
		t, err := e.leaf(n)
		return Result{Truth: t, Decided: n}, err
	case *Not:
		r, err := e.eval(n.Child)
		switch r.Truth {
		case True:
			r.Truth = False
		case False:
			r.Truth = True
		}
		return r, err
	case *And:
		return e.join(n.Children, False)
	case *Or:
		return e.join(n.Children, True)
	case *Has:
		return Result{}, ErrSearchOnly
	}
	return Result{}, invalid("a node is missing")
}

// join evaluates the children of an "and" (wins False) or an "or" (wins
// True): the first child of value wins decides; else the first unknown
// one; else the last child.
func (e *evaluator) join(children []Node, wins Truth) (Result, error) {
	var unknown, last Result
	seen := false
	for _, c := range children {
		r, err := e.eval(c)
		if err != nil {
			return Result{}, err
		}
		if r.Truth == wins {
			return r, nil
		}
		if r.Truth == Unknown && !seen {
			unknown, seen = r, true
		}
		last = r
	}
	if seen {
		return unknown, nil
	}
	return last, nil
}

// leaf evaluates one comparison.
func (e *evaluator) leaf(l *Leaf) (Truth, error) {
	if l.Field.Catalog == "" && e.s.Removed != nil && e.s.Removed(l.Field) {
		return Unknown, nil
	}
	vals, present, err := e.read(l.Field)
	if err != nil {
		return False, err
	}
	switch l.Op {
	case OpPresent:
		return truth(present), nil
	case OpAbsent:
		return truth(!present), nil
	}
	m, err := e.matcher(l)
	if err != nil {
		return False, err
	}
	for _, v := range vals {
		if m(v) {
			return True, nil
		}
	}
	return False, nil
}

// truth returns True or False.
func truth(b bool) Truth {
	if b {
		return True
	}
	return False
}

// read returns the values of a field that are not empty, and whether it
// has one (a sequence: an item).
func (e *evaluator) read(f Field) ([]string, bool, error) {
	if f.Catalog != "" {
		if e.s.Catalog == nil {
			return nil, false, &ReadError{Field: f.Catalog, Why: "routing knows no such catalog field"}
		}
		v, ok, err := e.s.Catalog(f.Catalog)
		if err != nil {
			return nil, false, &ReadError{Field: f.Catalog, Why: "routing knows no such catalog field"}
		}
		if !ok || v == "" {
			return nil, false, nil
		}
		return []string{v}, true, nil
	}
	r := reading{f: f, bigEndian: e.s.BigEndian}
	if err := r.collect(e.s.Dataset, f.Path); err != nil {
		return nil, false, err
	}
	return r.values, r.present, nil
}

// matcher returns the test of one value of a field against the leaf.
func (e *evaluator) matcher(l *Leaf) (func(string) bool, error) {
	lit := l.Values[0]
	switch sortOf(lit.Kind) {
	case KindNumber:
		return func(v string) bool {
			x, ok := parseNumber(v)
			return ok && compareAll(l, func(w Value) int { return cmpFloat(x, w.Number) })
		}, nil
	case KindDate:
		return func(v string) bool {
			x, ok := normalDate(v)
			return ok && compareAll(l, func(w Value) int { return strings.Compare(x, e.date(w)) })
		}, nil
	case KindTime:
		return func(v string) bool {
			x, ok := normalTime(v)
			return ok && compareAll(l, func(w Value) int { return strings.Compare(x, w.Text) })
		}, nil
	case KindDateTime:
		return func(v string) bool {
			x, ok := normalDateTime(v)
			return ok && compareAll(l, func(w Value) int { return strings.Compare(x, w.Text) })
		}, nil
	}
	fold := func(s string) string { return s }
	if l.Fold {
		fold = Fold
	}
	want := make([]string, len(l.Values))
	for i, w := range l.Values {
		want[i] = fold(w.Text)
	}
	switch l.Op {
	case OpEqual, OpIn:
		return func(v string) bool {
			v = fold(v)
			for _, w := range want {
				if v == w {
					return true
				}
			}
			return false
		}, nil
	case OpPrefix:
		return func(v string) bool { return strings.HasPrefix(fold(v), want[0]) }, nil
	case OpLike:
		return func(v string) bool { return glob(fold(v), want[0]) }, nil
	case OpContains:
		return func(v string) bool { return strings.Contains(fold(v), want[0]) }, nil
	case OpWords:
		words := strings.Fields(want[0])
		return func(v string) bool {
			v = fold(v)
			for _, w := range words {
				if !strings.Contains(v, w) {
					return false
				}
			}
			return true
		}, nil
	case OpMatches:
		if l.Fold {
			return nil, invalid("matches does not fold")
		}
		re, err := compilePattern(lit.Text)
		if err != nil {
			return nil, err
		}
		return re.MatchString, nil
	}
	return nil, invalid("the operator " + l.Op.String() + " does not compare text")
}

// date returns a date or relative date value as YYYYMMDD.
func (e *evaluator) date(v Value) string {
	if v.Kind == KindRelative {
		return RelativeDate(v, e.s.Now)
	}
	return v.Text
}

// RelativeDate returns the day a relative date names, counted back from
// now's day in now's location, as YYYYMMDD. A month back from the 31st is
// the day Go's calendar arithmetic gives (time.Time.AddDate), which may
// fall in the month after.
func RelativeDate(v Value, now time.Time) string {
	n := v.Relative
	switch v.Unit {
	case 'w':
		now = now.AddDate(0, 0, -7*n)
	case 'm':
		now = now.AddDate(0, -n, 0)
	case 'y':
		now = now.AddDate(-n, 0, 0)
	default:
		now = now.AddDate(0, 0, -n)
	}
	return now.Format("20060102")
}

// compareAll applies a leaf's ordering operator, or its equality or list,
// given how a field's value compares with one of the leaf's values.
func compareAll(l *Leaf, cmp func(Value) int) bool {
	switch l.Op {
	case OpEqual:
		return cmp(l.Values[0]) == 0
	case OpIn:
		for _, w := range l.Values {
			if cmp(w) == 0 {
				return true
			}
		}
		return false
	case OpLess:
		return cmp(l.Values[0]) < 0
	case OpLessEqual:
		return cmp(l.Values[0]) <= 0
	case OpGreater:
		return cmp(l.Values[0]) > 0
	case OpGreaterEqual:
		return cmp(l.Values[0]) >= 0
	case OpBetween:
		return cmp(l.Values[0]) >= 0 && cmp(l.Values[1]) <= 0
	}
	return false
}

// cmpFloat compares two numbers.
func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// glob reports whether s matches pattern, in which "*" stands for any run
// of characters and "?" for one (PS3.4 C.2.2.2.4). It backtracks only to
// the last "*", so it takes time in proportion to the product of the two
// lengths at worst, never more.
func glob(s, pattern string) bool {
	str, pat := []rune(s), []rune(pattern)
	i, j := 0, 0
	star, mark := -1, 0
	for i < len(str) {
		switch {
		case j < len(pat) && (pat[j] == '?' || pat[j] == str[i]):
			i++
			j++
		case j < len(pat) && pat[j] == '*':
			star, mark = j, i
			j++
		case star >= 0:
			mark++
			i, j = mark, star+1
		default:
			return false
		}
	}
	for j < len(pat) && pat[j] == '*' {
		j++
	}
	return j == len(pat)
}
