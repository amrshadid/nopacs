package engine

import (
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/tag"
)

// identifierVRExceptions are the identifying rows whose VR in the data
// dictionary is not one of text, each with why it is an identifier still.
var identifierVRExceptions = map[uint32]string{
	0x00203401: "Modifying Device ID is CS, but names the one device that changed the image",
	0x0040E012: "Display URI is not in this build's data dictionary, and holds an address",
}

// TestIdentifiersAreRowsTheBasicProfileRemoves guards the set of
// identifying attributes: each is a row of PS3.15 Table E.1-1 that the
// basic profile removes or replaces (never keeps or cleans), of a VR an
// identifier is written in, and not one the floor already lists; so the
// set names nothing the standard does not, and no row of a name, a date or
// a UID, which are needles by their VR.
func TestIdentifiersAreRowsTheBasicProfileRemoves(t *testing.T) {
	for tg, name := range identifierTags {
		e, ok := anonymize.LookupProfileEntry(tag.Tag(tg))
		if !ok {
			t.Errorf("%08X %s is not in Table E.1-1", tg, name)
			continue
		}
		if e.Name != name {
			t.Errorf("%08X is %q in Table E.1-1, listed as %q", tg, e.Name, name)
		}
		if c := e.Basic(); c == "" || c == "K" || c == "C" {
			t.Errorf("%08X %s: the basic profile's code is %q", tg, name, c)
		}
		vrs := strings.Split(tag.Tag(tg).GetVR(), " or ")
		if why, ok := identifierVRExceptions[tg]; !ok && !slices.ContainsFunc(vrs, func(v string) bool { return identifierText[v] }) {
			t.Errorf("%08X %s: VR %v holds no identifier text", tg, name, vrs)
		} else if ok && why == "" {
			t.Errorf("%08X %s: an exception with no reason", tg, name)
		}
		if _, ok := floorTags[tg]; ok {
			t.Errorf("%08X %s is listed by the floor too", tg, name)
		}
	}
}

// TestNeedleKindOf guards how a removed value is classed: by its VR for a
// name, a date and a UID; as an identifier for an attribute of the floor
// or of the identifying set, whatever VR the file wrote, for a GPS
// coordinate and for a standard attribute the dictionary does not know;
// as none for a time, a numeric VR, a coded string, a description or a
// private value of no known kind; and as an identifier for any text a rule
// hashes or looks up, and for every value but a number, a code, a time or
// a coding scheme inside a sequence covered whole.
func TestNeedleKindOf(t *testing.T) {
	for _, c := range []struct {
		name  string
		tag   uint32
		nvr   string
		basis needleBasis
		want  NeedleKind
	}{
		{"a name", 0x00080090, "PN", basisPlace, NeedleName},
		{"a private name of unknown VR", 0x00091010, "PN", basisPlace, NeedleName},
		{"a date", 0x00080020, "DA", basisPlace, NeedleDate},
		{"a private date", 0x00091020, "DA", basisPlace, NeedleDate},
		{"a date and time", 0x0008002A, "DT", basisPlace, NeedleDate},
		{"a time", 0x00080030, "TM", basisPlace, 0},
		{"a birth time, of the floor", 0x00100032, "TM", basisPlace, NeedleText},
		{"a time in a sequence covered whole", 0x00080030, "TM", basisWhole, 0},
		{"a GPS latitude", 0x00160072, "DS", basisPlace, NeedleText},
		{"a GPS altitude reference", 0x00160075, "US", basisPlace, 0},
		{"a standard attribute the dictionary does not know", 0x00100012, "UN", basisPlace, NeedleText},
		{"a modifying device's ID", 0x00203401, "CS", basisPlace, NeedleText},
		{"a selector's value", 0x00720066, "LO", basisPlace, NeedleText},
		{"a coding scheme in a sequence covered whole", 0x00080102, "SH", basisWhole, 0},
		{"a code value in a sequence covered whole", 0x00080100, "SH", basisWhole, NeedleText},
		{"a UID", 0x0020000D, "UI", basisPlace, NeedleUID},
		{"the patient's identifier", 0x00100020, "LO", basisPlace, NeedleText},
		{"the patient's identifier written as IS", 0x00100020, "IS", basisPlace, NeedleText},
		{"an accession number", 0x00080050, "SH", basisPlace, NeedleText},
		{"an institution's address", 0x00080081, "ST", basisPlace, NeedleText},
		{"a station name written as CS", 0x00081010, "CS", basisPlace, NeedleText},
		{"a weight", 0x00101030, "DS", basisPlace, 0},
		{"an age", 0x00101010, "AS", basisPlace, 0},
		{"a sex", 0x00100040, "CS", basisPlace, 0},
		{"a series description", 0x0008103E, "LO", basisPlace, 0},
		{"a protocol's name", 0x00181030, "LO", basisPlace, 0},
		{"a comment", 0x00204000, "LT", basisPlace, 0},
		{"a private identifier", 0x00091011, "LO", basisPlace, 0},
		{"a private value of unknown VR", 0x00091011, "UN", basisPlace, 0},
		{"a hashed description", 0x00081030, "LO", basisPseudonym, NeedleText},
		{"a hashed private identifier", 0x00091011, "LO", basisPseudonym, NeedleText},
		{"a hashed code", 0x00080060, "CS", basisPseudonym, 0},
		{"a code meaning in a sequence covered whole", 0x00080104, "LO", basisWhole, NeedleText},
		{"a private value in a sequence covered whole", 0x00091011, "UN", basisWhole, NeedleText},
		{"a code in a sequence covered whole", 0x00100022, "CS", basisWhole, 0},
		{"a number in a sequence covered whole", 0x00101030, "DS", basisWhole, 0},
	} {
		if got := needleKindOf(c.tag, c.nvr, c.basis); got != c.want {
			t.Errorf("%s: kind %d, want %d", c.name, got, c.want)
		}
	}
}

