package profile

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/tag"
	"github.com/amrshadid/go-dicom/valuerep"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// Tier is how specific a selector is. Of the rules that apply to an
// element, the one of the highest tier decides; between rules of one tier,
// the later. The order is the one the contract states on ProfileDocument.
type Tier int

// The tiers, from the least specific.
const (
	// TierVR: every element of a VR.
	TierVR Tier = iota + 1
	// TierValue: every element of a few text VRs whose value matches a
	// pattern. It names no element, so every selector that names one by
	// where it is (a group, a private block, a tag, a path) is more
	// specific; it chooses among the values of a few VRs, so it is more
	// specific than a whole VR.
	TierValue
	// TierGroup: every element of a group or family of groups.
	TierGroup
	// TierPrivateBlock: every element of a private creator's block.
	TierPrivateBlock
	// TierPrivateElement: one private element, by creator.
	TierPrivateElement
	// TierTag: one standard element by tag or keyword, at any depth.
	TierTag
	// TierPlace: one element at one place in sequences, or a content
	// item's value.
	TierPlace
)

// Bounds of a selector's text.
const (
	// MaxPathSteps is the most steps of a path.
	MaxPathSteps = 16
	// maxCreatorLength is the bound of a private creator: an LO value
	// (PS3.5 6.2).
	maxCreatorLength = 64
	// maxSchemeLength is the bound of a Coding Scheme Designator, an SH
	// value.
	maxSchemeLength = 16
	// maxCodeValueLength is the bound of a Code Value, an LO value; a
	// longer code is a Long Code Value (0008,0119), which a profile does not
	// name.
	maxCodeValueLength = 64
)

// tagSequenceVR is the VR of a sequence (PS3.5 7.5).
const tagSequenceVR = "SQ"

// isCreatorElement reports whether t is a private creator element
// (gggg,0010-00FF), which reserves a block (PS3.5 7.8.1).
func isCreatorElement(t uint32) bool {
	return group(t)%2 == 1 && t&0xFF00 == 0 && t&0xFF >= 0x10
}

// Concept is a coded concept (PS3.3 8.8): a coding scheme and a code value.
type Concept struct {
	// Scheme is the Coding Scheme Designator (0008,0102).
	Scheme string
	// Value is the Code Value (0008,0100).
	Value string
}

// step is a step of a path above its last element: a sequence, any item of
// it; or any number of sequence levels.
type step struct {
	anyDepth bool
	tag      uint32
}

// selector is a compiled ProfileSelector.
type selector struct {
	tier Tier
	// tag is the element of a tag, a keyword or a path's last step.
	tag uint32
	// steps are a path's steps above its last element; nil for a top-level
	// path of one step, which still differs from a tag (it does not reach
	// into sequences): path tells them apart.
	steps []step
	path  bool
	// groupValue and groupMask match a group: (group & mask) == value.
	groupValue, groupMask uint16
	vr                    string
	creator               string
	privGroup             uint16
	// privElem is a private element's byte within its block, or -1 for
	// every element of the block.
	privElem int
	concept  *Concept
	// valueType is a content item's value type; empty for any.
	valueType string
	// value is a value pattern's compiled pattern.
	value valuePattern
	// includePrivate is a group, value pattern or VR selector's opt-in to
	// private elements.
	includePrivate bool
	// vrs is what the selector says of the VR of what it selects: the
	// dictionary's VRs of a tag (several where PS3.6 lists "US or SS"), the
	// VR of a VR selector, those of a content item's value elements; nil
	// when it says nothing. A stored expanded form gives them.
	vrs []string
}

