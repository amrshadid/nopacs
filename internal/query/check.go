package query

import (
	"math"
	"regexp"
	"regexp/syntax"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Check refuses a tree that breaks a rule of the language, with an error
// wrapping ErrInvalid: a tree deeper than MaxDepth or with more than
// MaxLeaves leaves; an "and" or "or" with fewer than two children; an
// unknown level or operator; a field or value out of its bounds; values
// whose number or kind the operator does not take (a range over text, a
// text comparison of a number); folding asked of a value that is not
// text; "nocase" asked of "matches"; a "matches" pattern RE2 cannot
// compile, or whose program is longer than MaxPatternInstructions. Parse
// checks every tree it returns; a tree built otherwise (FromQIDO, or one
// read back from where it was kept) is checked before it is used.
func Check(n Node) error {
	leaves := 0
	return check(n, 1, &leaves)
}

// check checks n, at depth depth from the root (the root is 1), counting
// the leaves into leaves. It refuses a node past MaxDepth, and a leaf past
// MaxLeaves, before it looks inside it, so a tree built in code, however
// deep or wide, makes it recurse at most MaxDepth times and visit at most
// MaxLeaves leaves.
func check(n Node, depth int, leaves *int) error {
	if depth > MaxDepth {
		return invalid("the condition is more than " + strconv.Itoa(MaxDepth) + " levels deep")
	}
	children := func(cs []Node, what string) error {
		if len(cs) < 2 {
			return invalid("an " + what + " has at least two children")
		}
		for _, c := range cs {
			if err := check(c, depth+1, leaves); err != nil {
				return err
			}
		}
		return nil
	}
	switch n := n.(type) {
	case *And:
		if n == nil {
			break
		}
		return children(n.Children, "and")
	case *Or:
		if n == nil {
			break
		}
		return children(n.Children, "or")
	case *Not:
		if n == nil {
			break
		}
		return check(n.Child, depth+1, leaves)
	case *Has:
		if n == nil {
			break
		}
		if n.Level.depth() < 0 {
			return invalid("unknown level " + strconv.Quote(string(n.Level)))
		}
		return check(n.Child, depth+1, leaves)
	case *Leaf:
		if n == nil {
			break
		}
		if *leaves++; *leaves > MaxLeaves {
			return invalid("a condition has at most " + strconv.Itoa(MaxLeaves) + " comparisons")
		}
		return checkLeaf(n)
	}
	return invalid("a node is missing")
}

// checkLeaf checks one leaf.
func checkLeaf(l *Leaf) error {
	if err := checkField(l.Field); err != nil {
		return err
	}
	want := 1
	switch l.Op {
	case OpPresent, OpAbsent:
		want = 0
	case OpBetween:
		want = 2
	case OpIn:
		if len(l.Values) < 1 || len(l.Values) > MaxListValues {
			return invalid("a list has from 1 to " + strconv.Itoa(MaxListValues) + " values")
		}
		want = len(l.Values)
	case OpEqual, OpLess, OpLessEqual, OpGreater, OpGreaterEqual, OpPrefix, OpLike, OpContains, OpWords, OpMatches:
	default:
		return invalid("unknown operator")
	}
	if len(l.Values) != want {
		return invalid("the operator " + l.Op.String() + " takes " + strconv.Itoa(want) + " values")
	}
	for i, v := range l.Values {
		if err := checkValue(v); err != nil {
			return err
		}
		if i > 0 && sortOf(v.Kind) != sortOf(l.Values[0].Kind) {
			return invalid("the values of one comparison are of one kind")
		}
	}
	if len(l.Values) == 0 {
		if l.Fold {
			return invalid("present and absent do not fold")
		}
		return nil
	}
	k := l.Values[0].Kind
	switch {
	case l.Op.isText() && k != KindText:
		return invalid("the operator " + l.Op.String() + " compares text")
	case l.Op.isRange() && k == KindText:
		return invalid("the operator " + l.Op.String() + " compares numbers, dates and times")
	case l.Fold && k != KindText:
		return invalid("only text is folded")
	case (l.Op == OpContains || l.Op == OpWords) && !l.Fold:
		return invalid(l.Op.String() + " compares folded text")
	case l.Op == OpWords && len(strings.Fields(l.Values[0].Text)) == 0:
		return invalid("words needs a word")
	case l.Op == OpMatches && l.Fold:
		return invalid(`matches does not fold; write (?i) in the pattern to ignore case`)
	case l.Op == OpMatches:
		if _, err := compilePattern(l.Values[0].Text); err != nil {
			return err
		}
	}
	return nil
}

// MaxPatternInstructions is the most instructions the program of a
// "matches" pattern may have once compiled. RE2 matches in time in
// proportion to the text's length times the program's, and short text can
// compile to a long program ("(ab|cd|ef){400}", 15 bytes, is 4,002
// instructions); routing runs the pattern over every file a receiver
// takes, so its program is bounded as its text is.
const MaxPatternInstructions = 2000

// patternCacheSize is the most compiled patterns kept: a receiver's rules
// hold few, and a dry run or a search refused for its pattern adds a few
// more. When the cache is full it is emptied and fills again.
const patternCacheSize = 256

// patterns holds compiled "matches" patterns by their text, so a pattern
// is compiled once, not once a file.
var patterns = struct {
	sync.Mutex
	m map[string]*regexp.Regexp
}{m: map[string]*regexp.Regexp{}}

// compilePattern returns the compiled pattern of a "matches" leaf,
// refusing one RE2 cannot compile and one whose program is longer than
// MaxPatternInstructions, with an error wrapping ErrInvalid.
func compilePattern(expr string) (*regexp.Regexp, error) {
	patterns.Lock()
	re, ok := patterns.m[expr]
	patterns.Unlock()
	if ok {
		return re, nil
	}
	parsed, err := syntax.Parse(expr, syntax.Perl)
	if err != nil {
		return nil, invalid("the pattern of matches is not an RE2 expression")
	}
	prog, err := syntax.Compile(parsed.Simplify())
	if err != nil {
		return nil, invalid("the pattern of matches is not an RE2 expression")
	}
	if len(prog.Inst) > MaxPatternInstructions {
		return nil, invalid("the pattern of matches compiles to more than " + strconv.Itoa(MaxPatternInstructions) + " instructions")
	}
	re, err = regexp.Compile(expr)
	if err != nil {
		return nil, invalid("the pattern of matches is not an RE2 expression")
	}
	patterns.Lock()
	if len(patterns.m) >= patternCacheSize {
		clear(patterns.m)
	}
	patterns.m[expr] = re
	patterns.Unlock()
	return re, nil
}

// sortOf groups the kinds that compare with each other: a relative date is
// a date.
func sortOf(k Kind) Kind {
	if k == KindRelative {
		return KindDate
	}
	return k
}

// checkField checks a field's bounds.
func checkField(f Field) error {
	if f.Catalog != "" {
		if len(f.Path) > 0 || f.Indexed {
			return invalid("a catalog field has no path or index")
		}
		if len(f.Catalog) > 128 {
			return invalid("a catalog field's name is at most 128 bytes")
		}
		for _, n := range strings.Split(f.Catalog, ".") {
			if !isCatalogName(n) {
				return invalid("a catalog field's name is lower-case letters, digits and \"_\"")
			}
		}
		return nil
	}
	if len(f.Path) == 0 || len(f.Path) > MaxPathSteps {
		return invalid("a path has from 1 to " + strconv.Itoa(MaxPathSteps) + " steps")
	}
	if f.Indexed && (f.Index < 0 || f.Index > MaxIndex) {
		return invalid("a value index is from 0 to " + strconv.Itoa(MaxIndex))
	}
	if !f.Indexed && f.Index != 0 {
		return invalid("an index is given only with Indexed")
	}
	for _, s := range f.Path {
		if s.Creator == "" {
			continue
		}
		// PS3.5 7.8.1: a private data element is in an odd group, its
		// creator reserving one block of 256 elements.
		if (s.Tag>>16)%2 == 0 || s.Tag&0xFFFF > 0xFF || s.Tag>>16 <= 7 {
			return invalid("a private tag by creator is in an odd group above 0007, its element from 00 to FF")
		}
		if len(s.Creator) > MaxCreatorLength || !utf8.ValidString(s.Creator) || strings.ContainsAny(s.Creator, "\\") || hasControl(s.Creator) {
			return invalid("a private creator is at most " + strconv.Itoa(MaxCreatorLength) + " characters of text, with no backslash")
		}
		if strings.TrimRight(s.Creator, " ") != s.Creator || strings.TrimSpace(s.Creator) == "" {
			return invalid("a private creator has no trailing space and is not empty")
		}
	}
	return nil
}

// checkValue checks a value's bounds.
func checkValue(v Value) error {
	switch v.Kind {
	case KindText:
		if len(v.Text) > MaxValueLength || !utf8.ValidString(v.Text) || hasControl(v.Text) {
			return invalid("a text value is at most " + strconv.Itoa(MaxValueLength) + " bytes of UTF-8 with no control character")
		}
	case KindNumber:
		if math.IsInf(v.Number, 0) || math.IsNaN(v.Number) {
			return invalid("a number is finite")
		}
	case KindDate:
		if _, err := time.Parse("20060102", v.Text); err != nil || len(v.Text) != 8 {
			return invalid("a date is a day of the calendar")
		}
	case KindTime:
		if _, err := time.Parse("150405", v.Text); err != nil || len(v.Text) != 6 {
			return invalid("a time is a time of day")
		}
	case KindDateTime:
		if _, err := time.Parse("20060102150405", v.Text); err != nil || len(v.Text) != 14 {
			return invalid("a date and time is a day and a time of day")
		}
	case KindRelative:
		if v.Relative < 0 || v.Relative > MaxRelative || !strings.ContainsRune("dwmy", rune(v.Unit)) || v.Unit == 0 {
			return invalid("a relative date counts back from 0 to " + strconv.Itoa(MaxRelative) + " days, weeks, months or years")
		}
	default:
		return invalid("unknown kind of value")
	}
	if v.Kind != KindText && v.Text != "" && v.Kind != KindDate && v.Kind != KindTime && v.Kind != KindDateTime {
		return invalid("only text, dates and times have text")
	}
	return nil
}

// hasControl reports whether s holds a control character, which a value
// written into a log or a message could use to forge a line.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f || r >= 0x80 && r < 0xa0 {
			return true
		}
	}
	return false
}
