package query

import (
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/tag"
)

// The grammar Parse reads, in EBNF; words are matched in lower case only,
// and white space separates tokens:
//
//	expr     = and { "or" and }
//	and      = unary { "and" unary }
//	unary    = "not" unary | "(" expr ")" | has | leaf
//	has      = ( "has" | "every" ) level "(" expr ")"
//	level    = "subject" | "session" | "scan" | "instance"
//	leaf     = field ( "present" | "absent" )
//	         | field [ "nocase" ] op value
//	         | field [ "nocase" ] "in" "(" value { "," value } ")"
//	         | field "between" value "and" value
//	         | field "last" count ( "days" | "weeks" | "months" | "years" )
//	field    = catalog | step { "." step } [ "[" index "]" ]
//	catalog  = name { "." name }          ; lower case: calling_ae, session.date
//	step     = Keyword                    ; a PS3.6 keyword: SeriesDescription
//	         | "(" gggg "," eeee ")"      ; a tag: (0018,0050)
//	         | "(" gggg "," string "," ee ")" ; private, by creator: (0029,"SIEMENS CSA HEADER",10)
//	op       = "=" | "==" | "!=" | "<" | "<=" | ">" | ">="
//	         | "prefix" | "like" | "contains" | "words" | "matches"
//	value    = string | number | date | time | datetime | relative
//	string   = '"' text '"'                ; escapes \" and \\ only
//	date     = 2024-01-31 ; time = 10:30 or 10:30:15 ; datetime = 2024-01-31T10:30:15
//	relative = "-" count ( "d" | "w" | "m" | "y" ) | "today"
//
// "=" compares text folded (Fold), "==" exactly; "nocase" folds the other
// text comparisons, and "contains" and "words" always compare folded text.
// "matches" does not fold: folding the literals of a regular expression
// has no exact meaning (a class "[ß]" would have to match "ss"), so a
// pattern that ignores case says so itself, with RE2's (?i).
// "a != v" is "not a = v". "last 30 days" is ">= -30d".

// reserved are the words a catalog field's name cannot be.
var reserved = map[string]bool{
	"and": true, "or": true, "not": true, "has": true, "every": true, "nocase": true,
	"last": true, "today": true, "in": true, "between": true, "prefix": true, "like": true,
	"contains": true, "words": true, "matches": true, "present": true, "absent": true,
}

// Parse reads a condition and returns its tree, checked by Check. A
// condition that cannot be read is a *SyntaxError naming the byte where it
// went wrong; a tree that reads but breaks a rule of Check wraps
// ErrInvalid.
func Parse(text string) (Node, error) {
	if len(text) > MaxLength {
		return nil, fail(MaxLength, "a condition is longer than "+strconv.Itoa(MaxLength)+" bytes")
	}
	p := &parser{lex: lexer{src: text}}
	if err := p.advance(); err != nil {
		return nil, err
	}
	if p.tok.kind == tokEOF {
		return nil, fail(0, "the condition is empty")
	}
	n, _, err := p.expr()
	if err != nil {
		return nil, err
	}
	if p.tok.kind != tokEOF {
		return nil, fail(p.tok.pos, "expected \"and\", \"or\" or the end")
	}
	if err := Check(n); err != nil {
		return nil, err
	}
	return n, nil
}

// parser reads a condition with one token of look-ahead.
type parser struct {
	lex    lexer
	tok    token
	nest   int // parentheses, "not" and "has" open now
	leaves int
}

// advance reads the next token.
func (p *parser) advance() error {
	t, err := p.lex.next()
	if err != nil {
		return err
	}
	p.tok = t
	return nil
}

// word reports whether the token is the identifier w.
func (p *parser) word(w string) bool { return p.tok.kind == tokIdent && p.tok.text == w }

// expect moves past a token of kind k, or fails with what.
func (p *parser) expect(k tokKind, what string) error {
	if p.tok.kind != k {
		return fail(p.tok.pos, "expected "+what)
	}
	return p.advance()
}

// open counts one more parenthesis, "not" or "has" nested at pos.
func (p *parser) open(pos int) error {
	p.nest++
	if p.nest > MaxDepth {
		return fail(pos, "more than "+strconv.Itoa(MaxDepth)+" parentheses, \"not\" and \"has\" are nested")
	}
	return nil
}

