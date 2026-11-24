package engine

import (
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// floorSample is a data set holding a value of each attribute of the
// floor, a person's name in a nested item and a private element written
// as PN, each value distinct so a search for it finds only it.
func floorSample(t testing.TB) *dataset.Dataset {
	return build(t,
		el{tSOPClass, "UI", ctImageStorage},
		el{0x00080092, "ST", "Referrer Street 9"},
		el{0x00080094, "SH", "555-0100"},
		el{tPatientName, "PN", "Floor^Patient"},
		el{tPatientID, "LO", "FLOOR-ID-1"},
		el{tBirthDate, "DA", "19611213"},
		el{0x00100032, "TM", "081500"},
		el{0x00101000, "LO", "OTHER-ID-7"},
		el{0x00101001, "PN", "Other^Name"},
		el{0x00101002, "SQ", []*dataset.Dataset{build(t, el{tPatientID, "LO", "SEQ-ID-3"})}},
		el{0x00101005, "PN", "Birth^Name"},
		el{0x00101040, "LO", "Patient Road 12"},
		el{0x00101060, "PN", "Mother^Maiden"},
		el{0x00102154, "SH", "555-0199"},
		el{0x00102155, "LT", "tel:555-0142"},
		el{0x00400275, "SQ", []*dataset.Dataset{build(t,
			el{0x00401102, "ST", "Person Lane 3"},
			el{0x00401103, "LO", "555-0177"},
			el{0x00401104, "LT", "mailto:x@example.org"},
			el{0x00321032, "PN", "Requesting^Doctor"},
		)}},
		el{0x00290010, "LO", "VENDOR"},
		el{0x00291001, "PN", "Private^Person"},
		el{0x00101090, "LO", "MRL-4411"},
		el{0x00102150, "LO", "Freedonia"},
		el{0x00102152, "LO", "Upper Region"},
		el{0x00380400, "LO", "Ward Nine"},
		el{0x00401101, "SQ", []*dataset.Dataset{build(t, el{0x00080100, "SH", "PID-55821"}, el{0x00080102, "SH", "LOCAL"})}},
	)
}

// floorValues are the identifying values of floorSample.
var floorValues = []string{
	"Referrer Street 9", "555-0100", "Floor^Patient", "FLOOR-ID-1", "19611213", "081500", "OTHER-ID-7",
	"Other^Name", "SEQ-ID-3", "Birth^Name", "Patient Road 12", "Mother^Maiden", "555-0199", "tel:555-0142",
	"Person Lane 3", "555-0177", "mailto:x@example.org", "Requesting^Doctor", "Private^Person",
	"MRL-4411", "Freedonia", "Upper Region", "Ward Nine", "PID-55821",
}

// TestTheFloorHoldsWhateverTheDeciderSays guards the floor: a decider that
// answers keep for every element (as a wrong profile, or wrong code
// deciding from one, would) still leaves no value of the attributes that
// name or identify the person, at any depth, a private element of VR PN
// included; each of their values is a needle though no rule removed it;
// and the file stays readable.
func TestTheFloorHoldsWhateverTheDeciderSays(t *testing.T) {
	keepAll := fakeDecider{}
	en, err := newEngine(keepAll, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ds := floorSample(t)
	rec := apply(t, en, ds)
	hay := strings.Join(allText(readFile(t, writeFile(t, ds))), "\x00")
	needles := map[string]bool{}
	for _, n := range rec.Needles {
		needles[n.Value] = true
	}
	for _, v := range floorValues {
		if strings.Contains(hay, v) {
			t.Errorf("%q stayed in the file", v)
		}
		if !needles[v] {
			t.Errorf("%q is not a needle", v)
		}
	}
	// The CT Image IOD requires Patient's Name present (Type 2): it is
	// emptied, not removed.
	if v, ok := text(ds, tPatientName); !ok || v != "" {
		t.Errorf("Patient's Name: %q, present %v", v, ok)
	}
	floorEntries := 0
	for _, e := range rec.Entries {
		if strings.HasPrefix(e.Reason, "the engine never keeps") {
			floorEntries++
			if e.Rule != -1 || !e.Changed {
				t.Errorf("%s: rule %d, changed %v", e.Path, e.Rule, e.Changed)
			}
		}
	}
	if floorEntries < len(floorValues)-2 {
		t.Errorf("%d entries say the floor acted", floorEntries)
	}
}

// TestTheFloorLeavesWhatReplacesTheValue guards that the floor overrides
// only what would keep a value: a profile's hash of Patient ID, its
// template for Patient's Name and its shift of the birth date stand, and
// a clean of Patient ID, which leaves what it does not recognize (a short
// identifier is no term), does not.
func TestTheFloorLeavesWhatReplacesTheValue(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("id", kw("PatientID"), aHash),
		rule("name", kw("PatientName"), aTemplate("S-{#PatientID}")),
		rule("birth", kw("PatientBirthDate"), aShift),
		rule("other", kw("OtherPatientIDs"), aClean),
	)
	ds := build(t, el{tPatientID, "LO", "P1"}, el{tPatientName, "PN", "Doe^Jo"}, el{tBirthDate, "DA", "19611213"}, el{0x00101000, "LO", "P2"})
	apply(t, en, ds)
	code := naming.HashText(testKeys.Hash, []byte("P1"))
	if got := mustText(t, ds, tPatientID); got != code {
		t.Errorf("hash: %q", got)
	}
	if got := mustText(t, ds, tPatientName); got != "S-"+code {
		t.Errorf("template: %q", got)
	}
	if got := mustText(t, ds, tBirthDate); got == "19611213" || len(got) != 8 {
		t.Errorf("shift: %q", got)
	}
	if v, ok := text(ds, 0x00101000); ok && v == "P2" {
		t.Error("a clean of Other Patient IDs kept it")
	}
}

// privateBlockSample is a data set with a private block of creator ACME:
// a person's name, an identifier and a binary value, and a description
// that repeats the name and the identifier.
func privateBlockSample(t testing.TB) *dataset.Dataset {
	return build(t,
		el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4"},
		el{tPatientID, "LO", "PAT12345"},
		el{tStudyDesc, "LO", "scan for ROE MRN998877"},
		el{0x00090010, "LO", "ACME"},
		el{0x00091010, "PN", "ROE^RICHARD"},
		el{0x00091011, "LO", "MRN998877"},
		el{0x00091012, "OB", []byte{0x01, 0x00, 0xFF, 0x7F}},
	)
}

// TestAPrivateNameOfUnknownVRIsFloored guards the floor in a file that
// gives private elements no VR (Implicit VR Little Endian, PS3.5 6.2.2): a
// private value read as UN that has the form of a person's name is
// removed though the profile keeps its block, and is a needle of a name;
// the block's other values, which are not names, stay as the profile
// says. The same block written with explicit VRs is floored by its VR PN.
func TestAPrivateNameOfUnknownVRIsFloored(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("vendor", privSel("ACME", "0009", "xx"), aKeep))
	for _, ts := range []string{implicitVR, "1.2.840.10008.1.2.1"} {
		ds := readFile(t, writeFileAs(t, privateBlockSample(t), ts))
		if e, _ := ds.Get(tagOf(0x00091010)); ts == implicitVR && e.GetVR() != "" && e.GetVR() != "UN" {
			t.Fatalf("an implicit VR file gave the private name VR %s", e.GetVR())
		}
		rec := apply(t, en, ds)
		if v, ok := text(ds, 0x00091010); ok {
			t.Errorf("%s: the private name stayed: %q", ts, v)
		}
		if v, _ := text(ds, 0x00091011); v != "MRN998877" {
			t.Errorf("%s: the kept identifier became %q", ts, v)
		}
		if !hasNeedle(rec, NeedleName, "ROE^RICHARD") {
			t.Errorf("%s: the private name is not a needle of a name", ts)
		}
	}
}