// parseSelector compiles a selector and returns it with its normalized form,
// in which a keyword is its tag and hexadecimal is upper case. ok is false
// when a problem was recorded. For a document, the VRs of what it selects
// come from the data dictionary; for a stored expanded form (a.c.stored),
// stored gives them, as validation decided them, and the dictionary is not
// read.
func parseSelector(a at, s *v1.ProfileSelector, stored []string) (selector, *v1.ProfileSelector, bool) {
	before := len(a.c.list)
	var sel selector
	norm := &v1.ProfileSelector{}
	vrsOf := func(t uint32) []string {
		if a.c.stored {
			return stored
		}
		return dictVRs(t)
	}
	switch k := s.GetKind().(type) {
	case *v1.ProfileSelector_Tag:
		a := a.in("tag")
		t, good := parseTag(a, k.Tag)
		if good && checkStandard(a, t) {
			sel = selector{tier: TierTag, tag: t, vrs: vrsOf(t)}
			norm.Kind = &v1.ProfileSelector_Tag{Tag: formatTag(t)}
		}
	case *v1.ProfileSelector_Keyword:
		a := a.in("keyword")
		if a.c.stored {
			a.add(CodeBadTag, "an expanded form names an element by its tag, never its keyword")
			break
		}
		t, good := keywordTag(a, k.Keyword)
		if good && checkStandard(a, t) {
			sel = selector{tier: TierTag, tag: t, vrs: vrsOf(t)}
			norm.Kind = &v1.ProfileSelector_Tag{Tag: formatTag(t)}
		}
	case *v1.ProfileSelector_Group:
		value, mask, text, good := parseGroup(a.in("group"), k.Group, s.GetIncludePrivate())
		if good {
			sel = selector{tier: TierGroup, groupValue: value, groupMask: mask, includePrivate: s.GetIncludePrivate()}
			norm.Kind = &v1.ProfileSelector_Group{Group: text}
			norm.IncludePrivate = s.GetIncludePrivate()
		}
	case *v1.ProfileSelector_Vr:
		if !validVR(k.Vr) {
			a.in("vr").add(CodeBadVR, "%q is not a value representation of PS3.5 6.2", clipText(k.Vr))
		} else {
			sel = selector{tier: TierVR, vr: k.Vr, vrs: []string{k.Vr}, includePrivate: s.GetIncludePrivate()}
			norm.Kind = &v1.ProfileSelector_Vr{Vr: k.Vr}
			norm.IncludePrivate = s.GetIncludePrivate()
		}
	case *v1.ProfileSelector_PrivateElement:
		var pe *v1.PrivateElement
		sel, pe = parsePrivate(a.in("privateElement"), k.PrivateElement)
		if pe != nil {
			norm.Kind = &v1.ProfileSelector_PrivateElement{PrivateElement: pe}
		}
	case *v1.ProfileSelector_Path:
		var text string
		var good bool
		sel, text, good = parsePath(a.in("path"), k.Path)
		if good {
			sel.vrs = vrsOf(sel.tag)
			norm.Kind = &v1.ProfileSelector_Path{Path: text}
		}
	case *v1.ProfileSelector_ContentItem:
		c, vt, good := parseContentItem(a.in("contentItem"), k.ContentItem)
		if good {
			sel = selector{tier: TierPlace, concept: &c, valueType: vt, vrs: contentValueVRs(vt)}
			norm.Kind = &v1.ProfileSelector_ContentItem{ContentItem: &v1.ContentItemSelector{Scheme: c.Scheme, Value: c.Value, ValueType: vt}}
		}
	case *v1.ProfileSelector_ValuePattern:
		vp, nv, vrs, good := parseValuePattern(a.in("valuePattern"), k.ValuePattern)
		if good {
			sel = selector{tier: TierValue, value: vp, vrs: vrs, includePrivate: s.GetIncludePrivate()}
			norm.Kind = &v1.ProfileSelector_ValuePattern{ValuePattern: nv}
			norm.IncludePrivate = s.GetIncludePrivate()
		}
	default:
		a.add(CodeRequired, "a rule needs a selector: one of tag, keyword, group, vr, privateElement, path, contentItem or valuePattern")
	}
	if s.GetIncludePrivate() && sel.tier != TierGroup && sel.tier != TierVR && sel.tier != TierValue && len(a.c.list) == before {
		a.in("includePrivate").add(CodeIncludePrivate, "includePrivate goes with a group, a vr or a valuePattern selector only: privateElement names private elements by their creator, and the other selectors name standard elements")
	}
	if a.c.stored && len(a.c.list) == before {
		checkStoredVRs(a, sel, stored)
	}
	return sel, norm, len(a.c.list) == before
}

