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

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// facts is what conditions read of a file, read once from its top level
// before anything changes, so every condition of every element sees the
// file as received. A value is its text without the padding PS3.5 6.2
// says is not part of it, its case as written; an element absent, or
// present with no value, is absent.
type facts map[v1.ConditionSubject]string

// The elements the file's condition subjects are (PS3.6).
var factTags = map[v1.ConditionSubject]uint32{
	v1.ConditionSubject_CONDITION_SUBJECT_MODALITY:          0x00080060,
	v1.ConditionSubject_CONDITION_SUBJECT_SOP_CLASS_UID:     0x00080016,
	v1.ConditionSubject_CONDITION_SUBJECT_MANUFACTURER:      0x00080070,
	v1.ConditionSubject_CONDITION_SUBJECT_MODEL_NAME:        0x00081090,
	v1.ConditionSubject_CONDITION_SUBJECT_SOFTWARE_VERSIONS: 0x00181020,
}

// readFacts reads the facts of ds and its reception.
func readFacts(ds *dataset.Dataset, rec Reception) facts {
	f := facts{}
	for s, t := range factTags {
		if v, ok := topText(ds, t); ok && v != "" {
			f[s] = v
		}
	}
	for s, v := range map[v1.ConditionSubject]string{
		v1.ConditionSubject_CONDITION_SUBJECT_RECEIVER:         rec.Receiver,
		v1.ConditionSubject_CONDITION_SUBJECT_CALLING_AE_TITLE: rec.CallingAETitle,
		v1.ConditionSubject_CONDITION_SUBJECT_CALLED_AE_TITLE:  rec.CalledAETitle,
	} {
		if v = strings.TrimSpace(v); v != "" {
			f[s] = v
		}
	}
	return f
}

// Value implements profile.Facts.
func (f facts) Value(s v1.ConditionSubject) (string, bool) {
	v, ok := f[s]
	return v, ok
}

// sopClass is the file's SOP Class UID as received.
func (f facts) sopClass() (string, bool) {
	return f.Value(v1.ConditionSubject_CONDITION_SUBJECT_SOP_CLASS_UID)
}

// topText returns a top-level element's value as text without its
// padding, and whether ds holds it; a sequence, or a value not in memory,
// is present and empty.
func topText(ds *dataset.Dataset, t uint32) (string, bool) {
	elem, ok := ds.Get(tag.Tag(t))
	if !ok {
		return "", false
	}
	v, _ := elementText(ds, tag.Tag(t))
	return naming.TrimValue(effectiveVR(tag.Tag(t), elem), v), true
}

// elementText returns an element's value as its bytes read as text, false
// for a sequence, an element absent, and a value not in memory. A data set
// the DICOM library read holds its text decoded into UTF-8.
func elementText(ds *dataset.Dataset, t tag.Tag) (string, bool) {
	elem, ok := ds.Get(t)
	if !ok {
		return "", false
	}
	if _, isSeq := elem.GetValue().(*sequence.Sequence); isSeq {
		return "", false
	}
	b, ok := elem.Bytes()
	if !ok || len(b) > MaxValueLength {
		return "", false
	}
	return string(b), true
}

// effectiveVR is the VR an element is treated as: SQ for a sequence; the
// data dictionary's for a standard element the file holds as UN or with no
// VR, when the dictionary gives one (PS3.5 6.2.2: UN is a VR not known to
// the writer, not a VR of the attribute); the file's otherwise.
func effectiveVR(t tag.Tag, elem *dataelem.DataElement) string {
	if _, isSeq := elem.GetValue().(*sequence.Sequence); isSeq {
		return "SQ"
	}
	vr := string(elem.GetVR())
	if (vr == "UN" || vr == "") && t.Group()%2 == 0 {
		if d := t.GetVR(); d != "" && !strings.Contains(d, " or ") && d != "SQ" {
			return d
		}
	}
	if vr == "" {
		return "UN"
	}
	return vr
}