// deep refuses a tree deeper than MaxDepth, at pos.
func deep(d, pos int) error {
	if d > MaxDepth {
		return fail(pos, "the condition is more than "+strconv.Itoa(MaxDepth)+" levels deep")
	}
	return nil
}

// expr reads "and" terms joined by "or"; it returns the node and its depth.
func (p *parser) expr() (Node, int, error) {
	start := p.tok.pos
	first, d, err := p.and()
	if err != nil {
		return nil, 0, err
	}
	if !p.word("or") {
		return first, d, nil
	}
	or := &Or{Children: []Node{first}}
	for p.word("or") {
		if err := p.advance(); err != nil {
			return nil, 0, err
		}
		n, nd, err := p.and()
		if err != nil {
			return nil, 0, err
		}
		or.Children = append(or.Children, n)
		d = max(d, nd)
	}
	return or, d + 1, deep(d+1, start)
}

// and reads unary terms joined by "and".
func (p *parser) and() (Node, int, error) {
	start := p.tok.pos
	first, d, err := p.unary()
	if err != nil {
		return nil, 0, err
	}
	if !p.word("and") {
		return first, d, nil
	}
	and := &And{Children: []Node{first}}
	for p.word("and") {
		if err := p.advance(); err != nil {
			return nil, 0, err
		}
		n, nd, err := p.unary()
		if err != nil {
			return nil, 0, err
		}
		and.Children = append(and.Children, n)
		d = max(d, nd)
	}
	return and, d + 1, deep(d+1, start)
}

// unary reads a "not", a group, a "has" or a leaf.
func (p *parser) unary() (Node, int, error) {
	start := p.tok.pos
	switch {
	case p.word("not"):
		if err := p.open(start); err != nil {
			return nil, 0, err
		}
		if err := p.advance(); err != nil {
			return nil, 0, err
		}
		n, d, err := p.unary()
		if err != nil {
			return nil, 0, err
		}
		p.nest--
		return &Not{Child: n}, d + 1, deep(d+1, start)
	case p.word("has") || p.word("every"):
		return p.has()
	case p.tok.kind == tokLParen:
		// A tag starts a leaf; any other parenthesis opens a group.
		p.lex.pos = start
		if _, ok, err := p.lex.tag(); err != nil {
			return nil, 0, err
		} else if ok {
			p.lex.pos = start
			return p.leaf()
		}
		p.lex.pos = start + 1
		if err := p.open(start); err != nil {
			return nil, 0, err
		}
		if err := p.advance(); err != nil {
			return nil, 0, err
		}
		n, d, err := p.expr()
		if err != nil {
			return nil, 0, err
		}
		if err := p.expect(tokRParen, `")"`); err != nil {
			return nil, 0, err
		}
		p.nest--
		return n, d, nil
	case p.tok.kind == tokIdent:
		p.lex.pos = start
		return p.leaf()
	}
	return nil, 0, fail(start, "expected a condition")
}

// has reads ("has" | "every") level "(" expr ")".
func (p *parser) has() (Node, int, error) {
	start := p.tok.pos
	every := p.word("every")
	if err := p.open(start); err != nil {
		return nil, 0, err
	}
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	lv := Level(p.tok.text)
	if p.tok.kind != tokIdent || lv.depth() < 0 {
		return nil, 0, fail(p.tok.pos, "expected a level: subject, session, scan or instance")
	}
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	if err := p.expect(tokLParen, `"(" after the level`); err != nil {
		return nil, 0, err
	}
	n, d, err := p.expr()
	if err != nil {
		return nil, 0, err
	}
	if err := p.expect(tokRParen, `")"`); err != nil {
		return nil, 0, err
	}
	p.nest--
	return &Has{Every: every, Level: lv, Child: n}, d + 1, deep(d+1, start)
}

