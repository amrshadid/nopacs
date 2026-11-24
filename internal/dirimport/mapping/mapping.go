// Package mapping reads an import's map file: a CSV table that gives one
// value by a key of one or more others, such as a session's subject by its
// project and session labels, for a tree whose folders do not hold every
// label a site needs. Which columns form the key and which gives the value
// is the caller's configuration (Columns); nothing here names a column.
//
// # The form
//
// A CSV file (RFC 4180) in UTF-8, a byte order mark allowed at its start,
// whose first row names the columns. Each column the configuration names
// appears in the header exactly once, matched byte for byte; other
// columns are ignored. Every row has as many cells as the header, and its
// key and value cells are not empty. Blank lines are ignored. A key may be
// listed more than once with one value; naming two values for it refuses
// the whole map, since either choice would file data under a label the
// source did not say.
//
// # What it must never do
//
// A map holds labels as the source kept them, which can be names. No error
// of this package holds a cell, the header, or the file's path: an error
// names a line by its number and the fault, and its type has no field that
// could carry text from the file.
package mapping

import (
	"bytes"
	"encoding/csv"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The bounds of a map: one given inline with a job, and one read from a
// file under the import's root; the caller passes one of them to Parse.
// MaxKeyColumns bounds a key, more than any tree's nesting needs.
const (
	MaxInlineBytes = 3 << 20
	MaxFileBytes   = 64 << 20
	MaxKeyColumns  = 8
)

// Columns names the columns a map is read by, as the header names them.
type Columns struct {
	// Key names the columns whose cells form the key, in the order a
	// lookup gives them: at least one, at most MaxKeyColumns, each once.
	Key []string
	// Value names the column whose cell is the value, not one of Key.
	Value string
}

// ConfigError is a Columns refused. It names a column by its place in the
// configuration, never by its text.
type ConfigError struct {
	// Msg says what is wrong.
	Msg string
}

// Error returns the fault.
func (e *ConfigError) Error() string { return "mapping: the columns " + e.Msg }

// check returns nil when c can read a map, or a *ConfigError.
func (c Columns) check() error {
	switch {
	case len(c.Key) == 0:
		return &ConfigError{Msg: "name no key column"}
	case len(c.Key) > MaxKeyColumns:
		return &ConfigError{Msg: "name more than " + strconv.Itoa(MaxKeyColumns) + " key columns"}
	case c.Value == "":
		return &ConfigError{Msg: "name no value column"}
	case slices.Contains(c.Key, c.Value):
		return &ConfigError{Msg: "name the value column among the key columns"}
	}
	for i, k := range c.Key {
		switch {
		case k == "":
			return &ConfigError{Msg: "name key column " + strconv.Itoa(i+1) + " empty"}
		case slices.Contains(c.Key[:i], k):
			return &ConfigError{Msg: "name key column " + strconv.Itoa(i+1) + " as an earlier one"}
		}
	}
	return nil
}

// Fault says why a map is refused.
type Fault int

// The faults.
const (
	// TooLarge is a map larger than the limit given to Parse.
	TooLarge Fault = iota + 1
	// NotUTF8 is a map holding a byte sequence that is not UTF-8.
	NotUTF8
	// NoHeader is a map with no row at all.
	NoHeader
	// MissingColumn is a header lacking a column the configuration names.
	MissingColumn
	// DuplicateColumn is a header naming a configured column twice.
	DuplicateColumn
	// Malformed is a row that is not well-formed CSV, such as a quote not
	// closed or a bare quote inside a cell.
	Malformed
	// WrongCellCount is a row with more or fewer cells than the header.
	WrongCellCount
	// EmptyCell is a row with an empty key or value cell.
	EmptyCell
	// TwoValues is a row naming another value for a key an earlier row
	// named.
	TwoValues
)

// String returns the fault in words, as it appears in an error.
func (f Fault) String() string {
	switch f {
	case TooLarge:
		return "the map is larger than its limit"
	case NotUTF8:
		return "not UTF-8"
	case NoHeader:
		return "the map is empty, with no header row"
	case MissingColumn:
		return "the header lacks a configured column"
	case DuplicateColumn:
		return "the header names a configured column twice"
	case Malformed:
		return "not well-formed CSV"
	case WrongCellCount:
		return "another number of cells than the header"
	case EmptyCell:
		return "an empty key or value cell"
	case TwoValues:
		return "another value for a key an earlier line named"
	}
	return "Fault(" + strconv.Itoa(int(f)) + ")"
}

// Error is a map refused. It holds line numbers and the fault only.
type Error struct {
	// Line is the line at fault, from 1, or 0 when the fault is the whole
	// map's (TooLarge).
	Line int
	// FirstLine is, for TwoValues, the line that first named the key's
	// value; 0 otherwise.
	FirstLine int
	// Fault is what is wrong.
	Fault Fault
}

// Error returns the line numbers and the fault.
func (e *Error) Error() string {
	s := "mapping: "
	if e.Line > 0 {
		s += "line " + strconv.Itoa(e.Line) + ": "
	}
	s += e.Fault.String()
	if e.FirstLine > 0 {
		s += ", line " + strconv.Itoa(e.FirstLine)
	}
	return s
}

// entry is a key's value and the line that named it.
type entry struct {
	value string
	line  int
}

// Map gives a value by its key. It is not changed once made, so it is safe
// for concurrent use.
type Map struct {
	keyLen  int
	entries map[string]entry
}

// Lookup returns the value listed for key, its cells in the order of
// Columns.Key, and whether the map lists it. A key of another length is
// never listed.
func (m *Map) Lookup(key []string) (string, bool) {
	if len(key) != m.keyLen {
		return "", false
	}
	e, ok := m.entries[joinKey(key)]
	return e.value, ok
}

// Len returns how many keys the map lists.
func (m *Map) Len() int { return len(m.entries) }

// joinKey makes one string of a key's cells, each preceded by its length,
// so no two keys join alike whatever their cells hold.
func joinKey(cells []string) string {
	var b strings.Builder
	for _, c := range cells {
		b.WriteString(strconv.Itoa(len(c)))
		b.WriteByte(':')
		b.WriteString(c)
	}
	return b.String()
}

// bom is the UTF-8 byte order mark, which spreadsheet programs write at
// the start of a CSV file.
var bom = []byte("\xef\xbb\xbf")

// Parse reads a map of at most limit bytes (MaxInlineBytes or
// MaxFileBytes) by cols, or returns a *ConfigError for cols or an *Error
// for the map.
func Parse(data []byte, limit int, cols Columns) (*Map, error) {
	if err := cols.check(); err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, &Error{Fault: TooLarge}
	}
	if line := firstInvalidLine(data); line > 0 {
		return nil, &Error{Line: line, Fault: NotUTF8}
	}
	data = bytes.TrimPrefix(data, bom)
	r := csv.NewReader(bytes.NewReader(data))
	r.ReuseRecord = true
	header, err := r.Read()
	switch {
	case errors.Is(err, io.EOF):
		return nil, &Error{Line: 1, Fault: NoHeader}
	case err != nil:
		return nil, readError(err)
	}
	headerLine, _ := r.FieldPos(0)
	idx, fault := columns(header, append(slices.Clone(cols.Key), cols.Value))
	if fault != 0 {
		return nil, &Error{Line: headerLine, Fault: fault}
	}
	keyIdx, valueIdx := idx[:len(cols.Key)], idx[len(cols.Key)]
	m := &Map{keyLen: len(cols.Key), entries: map[string]entry{}}
	cells := make([]string, len(keyIdx))
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			return m, nil
		}
		if err != nil {
			return nil, readError(err)
		}
		line, _ := r.FieldPos(0)
		value := rec[valueIdx]
		empty := value == ""
		for i, j := range keyIdx {
			cells[i] = rec[j]
			empty = empty || rec[j] == ""
		}
		if empty {
			return nil, &Error{Line: line, Fault: EmptyCell}
		}
		k := joinKey(cells)
		if e, ok := m.entries[k]; ok {
			if e.value != value {
				return nil, &Error{Line: line, FirstLine: e.line, Fault: TwoValues}
			}
			continue
		}
		m.entries[k] = entry{value: value, line: line}
	}
}