// vrIfUN returns vr when the file holds the element as UN, so a value
// written into it is written with the VR it was treated as.
func vrIfUN(elem *dataelem.DataElement, vr string) dataelem.VR {
	if v := elem.GetVR(); v == "UN" || v == "" {
		return dataelem.VR(vr)
	}
	return ""
}

// isCreator reports whether t is a private creator element (PS3.5 7.8.1).
func isCreator(t tag.Tag) bool {
	return t.Group()%2 == 1 && t.Element() >= 0x10 && t.Element() <= 0xFF
}

// charsetOf returns the character set governing ds: its own declaration,
// or inherited from the data set it is an item of.
func charsetOf(ds *dataset.Dataset, inherited charsetKind) charsetKind {
	v, ok := elementText(ds, tagSpecificCharacterSet)
	if !ok {
		return inherited
	}
	switch strings.Trim(v, " \x00") {
	case "", "ISO_IR 6":
		return charsetAbsent
	case "ISO_IR 192":
		return charsetUTF8
	}
	return charsetOther
}

// contentItemOf returns, for a structured report content item, the
// concept name and the Value Type (0040,A040), each without padding: the
// scheme and value of the first item of its Concept Name Code Sequence
// (0040,A043), nil when it has none. A content item is any item that has a
// Value Type, wherever it sits: in a Content Sequence (0040,A730), and as
// well in the Acquisition Context Sequence (0040,0555), a specimen's
// preparation steps and the other sequences whose items PS3.3 C.17.3 and
// C.4.10 build of content items. An item without one returns "".
func contentItemOf(item *dataset.Dataset) (*profile.Concept, string) {
	vt, ok := topText(item, tagValueType)
	if !ok || vt == "" {
		return nil, ""
	}
	elem, ok := item.Get(tagConceptNameCode)
	if !ok {
		return nil, vt
	}
	seq, ok := elem.GetValue().(*sequence.Sequence)
	if !ok || seq.Length() == 0 {
		return nil, vt
	}
	code, ok := seq.Items()[0].(*dataset.Dataset)
	if !ok {
		return nil, vt
	}
	scheme, _ := topText(code, tagCodingScheme)
	value, _ := topText(code, tagCodeValue)
	if scheme == "" || value == "" {
		return nil, vt
	}
	return &profile.Concept{Scheme: scheme, Value: value}, vt
}

// planSets plans the elements set rules make: a set rule makes its element
// at the top level when the file lacks it and the rule is the one that
// would decide it there.
func (r *run) planSets() error {
	for _, s := range r.en.dec.Sets() {
		t := tag.Tag(s.Tag)
		if r.top.Contains(t) {
			continue
		}
		dec := r.en.dec.Decide(profile.Location{Tag: s.Tag, VR: s.VR}, r.facts)
		if dec.Rule != s.Rule {
			continue
		}
		rule := dec.Name
		if rule == "" {
			rule = "set rule"
		}
		path := formatTag(s.Tag)
		// A set rule writes text, and makes its element with one VR that
		// holds text; anything else ("US or SS", a binary VR) cannot be
		// made from a constant.
		if !slices.Contains(stringVRs, s.VR) {
			return &Failure{Cause: CauseBadValue, Path: path, Rule: rule}
		}
		out := dec.Action.Constant
		if dec.Action.Template != "" {
			var ok bool
			if out, ok = renderTemplate(dec.Action.Template, r.en.keys.Hash, r.receivedValue); !ok {
				return &Failure{Cause: CauseBadValue, Path: path, Rule: rule}
			}
		}
		if !validText(s.VR, out) || !utf8.ValidString(out) {
			return &Failure{Cause: CauseBadValue, Path: path, Rule: rule}
		}
		r.ops = append(r.ops, op{kind: opPut, ds: r.top, tag: t, value: []byte(out), vr: dataelem.VR(s.VR)})
		r.record.Entries = append(r.record.Entries, Entry{
			Path: path, Tag: s.Tag, VR: s.VR, Action: "create", Rule: dec.Rule,
			RuleName: dec.Name, Origin: dec.Origin, Reason: dec.Reason, Changed: true,
		})
	}
	return nil
}

