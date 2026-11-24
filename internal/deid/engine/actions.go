package engine

import (
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"
	"github.com/amrshadid/go-dicom/validate"
	"github.com/amrshadid/go-dicom/valuerep"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// errNoLookups is a profile with a lookup action run with no lookup
// tables: a fault of the caller, not of the file.
var errNoLookups = errors.New("engine: the profile looks values up and no lookup tables were given")

// errNotText is a value an action must read that is not in memory, or is
// longer than MaxValueLength.
var errNotText = errors.New("engine: the value cannot be read as text")

// The VRs each action reads or writes (PS3.5 6.2). An element of another
// VR is refused, never given a value it cannot hold.
var (
	// textVRs hold text a keyed code or a looked-up code can replace.
	textVRs = []string{"AE", "CS", "LO", "LT", "PN", "SH", "ST", "UC", "UT"}
	// freeTextVRs hold free text names can hide in.
	freeTextVRs = []string{"LO", "LT", "SH", "ST", "UC", "UT"}
	// stringVRs hold a value written as text, which a constant can be.
	stringVRs = []string{"AE", "AS", "CS", "DA", "DS", "DT", "IS", "LO", "LT", "PN", "SH", "ST", "TM", "UC", "UI", "UR", "UT"}
	// singleValueVRs may hold a backslash as text, not as a separator of
	// values (PS3.5 6.2).
	singleValueVRs = []string{"LT", "ST", "UT", "UR"}
)

// target is the element an action acts on.
type target struct {
	ds   *dataset.Dataset
	tag  tag.Tag
	elem *dataelem.DataElement
	// vr is the element's VR: the file's, or the dictionary's for an
	// element the file holds as UN.
	vr string
	// nvr is the VR its values are taken as by the floor and as needles
	// (needleVR): PN for a person's name whatever VR the file wrote.
	nvr  string
	seqs []uint32
	path string
	// entry is the element's record entry.
	entry int
	sc    scope
	rule  string
	// seq is the element's value when it is a sequence.
	seq *sequence.Sequence
	// unItems is the value of an element held as UN that begins with an
	// item: a sequence the library kept as bytes (unknown.go).
	unItems []byte
	// seqVR, when set, is the VR a sequence that was held as UN takes when
	// it is kept or emptied.
	seqVR dataelem.VR
	// byRule is set when a rule decided the element, not a default.
	byRule bool
	// floor is set when the element is one the floor covers, whose value
	// is a needle whatever decided it.
	floor bool
}

// assign is a lookup whose code the table assigns on first sight, asked
// for once the plan has succeeded (finishAssigns).
type assign struct {
	table, key string
	// vr is the element's VR, which the code must be a value of.
	vr string
	// op is the index of the opAssign it fills.
	op int
}

// needleAt keeps a value an action removed or replaced as a needle, when a
// rule decided the element or the floor covers it, and the value is one
// that identifies by itself (needleKindOf, under basis). A value a default
// removed (a private element, or any element under "unnamed: remove") is
// not one otherwise: the profile named it as nothing identifying, and such
// values (a model name, 0.000000) are common in what a file keeps, so the
// check after would refuse files for them.
func (r *run) needleAt(c *target, value, path string, basis needleBasis) {
	if c.byRule || c.floor {
		r.needle(needleKindOf(uint32(c.tag), c.nvr, basis), c.nvr, value, path)
	}
}

// subtreeNeedlesAt keeps as needles the values of a sequence an action
// removes or empties: every value of one the floor covers, or of one
// covered whole (wholeTags) a rule decided, but numbers, codes and times;
// and of any other a rule decided, those the profile or the floor removes
// or changes where they are (subtreeNeedles).
func (r *run) subtreeNeedlesAt(c *target) error {
	_, whole := wholeTags[uint32(c.tag)]
	switch {
	case c.floor, c.byRule && whole:
		return r.subtreeNeedles(c.seq, nil, c.path, len(c.seqs)+1, subtreeAll)
	case c.byRule:
		return r.subtreeNeedles(c.seq, slices.Concat(c.seqs, []uint32{uint32(c.tag)}), c.path, len(c.seqs)+1, subtreeByPlace)
	}
	return nil
}

// fail is a failure of the file at this element.
func (c *target) fail(cause Cause) error { return &Failure{Cause: cause, Path: c.path, Rule: c.rule} }

// act applies a decision's action to an element and reports whether the
// element is removed. Each action is one function.
func (r *run) act(c *target, a Action, vrs []string) (bool, error) {
	if c.unItems != nil {
		return r.actUnknownItems(c, a)
	}
	if c.seq != nil {
		return r.actSequence(c, a)
	}
	switch a.Kind {
	case ActKeep:
		return false, r.actKeep(c)
	case ActRemove:
		return r.actRemove(c), nil
	case ActEmpty:
		return false, r.actEmpty(c)
	case ActDummy:
		return false, r.actDummy(c)
	case ActRemoveUnlessRequired:
		return r.actRemoveUnlessRequired(c, a)
	case ActQuarantine:
		return false, c.fail(CauseRuleQuarantine)
	case ActClean:
		if c.vr == "OB" || c.vr == "OW" {
			// The bits of an overlay or a curve are not read for text;
			// removing them takes out no less than cleaning them would
			// (PS3.15 E.3.3, the Clean Graphics option).
			return r.actRemove(c), nil
		}
	}
	// The actions below read or write the value: the element must be of a
	// VR the rule's selector was checked for.
	if vrs != nil && !slices.Contains(vrs, c.vr) {
		return false, c.fail(CauseBadValue)
	}
	switch a.Kind {
	case ActReplace, ActSet:
		return false, r.actReplace(c, a)
	case ActHash:
		return false, r.actHash(c)
	case ActUID:
		return false, r.actUID(c)
	case ActShift:
		return false, r.actShift(c)
	case ActGeneralize:
		return false, r.actGeneralize(c)
	case ActLookup:
		return false, r.actLookup(c, a)
	case ActClean:
		return false, r.actClean(c)
	case ActRedact:
		return false, r.actRedact(c, a)
	}
	return false, c.fail(CauseBadValue)
}

// actSequence applies an action to a sequence: keep walks its items, and
// so does clean, since a sequence is cleaned by cleaning what it holds
// (PS3.15 E.3.4: each content item by its own rules); remove removes it;
// empty and dummy leave it with no items. Every other action is a value's
// and refuses the file.
func (r *run) actSequence(c *target, a Action) (bool, error) {
	switch a.Kind {
	case ActKeep, ActClean:
		return false, r.keepSequence(c)
	case ActRemove:
		r.record.Entries[c.entry].Changed = true
		r.ops = append(r.ops, op{kind: opRemove, ds: c.ds, tag: c.tag})
		return true, r.subtreeNeedlesAt(c)
	case ActEmpty, ActDummy:
		r.record.Entries[c.entry].Changed = c.seq.Length() > 0
		r.ops = append(r.ops, op{kind: opNoItems, ds: c.ds, tag: c.tag, vr: c.seqVR})
		return false, r.subtreeNeedlesAt(c)
	case ActRemoveUnlessRequired:
		switch r.required(c) {
		case requiredValue:
			if a.Fallback == FallbackUID {
				return false, r.keepSequence(c)
			}
			return r.actSequence(c, Action{Kind: ActEmpty})
		case requiredPresent:
			return r.actSequence(c, Action{Kind: ActEmpty})
		}
		return r.actSequence(c, Action{Kind: ActRemove})
	case ActQuarantine:
		return false, c.fail(CauseRuleQuarantine)
	}
	return false, c.fail(CauseBadValue)
}

// keepSequence keeps a sequence and plans the changes inside its items. A
// sequence that was held as UN bytes takes its parsed items, and VR SQ.
func (r *run) keepSequence(c *target) error {
	if c.seqVR != "" {
		r.ops = append(r.ops, op{kind: opSequence, ds: c.ds, tag: c.tag, seq: c.seq, vr: c.seqVR})
	}
	return r.items(c.seq, c.seqs, c.tag, c.path, c.sc)
}

// actKeep keeps an element, refusing a value it cannot judge: one of VR
// UN not in memory, which could hold anything, a patient's name included,
// and keeping it unread would pass what no rule looked at; and text that
// holds the escape of an ISO 2022 code extension, which no decoder turned
// into UTF-8, so clean and the needles never saw what it says (escaped).
// A UN value that begins with an item is not here: it is a sequence,
// walked (unknown.go).
func (r *run) actKeep(c *target) error {
	if (c.vr == "UN" || charsetVRs[c.vr]) && escaped(c.elem) {
		return c.fail(CauseBadValue)
	}
	if c.vr != "UN" {
		return nil
	}
	if _, ok := c.elem.Bytes(); !ok {
		return c.fail(CauseBadValue)
	}
	return nil
}

// actRemove removes an element.
func (r *run) actRemove(c *target) bool {
	r.record.Entries[c.entry].Changed = true
	r.ops = append(r.ops, op{kind: opRemove, ds: c.ds, tag: c.tag})
	if v, ok := elementText(c.ds, c.tag); ok {
		r.needleAt(c, naming.TrimValue(c.vr, v), c.path, basisPlace)
	}
	return true
}

// actEmpty leaves an element with no value.
func (r *run) actEmpty(c *target) error {
	v, ok := elementText(c.ds, c.tag)
	if ok && v == "" {
		return nil
	}
	if ok {
		r.needleAt(c, naming.TrimValue(c.vr, v), c.path, basisPlace)
	}
	r.record.Entries[c.entry].Changed = true
	r.ops = append(r.ops, op{kind: opValue, ds: c.ds, tag: c.tag, value: []byte{}, vr: vrIfUN(c.elem, c.vr)})
	return nil
}

// actDummy gives an element the dummy value of its VR.
func (r *run) actDummy(c *target) error {
	d, ok := Dummy(c.vr)
	if !ok {
		return c.fail(CauseBadValue)
	}
	if v, ok := elementText(c.ds, c.tag); ok {
		r.needleAt(c, naming.TrimValue(c.vr, v), c.path, basisPlace)
	}
	r.record.Entries[c.entry].Changed = true
	r.ops = append(r.ops, op{kind: opValue, ds: c.ds, tag: c.tag, value: d, vr: vrIfUN(c.elem, c.vr)})
	return nil
}

// actRemoveUnlessRequired removes an element the IOD of the file's SOP
// class does not require; empties one it requires present (Type 2); and
// applies the fallback to one it requires with a value (Type 1).
func (r *run) actRemoveUnlessRequired(c *target, a Action) (bool, error) {
	switch r.required(c) {
	case requiredValue:
		switch a.Fallback {
		case FallbackDummy:
			return false, r.actDummy(c)
		case FallbackUID:
			if c.vr != "UI" {
				return false, c.fail(CauseBadValue)
			}
			return false, r.actUID(c)
		}
		return false, r.actEmpty(c)
	case requiredPresent:
		return false, r.actEmpty(c)
	}
	return r.actRemove(c), nil
}

// requirement is what an IOD requires of an attribute.
type requirement int

// The requirements.
const (
	requiredNot     requirement = iota + 1 // Type 3: optional
	requiredPresent                        // Type 2 or 2C: present, may be empty
	requiredValue                          // Type 1 or 1C: present with a value
)

// required returns what the IOD of the file's SOP class requires of the
// element. An unknown SOP class, or an attribute its IOD does not list
// where the IOD is known, is taken as required with a value: the file
// stays valid whatever it is, and no original value stays (PS3.15 E.1.1
// resolves its combined actions toward conformance).
func (r *run) required(c *target) requirement {
	sop, _ := r.facts.sopClass()
	typ, known := r.en.types.Type(sop, c.seqs, uint32(c.tag))
	switch {
	case !known, typ == validate.Type1, typ == validate.Type1C:
		return requiredValue
	case typ == validate.Type2, typ == validate.Type2C:
		return requiredPresent
	}
	return requiredNot
}

// actReplace replaces a value present with a constant, or a template
// rendered over the values as received.
func (r *run) actReplace(c *target, a Action) error {
	if !slices.Contains(stringVRs, c.vr) || (a.Template != "" && !slices.Contains(textVRs, c.vr)) {
		return c.fail(CauseBadValue)
	}
	out := a.Constant
	if a.Template != "" {
		var ok bool
		if out, ok = renderTemplate(a.Template, r.en.keys.Hash, r.receivedValue); !ok {
			return c.fail(CauseBadValue)
		}
	}
	if !validText(c.vr, out) {
		return c.fail(CauseBadValue)
	}
	if v, ok := elementText(c.ds, c.tag); ok {
		v = naming.TrimValue(c.vr, v)
		if v == out {
			return nil
		}
		r.needleAt(c, v, c.path, basisPlace)
	}
	return r.write(c, out)
}

// actHash replaces a value with its keyed hash.
func (r *run) actHash(c *target) error {
	if !slices.Contains(textVRs, c.vr) {
		return c.fail(CauseBadValue)
	}
	orig, err := r.text(c.ds, c.tag, c.vr)
	if err != nil {
		return c.fail(CauseBadValue)
	}
	if orig == "" {
		return nil
	}
	out := hashValue(r.en.keys.Hash, c.vr, orig)
	if !validText(c.vr, out) {
		return c.fail(CauseBadValue)
	}
	r.needleAt(c, orig, c.path, basisPseudonym)
	return r.write(c, out)
}

// actUID remaps every UID of a value but the registered ones.
func (r *run) actUID(c *target) error {
	if c.vr != "UI" {
		return c.fail(CauseBadValue)
	}
	orig, err := r.text(c.ds, c.tag, c.vr)
	if err != nil {
		return c.fail(CauseBadValue)
	}
	out, replaced := remapUIDValue(r.en.keys.UID, orig)
	if len(replaced) == 0 {
		return nil
	}
	for _, u := range replaced {
		r.needleAt(c, u, c.path, basisPlace)
	}
	return r.write(c, out)
}

// actShift moves the dates of a DA or DT value back by the subject's
// shift.
func (r *run) actShift(c *target) error {
	if c.vr != "DA" && c.vr != "DT" {
		return c.fail(CauseBadValue)
	}
	orig, err := r.text(c.ds, c.tag, c.vr)
	if err != nil {
		return c.fail(CauseBadValue)
	}
	if orig == "" {
		return nil
	}
	days, err := r.shiftDays(c.path, c.rule)
	if err != nil {
		return err
	}
	var out string
	if c.vr == "DA" {
		out, err = shiftDA(orig, days)
	} else {
		out, err = shiftDT(orig, days)
	}
	if err != nil {
		return c.fail(CauseBadValue)
	}
	r.record.DatesShifted = true
	r.needleAt(c, orig, c.path, basisPlace)
	return r.write(c, out)
}

// actGeneralize makes an age or a date less precise.
func (r *run) actGeneralize(c *target) error {
	if c.vr != "AS" && c.vr != "DA" && c.vr != "DT" {
		return c.fail(CauseBadValue)
	}
	orig, err := r.text(c.ds, c.tag, c.vr)
	if err != nil {
		return c.fail(CauseBadValue)
	}
	var out string
	if c.vr == "AS" {
		out, err = generalizeAge(orig)
	} else {
		out, err = generalizeDate(orig)
	}
	if err != nil {
		return c.fail(CauseBadValue)
	}
	if out == orig {
		return nil
	}
	r.needleAt(c, orig, c.path, basisPlace)
	return r.write(c, out)
}

// actLookup replaces a value with the code a table of the tenant gives
// it, keyed by the value's keyed hash.
func (r *run) actLookup(c *target, a Action) error {
	if !slices.Contains(textVRs, c.vr) {
		return c.fail(CauseBadValue)
	}
	orig, err := r.text(c.ds, c.tag, c.vr)
	if err != nil {
		return c.fail(CauseBadValue)
	}
	if orig == "" {
		return nil
	}
	if r.en.lookups == nil {
		return errNoLookups
	}
	key := hashValue(r.en.keys.Hash, "", orig)
	code, found, err := r.en.lookups.Code(r.ctx, a.Table, key)
	if err != nil {
		return err
	}
	if !found {
		if !a.AssignMissing {
			return c.fail(CauseLookupMissing)
		}
		r.needleAt(c, orig, c.path, basisPseudonym)
		r.record.Entries[c.entry].Changed = true
		r.assigns = append(r.assigns, assign{table: a.Table, key: key, vr: c.vr, op: len(r.ops)})
		r.ops = append(r.ops, op{kind: opAssign, ds: c.ds, tag: c.tag, text: orig, entry: c.entry, path: c.path, rule: c.rule, vr: vrIfUN(c.elem, c.vr)})
		return nil
	}
	if code == "" || !isASCII(code) || !validText(c.vr, code) {
		return c.fail(CauseBadValue)
	}
	r.needleAt(c, orig, c.path, basisPseudonym)
	if code == orig {
		return nil
	}
	return r.write(c, code)
}

// actClean plans the removal of the file's identifying values from free
// text, worked out once every needle of the file is known.
func (r *run) actClean(c *target) error {
	if !slices.Contains(freeTextVRs, c.vr) {
		return c.fail(CauseBadValue)
	}
	v, err := r.text(c.ds, c.tag, c.vr)
	if err != nil || len(v) > maxCleanValue || strings.IndexByte(v, esc) >= 0 {
		return c.fail(CauseBadValue)
	}
	r.ops = append(r.ops, op{kind: opClean, ds: c.ds, tag: c.tag, text: v, entry: c.entry, path: c.path, rule: c.rule, vr: vrIfUN(c.elem, c.vr), charset: c.sc.charset})
	return nil
}

// write plans an element's new text value. Every value an action writes
// is of the default repertoire or valid UTF-8 (a value kept from the file,
// as clean leaves it), and the character set is declared once every value
// is known (declareCharset).
func (r *run) write(c *target, value string) error {
	if !utf8.ValidString(value) || (c.sc.charset == charsetOther && !isASCII(value)) {
		return c.fail(CauseBadValue)
	}
	r.record.Entries[c.entry].Changed = true
	r.ops = append(r.ops, op{kind: opValue, ds: c.ds, tag: c.tag, value: []byte(value), vr: vrIfUN(c.elem, c.vr)})
	return nil
}

// validText reports whether value is a value of VR vr (PS3.5 6.2), by the
// DICOM library's checks of form and length, each value of a multi-valued
// one apart.
func validText(vr, value string) bool {
	parts := []string{value}
	if !slices.Contains(singleValueVRs, vr) {
		parts = strings.Split(value, `\`)
	}
	for _, p := range parts {
		if p == "" {
			continue
		}
		if valuerep.ValidateValue(vr, p) != nil {
			return false
		}
	}
	return true
}
