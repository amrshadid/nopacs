package profile

import (
	"slices"
)

// contentValue is a value element of a content item and its VR.
type contentValue struct {
	tag uint32
	vr  string
}

// contentValues are the value elements of a structured report content item
// for each value type that has a value (PS3.3 C.17.3.3, Table C.17-5, and
// the macros of C.18 it includes): the element a content item rule
// selects. A CONTAINER has none: its children are content items of their
// own. The VRs are written here rather than read from the data dictionary,
// since they are part of what a content item rule means and a stored
// version must not change with the dictionary.
var contentValues = map[string][]contentValue{
	"TEXT":      {{0x0040A160, "UT"}},                                         // Text Value
	"NUM":       {{0x0040A300, "SQ"}, {0x0040A301, "SQ"}},                     // Measured Value Sequence, Numeric Value Qualifier Code Sequence (C.18.1)
	"CODE":      {{0x0040A168, "SQ"}},                                         // Concept Code Sequence (C.18.2)
	"DATE":      {{0x0040A121, "DA"}},                                         // Date
	"TIME":      {{0x0040A122, "TM"}},                                         // Time
	"DATETIME":  {{0x0040A120, "DT"}},                                         // DateTime
	"PNAME":     {{0x0040A123, "PN"}},                                         // Person Name
	"UIDREF":    {{0x0040A124, "UI"}},                                         // UID
	"COMPOSITE": {{0x00081199, "SQ"}},                                         // Referenced SOP Sequence (C.18.3)
	"IMAGE":     {{0x00081199, "SQ"}},                                         // Referenced SOP Sequence (C.18.4)
	"WAVEFORM":  {{0x00081199, "SQ"}},                                         // Referenced SOP Sequence (C.18.5)
	"SCOORD":    {{0x00700022, "FL"}},                                         // Graphic Data (C.18.6)
	"SCOORD3D":  {{0x00700022, "FL"}},                                         // Graphic Data (C.18.9)
	"TCOORD":    {{0x0040A132, "UL"}, {0x0040A138, "DS"}, {0x0040A13A, "DT"}}, // Referenced Sample Positions, Time Offsets, DateTime (C.18.7)
	"TABLE":     {{0x0040A801, "SQ"}},                                         // Tabulated Values Sequence (C.18.10)
}

// ValueTypes returns the value types of content items that have a value
// element, in byte order: the value types a content item selector may
// name.
func ValueTypes() []string {
	out := make([]string, 0, len(contentValues))
	for vt := range contentValues {
		out = append(out, vt)
	}
	slices.Sort(out)
	return out
}

// IsContentValue reports whether tag is a value element of a content item
// of valueType (PS3.3 C.17.3.3): the element a content item rule selects
// in such an item. The engine gives a content item's elements their
// item's concept and value type in Location; only the value element is
// selected by a content item rule.
func IsContentValue(valueType string, tag uint32) bool {
	for _, v := range contentValues[valueType] {
		if v.tag == tag {
			return true
		}
	}
	return false
}

// contentValueVRs returns the VRs of a value type's value elements, each
// once, in byte order; nil for no value type, which says nothing of the
// VR.
func contentValueVRs(valueType string) []string {
	var out []string
	for _, v := range contentValues[valueType] {
		if !slices.Contains(out, v.vr) {
			out = append(out, v.vr)
		}
	}
	slices.Sort(out)
	return out
}