// nonIdentifiers are the rows of PS3.15 Table E.1-1 the basic profile
// removes or replaces, of a text VR, whose value is no needle, each with
// why: free text is removed because it might hold an identifier, and what
// is typed into it is found through that identifier's own needle.
var nonIdentifiers = map[uint32]string{
	0x00080201: "Timezone Offset From UTC: a zone shared by a whole region",
	0x00081030: "Study Description: free text, removed because it might hold an identifier",
	0x0008103E: "Series Description: free text, removed because it might hold an identifier",
	0x00081080: "Admitting Diagnoses Description: free text, removed because it might hold an identifier",
	0x00081088: "Pyramid Description: free text, removed because it might hold an identifier",
	0x00082111: "Derivation Description: free text, removed because it might hold an identifier",
	0x00084000: "Identifying Comments: free text, removed because it might hold an identifier",
	0x00101080: "Military Rank: a characteristic shared by many people",
	0x00101081: "Branch of Service: a characteristic shared by many people",
	0x00102000: "Medical Alerts: clinical text about the patient",
	0x00102110: "Allergies: clinical text about the patient",
	0x00102160: "Ethnic Group: a characteristic shared by many people",
	0x00102180: "Occupation: a characteristic shared by many people",
	0x001021B0: "Additional Patient History: free text, removed because it might hold an identifier",
	0x001021F0: "Patient's Religious Preference: a characteristic shared by many people",
	0x00104000: "Patient Comments: free text, removed because it might hold an identifier",
	0x00120050: "Clinical Trial Time Point ID: a trial's time point, the same for every subject",
	0x00120051: "Clinical Trial Time Point Description: free text, removed because it might hold an identifier",
	0x00120071: "Clinical Trial Series ID: a trial's series label, the same for every subject",
	0x00120072: "Clinical Trial Series Description: free text, removed because it might hold an identifier",
	0x0016004F: "Lens Make: a product's name",
	0x00160050: "Lens Model: a product's name",
	0x00160078: "GPS Satellites: a setting of the camera's receiver, not a place",
	0x00160082: "GPS Map Datum: a setting of the camera's receiver, not a place",
	0x00180010: "Contrast/Bolus Agent: a product's name",
	0x00181030: "Protocol Name: a name given to an object of the study, not to a person or place",
	0x001811BB: "Acquisition Field of View Label: a name given to an object of the study, not to a person or place",
	0x00181400: "Acquisition Device Processing Description: free text, removed because it might hold an identifier",
	0x00184000: "Acquisition Comments: free text, removed because it might hold an identifier",
	0x00189185: "Respiratory Motion Compensation Technique Description: free text, removed because it might hold an identifier",
	0x00189373: "X-Ray Detector Label: a name given to an object of the study, not to a person or place",
	0x0018937B: "Multi-energy Acquisition Description: free text, removed because it might hold an identifier",
	0x0018937F: "Decomposition Description: free text, removed because it might hold an identifier",
	0x00189424: "Acquisition Protocol Description: free text, removed because it might hold an identifier",
	0x00189937: "Requested Series Description: free text, removed because it might hold an identifier",
	0x0018A003: "Contribution Description: free text, removed because it might hold an identifier",
	0x00200027: "Pyramid Label: a name given to an object of the study, not to a person or place",
	0x00203406: "Modified Image Description: free text, removed because it might hold an identifier",
	0x00204000: "Image Comments: free text, removed because it might hold an identifier",
	0x00209158: "Frame Comments: free text, removed because it might hold an identifier",
	0x00284000: "Image Presentation Comments: free text, removed because it might hold an identifier",
	0x00321030: "Reason for Study: free text, removed because it might hold an identifier",
	0x00321060: "Requested Procedure Description: free text, removed because it might hold an identifier",
	0x00321066: "Reason for Visit: free text, removed because it might hold an identifier",
	0x00321070: "Requested Contrast Agent: a product's name",
	0x00324000: "Study Comments: free text, removed because it might hold an identifier",
	0x00380040: "Discharge Diagnosis Description: free text, removed because it might hold an identifier",
	0x00380050: "Special Needs: clinical text about the patient",
	0x00380062: "Service Episode Description: free text, removed because it might hold an identifier",
	0x00380500: "Patient State: clinical text about the patient",
	0x00384000: "Visit Comments: free text, removed because it might hold an identifier",
	0x003A0020: "Multiplex Group Label: a name given to an object of the study, not to a person or place",
	0x003A0203: "Channel Label: a name given to an object of the study, not to a person or place",
	0x003A020C: "Channel Derivation Description: free text, removed because it might hold an identifier",
	0x003A0329: "Waveform Filter Description: free text, removed because it might hold an identifier",
	0x003A032B: "Filter Lookup Table Description: free text, removed because it might hold an identifier",
	0x00400007: "Scheduled Procedure Step Description: free text, removed because it might hold an identifier",
	0x00400012: "Pre-Medication: clinical text about the patient",
	0x00400254: "Performed Procedure Step Description: free text, removed because it might hold an identifier",
	0x00400280: "Comments on the Performed Procedure Step: free text, removed because it might hold an identifier",
	0x00400310: "Comments on Radiation Dose: free text, removed because it might hold an identifier",
	0x0040051A: "Container Description: free text, removed because it might hold an identifier",
	0x00400556: "Acquisition Context Description: free text, removed because it might hold an identifier",
	0x00400600: "Specimen Short Description: free text, removed because it might hold an identifier",
	0x00400602: "Specimen Detailed Description: free text, removed because it might hold an identifier",
	0x00401002: "Reason for the Requested Procedure: free text, removed because it might hold an identifier",
	0x00401004: "Patient Transport Arrangements: clinical text about the patient",
	0x00401400: "Requested Procedure Comments: free text, removed because it might hold an identifier",
	0x00402001: "Reason for the Imaging Service Request: free text, removed because it might hold an identifier",
	0x00402400: "Imaging Service Request Comments: free text, removed because it might hold an identifier",
	0x00403001: "Confidentiality Constraint on Patient Data Description: free text, removed because it might hold an identifier",
	0x00500020: "Device Description: free text, removed because it might hold an identifier",
	0x00500021: "Long Device Description: free text, removed because it might hold an identifier",
	0x006A0005: "Annotation Group Label: a name given to an object of the study, not to a person or place",
	0x006A0006: "Annotation Group Description: free text, removed because it might hold an identifier",
	0x00700006: "Unformatted Text Value: free text, removed because it might hold an identifier",
	0x00880904: "Topic Title: free text, removed because it might hold an identifier",
	0x00880906: "Topic Subject: free text, removed because it might hold an identifier",
	0x00880912: "Topic Keywords: free text, removed because it might hold an identifier",
	0x20300020: "Text String: free text, removed because it might hold an identifier",
	0x22000002: "Label Text: free text, removed because it might hold an identifier",
	0x30020121: "Position Acquisition Template Name: a name given to an object of the study, not to a person or place",
	0x30020123: "Position Acquisition Template Description: free text, removed because it might hold an identifier",
	0x30060002: "Structure Set Label: a name given to an object of the study, not to a person or place",
	0x30060004: "Structure Set Name: a name given to an object of the study, not to a person or place",
	0x30060006: "Structure Set Description: free text, removed because it might hold an identifier",
	0x30060026: "ROI Name: a name given to an object of the study, not to a person or place",
	0x30060028: "ROI Description: free text, removed because it might hold an identifier",
	0x30060038: "ROI Generation Description: free text, removed because it might hold an identifier",
	0x30060085: "ROI Observation Label: a name given to an object of the study, not to a person or place",
	0x30060088: "ROI Observation Description: free text, removed because it might hold an identifier",
	0x300A0002: "RT Plan Label: a name given to an object of the study, not to a person or place",
	0x300A0003: "RT Plan Name: a name given to an object of the study, not to a person or place",
	0x300A0004: "RT Plan Description: free text, removed because it might hold an identifier",
	0x300A000B: "Treatment Sites: anatomy treated, clinical text",
	0x300A000E: "Prescription Description: free text, removed because it might hold an identifier",
	0x300A0016: "Dose Reference Description: free text, removed because it might hold an identifier",
	0x300A0072: "Fraction Group Description: free text, removed because it might hold an identifier",
	0x300A00C3: "Beam Description: free text, removed because it might hold an identifier",
	0x300A00DD: "Bolus Description: free text, removed because it might hold an identifier",
	0x300A0196: "Fixation Device Description: free text, removed because it might hold an identifier",
	0x300A01A6: "Shielding Device Description: free text, removed because it might hold an identifier",
	0x300A01B2: "Setup Technique Description: free text, removed because it might hold an identifier",
	0x300A0216: "Source Manufacturer: a product's name",
	0x300A02EB: "Compensator Description: free text, removed because it might hold an identifier",
	0x300A0608: "Treatment Position Group Label: a name given to an object of the study, not to a person or place",
	0x300A0611: "RT Accessory Holder Slot ID: a slot's position on a treatment machine",
	0x300A0615: "RT Accessory Device Slot ID: a slot's position on a treatment machine",
	0x300A0619: "Radiation Dose Identification Label: a name given to an object of the study, not to a person or place",
	0x300A0623: "Radiation Dose In-Vivo Measurement Label: a name given to an object of the study, not to a person or place",
	0x300A062A: "RT Tolerance Set Label: a name given to an object of the study, not to a person or place",
	0x300A0676: "Equipment Frame of Reference Description: free text, removed because it might hold an identifier",
	0x300A067C: "Radiation Generation Mode Label: a name given to an object of the study, not to a person or place",
	0x300A067D: "Radiation Generation Mode Description: free text, removed because it might hold an identifier",
	0x300A0734: "Treatment Tolerance Violation Description: free text, removed because it might hold an identifier",
	0x300A0742: "Interlock Description: free text, removed because it might hold an identifier",
	0x300A0783: "Interlock Origin Description: free text, removed because it might hold an identifier",
	0x300A078E: "Patient Treatment Preparation Procedure Parameter Description: free text, removed because it might hold an identifier",
	0x300A0792: "Patient Treatment Preparation Method Description: free text, removed because it might hold an identifier",
	0x300A0794: "Patient Setup Photo Description: free text, removed because it might hold an identifier",
	0x300A079A: "Displacement Reference Label: a name given to an object of the study, not to a person or place",
	0x300C0113: "Reason for Omission Description: free text, removed because it might hold an identifier",
	0x3010000F: "Conceptual Volume Combination Description: free text, removed because it might hold an identifier",
	0x30100017: "Conceptual Volume Description: free text, removed because it might hold an identifier",
	0x3010002D: "Device Label: a name given to an object of the study, not to a person or place",
	0x30100033: "User Content Label: a name given to an object of the study, not to a person or place",
	0x30100034: "User Content Long Label: a name given to an object of the study, not to a person or place",
	0x30100035: "Entity Label: a name given to an object of the study, not to a person or place",
	0x30100036: "Entity Name: a name given to an object of the study, not to a person or place",
	0x30100037: "Entity Description: free text, removed because it might hold an identifier",
	0x30100038: "Entity Long Label: a name given to an object of the study, not to a person or place",
	0x30100054: "RT Prescription Label: a name given to an object of the study, not to a person or place",
	0x30100056: "RT Treatment Approach Label: a name given to an object of the study, not to a person or place",
	0x3010005A: "RT Physician Intent Narrative: free text, removed because it might hold an identifier",
	0x3010005C: "Reason for Superseding: free text, removed because it might hold an identifier",
	0x30100061: "Prior Treatment Dose Description: free text, removed because it might hold an identifier",
	0x30100077: "Treatment Site: anatomy treated, clinical text",
	0x3010007A: "Treatment Technique Notes: free text, removed because it might hold an identifier",
	0x3010007B: "Prescription Notes: free text, removed because it might hold an identifier",
	0x3010007F: "Fractionation Notes: free text, removed because it might hold an identifier",
	0x40000010: "Arbitrary: free text, removed because it might hold an identifier",
	0x40004000: "Text Comments: free text, removed because it might hold an identifier",
	0x4008010B: "Interpretation Text: free text, removed because it might hold an identifier",
	0x40080115: "Interpretation Diagnosis Description: free text, removed because it might hold an identifier",
	0x40080300: "Impressions: free text, removed because it might hold an identifier",
	0x40084000: "Results Comments: free text, removed because it might hold an identifier",
	0x60004000: "Overlay Comments: free text, removed because it might hold an identifier",
}

