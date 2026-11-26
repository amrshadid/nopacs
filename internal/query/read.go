package query

import (
	"encoding/binary"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// Values returns the values of an attribute that a comparison sees in a
// data set, as routing reads them: each value that is not empty, in every
// item of the sequences on the field's path, a person's name once per
// component group. A search writer keeps these values, one row each in a
// multi-valued attribute's values table, so a search compares what routing
// compares. A catalog field has no values here; a value routing cannot
// read is a *ReadError.
func Values(ds *dataset.Dataset, f Field, bigEndian bool) ([]string, error) {
	if f.Catalog != "" || len(f.Path) == 0 || ds == nil {
		return nil, nil
	}
	r := reading{f: f, bigEndian: bigEndian}
	if err := r.collect(ds, f.Path); err != nil {
		return nil, err
	}
	return r.values, nil
}

// reading gathers the values of one field over a data set.
type reading struct {
	f         Field
	bigEndian bool
	values    []string
	present   bool
}

// collect gathers the values at path in ds and, through a sequence, in each
// of its items.
func (r *reading) collect(ds *dataset.Dataset, path []Step) error {
	elem := find(ds, path[0])
	if elem == nil {
		return nil
	}
	seq, isSeq := elem.GetValue().(*sequence.Sequence)
	if len(path) > 1 {
		if !isSeq {
			return nil
		}
		for _, it := range seq.Items() {
			if item, ok := it.(*dataset.Dataset); ok {
				if err := r.collect(item, path[1:]); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if isSeq {
		r.present = r.present || seq.Length() > 0
		return nil
	}
	vals, vr, err := r.elementValues(elem, path[0])
	if err != nil {
		return err
	}
	if r.f.Indexed {
		if r.f.Index >= len(vals) {
			return nil
		}
		vals = vals[r.f.Index : r.f.Index+1]
	}
	for _, v := range vals {
		if vr == "PN" {
			r.addGroups(v)
			continue
		}
		if v != "" {
			r.values = append(r.values, v)
			r.present = true
		}
	}
	return nil
}

// addGroups adds the component groups of a person's name that are not
// empty, each a value: a name is written once per group, alphabetic,
// ideographic and phonetic, separated by "=" (PS3.5 6.2.1.2), and a
// comparison matches a name by any of them, as PS3.4 C.2.2.2.1 lets a
// matching key be written in any group. A search keeps the same groups,
// one row each, so search and routing match a name alike.
func (r *reading) addGroups(name string) {
	for _, g := range strings.Split(name, "=") {
		if g = naming.TrimValue("PN", g); g != "" {
			r.values = append(r.values, g)
			r.present = true
		}
	}
}

// find returns the element a step names in ds, nil when ds has none. A
// private tag by creator is found in the block the data set reserves for
// that creator in the step's group (PS3.5 7.8.1), as go-dicom finds it.
func find(ds *dataset.Dataset, s Step) *dataelem.DataElement {
	if s.Creator == "" {
		e, ok := ds.Get(tag.Tag(s.Tag))
		if !ok {
			return nil
		}
		return e
	}
	e, err := ds.GetPrivateItem(uint16(s.Tag>>16), uint8(s.Tag&0xFF), s.Creator) // #nosec G115 -- a group and a byte, masked.
	if err != nil {
		return nil
	}
	return e
}

// splitVRs are the text VRs whose value may hold several values separated
// by a backslash (PS3.5 6.2: LT, ST, UT and UR hold one value, in which a
// backslash is a character).
var splitVRs = map[string]bool{
	"AE": true, "AS": true, "CS": true, "DA": true, "DS": true, "DT": true, "IS": true,
	"LO": true, "PN": true, "SH": true, "TM": true, "UC": true, "UI": true,
}

// textVRs are the VRs whose value is text.
var textVRs = map[string]bool{"LT": true, "ST": true, "UT": true, "UR": true}

// elementValues returns an element's values as text: text split into its
// values and trimmed of padding (naming.TrimValue), a binary number written
// in decimal, an attribute tag as GGGGEEEE; a value of VR UN, which a
// private element of an implicit-VR file is, read as text when it is
// UTF-8 with no NUL; nothing for other bytes. It returns the VR the values
// were read by.
func (r *reading) elementValues(elem *dataelem.DataElement, s Step) ([]string, string, error) {
	b, ok := elem.Bytes()
	if !ok {
		return nil, "", &ReadError{Field: FieldString(r.f), Why: "the value is not held in memory"}
	}
	if len(b) > MaxReadLength {
		return nil, "", &ReadError{Field: FieldString(r.f), Why: "the value is longer than " + strconv.Itoa(MaxReadLength) + " bytes"}
	}
	vr := string(elem.GetVR())
	if (vr == "UN" || vr == "") && s.Creator == "" && (s.Tag>>16)%2 == 0 {
		// PS3.5 6.2.2: UN is a VR the writer did not know, not the
		// attribute's; the dictionary knows it.
		if d := tag.Tag(s.Tag).GetVR(); d != "" && !strings.Contains(d, " or ") {
			vr = d
		}
	}
	switch {
	case splitVRs[vr]:
		parts := strings.Split(string(b), `\`)
		for i, p := range parts {
			parts[i] = naming.TrimValue(vr, p)
		}
		return parts, vr, nil
	case textVRs[vr]:
		return []string{naming.TrimValue(vr, string(b))}, vr, nil
	case vr == "UN" || vr == "":
		t := strings.TrimRight(string(b), " \x00")
		if !utf8.ValidString(t) || strings.ContainsRune(t, 0) {
			return nil, vr, nil
		}
		return []string{strings.TrimLeft(t, " ")}, vr, nil
	}
	return r.binary(vr, b), vr, nil
}

// binary returns the values of a binary VR in decimal (US, SS, UL, SL, UV,
// SV, FL, FD) or as GGGGEEEE (AT); nothing for the others.
func (r *reading) binary(vr string, b []byte) []string {
	var order binary.ByteOrder = binary.LittleEndian
	if r.bigEndian {
		order = binary.BigEndian
	}
	width := map[string]int{"US": 2, "SS": 2, "UL": 4, "SL": 4, "FL": 4, "AT": 4, "UV": 8, "SV": 8, "FD": 8}[vr]
	if width == 0 || len(b)%width != 0 {
		return nil
	}
	out := make([]string, 0, len(b)/width)
	for i := 0; i+width <= len(b); i += width {
		w := b[i : i+width]
		var s string
		switch vr {
		case "US":
			s = strconv.FormatUint(uint64(order.Uint16(w)), 10)
		case "SS":
			s = strconv.FormatInt(int64(int16(order.Uint16(w))), 10) // #nosec G115 -- reinterpreting the 16 bits as signed is what SS means.
		case "UL":
			s = strconv.FormatUint(uint64(order.Uint32(w)), 10)
		case "SL":
			s = strconv.FormatInt(int64(int32(order.Uint32(w))), 10) // #nosec G115 -- reinterpreting the 32 bits as signed is what SL means.
		case "UV":
			s = strconv.FormatUint(order.Uint64(w), 10)
		case "SV":
			s = strconv.FormatInt(int64(order.Uint64(w)), 10) // #nosec G115 -- reinterpreting the 64 bits as signed is what SV means.
		case "FL":
			s = strconv.FormatFloat(float64(math.Float32frombits(order.Uint32(w))), 'g', -1, 32)
		case "FD":
			s = strconv.FormatFloat(math.Float64frombits(order.Uint64(w)), 'g', -1, 64)
		case "AT":
			s = hex4(uint32(order.Uint16(w[:2]))) + hex4(uint32(order.Uint16(w[2:])))
		}
		out = append(out, s)
	}
	return out
}

// parseNumber reads a value as a number: an IS or DS value, or a binary
// number written in decimal. A value that is not one (a malformed DS)
// matches no number.
func parseNumber(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return 0, false
	}
	return f, true
}

// digitsOnly returns s without the separators an old form of a date or
// time holds (ACR-NEMA's 2004.01.19 and 10:12:00, which PS3.5 6.2 notes
// some writers still use), or false when it holds anything else.
func digitsOnly(s string, seps string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case isDigit(c):
			b.WriteByte(c)
		case strings.IndexByte(seps, c) >= 0:
		default:
			return "", false
		}
	}
	return b.String(), true
}

// normalDate reads a DA value, or the date of a DT value, as YYYYMMDD: the
// first eight characters when they are digits, or the old form 2004.01.19.
func normalDate(s string) (string, bool) {
	if len(s) >= 8 {
		if d, ok := digitsOnly(s[:8], ""); ok {
			return d, true
		}
	}
	if len(s) == 10 && s[4] == '.' && s[7] == '.' {
		if d, ok := digitsOnly(s, "."); ok && len(d) == 8 {
			return d, true
		}
	}
	return "", false
}

// normalTime reads a TM value as HHMMSS: the parts it leaves out are zero
// (PS3.5 6.2, TM: "HH", "HHMM" and "HHMMSS" are each a time), its fraction
// dropped.
func normalTime(s string) (string, bool) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	d, ok := digitsOnly(s, ":")
	if !ok || len(d) == 0 || len(d) > 6 || len(d)%2 != 0 {
		return "", false
	}
	return d + "000000"[len(d):], true
}

// normalDateTime reads a DT value, or a DA value, as YYYYMMDDHHMMSS: the
// parts it leaves out are zero (a year alone is its first day), its
// fraction and its offset from UTC dropped (PS3.5 6.2, DT).
func normalDateTime(s string) (string, bool) {
	if len(s) == 10 && s[4] == '.' && s[7] == '.' {
		d, ok := normalDate(s)
		return d + "000000", ok
	}
	if i := strings.IndexAny(s, "+-"); i >= 0 {
		s = s[:i]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	d, ok := digitsOnly(s, "")
	if !ok || len(d) < 4 || len(d) > 14 || len(d)%2 != 0 {
		return "", false
	}
	if len(d) < 8 {
		return (d + "0101")[:8] + "000000", true
	}
	return d + "00000000000000"[len(d):], true
}