// leaf reads a field and its comparison, the lexer standing at the field.
func (p *parser) leaf() (Node, int, error) {
	start := p.lex.pos
	p.leaves++
	if p.leaves > MaxLeaves {
		return nil, 0, fail(p.lex.spaceAt(start), "a condition has at most "+strconv.Itoa(MaxLeaves)+" comparisons")
	}
	f, err := p.field()
	if err != nil {
		return nil, 0, err
	}
	l := &Leaf{Field: f}
	if p.word("nocase") {
		l.Fold = true
		if err := p.advance(); err != nil {
			return nil, 0, err
		}
	}
	opPos := p.tok.pos
	switch {
	case p.tok.kind == tokOp:
		return p.compare(l, p.tok.text)
	case p.word("last") && !l.Fold:
		return p.last(l)
	case p.tok.kind == tokIdent && opWords[p.tok.text] != 0:
		l.Op = opWords[p.tok.text]
	default:
		return nil, 0, fail(opPos, "expected a comparison after the field")
	}
	if l.Fold && l.Op != OpIn && l.Op != OpPrefix && l.Op != OpLike && l.Op != OpContains && l.Op != OpWords {
		return nil, 0, fail(opPos, `"nocase" goes before "=", "==", "in", "prefix", "like", "contains" or "words"; a "matches" pattern ignores case with (?i)`)
	}
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	switch l.Op {
	case OpPresent, OpAbsent:
	case OpIn:
		if err := p.list(l); err != nil {
			return nil, 0, err
		}
	case OpBetween:
		a, err := p.value()
		if err != nil {
			return nil, 0, err
		}
		if !p.word("and") {
			return nil, 0, fail(p.tok.pos, `expected "and" between the two values`)
		}
		if err := p.advance(); err != nil {
			return nil, 0, err
		}
		b, err := p.value()
		if err != nil {
			return nil, 0, err
		}
		l.Values = []Value{a, b}
	default:
		v, err := p.value()
		if err != nil {
			return nil, 0, err
		}
		l.Values = []Value{v}
	}
	if l.Op == OpContains || l.Op == OpWords {
		l.Fold = true
	}
	if l.Op == OpIn && !allText(l.Values) {
		l.Fold = false
	}
	return l, 1, nil
}

// compare finishes a leaf whose operator is a symbol.
func (p *parser) compare(l *Leaf, op string) (Node, int, error) {
	opPos := p.tok.pos
	if l.Fold && op != "=" && op != "==" {
		return nil, 0, fail(opPos, `"nocase" goes before "=", "==", "in", "prefix", "like", "contains" or "words"`)
	}
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	v, err := p.value()
	if err != nil {
		return nil, 0, err
	}
	l.Values = []Value{v}
	text := v.Kind == KindText
	switch op {
	case "=":
		l.Op, l.Fold = OpEqual, text
	case "==":
		l.Op, l.Fold = OpEqual, l.Fold && text
	case "!=":
		l.Op, l.Fold = OpEqual, text
		return &Not{Child: l}, 2, nil
	case "<":
		l.Op = OpLess
	case "<=":
		l.Op = OpLessEqual
	case ">":
		l.Op = OpGreater
	case ">=":
		l.Op = OpGreaterEqual
	}
	return l, 1, nil
}

// last finishes "field last N days": the field on or after N units back.
func (p *parser) last(l *Leaf) (Node, int, error) {
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	countPos := p.tok.pos
	v := p.tok.val
	if p.tok.kind != tokValue || v.Kind != KindNumber || v.Number < 0 || v.Number > MaxRelative || v.Number != float64(int(v.Number)) {
		return nil, 0, fail(countPos, "expected a whole number of days, weeks, months or years")
	}
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	units := map[string]byte{"day": 'd', "days": 'd', "week": 'w', "weeks": 'w', "month": 'm', "months": 'm', "year": 'y', "years": 'y'}
	u, ok := units[p.tok.text]
	if p.tok.kind != tokIdent || !ok {
		return nil, 0, fail(p.tok.pos, "expected days, weeks, months or years")
	}
	if err := p.advance(); err != nil {
		return nil, 0, err
	}
	l.Op = OpGreaterEqual
	l.Values = []Value{{Kind: KindRelative, Relative: int(v.Number), Unit: u}}
	return l, 1, nil
}

// list reads "(" value { "," value } ")".
func (p *parser) list(l *Leaf) error {
	if err := p.expect(tokLParen, `"(" before the list`); err != nil {
		return err
	}
	for {
		if len(l.Values) == MaxListValues {
			return fail(p.tok.pos, "a list has at most "+strconv.Itoa(MaxListValues)+" values")
		}
		v, err := p.value()
		if err != nil {
			return err
		}
		l.Values = append(l.Values, v)
		if p.tok.kind != tokComma {
			break
		}
		if err := p.advance(); err != nil {
			return err
		}
	}
	return p.expect(tokRParen, `"," or ")" in the list`)
}