// checkStoredVRs checks that the VRs a stored rule holds agree with its
// selector, so a changed expanded form cannot give a group a VR its
// actions would then be checked against. The tag, path and content item
// VRs were decided by validation and are taken as stored.
func checkStoredVRs(a at, sel selector, stored []string) {
	for _, v := range stored {
		if !validVR(v) {
			a.add(CodeBadVR, "the stored VR %q is not a value representation", clipText(v))
			return
		}
	}
	switch sel.tier {
	case TierVR:
		if len(stored) != 1 || stored[0] != sel.vr {
			a.add(CodeBadVR, "a vr selector's stored VRs are its VR alone")
		}
	case TierValue:
		if !slices.Equal(stored, sel.vrs) {
			a.add(CodeBadVR, "a value pattern's stored VRs are those it reads")
		}
	case TierGroup, TierPrivateBlock, TierPrivateElement:
		if len(stored) != 0 {
			a.add(CodeBadVR, "a group or a private selector says nothing of the VR, and stores none")
		}
	case TierPlace:
		if sel.concept != nil && !slices.Equal(stored, contentValueVRs(sel.valueType)) {
			a.add(CodeBadVR, "a content item's stored VRs are those of its value type")
		}
	}
}

// parseTag reads a tag written as eight hexadecimal digits, the form DICOM
// JSON uses (PS3.18 F.2).
func parseTag(a at, s string) (uint32, bool) {
	if len(s) != 8 || !isHex(s) {
		a.add(CodeBadTag, "%q is not a tag: eight hexadecimal digits, group then element, such as 00100010", clipText(s))
		return 0, false
	}
	v, err := strconv.ParseUint(s, 16, 32)
	if err != nil {
		a.add(CodeBadTag, "%q is not a tag: %v", clipText(s), err)
		return 0, false
	}
	return uint32(v), true
}

// keywordTag finds a keyword in the data dictionary (PS3.6).
func keywordTag(a at, kw string) (uint32, bool) {
	t := uint32(tag.GlobalDictionary().GetByKeyword(kw))
	// The dictionary answers zero for a keyword it does not know; the
	// element whose tag is zero is a command element, which no rule may
	// name anyway.
	if t == 0 {
		a.add(CodeUnknownKeyword, "%q is not a keyword of the data dictionary (PS3.6); a repeating group's element is named by its tag or its group", clipText(kw))
		return 0, false
	}
	return t, true
}

// checkStandard refuses a tag a tag selector may not name: a private
// element, which only its creator identifies, and the reserved elements.
func checkStandard(a at, t uint32) bool {
	if group(t)%2 == 1 {
		a.add(CodePrivateTag, "%s is in an odd group, so it is private and is identified by its creator: use privateElement", formatTag(t))
		return false
	}
	return checkReserved(a, t)
}

// checkReserved refuses the elements no rule may name: those the pipeline
// handles in every file (Pipeline).
func checkReserved(a at, t uint32) bool {
	if h, why := Pipeline(t); h != NotHandled {
		a.add(CodeReservedElement, "%s is %s, which no rule may name", formatTag(t), why)
		return false
	}
	return true
}

// parseGroup reads a group of four hexadecimal digits, "x" standing for any
// digit, and returns its value and mask and its normalized text.
// withPrivate is the selector's include_private: without it, a group whose
// last digit is odd would select nothing.
func parseGroup(a at, s string, withPrivate bool) (value, mask uint16, text string, ok bool) {
	if len(s) != 4 {
		a.add(CodeBadGroup, "%q is not a group: four hexadecimal digits, x for any, such as 0010 or 60xx", clipText(s))
		return 0, 0, "", false
	}
	var b strings.Builder
	for i := range 4 {
		c := s[i]
		value <<= 4
		mask <<= 4
		switch {
		case c == 'x' || c == 'X':
			b.WriteByte('x')
		case isHexDigit(c):
			d, _ := strconv.ParseUint(string(c), 16, 8)
			value |= uint16(d)
			mask |= 0xF
			b.WriteString(strings.ToUpper(string(c)))
		default:
			a.add(CodeBadGroup, "%q is not a group: four hexadecimal digits, x for any, such as 0010 or 60xx", clipText(s))
			return 0, 0, "", false
		}
	}
	if mask == 0 {
		a.add(CodeBadGroup, "xxxx would select every element: say what happens to elements no rule names with unnamed")
		return 0, 0, "", false
	}
	for _, g := range []uint16{groupCommand, groupFileMeta, groupDirectory, groupItems} {
		if g&mask == value {
			a.add(CodeReservedElement, "%s would select group %04X, which no rule may change", b.String(), g)
			return 0, 0, "", false
		}
	}
	// A group of only elements the pipeline handles would select nothing.
	if mask == 0xFFFF && (value == groupPixel || value == groupSignatures || value == groupPadding) {
		a.add(CodeReservedElement, "group %s holds only elements the pipeline handles in every file", b.String())
		return 0, 0, "", false
	}
	if mask&0xF == 0xF && value%2 == 1 && !withPrivate {
		a.add(CodeIncludePrivate, "%s is a private group, so a group rule without includePrivate would select nothing: name private elements by their creator with privateElement, or set includePrivate", b.String())
		return 0, 0, "", false
	}
	return value, mask, b.String(), true
}

