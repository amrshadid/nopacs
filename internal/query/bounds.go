package query

import (
	"errors"
	"strconv"
)

// The bounds of a condition. A condition is text a person or a program
// writes and the server keeps, evaluates for every study a receiver routes
// and compiles for every search, so each bound keeps one condition from
// costing more than a person writes on purpose.
const (
	// MaxLength is the longest condition text, in bytes: room for
	// MaxLeaves leaves of ordinary length. Parse refuses longer text before
	// reading it.
	MaxLength = 16 << 10
	// MaxDepth is the deepest tree, a leaf counting one, and the most
	// parentheses, "not" and "has" nested in the text. The parser
	// recurses once a level, so the bound is also its stack's.
	MaxDepth = 16
	// MaxLeaves is the most leaves of one condition: the search compiles
	// each into a fragment of SQL with its parameters, and a dry run
	// evaluates each over every file it reads.
	MaxLeaves = 64
	// MaxListValues is the most values of one "in" list.
	MaxListValues = 100
	// MaxValueLength is the longest text value, in bytes: the most routing
	// reads of an attribute's value (MaxReadLength), so a longer value
	// could never be equal to one read.
	MaxValueLength = 1024
	// MaxPathSteps is the most steps of an attribute's path: an attribute
	// inside sequences nested this deep. PS3.3 nests few sequences more
	// than four deep.
	MaxPathSteps = 8
	// MaxCreatorLength is the longest private creator name: a private
	// creator element is LO, at most 64 characters (PS3.5 6.2, 7.8.1).
	MaxCreatorLength = 64
	// MaxIndex is the greatest value index: an attribute with more than
	// this many values is not one a person picks a value of by its place.
	MaxIndex = 1023
	// MaxRelative is the most units a relative date counts back: a
	// hundred thousand days is more than any archive's age.
	MaxRelative = 100000
)

// SyntaxError is a condition Parse refuses: Offset is the byte of the text
// where it went wrong, from 0. Its text names the offset and what was
// expected, and never quotes a value of the condition, which may be a
// patient's name in a routing rule.
type SyntaxError struct {
	// Offset is the byte where the condition went wrong, from 0.
	Offset int
	// Msg says what was wrong.
	Msg string
}

// Error returns the offset and the message.
func (e *SyntaxError) Error() string {
	return "query: at offset " + strconv.Itoa(e.Offset) + ": " + e.Msg
}

// ErrInvalid is the error a tree Check refuses wraps.
var ErrInvalid = errors.New("query: invalid condition")

// invalid returns an error wrapping ErrInvalid with why.
func invalid(why string) error {
	return &invalidError{why: why}
}

// invalidError is a tree Check refuses.
type invalidError struct{ why string }

func (e *invalidError) Error() string { return ErrInvalid.Error() + ": " + e.why }
func (e *invalidError) Unwrap() error { return ErrInvalid }
