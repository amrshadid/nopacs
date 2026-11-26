package query

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// tokKind is a token's kind.
type tokKind int

const (
	tokEOF tokKind = iota
	tokIdent
	tokString
	tokValue // a number, date, time, date-time or relative date
	tokOp    // = == != < <= > >=
	tokLParen
	tokRParen
	tokComma
	tokLBracket
	tokRBracket
	tokDot
)

// token is one token of a condition.
type token struct {
	kind tokKind
	text string // an identifier or an operator as written
	pos  int
	val  Value // a string or a value literal
}

// lexer reads tokens from a condition, on demand: the parser asks for the
// next token, or for a tag where a field may start, and may go back to a
// position it saw.
type lexer struct {
	src string
	pos int
}

// fail returns a *SyntaxError at pos.
func fail(pos int, msg string) error { return &SyntaxError{Offset: pos, Msg: msg} }

// skipSpace moves past white space.
func (l *lexer) skipSpace() {
	for l.pos < len(l.src) {
		switch l.src[l.pos] {
		case ' ', '\t', '\n', '\r':
			l.pos++
		default:
			return
		}
	}
}

func isLetter(c byte) bool { return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || c == '_' }
func isDigit(c byte) bool  { return '0' <= c && c <= '9' }
func isHex(c byte) bool    { return isDigit(c) || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F' }

// next returns the next token.
func (l *lexer) next() (token, error) {
	l.skipSpace()
	start := l.pos
	if l.pos >= len(l.src) {
		return token{kind: tokEOF, pos: start}, nil
	}
	c := l.src[l.pos]
	switch {
	case isLetter(c):
		for l.pos < len(l.src) && (isLetter(l.src[l.pos]) || isDigit(l.src[l.pos])) {
			l.pos++
		}
		return token{kind: tokIdent, text: l.src[start:l.pos], pos: start}, nil
	case c == '"':
		s, err := l.str()
		if err != nil {
			return token{}, err
		}
		return token{kind: tokString, pos: start, val: Value{Kind: KindText, Text: s}}, nil
	case isDigit(c) || c == '-' && l.pos+1 < len(l.src) && isDigit(l.src[l.pos+1]):
		v, err := l.literal()
		if err != nil {
			return token{}, err
		}
		return token{kind: tokValue, pos: start, val: v}, nil
	}
	l.pos++
	switch c {
	case '(':
		return token{kind: tokLParen, pos: start}, nil
	case ')':
		return token{kind: tokRParen, pos: start}, nil
	case ',':
		return token{kind: tokComma, pos: start}, nil
	case '[':
		return token{kind: tokLBracket, pos: start}, nil
	case ']':
		return token{kind: tokRBracket, pos: start}, nil
	case '.':
		return token{kind: tokDot, pos: start}, nil
	case '=', '<', '>', '!':
		op := string(c)
		if l.pos < len(l.src) && l.src[l.pos] == '=' {
			l.pos++
			op += "="
		}
		if op == "!" {
			return token{}, fail(start, `"!" is written "!="`)
		}
		return token{kind: tokOp, text: op, pos: start}, nil
	}
	return token{}, fail(start, "unexpected character")
}

// str reads a quoted string at l.pos: '"' text '"', in which only `\"`
// and `\\` are escapes. A control character, text that is not UTF-8, or
// more than MaxValueLength bytes is refused.
func (l *lexer) str() (string, error) {
	start := l.pos
	l.pos++
	var b strings.Builder
	for {
		if l.pos >= len(l.src) {
			return "", fail(start, "a string is not closed")
		}
		c := l.src[l.pos]
		switch {
		case c == '"':
			l.pos++
			if b.Len() > MaxValueLength {
				return "", fail(start, "a string is longer than "+strconv.Itoa(MaxValueLength)+" bytes")
			}
			return b.String(), nil
		case c == '\\':
			if l.pos+1 >= len(l.src) || l.src[l.pos+1] != '"' && l.src[l.pos+1] != '\\' {
				return "", fail(l.pos, `the only escapes in a string are \" and \\`)
			}
			b.WriteByte(l.src[l.pos+1])
			l.pos += 2
		case c < 0x20 || c == 0x7f:
			return "", fail(l.pos, "a string holds a control character")
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			l.pos++
		default:
			r, n := utf8.DecodeRuneInString(l.src[l.pos:])
			if r == utf8.RuneError {
				return "", fail(l.pos, "a string is not UTF-8")
			}
			if r < 0xa0 {
				return "", fail(l.pos, "a string holds a control character")
			}
			b.WriteString(l.src[l.pos : l.pos+n])
			l.pos += n
		}
	}
}

// digitsAt returns how many digits are at i.
func (l *lexer) digitsAt(i int) int {
	n := 0
	for i+n < len(l.src) && isDigit(l.src[i+n]) {
		n++
	}
	return n
}

// matchAt reports whether the text at i has the shape of pattern, in which
// 'd' stands for a digit and every other byte for itself.
func (l *lexer) matchAt(i int, pattern string) bool {
	if i+len(pattern) > len(l.src) {
		return false
	}
	for j := 0; j < len(pattern); j++ {
		c := l.src[i+j]
		if pattern[j] == 'd' {
			if !isDigit(c) {
				return false
			}
		} else if c != pattern[j] {
			return false
		}
	}
	return true
}

// literal reads a number, date (2024-01-31), time (10:30 or 10:30:15),
// date-time (2024-01-31T10:30:15) or relative date (-30d) at l.pos.
func (l *lexer) literal() (Value, error) {
	start := l.pos
	var v Value
	switch {
	case l.matchAt(start, "dddd-dd-dd"):
		date := l.src[start:start+4] + l.src[start+5:start+7] + l.src[start+8:start+10]
		l.pos += 10
		v = Value{Kind: KindDate, Text: date}
		if l.pos < len(l.src) && l.src[l.pos] == 'T' {
			l.pos++
			t, ok := l.clock()
			if !ok {
				return Value{}, fail(start, "a date and time is written 2024-01-31T10:30:00")
			}
			v = Value{Kind: KindDateTime, Text: date + t}
		}
	case l.matchAt(start, "dd:dd"):
		t, _ := l.clock()
		v = Value{Kind: KindTime, Text: t}
	case l.src[start] == '-' && l.relativeAt(start+1):
		n := l.digitsAt(start + 1)
		count, err := strconv.Atoi(l.src[start+1 : start+1+n])
		if err != nil || count > MaxRelative {
			return Value{}, fail(start, "a relative date counts back at most "+strconv.Itoa(MaxRelative)+" units")
		}
		l.pos = start + 1 + n + 1
		v = Value{Kind: KindRelative, Relative: count, Unit: l.src[start+1+n]}
	default:
		end := start
		if l.src[end] == '-' {
			end++
		}
		end += l.digitsAt(end)
		if end < len(l.src) && l.src[end] == '.' && end+1 < len(l.src) && isDigit(l.src[end+1]) {
			end++
			end += l.digitsAt(end)
		}
		if end < len(l.src) && (l.src[end] == 'e' || l.src[end] == 'E') {
			e := end + 1
			if e < len(l.src) && (l.src[e] == '+' || l.src[e] == '-') {
				e++
			}
			if n := l.digitsAt(e); n > 0 {
				end = e + n
			}
		}
		f, err := strconv.ParseFloat(l.src[start:end], 64)
		if err != nil || math.IsInf(f, 0) || math.IsNaN(f) {
			return Value{}, fail(start, "a number is out of range")
		}
		l.pos = end
		v = Value{Kind: KindNumber, Number: f}
	}
	if l.pos < len(l.src) && (isLetter(l.src[l.pos]) || isDigit(l.src[l.pos])) {
		return Value{}, fail(start, "a value runs into a letter or digit")
	}
	return v, nil
}

// relativeAt reports whether a relative date's count and unit are at i:
// digits, then d, w, m or y.
func (l *lexer) relativeAt(i int) bool {
	n := l.digitsAt(i)
	if n == 0 || i+n >= len(l.src) {
		return false
	}
	switch l.src[i+n] {
	case 'd', 'w', 'm', 'y':
		return i+n+1 >= len(l.src) || !isLetter(l.src[i+n+1]) && !isDigit(l.src[i+n+1])
	}
	return false
}

// clock reads HH:MM or HH:MM:SS at l.pos and returns it as HHMMSS.
func (l *lexer) clock() (string, bool) {
	i := l.pos
	if !l.matchAt(i, "dd:dd") {
		return "", false
	}
	t := l.src[i:i+2] + l.src[i+3:i+5]
	l.pos += 5
	if l.matchAt(l.pos, ":dd") {
		t += l.src[l.pos+1 : l.pos+3]
		l.pos += 3
	} else {
		t += "00"
	}
	return t, true
}

// tag reads a tag where a field may start, if one is there: (gggg,eeee),
// or (gggg,"CREATOR",ee) for a private tag by its creator, with spaces
// allowed after the parenthesis and around the commas. It reports false,
// moving nothing, when the text there is not a tag's start: "(" then four
// hexadecimal digits and a comma; past that start, a malformed tag is an
// error.
func (l *lexer) tag() (Step, bool, error) {
	l.skipSpace()
	start := l.pos
	i := start
	if i >= len(l.src) || l.src[i] != '(' {
		return Step{}, false, nil
	}
	i = l.spaceAt(i + 1)
	if i+4 > len(l.src) || !isHex(l.src[i]) || !isHex(l.src[i+1]) || !isHex(l.src[i+2]) || !isHex(l.src[i+3]) {
		return Step{}, false, nil
	}
	group, _ := strconv.ParseUint(l.src[i:i+4], 16, 16)
	i = l.spaceAt(i + 4)
	if i >= len(l.src) || l.src[i] != ',' {
		return Step{}, false, nil
	}
	i = l.spaceAt(i + 1)
	malformed := fail(start, `a tag is written (gggg,eeee) or (gggg,"CREATOR",ee)`)
	if i < len(l.src) && l.src[i] == '"' {
		l.pos = i
		creator, err := l.str()
		if err != nil {
			return Step{}, false, err
		}
		i = l.spaceAt(l.pos)
		if i >= len(l.src) || l.src[i] != ',' {
			return Step{}, false, malformed
		}
		i = l.spaceAt(i + 1)
		if i+2 > len(l.src) || !isHex(l.src[i]) || !isHex(l.src[i+1]) {
			return Step{}, false, malformed
		}
		elem, _ := strconv.ParseUint(l.src[i:i+2], 16, 8)
		i = l.spaceAt(i + 2)
		if i >= len(l.src) || l.src[i] != ')' {
			return Step{}, false, malformed
		}
		l.pos = i + 1
		// The creator is compared without the space that pads it to an even
		// length (PS3.5 6.2), as the data set's creator is.
		return Step{Tag: uint32(group)<<16 | uint32(elem), Creator: strings.TrimRight(creator, " ")}, true, nil
	}
	if i+4 > len(l.src) || !isHex(l.src[i]) || !isHex(l.src[i+1]) || !isHex(l.src[i+2]) || !isHex(l.src[i+3]) {
		return Step{}, false, malformed
	}
	elem, _ := strconv.ParseUint(l.src[i:i+4], 16, 16)
	i = l.spaceAt(i + 4)
	if i >= len(l.src) || l.src[i] != ')' {
		return Step{}, false, malformed
	}
	l.pos = i + 1
	return Step{Tag: uint32(group)<<16 | uint32(elem)}, true, nil
}

// spaceAt returns the first byte at or after i that is not white space.
func (l *lexer) spaceAt(i int) int {
	for i < len(l.src) && (l.src[i] == ' ' || l.src[i] == '\t' || l.src[i] == '\n' || l.src[i] == '\r') {
		i++
	}
	return i
}