// value reads one value.
func (p *parser) value() (Value, error) {
	var v Value
	switch {
	case p.tok.kind == tokString || p.tok.kind == tokValue:
		v = p.tok.val
	case p.word("today"):
		v = Value{Kind: KindRelative, Unit: 'd'}
	default:
		return Value{}, fail(p.tok.pos, "expected a value: a quoted string, a number, a date, a time or a relative date")
	}
	return v, p.advance()
}

// field reads a field, the lexer standing at its start, and leaves the
// token after it read.
func (p *parser) field() (Field, error) {
	var f Field
	p.lex.skipSpace()
	start := p.lex.pos
	if err := p.advance(); err != nil {
		return Field{}, err
	}
	if p.tok.kind == tokIdent && p.tok.text[0] >= 'a' && p.tok.text[0] <= 'z' {
		names := []string{}
		for {
			if p.tok.kind != tokIdent || !isCatalogName(p.tok.text) {
				return Field{}, fail(p.tok.pos, "a catalog field's name is lower-case letters, digits and \"_\", and is not a word of the language")
			}
			names = append(names, p.tok.text)
			if err := p.advance(); err != nil {
				return Field{}, err
			}
			if p.tok.kind != tokDot {
				break
			}
			if err := p.advance(); err != nil {
				return Field{}, err
			}
		}
		f.Catalog = strings.Join(names, ".")
		return f, nil
	}
	p.lex.pos = start
	for {
		if len(f.Path) == MaxPathSteps {
			return Field{}, fail(p.lex.spaceAt(p.lex.pos), "a path has at most "+strconv.Itoa(MaxPathSteps)+" steps")
		}
		s, err := p.step()
		if err != nil {
			return Field{}, err
		}
		f.Path = append(f.Path, s)
		if err := p.advance(); err != nil {
			return Field{}, err
		}
		if p.tok.kind != tokDot {
			break
		}
	}
	if p.tok.kind == tokLBracket {
		if err := p.advance(); err != nil {
			return Field{}, err
		}
		v := p.tok.val
		if p.tok.kind != tokValue || v.Kind != KindNumber || v.Number < 0 || v.Number > MaxIndex || v.Number != float64(int(v.Number)) {
			return Field{}, fail(p.tok.pos, "a value index is a whole number from 0 to "+strconv.Itoa(MaxIndex))
		}
		f.Indexed, f.Index = true, int(v.Number)
		if err := p.advance(); err != nil {
			return Field{}, err
		}
		if err := p.expect(tokRBracket, `"]"`); err != nil {
			return Field{}, err
		}
	}
	return f, nil
}

// step reads one step of a path, the lexer standing at it.
func (p *parser) step() (Step, error) {
	s, ok, err := p.lex.tag()
	if err != nil || ok {
		return s, err
	}
	t, err := p.lex.next()
	if err != nil {
		return Step{}, err
	}
	if t.kind != tokIdent || t.text[0] < 'A' || t.text[0] > 'Z' {
		return Step{}, fail(t.pos, "expected an attribute: a keyword, a tag (gggg,eeee) or a private tag (gggg,\"CREATOR\",ee)")
	}
	tg, ok := keywordTag(t.text)
	if !ok {
		return Step{}, fail(t.pos, "unknown attribute keyword "+strconv.Quote(t.text))
	}
	return Step{Tag: tg}, nil
}

// keywordTag returns the tag of a PS3.6 keyword in go-dicom's data
// dictionary.
func keywordTag(k string) (uint32, bool) {
	t := tag.GlobalDictionary().GetByKeyword(k)
	if t == 0 {
		return 0, false
	}
	return uint32(t), true
}

// tagKeyword returns the keyword of a tag, when the dictionary names it
// with one that names it back (a repeating group's keyword names a range,
// not one tag).
func tagKeyword(t uint32) (string, bool) {
	k := tag.Tag(t).GetKeyword()
	if k == "" {
		return "", false
	}
	if back, ok := keywordTag(k); !ok || back != t {
		return "", false
	}
	return k, true
}

// isCatalogName reports whether s is one name of a catalog field.
func isCatalogName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' || reserved[s] {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && !isDigit(c) && c != '_' {
			return false
		}
	}
	return true
}

// allText reports whether every value is text.
func allText(vs []Value) bool {
	for _, v := range vs {
		if v.Kind != KindText {
			return false
		}
	}
	return true
}