// TestEveryIdentifyingRowIsANeedle guards that the needles keep up with
// PS3.15 Table E.1-1: every row the basic profile removes or replaces
// whose value is text, a name, a date or a UID in the data dictionary is a
// needle by its VR (PN, DA, DT, UI), an attribute of the floor or of the
// identifying set, or listed with a reason as no identifier. A row a new
// edition adds fails here until it is classed.
func TestEveryIdentifyingRowIsANeedle(t *testing.T) {
	for e := range anonymize.ProfileEntries() {
		if c := e.Basic(); c == "" || c == "K" || c == "C" {
			continue
		}
		tg := uint32(e.Tag)
		vrs := strings.Split(e.Tag.GetVR(), " or ")
		if slices.ContainsFunc(vrs, func(v string) bool { return v == "PN" || v == "DA" || v == "DT" || v == "UI" }) {
			continue
		}
		if !slices.ContainsFunc(vrs, func(v string) bool { return identifierText[v] }) {
			continue
		}
		_, floor := floorTags[tg]
		_, ident := identifierTags[tg]
		why, listed := nonIdentifiers[tg]
		switch {
		case floor || ident:
			if listed {
				t.Errorf("%08X %s is a needle and listed as none", tg, e.Name)
			}
		case !listed:
			t.Errorf("%08X %s (%v) is neither a needle nor listed as none", tg, e.Name, vrs)
		case why == "":
			t.Errorf("%08X %s is listed with no reason", tg, e.Name)
		}
	}
	for tg := range nonIdentifiers {
		if _, ok := anonymize.LookupProfileEntry(tag.Tag(tg)); !ok {
			t.Errorf("%08X is listed as no identifier but is not in Table E.1-1", tg)
		}
	}
}
