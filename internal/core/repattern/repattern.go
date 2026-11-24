package repattern

import (
	"errors"
	"regexp"
	"regexp/syntax"
	"strconv"
)

// The bounds of one pattern.
const (
	// MaxLength is the longest pattern text, in bytes.
	MaxLength = 256
	// MaxRepeat is the largest count of a repetition, such as {100}.
	MaxRepeat = 100
	// MaxCost is the largest compiled size of one pattern: its
	// instructions and the ranges of its character classes.
	MaxCost = 10000
)

// Compile returns the pattern compiled to match a whole value, and its
// compiled size, which a caller adds up against its own budget, or an
// error saying in plain words why the pattern is refused. The pattern is
// parsed alone first, so a text such as "a)|(b" cannot escape the anchors
// around it, and measured on its compiled program before the regexp
// package compiles it.
func Compile(p string) (*regexp.Regexp, int, error) {
	cost, err := Measure(p)
	if err != nil {
		return nil, 0, err
	}
	full, err := regexp.Compile(`\A(?:` + p + `)\z`)
	if err != nil {
		return nil, 0, errors.New("the pattern does not compile: " + err.Error())
	}
	return full, cost, nil
}

// CompileSearch returns the pattern compiled to find its matches anywhere
// in a value, within the same bounds as Compile, and its compiled size.
// RE2 finds every match of a value in time linear in its length. Matching
// is leftmost-longest (POSIX), not leftmost-first: "MRN|MRN\d+" finds
// "MRN123456" whole, so a cut or a needle is never only the first
// alternative's prefix of what the pattern describes.
func CompileSearch(p string) (*regexp.Regexp, int, error) {
	cost, err := Measure(p)
	if err != nil {
		return nil, 0, err
	}
	re, err := regexp.Compile(p)
	if err != nil {
		return nil, 0, errors.New("the pattern does not compile: " + err.Error())
	}
	re.Longest()
	return re, cost, nil
}

// Measure checks a pattern against the bounds without the regexp
// package's own compilation, whose cost is what is bounded, and returns
// its compiled size: its instructions and the ranges of its character
// classes. The error says in plain words why the pattern is refused.
func Measure(p string) (int, error) {
	if len(p) > MaxLength {
		return 0, errors.New("a pattern is at most " + strconv.Itoa(MaxLength) + " bytes")
	}
	re, err := syntax.Parse(p, syntax.Perl)
	if err != nil {
		return 0, errors.New("the pattern is not a regular expression of RE2: " + err.Error())
	}
	if n := largestRepeat(re); n > MaxRepeat {
		return 0, errors.New("a repetition counts at most " + strconv.Itoa(MaxRepeat) + ", and this one " + strconv.Itoa(n))
	}
	prog, err := syntax.Compile(re.Simplify())
	if err != nil {
		return 0, errors.New("the pattern does not compile: " + err.Error())
	}
	cost := len(prog.Inst)
	for _, in := range prog.Inst {
		cost += len(in.Rune) / 2
	}
	if cost > MaxCost {
		return 0, errors.New("the pattern compiles to " + strconv.Itoa(cost) + " instructions and ranges, more than " + strconv.Itoa(MaxCost))
	}
	return cost, nil
}

// largestRepeat returns the largest count of a repetition in re, such as
// 999 for x{2,999}.
func largestRepeat(re *syntax.Regexp) int {
	n := 0
	if re.Op == syntax.OpRepeat {
		n = max(re.Min, re.Max)
	}
	for _, sub := range re.Sub {
		n = max(n, largestRepeat(sub))
	}
	return n
}