// parsePrivate reads a private element and returns it compiled and
// normalized; the normalized form is nil when a problem was recorded.
func parsePrivate(a at, p *v1.PrivateElement) (selector, *v1.PrivateElement) {
	before := len(a.c.list)
	creator := p.GetCreator()
	switch {
	case creator == "":
		a.in("creator").add(CodeRequired, "a private element needs its creator")
	case len(creator) > maxCreatorLength:
		a.in("creator").add(CodeTooLong, "a private creator is at most %d bytes", maxCreatorLength)
	case strings.ContainsAny(creator, "\\") || hasControl(creator):
		a.in("creator").add(CodeBadPrivate, "a private creator holds no backslash and no control character")
	case strings.TrimRight(creator, " ") != creator || strings.TrimLeft(creator, " ") != creator:
		a.in("creator").add(CodeBadPrivate, "write the private creator without leading or trailing spaces; the padding of the file is not part of it")
	}
	var g uint16
	gs := p.GetGroup()
	if len(gs) != 4 || !isHex(gs) {
		a.in("group").add(CodeBadPrivate, "%q is not a group: four hexadecimal digits", clipText(gs))
	} else {
		v, _ := strconv.ParseUint(gs, 16, 16)
		g = uint16(v)
		// PS3.5 7.8: private groups are the odd groups but 0001, 0003,
		// 0005, 0007 and FFFF.
		if g%2 == 0 || g < 0x0009 || g == 0xFFFF {
			a.in("group").add(CodeBadPrivate, "%s is not a private group: an odd group from 0009 to FFFD (PS3.5 7.8)", strings.ToUpper(gs))
		}
	}
	elem := -1
	es := p.GetElement()
	switch {
	case es == "xx" || es == "XX":
	case len(es) == 2 && isHex(es):
		v, _ := strconv.ParseUint(es, 16, 8)
		elem = int(v)
	default:
		a.in("element").add(CodeBadPrivate, "%q is not an element of a block: two hexadecimal digits, or xx for the whole block", clipText(es))
	}
	if len(a.c.list) != before {
		return selector{}, nil
	}
	sel := selector{tier: TierPrivateElement, creator: creator, privGroup: g, privElem: elem}
	text := "xx"
	if elem < 0 {
		sel.tier = TierPrivateBlock
	} else {
		text = fmt.Sprintf("%02X", elem)
	}
	return sel, &v1.PrivateElement{Creator: creator, Group: fmt.Sprintf("%04X", g), Element: text}
}

