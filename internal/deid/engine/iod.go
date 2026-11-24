package engine

import (
	"slices"
	"strings"
	"sync"

	"github.com/amrshadid/go-dicom/validate"
)

// Types gives an attribute's Type in the IOD of a SOP class (PS3.3, PS3.5
// 7.4), by the sequences it is inside, outermost first, and its tag; false
// when the SOP class or the attribute is not known there.
type Types interface {
	Type(sopClass string, sequences []uint32, tag uint32) (validate.AttributeType, bool)
}

// IODTypes is Types over the DICOM library's IOD tables (validate's
// IODForSOPClass and LookupTable): the modules of the SOP class's IOD, and
// for an enhanced multi-frame object the functional group macros under its
// Shared and Per-frame Functional Groups Sequences (PS3.3 C.7.6.16). When
// the IOD lists an attribute at a place more than once (two modules, or a
// macro included twice), the strictest Type is taken. Answers are kept, so
// each is worked out once; one IODTypes serves every caller at once.
type IODTypes struct {
	answers sync.Map // key string -> typeAnswer
}

// typeAnswer is a kept answer.
type typeAnswer struct {
	typ   validate.AttributeType
	known bool
}

// maxTableNesting bounds how often a table may include itself on one path:
// the SR content macros include each other without end (PS3.3 C.17.3).
const maxTableNesting = 12

// The sequences under which an enhanced object's functional group macros
// are (PS3.3 C.7.6.16).
const (
	sharedFunctionalGroups   = 0x52009229
	perFrameFunctionalGroups = 0x52009230
)

// Type implements Types.
func (it *IODTypes) Type(sopClass string, sequences []uint32, tag uint32) (validate.AttributeType, bool) {
	var key strings.Builder
	key.WriteString(sopClass)
	for _, s := range sequences {
		key.WriteString("/" + formatTag(s))
	}
	key.WriteString("/" + formatTag(tag))
	if a, ok := it.answers.Load(key.String()); ok {
		ans := a.(typeAnswer)
		return ans.typ, ans.known
	}
	typ, known := lookupType(sopClass, sequences, tag)
	it.answers.Store(key.String(), typeAnswer{typ, known})
	return typ, known
}

// lookupType works out Type for IODTypes.
func lookupType(sopClass string, sequences []uint32, tag uint32) (validate.AttributeType, bool) {
	iod, ok := validate.IODForSOPClass(sopClass)
	if !ok {
		return 0, false
	}
	var found []validate.AttributeType
	for _, m := range iod.Modules {
		found = findType(m.Table, sequences, tag, found)
	}
	if len(sequences) > 0 && (sequences[0] == sharedFunctionalGroups || sequences[0] == perFrameFunctionalGroups) {
		for _, g := range iod.FunctionalGroups {
			found = findType(g.Table, sequences[1:], tag, found)
		}
	}
	if len(found) == 0 {
		return 0, false
	}
	// The Types run from Type1 to Type3, strictest first.
	return slices.Min(found), true
}

// findType appends to found the Type of every row of the table labeled
// label, its includes followed, that is the attribute tag inside the
// sequences given.
func findType(label string, sequences []uint32, tag uint32, found []validate.AttributeType) []validate.AttributeType {
	m := &typeMatch{sequences: sequences, tag: tag, seen: map[string]int{}, found: found}
	m.walk(label, 0)
	return m.found
}

// typeMatch walks a table's rows in order, as PS3.3 writes them flat with
// a depth each, and follows the path: matched is how many of the sequences
// the rows above the current one have matched. An include is followed only
// where its rows could be on the path, which keeps the SR content macros'
// mutual inclusion from growing without end.
type typeMatch struct {
	sequences []uint32
	tag       uint32
	seen      map[string]int
	matched   int
	found     []validate.AttributeType
}

// walk visits the rows of the table labeled label at depth base.
func (m *typeMatch) walk(label string, base int) {
	t, ok := validate.LookupTable(label)
	if !ok || m.seen[label] >= maxTableNesting {
		return
	}
	m.seen[label]++
	defer func() { m.seen[label]-- }()
	for _, r := range t.Rows {
		d := base + r.Depth
		if r.Include != "" {
			// The included rows are at depth d and below, under the row at
			// d-1: only reachable when that row is on the path.
			if d <= m.matched && d <= len(m.sequences) {
				m.walk(r.Include, d)
			}
			continue
		}
		if d > m.matched {
			// Inside an attribute that is not on the path.
			continue
		}
		m.matched = d
		switch {
		case d < len(m.sequences) && r.Mask == 0 && r.Tag == m.sequences[d]:
			m.matched = d + 1
		case d == len(m.sequences) && tagMatches(r, m.tag):
			m.found = append(m.found, r.Type)
		}
	}
}

// tagMatches reports whether a row is the attribute tag: its tag, or for a
// repeating group the bits of its mask.
func tagMatches(r validate.Row, tag uint32) bool {
	if r.Mask == 0 {
		return r.Tag == tag
	}
	return tag&r.Mask == r.Tag&r.Mask
}