// finishCleans works out each clean's text now that every needle of the
// file is known. The terms are made once for the file, and the work of
// all its cleans together is bounded (maxCleanWork): a file past it is
// refused as too large before any text is searched, or as soon as the
// bound is reached, never left to run for minutes.
func (r *run) finishCleans() error {
	var cleans []int
	total := 0
	for i := range r.ops {
		if r.ops[i].kind == opClean {
			cleans = append(cleans, i)
			total += utf8.RuneCountInString(r.ops[i].text)
		}
	}
	if len(cleans) == 0 {
		return nil
	}
	terms, ok := cleanTerms(r.record.Needles)
	if !ok {
		return &Failure{Cause: CauseTooLarge, Path: r.ops[cleans[0]].path, Rule: r.ops[cleans[0]].rule}
	}
	cl := newCleaner(terms, maxCleanWork)
	if !cl.affords(total) {
		return &Failure{Cause: CauseTooLarge, Path: r.ops[cleans[0]].path, Rule: r.ops[cleans[0]].rule}
	}
	for _, i := range cleans {
		o := &r.ops[i]
		out, err := cl.cleanWith(r.ctx, o.text, r.en.lists, o.lists)
		if errors.Is(err, errCleanTooLarge) {
			return &Failure{Cause: CauseTooLarge, Path: o.path, Rule: o.rule}
		}
		if err != nil {
			return err
		}
		if out == o.text {
			o.kind = 0
			continue
		}
		if !utf8.ValidString(out) || (o.charset == charsetOther && !isASCII(out)) {
			return &Failure{Cause: CauseBadValue, Path: o.path, Rule: o.rule}
		}
		o.kind, o.value = opValue, []byte(out)
		r.record.Entries[o.entry].Changed = true
	}
	return nil
}

// finishAssigns asks the lookup tables for the codes they assign on first
// sight, once the plan has succeeded, so that a file refused for any other
// reason takes no code from a table. A value asked for twice in one file
// is asked once.
func (r *run) finishAssigns() error {
	if len(r.assigns) == 0 {
		return nil
	}
	var keys []LookupKey
	at := map[LookupKey]int{}
	for _, a := range r.assigns {
		k := LookupKey{Table: a.table, Key: a.key}
		if _, ok := at[k]; !ok {
			at[k] = len(keys)
			keys = append(keys, k)
		}
	}
	if len(keys) > MaxLookups {
		return &Failure{Cause: CauseTooLarge}
	}
	codes, err := r.en.lookups.Assign(r.ctx, keys)
	if err != nil {
		return err
	}
	if len(codes) != len(keys) {
		return errors.New("engine: the lookup tables answered another number of codes than asked for")
	}
	for _, a := range r.assigns {
		code := codes[at[LookupKey{Table: a.table, Key: a.key}]]
		o := &r.ops[a.op]
		if code == "" || !isASCII(code) || !validText(a.vr, code) {
			return &Failure{Cause: CauseBadValue, Path: o.path, Rule: o.rule}
		}
		if code == o.text {
			o.kind = 0
			continue
		}
		o.kind, o.value = opValue, []byte(code)
		r.record.Entries[o.entry].Changed = true
	}
	return nil
}

// put plans a top-level element the engine writes itself, with its record
// entry.
func (r *run) put(t uint32, vr, value, reason string) {
	r.ops = append(r.ops, op{kind: opPut, ds: r.top, tag: tag.Tag(t), value: []byte(value), vr: dataelem.VR(vr)})
	r.record.Entries = append(r.record.Entries, Entry{Path: formatTag(t), Tag: t, VR: vr, Action: "set", Rule: -1, Reason: reason, Changed: true})
}