// columns returns the index in header of each name in want, or the fault
// of a header without each exactly once.
func columns(header, want []string) ([]int, Fault) {
	idx := make([]int, len(want))
	for j, w := range want {
		idx[j] = -1
		for i, name := range header {
			if name != w {
				continue
			}
			if idx[j] >= 0 {
				return nil, DuplicateColumn
			}
			idx[j] = i
		}
		if idx[j] < 0 {
			return nil, MissingColumn
		}
	}
	return idx, 0
}

// readError turns an error of the CSV reader into an *Error naming its
// line. The reader's own error is not kept: its text is generic today, but
// nothing here relies on a library never quoting a cell.
func readError(err error) error {
	var pe *csv.ParseError
	if !errors.As(err, &pe) {
		return &Error{Fault: Malformed}
	}
	if errors.Is(pe.Err, csv.ErrFieldCount) {
		return &Error{Line: pe.StartLine, Fault: WrongCellCount}
	}
	return &Error{Line: pe.Line, Fault: Malformed}
}

// firstInvalidLine returns the line, from 1, of the first byte sequence in
// data that is not UTF-8, or 0 when all of it is.
func firstInvalidLine(data []byte) int {
	if utf8.Valid(data) {
		return 0
	}
	line := 1
	for len(data) > 0 {
		r, n := utf8.DecodeRune(data)
		if r == utf8.RuneError && n == 1 {
			return line
		}
		if r == '\n' {
			line++
		}
		data = data[n:]
	}
	return line
}
