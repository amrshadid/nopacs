package engine

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// MaxUnnamed is the most distinct elements one file's record lists that
// no rule named (Record.Unnamed); past it they are counted only
// (Record.UnnamedOver), so a file of many private blocks cannot make the
// record grow without bound.
const MaxUnnamed = 1024

// maxUnnamedCreator is the longest private creator an Unnamed names: the
// length of an LO value (PS3.5 6.2).
const maxUnnamedCreator = 64

// Unnamed is an element of a file that no rule of the profile named, so a
// default decided it (a private element removed, any other as the profile's
// unnamed says): one element by its tag, or by its private creator and
// element, with its VR and how many times the file holds it. It holds no
// value. Counted per tenant over many files, it shows what a profile never
// decided, the private elements a vendor writes first among them.
//
// An element inside a sequence a rule removed or emptied is not visited
// apart from it and is not listed: its sequence's rule decided it.
type Unnamed struct {
	// Tag is a standard element's tag; for a private data element its group
	// and its element within the block, gggg00ee: the block number, which
	// a file chooses (PS3.5 7.8.1), is written 00.
	Tag uint32
	// Creator is the private creator of a private element's block, without
	// padding; empty for a standard element, a private element whose block
	// has no creator, and a creator that is not printable ASCII of at most
	// 64 bytes or that holds a term of the profile's lists
	// (CreatorUnreadable).
	Creator string
	// CreatorUnreadable reports a private creator that is not printable
	// ASCII of at most 64 bytes, or that holds a term of the profile's
	// lists or a value the profile removed from the file (a needle, as
	// clean finds it), which is not given: the record names no text it
	// cannot show is a vendor's name.
	CreatorUnreadable bool
	// VR is the element's VR as the engine read it ("UN" for a private
	// element of a file in Implicit VR).
	VR string
	// Count is how many times the file holds it, at any depth.
	Count int
}

// Private reports whether u is a private element.
func (u Unnamed) Private() bool { return (u.Tag>>16)%2 == 1 }

// String writes u as "gggg,eeee VR" or, for a private element,
// "gggg,xxee VR creator", without a value.
func (u Unnamed) String() string {
	if u.Private() {
		return fmt.Sprintf("%04X,xx%02X %s %q", u.Tag>>16, u.Tag&0xFF, u.VR, u.Creator)
	}
	return fmt.Sprintf("%04X,%04X %s", u.Tag>>16, u.Tag&0xFFFF, u.VR)
}

// unreadableCreator is the number a creator not given has in a key.
const unreadableCreator = 0xFFFF

// noteUnnamed counts an element no rule named, of tag t, in the block of
// creator, of VR vr. An entry is found by a key of plain numbers (the tag,
// the creator's number among the file's creators, the VR's two letters),
// so counting hashes no string and takes no allocation but the entry's.
func (r *run) noteUnnamed(t uint32, creator, vr string) {
	var id uint64
	given := ""
	if (t>>16)%2 == 1 {
		t = t&0xFFFF0000 | t&0xFF
		id = uint64(r.creatorID(creator))
		if id != unreadableCreator {
			given = creator
		}
	}
	var code uint64
	if len(vr) == 2 {
		code = uint64(vr[0])<<8 | uint64(vr[1])
	}
	k := uint64(t)<<32 | id<<16 | code
	if i, ok := r.unnamedAt[k]; ok {
		r.record.Unnamed[i].Count++
		return
	}
	if r.addUnnamed(Unnamed{Tag: t, Creator: given, CreatorUnreadable: id == unreadableCreator, VR: vr, Count: 1}) {
		if r.unnamedAt == nil {
			r.unnamedAt = make(map[uint64]int, r.unnamedRoom())
		}
		r.unnamedAt[k] = len(r.record.Unnamed) - 1
	}
}

// creatorID returns a private creator's number among the file's creators,
// from 1, judged once: unreadableCreator for one that is not printable
// ASCII of at most 64 bytes or holds a term of the profile's lists.
func (r *run) creatorID(creator string) uint16 {
	if id, ok := r.creators[creator]; ok {
		return id
	}
	if r.creators == nil {
		r.creators = map[string]uint16{}
	}
	id := uint16(unreadableCreator)
	if printableCreator(creator) && !r.holdsListTerm(creator) && len(r.creators) < unreadableCreator-1 {
		id = uint16(len(r.creators) + 1) //nolint:gosec // G115: bounded just above.
	}
	r.creators[creator] = id
	return id
}

