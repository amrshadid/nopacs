package query

// Node is one node of a condition's tree: *And, *Or, *Not, *Has or *Leaf.
// The tree is built by Parse or FromQIDO, or by a caller, and checked by
// Check before it is used.
type Node interface {
	// node keeps the set of node types to this package's own.
	node()
}

// And holds when every child holds. It has at least two children.
type And struct {
	// Children are the conditions, in the order written.
	Children []Node
}

// Or holds when one child holds. It has at least two children.
type Or struct {
	// Children are the conditions, in the order written.
	Children []Node
}

// Not holds when its child does not.
type Not struct {
	// Child is the condition negated.
	Child Node
}

// Has holds for a row at one level when rows at another level that belong
// with it satisfy its child: one of them ("has"), or at least one and all of
// them ("every"). It is a search condition only: routing reads one file,
// which has no other level to look at.
type Has struct {
	// Every says the child must hold for every row of the other level, and
	// that there must be one.
	Every bool
	// Level is the level whose rows the child is evaluated on.
	Level Level
	// Child is the condition evaluated at Level.
	Child Node
}

// Leaf compares one field with values.
type Leaf struct {
	// Field is the attribute or catalog field compared.
	Field Field
	// Op is the comparison.
	Op Op
	// Fold says text is compared folded (Fold): case and accents ignored.
	// It is false for a value that is not text.
	Fold bool
	// Values are the operands: none for OpPresent and OpAbsent, two for
	// OpBetween, one or more for OpIn, one otherwise.
	Values []Value
}

func (*And) node()  {}
func (*Or) node()   {}
func (*Not) node()  {}
func (*Has) node()  {}
func (*Leaf) node() {}

// Level is a level of the catalog.
type Level string

// The levels, from the top.
const (
	LevelSubject  Level = "subject"
	LevelSession  Level = "session"
	LevelScan     Level = "scan"
	LevelInstance Level = "instance"
)

// levels are the levels in order from the top.
var levels = []Level{LevelSubject, LevelSession, LevelScan, LevelInstance}

// depth returns the level's place from the top, or -1 for no level.
func (l Level) depth() int {
	for i, v := range levels {
		if v == l {
			return i
		}
	}
	return -1
}

// Op is a leaf's comparison.
type Op int

// The comparisons. Text comparisons compare folded text when the leaf says
// Fold, the exact text otherwise.
const (
	// OpEqual is "=" (folded text) or "==" (exact).
	OpEqual Op = iota + 1
	// OpIn holds when the value equals one of the leaf's values.
	OpIn
	// OpLess is "<".
	OpLess
	// OpLessEqual is "<=".
	OpLessEqual
	// OpGreater is ">".
	OpGreater
	// OpGreaterEqual is ">=".
	OpGreaterEqual
	// OpBetween holds from the first value to the second, both included.
	OpBetween
	// OpPrefix holds when the text starts with the value.
	OpPrefix
	// OpLike matches a pattern in which "*" stands for any run of
	// characters and "?" for one (PS3.4 C.2.2.2.4); no other character is
	// special, so "%" and "_" are themselves.
	OpLike
	// OpContains holds when the folded text holds the folded value.
	OpContains
	// OpWords holds when the folded text holds every word of the value.
	OpWords
	// OpMatches matches an RE2 regular expression. Routing only: no index
	// can serve it.
	OpMatches
	// OpPresent holds when the field has a value that is not empty.
	OpPresent
	// OpAbsent holds when it has none.
	OpAbsent
)

// opWords are the operators written as words, by their spelling.
var opWords = map[string]Op{
	"in": OpIn, "between": OpBetween, "prefix": OpPrefix, "like": OpLike,
	"contains": OpContains, "words": OpWords, "matches": OpMatches,
	"present": OpPresent, "absent": OpAbsent,
}

// String returns the operator as Print writes it ("=" for OpEqual).
func (o Op) String() string {
	switch o {
	case OpEqual:
		return "="
	case OpLess:
		return "<"
	case OpLessEqual:
		return "<="
	case OpGreater:
		return ">"
	case OpGreaterEqual:
		return ">="
	}
	for w, v := range opWords {
		if v == o {
			return w
		}
	}
	return "?"
}

// isRange reports whether o orders values.
func (o Op) isRange() bool {
	return o == OpLess || o == OpLessEqual || o == OpGreater || o == OpGreaterEqual || o == OpBetween
}

// isText reports whether o compares text only.
func (o Op) isText() bool {
	return o == OpPrefix || o == OpLike || o == OpContains || o == OpWords || o == OpMatches
}

// Kind is the type of a value.
type Kind int

// The kinds of value.
const (
	// KindText is a quoted string.
	KindText Kind = iota + 1
	// KindNumber is a decimal number.
	KindNumber
	// KindDate is a date, held as DICOM writes a DA value: YYYYMMDD.
	KindDate
	// KindTime is a time of day, held as HHMMSS (TM without fraction).
	KindTime
	// KindDateTime is a date and time, held as YYYYMMDDHHMMSS.
	KindDateTime
	// KindRelative is a date counted back from today: Relative days,
	// weeks, months or years before the day the condition is evaluated.
	KindRelative
)

// Value is one operand of a leaf.
type Value struct {
	// Kind is the value's type.
	Kind Kind
	// Text is the value of a text, date, time or date-time value.
	Text string
	// Number is the value of a number.
	Number float64
	// Relative is how many units back a relative date is (0 is today).
	Relative int
	// Unit is the unit of a relative date: 'd', 'w', 'm' or 'y'.
	Unit byte
}

// Field names what a leaf compares: a catalog field, or a DICOM attribute
// by a path of one or more steps through sequences.
type Field struct {
	// Catalog is a catalog field's name ("calling_ae", "session.date"),
	// empty for a DICOM attribute.
	Catalog string
	// Path is the attribute's steps: the last names the attribute, each
	// one before it a sequence whose items, every one of them, are looked
	// into.
	Path []Step
	// Indexed says the leaf compares only one value of a multi-valued
	// attribute, the one at Index (from 0).
	Indexed bool
	// Index is the value's place, when Indexed.
	Index int
}

// Step is one step of an attribute's path: a tag, or a private tag by its
// creator (PS3.5 7.8.1), whose block the data set reserves under that
// creator's name.
type Step struct {
	// Tag is the attribute's tag, group in the high 16 bits; for a private
	// tag by creator, its element is only the low byte, the element within
	// the block.
	Tag uint32
	// Creator is the private creator's name, empty for a tag named whole.
	Creator string
}

// Truth is the result of a condition evaluated over a file.
type Truth int8

// The three values (Kleene's logic).
const (
	// False: the condition does not hold.
	False Truth = iota
	// True: the condition holds.
	True
	// Unknown: whether it holds cannot be known, because a value it reads
	// was removed before the file was kept.
	Unknown
)

// String returns "true", "false" or "unknown".
func (t Truth) String() string {
	switch t {
	case True:
		return "true"
	case Unknown:
		return "unknown"
	}
	return "false"
}
