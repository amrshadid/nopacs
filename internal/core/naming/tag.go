package naming

import (
	"errors"
	"strconv"
)

// Tag is a DICOM attribute's tag, its group in the high 16 bits and its
// element in the low 16 (PS3.5 7.1).
type Tag uint32

// NewTag returns the tag of group and element.
func NewTag(group, element uint16) Tag { return Tag(group)<<16 | Tag(element) }

// Group returns the tag's group number.
func (t Tag) Group() uint16 { return uint16(t >> 16) }

// Element returns the tag's element number.
func (t Tag) Element() uint16 { return uint16(t & 0xFFFF) }

// String returns the tag as (gggg,eeee) in upper case hexadecimal, the form
// PS3.6 lists tags in.
func (t Tag) String() string {
	const hex = "0123456789ABCDEF"
	b := []byte("(0000,0000)")
	for i, v := range []uint16{t.Group(), t.Element()} {
		for j := range 4 {
			b[1+i*5+j] = hex[v>>(12-4*j)&0xF]
		}
	}
	return string(b)
}

// keyword is one attribute a rule may name by keyword.
type keyword struct {
	tag Tag
	// uid marks an attribute of value representation UI, whose value is
	// also checked as a UID (PS3.5 9.1) before it enters a label.
	uid bool
}

// keywords is the attributes a naming rule plausibly uses, with their tags
// as PS3.6 gives them: patient, study, series and instance identifiers,
// dates and numbers, descriptions and comments, where the data came from,
// and the clinical trial attributes (PS3.3 C.7.1.3) that research sites use
// to say which subject and time point a study belongs to. The list is short
// on purpose: each entry is a fact that must be right, and any other
// attribute can be named by its tag.
var keywords = map[string]keyword{
	"PatientID":                   {tag: 0x00100020},
	"PatientName":                 {tag: 0x00100010},
	"IssuerOfPatientID":           {tag: 0x00100021},
	"PatientBirthDate":            {tag: 0x00100030},
	"PatientSex":                  {tag: 0x00100040},
	"PatientAge":                  {tag: 0x00101010},
	"PatientComments":             {tag: 0x00104000},
	"StudyInstanceUID":            {tag: 0x0020000D, uid: true},
	"StudyID":                     {tag: 0x00200010},
	"StudyDate":                   {tag: 0x00080020},
	"StudyTime":                   {tag: 0x00080030},
	"StudyDescription":            {tag: 0x00081030},
	"AccessionNumber":             {tag: 0x00080050},
	"RequestedProcedureID":        {tag: 0x00401001},
	"SeriesInstanceUID":           {tag: 0x0020000E, uid: true},
	"SeriesNumber":                {tag: 0x00200011},
	"SeriesDate":                  {tag: 0x00080021},
	"SeriesTime":                  {tag: 0x00080031},
	"SeriesDescription":           {tag: 0x0008103E},
	"ProtocolName":                {tag: 0x00181030},
	"BodyPartExamined":            {tag: 0x00180015},
	"Modality":                    {tag: 0x00080060},
	"AcquisitionDate":             {tag: 0x00080022},
	"AcquisitionTime":             {tag: 0x00080032},
	"AcquisitionNumber":           {tag: 0x00200012},
	"ContentDate":                 {tag: 0x00080023},
	"InstanceNumber":              {tag: 0x00200013},
	"ImageComments":               {tag: 0x00204000},
	"SOPInstanceUID":              {tag: 0x00080018, uid: true},
	"SOPClassUID":                 {tag: 0x00080016, uid: true},
	"FrameOfReferenceUID":         {tag: 0x00200052, uid: true},
	"InstitutionName":             {tag: 0x00080080},
	"InstitutionalDepartmentName": {tag: 0x00081040},
	"StationName":                 {tag: 0x00081010},
	"Manufacturer":                {tag: 0x00080070},
	"ManufacturerModelName":       {tag: 0x00081090},
	"ClinicalTrialSponsorName":    {tag: 0x00120010},
	"ClinicalTrialProtocolID":     {tag: 0x00120020},
	"ClinicalTrialSiteID":         {tag: 0x00120030},
	"ClinicalTrialSubjectID":      {tag: 0x00120040},
	"ClinicalTrialTimePointID":    {tag: 0x00120050},
	"ClinicalTrialSeriesID":       {tag: 0x00120071},
}

// Keywords returns the keywords a rule may use, each with its tag, as a new
// map each call.
func Keywords() map[string]Tag {
	m := make(map[string]Tag, len(keywords))
	for k, v := range keywords {
		m[k] = v.tag
	}
	return m
}

// ParseAttribute returns the tag an attribute written in a rule names: a
// keyword of Keywords, or a tag written (gggg,eeee) in hexadecimal of
// either case. It refuses a tag no data set's attribute can have or whose
// value is not text: groups 0000 to 0007 (the command set, the file meta
// information and the groups PS3.5 7.8.1 keeps out of private use), group
// FFFE (item and delimiter tags, PS3.5 7.5), group 7FE0 (pixel data), and
// element 0000 (a group length, PS3.5 7.2).
func ParseAttribute(s string) (Tag, error) {
	if k, ok := keywords[s]; ok {
		return k.tag, nil
	}
	if len(s) != 11 || s[0] != '(' || s[5] != ',' || s[10] != ')' {
		return 0, errors.New("is neither a known keyword nor a tag written (gggg,eeee)")
	}
	if !hex4(s[1:5]) || !hex4(s[6:10]) {
		return 0, errors.New("is not a tag: (gggg,eeee) takes four hexadecimal digits each")
	}
	g, err1 := strconv.ParseUint(s[1:5], 16, 16)
	e, err2 := strconv.ParseUint(s[6:10], 16, 16)
	if err1 != nil || err2 != nil {
		return 0, errors.New("is not a tag: (gggg,eeee) takes four hexadecimal digits each")
	}
	t := NewTag(uint16(g), uint16(e))
	switch {
	case t.Group() <= 0x0007:
		return 0, errors.New("is in groups 0000 to 0007, which hold no attribute of a data set")
	case t.Group() == 0xFFFE:
		return 0, errors.New("is an item or delimiter tag, not an attribute")
	case t.Group() == 0x7FE0:
		return 0, errors.New("is in group 7FE0, which holds pixel data, not text")
	case t.Element() == 0x0000:
		return 0, errors.New("is a group length, not an attribute")
	}
	return t, nil
}

// hex4 reports whether s is exactly four hexadecimal digits, so a tag has
// one written form per case and nothing ParseUint might read differently.
func hex4(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return len(s) == 4
}

// isUID reports whether an attribute written in a rule is one of the
// keywords whose value is a UID. An attribute written by tag is not known
// to be one, so its value is held only to the rule for labels.
func isUID(written string) bool { return keywords[written].uid }