// parsePath reads a path and returns it compiled and normalized, without
// its VRs. A stored path is read without the data dictionary: its steps
// were checked as sequences when it was validated.
func parsePath(a at, s string) (selector, string, bool) {
	if s == "" {
		a.add(CodeRequired, "a path needs at least one step")
		return selector{}, "", false
	}
	parts := strings.Split(s, "/")
	if len(parts) > MaxPathSteps {
		a.add(CodeTooMany, "a path has at most %d steps", MaxPathSteps)
		return selector{}, "", false
	}
	sel := selector{tier: TierPlace, path: true}
	norm := make([]string, 0, len(parts))
	for i, p := range parts {
		last := i == len(parts)-1
		if p == "**" {
			switch {
			case last:
				a.add(CodeBadPath, "a path ends with an element, not **")
				return selector{}, "", false
			case i > 0 && parts[i-1] == "**":
				a.add(CodeBadPath, "** twice in a row says nothing more than once")
				return selector{}, "", false
			}
			sel.steps = append(sel.steps, step{anyDepth: true})
			norm = append(norm, "**")
			continue
		}
		name, items := strings.CutSuffix(p, "[*]")
		if items == last {
			if last {
				a.add(CodeBadPath, "the last step %q is the element itself, without [*]", clipText(p))
			} else {
				a.add(CodeBadPath, "step %d, %q, is a sequence on the way and needs [*] for its items", i+1, clipText(p))
			}
			return selector{}, "", false
		}
		t, ok := pathStepTag(a, name)
		if !ok {
			return selector{}, "", false
		}
		if !last && !a.c.stored && tag.Tag(t).GetVR() != tagSequenceVR {
			a.add(CodeBadPath, "step %d, %s, is not a sequence in the data dictionary", i+1, formatTag(t))
			return selector{}, "", false
		}
		if last {
			sel.tag = t
			norm = append(norm, formatTag(t))
		} else {
			sel.steps = append(sel.steps, step{tag: t})
			norm = append(norm, formatTag(t)+"[*]")
		}
	}
	if len(sel.steps) == 1 && sel.steps[0].anyDepth {
		a.add(CodePathIsTag, "**/%s selects the element at every depth, which is what a tag selector says: use tag or keyword", formatTag(sel.tag))
		return selector{}, "", false
	}
	return sel, strings.Join(norm, "/"), true
}

// pathStepTag reads a step's element, a tag or a keyword (a tag only in a
// stored form), and refuses what a path may not hold.
func pathStepTag(a at, name string) (uint32, bool) {
	var t uint32
	var ok bool
	if len(name) == 8 && isHex(name) || a.c.stored {
		t, ok = parseTag(a, name)
	} else {
		t, ok = keywordTag(a, name)
	}
	if !ok {
		return 0, false
	}
	return t, checkStandard(a, t)
}

// parseContentItem reads a content item selector: its concept and its
// value type.
func parseContentItem(a at, c *v1.ContentItemSelector) (Concept, string, bool) {
	before := len(a.c.list)
	checkCodeText(a.in("scheme"), c.GetScheme(), maxSchemeLength)
	checkCodeText(a.in("value"), c.GetValue(), maxCodeValueLength)
	vt := c.GetValueType()
	if _, known := contentValues[vt]; vt != "" && !known {
		a.in("valueType").add(CodeBadValueType, "%q is not a value type of a content item that has a value (PS3.3 C.17.3): one of %s", clipText(vt), strings.Join(ValueTypes(), ", "))
	}
	return Concept{Scheme: c.GetScheme(), Value: c.GetValue()}, vt, len(a.c.list) == before
}

// checkCodeText checks a part of a code.
func checkCodeText(a at, s string, limit int) {
	switch {
	case s == "":
		a.add(CodeRequired, "a code needs its scheme and its value")
	case len(s) > limit:
		a.add(CodeTooLong, "at most %d bytes", limit)
	case strings.ContainsAny(s, "\\") || hasControl(s) || strings.TrimSpace(s) != s:
		a.add(CodeBadCode, "%q holds a backslash, a control character or surrounding spaces", clipText(s))
	}
}

// dictVRs returns the VRs the data dictionary gives a tag, nil when it does
// not know the tag.
func dictVRs(t uint32) []string {
	v := tag.Tag(t).GetVR()
	if v == "" {
		return nil
	}
	return strings.Split(v, " or ")
}

// validVR reports whether s is a VR the library knows (PS3.5 6.2).
func validVR(s string) bool {
	return len(s) == 2 && strings.ToUpper(s) == s && valuerep.IsValidVR(s)
}

// group returns a tag's group.
func group(t uint32) uint16 { return uint16(t >> 16) }

// formatTag writes a tag as eight upper-case hexadecimal digits.
func formatTag(t uint32) string { return fmt.Sprintf("%08X", t) }

// isHex reports whether s is only hexadecimal digits.
func isHex(s string) bool {
	for i := range len(s) {
		if !isHexDigit(s[i]) {
			return false
		}
	}
	return s != ""
}

// isHexDigit reports whether c is a hexadecimal digit.
func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// hasControl reports whether s holds a control character.
func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			return true
		}
	}
	return false
}

// clipText bounds what a problem's message quotes of a value it refuses.
func clipText(s string) string {
	const limit = 64
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
