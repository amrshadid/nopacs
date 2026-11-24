package engine

import (
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// What a needle is made from. A needle is a value searched for after
// de-identification, in the file and by clean in its free text; a value
// found again holds the file. So a needle is made only from a value that
// can identify someone by itself, and never from one that is merely
// removed: a needle that identifies no one makes false holds on ordinary
// words and numbers (a weight of 0.000000 removed by the basic profile is
// found in a gantry tilt of 0.000000; a series description "Scout" in Scan
// Options "SCOUT MODE") without catching anything the identifying needles
// miss, and a high rate of false holds teaches people to release held
// files without looking.
//
// A value an action removes or replaces is classed by needleKindOf, from
// the attribute, its VR and how it was removed:
//
//   - a name (NeedleName): any person's name, which the floor covers
//     (needleVR: an element of VR PN, a standard attribute the data
//     dictionary gives VR PN whatever VR the file wrote, a private value of
//     unknown VR that has a name's form);
//   - a date (NeedleDate): a value of VR DA or DT, standard or private;
//   - a UID (NeedleUID): a value of VR UI the standard does not register;
//   - an identifier (NeedleText): the value of an attribute of the floor
//     (floorTags) or of identifierTags, the rows of PS3.15 Table E.1-1 whose
//     value identifies a person, a place, a device, an order or a study by
//     itself, whatever VR the file wrote it with; a GPS coordinate
//     (coordinateTags), which locates a place by itself, the one value of a
//     numeric VR that is a needle, matched only where no digit is next to
//     it; the value of a standard attribute the data dictionary does not
//     know, read as UN (Name to Use, Effective Start DateTime, Display URI
//     in this build's dictionary), since what it holds is not known; and
//     any value of a text VR (AE, LO, LT, SH, ST, UC, UR, UT) a rule hashes
//     or looks up, since a profile that keeps a pseudonym of a value treats
//     it as an identifier (a hashed coded string is still none).
//
// A date of only zeros is never one (allZeros). Not needles: a time of day
// (VR TM), which identifies no one alone, is found in ordinary numbers
// (a Study Time of 1200 in an Exposure Time of 1200) and has written forms
// no search covers (a date and time, DT, is a date), but Patient's Birth
// Time, which the floor lists; a value of a numeric
// or binary VR (AS, DS, IS, FL, FD, SL, SS, SV, UL, US, UV, OB, OD, OF, OL,
// OV, OW) but a coordinate; a coded string (CS), whose values are the
// standard's or a vendor's terms; the text of any other standard
// attribute, a description, a comment, a protocol's or a series' name,
// which a profile removes only because it might hold an identifier, and in
// which a name or an identifier typed into it is found through that name's
// or identifier's own needle; and a private value but a name, a date or a
// UID: a private element's meaning is not known, and one of unknown VR is
// a needle only when it has a name's form (the floor takes it as a name).
//
// The exception is a sequence covered whole (wholeTags) when it is
// removed or emptied, at any depth: one the floor covers (Other Patient
// IDs Sequence, Person Identification Code Sequence), a person's or an
// institution's identification code sequence, an issuer's sequence. All
// it holds names or identifies, so every value in it is a needle but a
// number, a coded string, a time and the attributes that name a coding
// scheme (codingTags); a private value of unknown VR among them when it
// reads as text.

// identifierTags are the attributes, besides the floor's (floorTags),
// whose text value is an identifier needle: the rows of PS3.15 Table E.1-1
// (2026d, every one removed or replaced by the basic profile) whose value
// names or identifies by itself a person, an institution or a place, a
// device or station, an order, a study, a specimen or a trial's subject or
// site: identifiers and their issuers, accession and order numbers,
// institution, department, station and organization names, addresses,
// telephone numbers and AE titles; and the rows that can copy any value
// (a hanging protocol's selector values, the system and source of an
// attribute's previous values). A row of free text (a description, a
// comment, a reason, a label, an acquisition protocol's name) is not here,
// nor one of a person's name (VR PN, a name through needleVR). Every row
// of the table is classed (TestEveryIdentifyingRowIsANeedle).
var identifierTags = map[uint32]string{
	0x00080050: "Accession Number",
	0x00080054: "Retrieve AE Title",
	0x00080055: "Station AE Title",
	0x00080080: "Institution Name",
	0x00080081: "Institution Address",
	0x00081000: "Network ID",
	0x00081010: "Station Name",
	0x00081040: "Institutional Department Name",
	0x00100021: "Issuer of Patient ID",
	0x00101050: "Insurance Plan Identification",
	0x00102299: "Responsible Organization",
	0x00120010: "Clinical Trial Sponsor Name",
	0x00120020: "Clinical Trial Protocol ID",
	0x00120022: "Issuer of Clinical Trial Protocol ID",
	0x00120030: "Clinical Trial Site ID",
	0x00120031: "Clinical Trial Site Name",
	0x00120032: "Issuer of Clinical Trial Site ID",
	0x00120040: "Clinical Trial Subject ID",
	0x00120041: "Issuer of Clinical Trial Subject ID",
	0x00120042: "Clinical Trial Subject Reading ID",
	0x00120043: "Issuer of Clinical Trial Subject Reading ID",
	0x00120060: "Clinical Trial Coordinating Center Name",
	0x00120081: "Clinical Trial Protocol Ethics Committee Name",
	0x00120082: "Clinical Trial Protocol Ethics Committee Approval Number",
	0x0016004D: "Camera Owner Name",
	0x00160051: "Lens Serial Number",
	0x00181000: "Device Serial Number",
	0x00181004: "Plate ID",
	0x00181005: "Generator ID",
	0x00181007: "Cassette ID",
	0x00181008: "Gantry ID",
	0x00181009: "Unique Device Identifier",
	0x00181010: "Secondary Capture Device ID",
	0x00181011: "Hardcopy Creation Device ID",
	0x0018700A: "Detector ID",
	0x00189367: "X-Ray Source ID",
	0x00189371: "X-Ray Detector ID",
	0x00200010: "Study ID",
	0x00320012: "Study ID Issuer",
	0x00321020: "Scheduled Study Location",
	0x00321021: "Scheduled Study Location AE Title",
	0x00321033: "Requesting Service",
	0x00380010: "Admission ID",
	0x00380011: "Issuer of Admission ID",
	0x0038001E: "Scheduled Patient Institution Residence",
	0x00380060: "Service Episode ID",
	0x00380061: "Issuer of Service Episode ID",
	0x00380300: "Current Patient Location",
	0x00400001: "Scheduled Station AE Title",
	0x00400009: "Scheduled Procedure Step ID",
	0x00400010: "Scheduled Station Name",
	0x00400011: "Scheduled Procedure Step Location",
	0x00400241: "Performed Station AE Title",
	0x00400242: "Performed Station Name",
	0x00400243: "Performed Location",
	0x00400253: "Performed Procedure Step ID",
	0x0040050A: "Specimen Accession Number",
	0x00400512: "Container Identifier",
	0x00400551: "Specimen Identifier",
	0x004006FA: "Slide Identifier",
	0x00401001: "Requested Procedure ID",
	0x00401005: "Requested Procedure Location",
	0x00402009: "Order Enterer's Location",
	0x00402010: "Order Callback Phone Number",
	0x00402011: "Order Callback Telecom Information",
	0x00402016: "Placer Order Number / Imaging Service Request",
	0x00402017: "Filler Order Number / Imaging Service Request",
	0x00404036: "Human Performer's Organization",
	0x0040A027: "Verifying Organization",
	0x0040A353: "Address (Trial)",
	0x0040A354: "Telephone Number (Trial)",
	0x0050001B: "Container Component ID",
	0x00741234: "Receiving AE",
	0x00741236: "Requesting AE",
	0x00880910: "Topic Author",
	0x21000070: "Originator",
	0x21000140: "Destination AE",
	0x22000005: "Barcode Value",
	0x30080105: "Source Serial Number",
	0x300A00B2: "Treatment Machine Name",
	0x3010001B: "Device Alternate Identifier",
	0x30100043: "Manufacturer's Device Identifier",
	0x40080040: "Results ID",
	0x40080042: "Results ID Issuer",
	0x4008011A: "Distribution Address",
	0x40080200: "Interpretation ID",
	0x40080202: "Interpretation ID Issuer",
	0x00120021: "Clinical Trial Protocol Name",
	0x00120055: "Issuer of Clinical Trial Time Point ID",
	0x00120073: "Issuer of Clinical Trial Series ID",
	0x00203401: "Modifying Device ID",
	0x0040E012: "Display URI",
	0x04000563: "Modifying System",
	0x04000564: "Source of Previous Values",
	// The selector values of a hanging protocol (PS3.3 C.23.2) match any
	// attribute, so they can copy any identifying value.
	0x0072005E: "Selector AE Value",
	0x00720066: "Selector LO Value",
	0x00720068: "Selector LT Value",
	0x0072006C: "Selector SH Value",
	0x0072006E: "Selector ST Value",
	0x00720070: "Selector UT Value",
	0x00720071: "Selector UR Value",
}

// patientIdentifierTags are the identifiers, besides the floor's
// (floorTags) and the sequences covered whole (wholeTags), that identify
// the patient or the patient's encounter, order or study: the accession
// and order numbers, the admission, service episode, procedure step,
// study and trial subject IDs, and every issuer of an identifier. Such a
// value is never given a word edge (wordEdgeAllowed), whatever its text:
// a copy of it glued to other text ("MRN_SMITHJ") is how it leaks, and a
// false hold costs less than a leak of it.
var patientIdentifierTags = map[uint32]string{
	0x00080050: "Accession Number",
	0x00100021: "Issuer of Patient ID",
	0x00120022: "Issuer of Clinical Trial Protocol ID",
	0x00120032: "Issuer of Clinical Trial Site ID",
	0x00120040: "Clinical Trial Subject ID",
	0x00120041: "Issuer of Clinical Trial Subject ID",
	0x00120042: "Clinical Trial Subject Reading ID",
	0x00120043: "Issuer of Clinical Trial Subject Reading ID",
	0x00120055: "Issuer of Clinical Trial Time Point ID",
	0x00120073: "Issuer of Clinical Trial Series ID",
	0x00200010: "Study ID",
	0x00320012: "Study ID Issuer",
	0x00380010: "Admission ID",
	0x00380011: "Issuer of Admission ID",
	0x00380060: "Service Episode ID",
	0x00380061: "Issuer of Service Episode ID",
	0x00400009: "Scheduled Procedure Step ID",
	0x00400253: "Performed Procedure Step ID",
	0x0040050A: "Specimen Accession Number",
	0x00401001: "Requested Procedure ID",
	0x00402016: "Placer Order Number / Imaging Service Request",
	0x00402017: "Filler Order Number / Imaging Service Request",
	0x40080042: "Results ID Issuer",
	0x40080202: "Interpretation ID Issuer",
}

// wordEdgeAllowed reports whether an identifier at path (a record's path,
// "00081115[0]/00100020") may be given a word edge: neither it nor any
// sequence it is inside is a floor attribute, a sequence covered whole or
// one of patientIdentifierTags.
func wordEdgeAllowed(path string) bool {
	for _, step := range strings.Split(path, "/") {
		ts, _, _ := strings.Cut(step, "[")
		t, err := strconv.ParseUint(ts, 16, 32)
		if err != nil || len(ts) != 8 {
			return false
		}
		_, floor := floorTags[uint32(t)]
		_, whole := wholeTags[uint32(t)]
		_, patient := patientIdentifierTags[uint32(t)]
		if floor || whole || patient {
			return false
		}
	}
	return true
}

// coordinateTags are the GPS coordinates of PS3.15 Table E.1-1 (VR DS):
// a latitude, a longitude or an altitude locates a place by itself, so
// each is an identifier needle though its VR is numeric. The check after
// matches a needle that begins or ends with a digit only where no digit is
// next to it, so a coordinate is not found inside a longer number.
var coordinateTags = map[uint32]string{
	0x00160072: "GPS Latitude",
	0x00160074: "GPS Longitude",
	0x00160076: "GPS Altitude",
	0x00160084: "GPS Dest Latitude",
	0x00160086: "GPS Dest Longitude",
}

// wholeTags are the sequences whose every value is a needle when the
// sequence is removed or emptied (subtreeAll), at any depth: the floor's
// two, which name or identify the person; the identification sequences of
// PS3.15 Table E.1-1, whose code values and meanings are a person's or an
// institution's identifier; and its issuers' sequences, whose Local
// Namespace Entity ID (0040,0031) and Universal Entity ID (0040,0032) name
// the issuing organization.
var wholeTags = map[uint32]string{
	0x00101002: "Other Patient IDs Sequence",
	0x00401101: "Person Identification Code Sequence",
	0x00080082: "Institution Code Sequence",
	0x00080096: "Referring Physician Identification Sequence",
	0x0008009D: "Consulting Physician Identification Sequence",
	0x00081049: "Physician(s) of Record Identification Sequence",
	0x00081052: "Performing Physician Identification Sequence",
	0x00081062: "Physician(s) Reading Study Identification Sequence",
	0x00081072: "Operator Identification Sequence",
	0x0040000B: "Scheduled Performing Physician Identification Sequence",
	0x00401011: "Intended Recipients of Results Identification Sequence",
	0x0040A088: "Verifying Observer Identification Code Sequence",
	0x0040A358: "Verbal Source Identifier Code Sequence (Trial)",
	0x00700086: "Content Creator's Identification Code Sequence",
	0x00380014: "Issuer of Admission ID Sequence",
	0x00380064: "Issuer of Service Episode ID Sequence",
	0x00400513: "Issuer of the Container Identifier Sequence",
	0x00400562: "Issuer of the Specimen Identifier Sequence",
}

// codingTags name a coding scheme or a context group (PS3.3 8.1-8.6): the
// same in every file that uses the scheme, so inside a sequence covered
// whole they are no needle, or a scheme a kept code also names ("99LOCAL")
// would hold the file.
var codingTags = map[uint32]bool{
	0x00080102: true, // Coding Scheme Designator
	0x00080103: true, // Coding Scheme Version
	0x00080105: true, // Mapping Resource
	0x00080106: true, // Context Group Version
	0x00080107: true, // Context Group Local Version
	0x0008010B: true, // Context Group Extension Flag
	0x0008010D: true, // Context Group Extension Creator UID
	0x0008010F: true, // Context Identifier
	0x00080117: true, // Context UID
}

// needleBasis is how a value came to be removed or replaced, which can
// make it an identifier whatever its attribute.
type needleBasis int

// The bases.
const (
	// basisPlace: the value is classed by its attribute and VR alone.
	basisPlace needleBasis = iota + 1
	// basisPseudonym: a rule hashes the value or looks it up, which only
	// an identifier is given.
	basisPseudonym
	// basisWhole: the value is inside a sequence covered whole (wholeTags),
	// all of which names or identifies.
	basisWhole
)

// basisOf is the basis of a value an action removes or replaces at its
// place: a pseudonym for hash and lookup.
func basisOf(a Action) needleBasis {
	if a.Kind == ActHash || a.Kind == ActLookup {
		return basisPseudonym
	}
	return basisPlace
}

// identifierText are the VRs of text an identifier is written in.
var identifierText = map[string]bool{
	"AE": true, "LO": true, "LT": true, "SH": true, "ST": true, "UC": true, "UR": true, "UT": true,
}

// needleKindOf returns the kind of needle a removed or replaced value of
// tag t is, or 0 when it is none, nvr being the VR its value is taken as
// (needleVR: the element's VR as the engine reads it, PN for a name).
func needleKindOf(t uint32, nvr string, basis needleBasis) NeedleKind {
	private := (t>>16)%2 == 1
	if basis == basisWhole && codingTags[t] {
		return 0
	}
	switch nvr {
	case "PN":
		return NeedleName
	case "DA", "DT":
		return NeedleDate
	case "UI":
		return NeedleUID
	}
	if !private {
		// An attribute that identifies is a needle whatever VR the file
		// wrote it with (PS3.6 gives what it holds, PS3.5 6.2.2 lets a
		// writer not know its VR).
		if _, ok := floorTags[t]; ok {
			return NeedleText
		}
		if _, ok := identifierTags[t]; ok {
			return NeedleText
		}
		if _, ok := coordinateTags[t]; ok {
			return NeedleText
		}
	}
	switch {
	case nvr == "UN":
		// A standard attribute read as UN is one the data dictionary does
		// not know (effectiveVR gives a known one its VR), so what it holds
		// is not known; a private one is not a name (needleVR).
		if !private || basis == basisWhole {
			return NeedleText
		}
		return 0
	case !identifierText[nvr]:
		return 0
	case basis != basisPlace:
		return NeedleText
	}
	return 0
}

// needle keeps a removed or replaced value for the check after, as a
// needle of kind (needleKindOf; 0 keeps nothing): each value of a
// multi-valued one, and the whole. An identifier that begins or ends with
// words is taken only as a whole word (IdentifierEdge), unless it is, or
// is inside, an attribute that identifies the patient (wordEdgeAllowed).
// vr is the VR the value is taken as. A
// registered UID is not one: it names a kind of object, stays wherever
// else it is, and is no trace of the patient. A value of VR UN is one only
// when it reads as text (unknownText). A needle is kept in NFC
// (profile.NFC), the form its edge is judged in: a name ending in a
// combining accent ends in a letter once composed.
func (r *run) needle(kind NeedleKind, vr, value, path string) {
	if vr == "UN" {
		var ok bool
		if value, ok = unknownText(value); !ok {
			return
		}
	}
	if kind == 0 || value == "" {
		return
	}
	add := func(v string) {
		v = profile.NFC(strings.Trim(v, " \x00"))
		if v == "" || (kind == NeedleUID && Registered(v)) || (temporalVRs[vr] && allZeros(v)) {
			return
		}
		n := Needle{Kind: kind, Value: v, Path: path}
		if kind == NeedleText && wordEdgeAllowed(path) {
			n.Edge = IdentifierEdge(v)
		}
		r.record.Needles = append(r.record.Needles, n)
	}
	parts := strings.Split(value, `\`)
	for _, p := range parts {
		add(p)
	}
	if len(parts) > 1 {
		add(value)
	}
}

// temporalVRs are the VRs of a date or a time (PS3.5 6.2).
var temporalVRs = map[string]bool{"DA": true, "DT": true, "TM": true}

// allZeros reports whether a date or time value holds no digit but zeros
// (a time of midnight, 000000, a date never filled in, 00000000). It is
// removed as the rule says, but is no needle: it identifies no one, and
// as a run of zeros it is found inside ordinary numbers (a decimal string
// such as 500.000000), which would hold every such file for nothing.
func allZeros(v string) bool {
	digit := false
	for _, c := range v {
		switch c {
		case '0':
			digit = true
		case '.', ':', '+', '-', ' ':
		default:
			return false
		}
	}
	return digit
}