// hideCreators withholds, once every needle of the file is known, each
// private creator of the unnamed report that holds one of them, or a part
// of a name of at least minCleanLength characters, in any case or width
// (a patient's name written as a block's creator), merging the entries
// that then name the same element. It errs toward withholding: a part is
// matched inside other words too, and the element stays counted.
func (r *run) hideCreators() {
	// The file's creators the report gives (a few; many elements of their
	// blocks), each judged once.
	var creators []string
	for id := range r.creators {
		if r.creators[id] != unreadableCreator {
			creators = append(creators, id)
		}
	}
	if len(creators) == 0 {
		return
	}
	patterns := r.needlePatterns()
	if len(patterns) == 0 {
		return
	}
	hidden := map[string]bool{}
	if len(creators)*len(patterns) <= maxPlainComparisons {
		for _, c := range creators {
			folded := profile.FoldText(c)
			for _, p := range patterns {
				if strings.Contains(folded, string(p)) {
					hidden[c] = true
					break
				}
			}
		}
	} else {
		m := textmatch.New(patterns)
		for _, c := range creators {
			budget := int64(maxCleanWork)
			if _, err := m.Scan(r.ctx, []byte(profile.FoldText(c)), &budget, func(int, int) { hidden[c] = true }); err != nil {
				hidden[c] = true
			}
		}
	}
	if len(hidden) == 0 {
		return
	}
	for i := range r.record.Unnamed {
		if u := &r.record.Unnamed[i]; hidden[u.Creator] {
			u.Creator, u.CreatorUnreadable = "", true
		}
	}
	r.mergeUnnamed()
}

// maxPlainComparisons is the most creator-by-needle comparisons
// hideCreators makes one by one; past it one automaton of the needles
// searches each creator in one pass.
const maxPlainComparisons = 4096

// needlePatterns returns the file's names and identifier needles, and for
// a name each part of it, folded (profile.FoldText), each once; a part
// shorter than minCleanLength matches inside ordinary words and is not
// taken. A UID or a date in a creator would be no person's name.
func (r *run) needlePatterns() [][]byte {
	seen := map[string]bool{}
	var patterns [][]byte
	add := func(p string) {
		f := profile.FoldText(strings.TrimSpace(p))
		if f == "" || seen[f] || (utf8.RuneCountInString(f) < minCleanLength && !Unspaced(f)) {
			return
		}
		seen[f] = true
		patterns = append(patterns, []byte(f))
	}
	for _, n := range r.record.Needles {
		switch n.Kind {
		case NeedleName:
			add(n.Value)
			for _, p := range strings.FieldsFunc(n.Value, func(c rune) bool { return c == '^' || c == '=' || c == ' ' }) {
				add(p)
			}
		case NeedleText:
			add(n.Value)
		}
	}
	return patterns
}

// mergeUnnamed merges the private entries that name the same element once
// their creators were withheld; standard entries are distinct already.
func (r *run) mergeUnnamed() {
	merged := r.record.Unnamed[:0]
	for _, u := range r.record.Unnamed {
		if u.Private() && u.CreatorUnreadable {
			if i := slices.IndexFunc(merged, func(m Unnamed) bool {
				return m.CreatorUnreadable && m.Tag == u.Tag && m.VR == u.VR
			}); i >= 0 {
				merged[i].Count += u.Count
				continue
			}
		}
		merged = append(merged, u)
	}
	r.record.Unnamed = merged
}

// unnamedRoom is the room the report is made with: the top level's
// elements, at most MaxUnnamed, so it is made once rather than grown by
// doubling as a header of a few hundred elements is walked.
func (r *run) unnamedRoom() int {
	return max(16, min(r.top.Length(), MaxUnnamed))
}

// addUnnamed appends u to the record, or counts it over the bound, and
// reports whether it was appended.
func (r *run) addUnnamed(u Unnamed) bool {
	if len(r.record.Unnamed) >= MaxUnnamed {
		r.record.UnnamedOver++
		return false
	}
	if r.record.Unnamed == nil {
		r.record.Unnamed = make([]Unnamed, 0, r.unnamedRoom())
	}
	r.record.Unnamed = append(r.record.Unnamed, u)
	return true
}

// holdsListTerm reports whether s holds a term of the profile's lists, or
// could not be searched for one: such a creator is not given.
func (r *run) holdsListTerm(s string) bool {
	if r.en.lists == nil || s == "" {
		return false
	}
	budget := int64(len(s)) * profile.MaxTermBytes
	spans, err := r.en.lists.Find(r.ctx, []rune(profile.NFC(s)), r.en.lists.All(), &budget)
	return err != nil || len(spans) > 0
}

// printableCreator reports whether a creator is empty or printable ASCII of
// at most maxUnnamedCreator bytes.
func printableCreator(s string) bool {
	if len(s) > maxUnnamedCreator {
		return false
	}
	for i := range len(s) {
		if s[i] < 0x20 || s[i] > 0x7E {
			return false
		}
	}
	return true
}
