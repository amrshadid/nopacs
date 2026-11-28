package sidecar

import (
	"encoding/json"
	"fmt"
	"strconv"
)

// MaxUnnamed is the most distinct elements one line lists as unnamed
// (Instance.Unnamed): the bound of one file's record, past which the rest are
// counted in Instance.UnnamedOver.
const MaxUnnamed = 1024

// maxUnnamedCreator is the longest private creator a line names: the length
// of an LO value (PS3.5 6.2).
const maxUnnamedCreator = 64

// maxUnnamedText is the longest one element's text may be in a line's list:
// ["gggg00ee","VR",count,"creator",true] with a creator of the most
// printable bytes, each written as a two-byte escape at worst.
const maxUnnamedText = 2 + 10 + 1 + 4 + 1 + 11 + 1 + 2*maxUnnamedCreator + 2 + 6 + 1

// UnnamedElement is an element of the file as received that no rule of the
// profile named, so a default decided it: one element by its tag, or by its
// private creator and element, with its VR and how many times the file held
// it. It holds no value. A line lists them so a report of what a tenant's
// profile never decided is made from the metadata alone.
//
// It is written as the array ["gggg00ee","VR",count] or, with a creator,
// ["gggg00ee","VR",count,"creator"], or, for a creator that is not given,
// ["gggg00ee","VR",count,"",true]: the tag as eight upper case hexadecimal
// digits, a private element's block number (PS3.5 7.8.1) written 00 because
// a file chooses it.
type UnnamedElement struct {
	// Tag is the element's tag; for a private element its group and its
	// element within the block, gggg00ee.
	Tag uint32
	// VR is the VR as the engine read it, two upper case letters.
	VR string
	// Count is how many times the file held it, at any depth.
	Count int
	// Creator is the private creator of a private element's block, printable
	// ASCII of at most 64 bytes; empty for a standard element, for one whose
	// block has no creator, and when Withheld.
	Creator string
	// Withheld says the creator is not given: it was not printable ASCII of at
	// most 64 bytes, or held a value the profile removed or a term of its
	// lists, which a report must not name.
	Withheld bool
}

// Private reports whether the element is private (an odd group).
func (u UnnamedElement) Private() bool { return (u.Tag>>16)%2 == 1 }

// MarshalJSON writes the element as its array.
func (u UnnamedElement) MarshalJSON() ([]byte, error) {
	b := make([]byte, 0, 40+len(u.Creator))
	b = append(b, '[', '"')
	const hexdigits = "0123456789ABCDEF"
	for shift := 28; shift >= 0; shift -= 4 {
		b = append(b, hexdigits[(u.Tag>>uint(shift))&0xF])
	}
	b = append(b, '"', ',')
	b = strconv.AppendQuote(b, u.VR)
	b = append(b, ',')
	b = strconv.AppendInt(b, int64(u.Count), 10)
	if u.Creator != "" || u.Withheld {
		b = append(b, ',')
		enc, err := json.Marshal(u.Creator)
		if err != nil {
			return nil, err
		}
		b = append(b, enc...)
		if u.Withheld {
			b = append(b, ",true"...)
		}
	}
	return append(b, ']'), nil
}

// UnmarshalJSON reads an element written as MarshalJSON writes it, and
// nothing else.
func (u *UnnamedElement) UnmarshalJSON(b []byte) error {
	var parts []json.RawMessage
	if err := json.Unmarshal(b, &parts); err != nil || len(parts) < 3 || len(parts) > 5 {
		return fmt.Errorf("%w: an unnamed element is [tag,vr,count] with an optional creator and withheld", ErrFormat)
	}
	var tag, vr string
	var count int
	if json.Unmarshal(parts[0], &tag) != nil || json.Unmarshal(parts[1], &vr) != nil || json.Unmarshal(parts[2], &count) != nil {
		return fmt.Errorf("%w: an unnamed element's tag and VR are text and its count a number", ErrFormat)
	}
	n, err := strconv.ParseUint(tag, 16, 32)
	if err != nil || len(tag) != 8 || tag != fmt.Sprintf("%08X", n) {
		return fmt.Errorf("%w: an unnamed element's tag is eight upper case hexadecimal digits", ErrFormat)
	}
	out := UnnamedElement{Tag: uint32(n), VR: vr, Count: count}
	if len(parts) >= 4 {
		if json.Unmarshal(parts[3], &out.Creator) != nil {
			return fmt.Errorf("%w: an unnamed element's creator is text", ErrFormat)
		}
	}
	if len(parts) == 5 {
		if string(parts[4]) != "true" {
			return fmt.Errorf("%w: an unnamed element's fifth member is true or absent", ErrFormat)
		}
		out.Withheld = true
	}
	*u = out
	return nil
}

// validate returns an ErrFormat error unless the element follows the rules
// of a line: a block number of 00 for a private element, a VR of two upper
// case letters, a count of at least one, and a creator only where one can
// be, printable ASCII of at most 64 bytes, or withheld alone.
func (u UnnamedElement) validate() error {
	switch {
	case u.Private() && u.Tag&0xFF00 != 0:
		return fmt.Errorf("%w: a private unnamed element's block number is written 00", ErrFormat)
	case len(u.VR) != 2 || u.VR[0] < 'A' || u.VR[0] > 'Z' || u.VR[1] < 'A' || u.VR[1] > 'Z':
		return fmt.Errorf("%w: an unnamed element's VR is two upper case letters", ErrFormat)
	case u.Count < 1:
		return fmt.Errorf("%w: an unnamed element is held at least once", ErrFormat)
	case !u.Private() && (u.Creator != "" || u.Withheld):
		return fmt.Errorf("%w: only a private unnamed element has a creator", ErrFormat)
	case u.Withheld && u.Creator != "":
		return fmt.Errorf("%w: a withheld creator is not given", ErrFormat)
	}
	if len(u.Creator) > maxUnnamedCreator {
		return fmt.Errorf("%w: an unnamed element's creator is at most %d bytes", ErrFormat, maxUnnamedCreator)
	}
	for i := range len(u.Creator) {
		if c := u.Creator[i]; c < 0x20 || c > 0x7E {
			return fmt.Errorf("%w: an unnamed element's creator is printable ASCII", ErrFormat)
		}
	}
	return nil
}

// checkUnnamed returns an ErrFormat error unless the line's list of unnamed
// elements follows its rules: at most MaxUnnamed elements, in the order the
// file first held them, none repeated, each valid; and a count of elements
// past the bound only with the list full.
func checkUnnamed(in Instance) error {
	if in.Unnamed == nil {
		if in.UnnamedOver != 0 {
			return fmt.Errorf("%w: unnamed_over goes with unnamed", ErrFormat)
		}
		return nil
	}
	if len(in.Unnamed) > MaxUnnamed || in.UnnamedOver < 0 || (in.UnnamedOver > 0 && len(in.Unnamed) < MaxUnnamed) {
		return fmt.Errorf("%w: unnamed lists at most %d elements and unnamed_over counts past a full list", ErrFormat, MaxUnnamed)
	}
	seen := make(map[unnamedKey]bool, len(in.Unnamed))
	for _, u := range in.Unnamed {
		if err := u.validate(); err != nil {
			return err
		}
		k := unnamedKey{u.Tag, u.Creator, u.Withheld, u.VR}
		if seen[k] {
			return fmt.Errorf("%w: an unnamed element is listed once", ErrFormat)
		}
		seen[k] = true
	}
	return nil
}

// unnamedKey is what makes one unnamed element of a line.
type unnamedKey struct {
	tag      uint32
	creator  string
	withheld bool
	vr       string
}