// declareCharset writes Specific Character Set (0008,0005) at the top
// level once every change is made: ISO_IR 192 when any text of the data
// set, at any depth, is outside the default repertoire, and nothing
// otherwise (PS3.5 6.1.2.3: the default repertoire needs no declaration).
// The library holds every text it decoded in UTF-8, and every declaration
// the file had, in any item, was removed by the walk, so the one written
// here is true of every value. It is the engine's alone: no profile names
// it.
func (r *run) declareCharset() {
	if !nonASCIIText(r.top) {
		return
	}
	r.put(tagSpecificCharacterSet, "CS", "ISO_IR 192", "text outside the default repertoire is kept or written, and the data set's text is UTF-8 (PS3.5 6.1.2.3)")
	r.applyOp(r.ops[len(r.ops)-1])
}

// nonASCIIText reports whether any text value of ds, at any depth, is
// outside the default repertoire.
func nonASCIIText(ds *dataset.Dataset) bool {
	for _, t := range ds.Tags() {
		elem, ok := ds.Get(t)
		if !ok {
			continue
		}
		if seq, isSeq := elem.GetValue().(*sequence.Sequence); isSeq {
			for _, it := range seq.Items() {
				if item, ok := it.(*dataset.Dataset); ok && nonASCIIText(item) {
					return true
				}
			}
			continue
		}
		if !charsetVRs[effectiveVR(t, elem)] {
			continue
		}
		if b, ok := elem.Bytes(); ok && !isASCII(string(b)) {
			return true
		}
	}
	return false
}

// apply makes every planned change. Nothing here should fail: every value
// was worked out and checked before. Each change notes how to undo it, so
// that a fault part way (a panic, which Apply recovers) leaves the data set
// as it was.
func (r *run) apply() {
	for i, o := range r.ops {
		if r.en.seams.beforeOp != nil {
			r.en.seams.beforeOp(i)
		}
		r.applyOp(o)
	}
}

// applyOp makes one change, noting how to undo it.
func (r *run) applyOp(o op) {
	switch o.kind {
	case opRemove:
		if elem, ok := o.ds.Get(o.tag); ok {
			o.ds.Remove(o.tag)
			r.undo = append(r.undo, func() { _ = o.ds.Add(elem) })
		}
	case opValue, opNoItems:
		if elem, ok := o.ds.Get(o.tag); ok {
			r.noteValue(elem)
			if o.kind == opNoItems {
				elem.SetValue(sequence.New())
				if o.vr != "" {
					elem.SetVR(o.vr)
				}
				return
			}
			elem.SetValue(o.value)
			if o.vr != "" {
				elem.SetVR(o.vr)
			}
		}
	case opSequence:
		if elem, ok := o.ds.Get(o.tag); ok {
			r.noteValue(elem)
			elem.SetValue(o.seq)
			elem.SetVR(o.vr)
		}
	case opElement:
		if old, ok := o.ds.Get(o.tag); ok {
			o.ds.Remove(o.tag)
			r.undo = append(r.undo, func() { _ = o.ds.Add(old) })
		}
		// Add fails only for a nil element or a tag that is not a tag.Tag,
		// neither of which can be here.
		_ = o.ds.Add(o.elem)
		r.undo = append(r.undo, func() { o.ds.Remove(o.tag) })
	case opPut:
		if elem, ok := o.ds.Get(o.tag); ok {
			r.noteValue(elem)
			elem.SetValue(o.value)
			elem.SetVR(o.vr)
			return
		}
		// Add fails only for a nil element or a tag that is not a tag.Tag,
		// neither of which can be here.
		_ = o.ds.Add(dataelem.NewDataElement(o.tag, o.vr, o.value))
		r.undo = append(r.undo, func() { o.ds.Remove(o.tag) })
	}
}

// noteValue notes how to give an element back its value and VR.
func (r *run) noteValue(elem *dataelem.DataElement) {
	value, vr := elem.GetValue(), elem.GetVR()
	r.undo = append(r.undo, func() {
		elem.SetValue(value)
		elem.SetVR(vr)
	})
}

// rollback undoes every change apply made, last first.
func (r *run) rollback() {
	for i := len(r.undo) - 1; i >= 0; i-- {
		r.undo[i]()
	}
	r.undo = nil
}