// TestAPersonsNameWrittenWithAnotherVRIsFloored guards that the floor
// reads what an attribute is from the data dictionary, not from the VR the
// file wrote: Referring Physician's Name and Operators' Name, VR PN in
// PS3.6, written as LO, are not kept by a decider that keeps everything,
// and their values are needles of names, so their parts are searched for.
func TestAPersonsNameWrittenWithAnotherVRIsFloored(t *testing.T) {
	en, err := newEngine(fakeDecider{}, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ds := readFile(t, writeFile(t, build(t,
		el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4"},
		el{tReferring, "LO", "SMITH^ALICE"}, el{0x00081070, "LO", "JONES^BOB"},
	)))
	rec := apply(t, en, ds)
	for tg, v := range map[uint32]string{tReferring: "SMITH^ALICE", 0x00081070: "JONES^BOB"} {
		if got, _ := text(ds, tg); strings.Contains(got, v) {
			t.Errorf("%08X kept %q", tg, got)
		}
		if !hasNeedle(rec, NeedleName, v) {
			t.Errorf("%q is not a needle of a name", v)
		}
	}
}

// TestNameShapes guards what a private value of unknown VR must look like
// to be taken as a person's name, and how its bytes are read as text.
func TestNameShapes(t *testing.T) {
	for v, want := range map[string]bool{
		"ROE^RICHARD": true, "Müller^Jürgen": true, "O'Neil^Ann-Marie": true, "Yamada^Tarou=山田^太郎": true,
		"ROE^A\\DOE^B": true, "JOHN DOE": false, "1^2": false, "A^": false, "1.2.3^4": false, "CT^HEAD 5": false,
	} {
		if got := looksLikeName(v); got != want {
			t.Errorf("looksLikeName(%q) = %v", v, got)
		}
	}
	if v, ok := unknownText("M\xfcller^J"); !ok || v != "Müller^J" {
		t.Errorf("ISO 8859-1 bytes read as %q, %v", v, ok)
	}
	if _, ok := unknownText("\x01\x00\xff\x7f"); ok {
		t.Error("binary bytes read as text")
	}
}

// hasNeedle reports whether rec holds value as a needle of kind.
func hasNeedle(rec *Record, kind NeedleKind, value string) bool {
	for _, n := range rec.Needles {
		if n.Kind == kind && n.Value == value {
			return true
		}
	}
	return false
}
