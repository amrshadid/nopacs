package engine

import (
	"bytes"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/charset"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/network"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// Values of VR UN. A file gives VR UN to an element its writer did not
// know: every private element of an implicit VR file reads without a VR,
// and a sender that did not know a tag passes its value on as UN (PS3.5
// 6.2.2). An element with no VR is taken as UN.
//
// A sequence among them (a private sequence of defined length, or a
// standard one a system without its dictionary passed on) is a value of
// items in Implicit VR Little Endian, whatever the file's transfer syntax
// (PS3.5 6.2.2). The DICOM library keeps it as bytes, since a reader
// cannot tell items from other bytes, and parses it on request
// (network.DecodeUNSequence). The engine walks it as the sequence it is:
// its items are judged element by element like any other, and a kept one
// is written back as a sequence (VR SQ) holding what the profile left. Its
// bytes are bounded by MaxValueLength and its items by MaxDepth and
// MaxElements, as every other value; the library's parse accepts an item
// of undefined length that ends with the value, so those bounds, not the
// parse, are what limit the work. A value that begins with an item and is
// not items refuses the file when it would be kept, and is removed
// without being read when a rule removes it.

// itemTag is the first four bytes of an item, (FFFE,E000) in little endian
// (PS3.5 7.5): a UN value that begins with them is a sequence the library
// kept as bytes.
var itemTag = []byte{0xFE, 0xFF, 0x00, 0xE0}

// unknownItems returns the bytes of an element the engine treats as UN
// (vr, from effectiveVR) whose value begins with an item, and nil for any
// other: a sequence the library kept as bytes. A value not in memory is
// not one; actKeep refuses to keep it unread.
func unknownItems(elem *dataelem.DataElement, vr string) []byte {
	if vr != "UN" {
		return nil
	}
	if b, ok := elem.Bytes(); ok && bytes.HasPrefix(b, itemTag) {
		return b
	}
	return nil
}

// decodeUnknownItems parses a UN value as the items of a sequence, within
// the engine's bound of a value's length.
func decodeUnknownItems(b []byte) (*sequence.Sequence, error) {
	if len(b) > MaxValueLength {
		return nil, errNotText
	}
	return network.DecodeUNSequence(b)
}

// textSource is what bytes the DICOM library did not decode were written
// in: the encodings of a Specific Character Set (0008,0005), and whether
// it is known at all.
type textSource struct {
	encodings []string
	known     bool
}

// sourceFrom returns the text source a Specific Character Set gives: its
// values, backslash between them, as Reception.CharacterSet holds them;
// nil is not known, and a value the DICOM library does not know is not
// known either, so text outside the default repertoire is refused rather
// than guessed at.
func sourceFrom(cs *string) textSource {
	if cs == nil {
		return textSource{}
	}
	enc, err := charset.ConvertEncodings(strings.Split(*cs, `\`))
	if err != nil {
		return textSource{}
	}
	return textSource{encodings: enc, known: true}
}

// sourceOf returns what the undecoded bytes of ds are written in: its own
// when it is an item of a sequence held as UN bytes, the file's otherwise.
func (r *run) sourceOf(ds *dataset.Dataset) textSource {
	if src, ok := r.rawSources[ds]; ok {
		return src
	}
	return r.source
}

// textByCharset are the VRs whose values the Specific Character Set
// governs beyond the default repertoire (PS3.5 6.1.2.3).
var textByCharset = map[string]bool{"SH": true, "LO": true, "ST": true, "LT": true, "UC": true, "UT": true, "PN": true}

// esc is the escape byte that starts an ISO 2022 code extension (PS3.5
// 6.1.2.5.3): text that holds one was not decoded.
const esc = 0x1B

// decodeText decodes the bytes of a text value of VR vr as src says they
// were written, and reports whether it could: text of the default
// repertoire is itself; other text needs a known source and must decode to
// UTF-8 with no replacement character and no escape left in it.
func decodeText(b []byte, vr string, src textSource) (string, bool) {
	if isASCII(string(b)) && bytes.IndexByte(b, esc) < 0 {
		return string(b), true
	}
	if !src.known {
		return "", false
	}
	delims := charset.DefaultTextDelimiters
	if vr == "PN" {
		delims = charset.PersonNameDelimiters
	}
	out, err := charset.DecodeBytes(b, src.encodings, delims)
	if err != nil || !utf8.ValidString(out) || strings.ContainsRune(out, utf8.RuneError) || strings.IndexByte(out, esc) >= 0 {
		return "", false
	}
	return out, true
}

// decodeRawItems turns the text of the items of a sequence held as UN
// bytes into UTF-8, the form the library gives every other text of the
// data set, so clean, the needles and the check after see the text the
// file holds: each value of a VR the character set governs is decoded as
// src says (or as the item's own Specific Character Set says, which then
// declares ISO_IR 192 over what it now holds), in every nested sequence,
// to MaxDepth. Text that cannot be decoded fails the whole: the sequence
// cannot be judged. A value of VR UN inside is left as it is, and is
// decoded with its item's source if it is a sequence (rawSources). The
// sequence is the engine's own, made by the parse and in no data set yet,
// so changing it changes nothing a refusal must undo.
func (r *run) decodeRawItems(seq *sequence.Sequence, src textSource, depth int) error {
	if depth > MaxDepth {
		return &Failure{Cause: CauseTooDeep}
	}
	for _, it := range seq.Items() {
		item, ok := it.(*dataset.Dataset)
		if !ok {
			return errNotText
		}
		isrc := src
		declared, hasDeclared := item.Get(tag.Tag(tagSpecificCharacterSet))
		if hasDeclared {
			v, _ := declared.Bytes()
			isrc = sourceFrom(ptr(strings.Trim(string(v), " \x00")))
		}
		r.rawSources[item] = isrc
		for _, t := range item.Tags() {
			elem, ok := item.Get(t)
			if !ok || t == tag.Tag(tagSpecificCharacterSet) {
				continue
			}
			if inner, isSeq := elem.GetValue().(*sequence.Sequence); isSeq {
				if err := r.decodeRawItems(inner, isrc, depth+1); err != nil {
					return err
				}
				continue
			}
			vr := string(elem.GetVR())
			b, ok := elem.Bytes()
			if !ok || !textByCharset[vr] {
				continue
			}
			text, ok := decodeText(b, vr, isrc)
			if !ok {
				return errNotText
			}
			if text != string(b) {
				elem.SetValue([]byte(text))
			}
		}
		if hasDeclared {
			declared.SetValue([]byte("ISO_IR 192"))
		}
	}
	return nil
}

// ptr returns a pointer to s.
func ptr(s string) *string { return &s }

// binaryWidths are the bytes of one value of each VR Table E.3.10-1 may
// give that is not text (PS3.5 6.2).
var binaryWidths = map[string]int{"FL": 4, "FD": 8, "SL": 4, "SS": 2, "UL": 4, "US": 2, "SV": 8, "UV": 8, "AT": 4,
	"OB": 1, "OW": 2, "OF": 4, "OD": 8, "OL": 4, "OV": 8}

// keepSafePrivate gives a private element the file holds as UN, which the
// profile keeps and the floor does not cover, the VR PS3.15 Table
// E.3.10-1 gives it (safePrivateVR), when the version is built on the
// Retain Safe Private base of the table's edition (Engine.safeVRs) and the
// value is what that VR says: a whole number of values of a binary VR that
// do not read as text, or text that decodes as the file wrote it and is a
// value of the VR. A text value is then kept decoded, with that VR. Any
// other value stays UN, and is judged as one: taken for a name when it
// reads as one (needleVR), which the floor removes. The table says which
// attributes are safe, not what a file put in them; a name written into
// one stays a name.
func (r *run) keepSafePrivate(c *target, creator string, e *Entry) {
	if !r.en.safeVRs {
		return
	}
	tvr := safePrivateVR(c.tag, creator)
	b, ok := c.elem.Bytes()
	if tvr == "UN" || !ok {
		return
	}
	if w, binary := binaryWidths[tvr]; binary {
		if _, printable := unknownText(naming.TrimValue("UN", string(b))); len(b)%w != 0 || printable {
			return
		}
		c.vr, c.nvr, e.VR = tvr, tvr, tvr
		r.ops = append(r.ops, op{kind: opValue, ds: c.ds, tag: c.tag, value: b, vr: dataelem.VR(tvr)})
		return
	}
	if !charsetVRs[tvr] {
		return
	}
	text, ok := decodeText(b, tvr, c.sc.source)
	if !ok || looksLikeName(naming.TrimValue(tvr, text)) || !validText(tvr, naming.TrimValue(tvr, text)) {
		return
	}
	c.vr, c.nvr, e.VR = tvr, tvr, tvr
	r.ops = append(r.ops, op{kind: opValue, ds: c.ds, tag: c.tag, value: []byte(text), vr: dataelem.VR(tvr)})
}

// safePrivateVR returns the VR PS3.15 Table E.3.10-1 gives a private
// element, matched by its group, its creator and its element within the
// creator's block (anonymize.LookupSafePrivate), and "UN" when the table
// does not list it or gives no VR. keepSafePrivate says when it is used.
func safePrivateVR(t tag.Tag, creator string) string {
	if t.Group()%2 == 0 || t.Element()>>8 < 0x10 {
		return "UN"
	}
	vr, ok := anonymize.LookupSafePrivate(t.Group(), creator, byte(t.Element()&0x00FF))
	if !ok || vr == "" || vr == "SQ" || vr == "UN" {
		return "UN"
	}
	return string(vr)
}

// actUnknownItems applies an action to a sequence held as UN bytes: it is
// parsed, its text decoded as the file wrote it (decodeRawItems), and
// acted on as the sequence it is (actSequence); when kept or emptied it is
// written back as a sequence. A value that is not items, or whose text
// cannot be decoded, is removed unread by an action that removes it, and
// refuses the file for any other: it cannot be judged.
func (r *run) actUnknownItems(c *target, a Action) (bool, error) {
	seq, err := decodeUnknownItems(c.unItems)
	if err == nil {
		err = r.decodeRawItems(seq, c.sc.source, len(c.seqs)+1)
	}
	if err != nil {
		if a.Kind == ActRemove || (a.Kind == ActRemoveUnlessRequired && r.required(c) == requiredNot) {
			r.record.Entries[c.entry].Changed = true
			r.ops = append(r.ops, op{kind: opRemove, ds: c.ds, tag: c.tag})
			return true, nil
		}
		var f *Failure
		if errors.As(err, &f) {
			f.Path = c.path
			return false, f
		}
		return false, c.fail(CauseBadValue)
	}
	c.unItems, c.seq, c.seqVR = nil, seq, dataelem.SQ
	return r.actSequence(c, a)
}

// escaped reports whether a value holds the escape of an ISO 2022 code
// extension: text no decoder turned into UTF-8, whose bytes could be a
// name in any script. Such a value is never kept.
func escaped(elem *dataelem.DataElement) bool {
	b, ok := elem.Bytes()
	return ok && bytes.IndexByte(b, esc) >= 0
}
