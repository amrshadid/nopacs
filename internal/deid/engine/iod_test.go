package engine

import (
	"context"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/validate"
	"github.com/amrshadid/go-dicom/valuerep"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestTypesComeFromTheLibrarysIODs guards IODTypes against attributes
// whose Types PS3.3 states: in the CT Image IOD, Patient's Name is Type 2
// (C.7.1.1), Study Instance UID and SOP Instance UID Type 1 (C.7.2.1,
// C.12.1), Patient Comments Type 3, Referenced Image Sequence Type 3 with
// its Referenced SOP Instance UID Type 1 (C.7.6.1, Table 10-11); in the
// Enhanced CT Image IOD, Pixel Spacing in the shared Pixel Measures macro
// is 1C (C.7.6.16.2.1); and nothing is known of a SOP class PS3.4 does not
// list.
func TestTypesComeFromTheLibrarysIODs(t *testing.T) {
	it := &IODTypes{}
	cases := []struct {
		sop   string
		seqs  []uint32
		tag   uint32
		want  validate.AttributeType
		known bool
	}{
		{ctImageStorage, nil, tPatientName, validate.Type2, true},
		{ctImageStorage, nil, tStudyUID, validate.Type1, true},
		{ctImageStorage, nil, tSOPInstance, validate.Type1, true},
		{ctImageStorage, nil, tPatientComments, validate.Type3, true},
		{ctImageStorage, nil, 0x00081140, validate.Type3, true},
		{ctImageStorage, []uint32{0x00081140}, tRefSOPInstance, validate.Type1, true},
		{"1.2.840.10008.5.1.4.1.1.2.1", []uint32{0x52009229, 0x00289110}, 0x00280030, validate.Type1C, true},
		{"1.2.3.4", nil, tPatientName, 0, false},
	}
	for _, c := range cases {
		for range 2 { // the second answer is the kept one
			got, ok := it.Type(c.sop, c.seqs, c.tag)
			if ok != c.known || (ok && got != c.want) {
				t.Errorf("%s %v %08X: %v %v, want %v %v", c.sop, c.seqs, c.tag, got, ok, c.want, c.known)
			}
		}
	}
}

// fakeDecider decides by tag, for the actions the language does not yet
// write.
type fakeDecider map[uint32]Action

func (f fakeDecider) Decide(loc profile.Location, _ profile.Facts) Decision {
	if a, ok := f[loc.Tag]; ok {
		return Decision{Rule: 0, Name: "fake", Action: a}
	}
	return Decision{Rule: -1, Action: Action{Kind: ActKeep}}
}
func (fakeDecider) Sets() []SetRule      { return nil }
func (fakeDecider) Templates() []string  { return nil }
func (fakeDecider) DateShiftDays() int32 { return 365 }

// TestRemoveUnlessRequiredAndDummy guards the two actions decided by the
// object's IOD and the VR: an optional element is removed, one required
// present is emptied, one required with a value takes the fallback (a
// dummy, or a remapped UID; a sequence kept and walked), an unknown SOP
// class keeps every such element with the fallback; dummy writes a value
// valid for the VR, and empties a sequence.
func TestRemoveUnlessRequiredAndDummy(t *testing.T) {
	dec := fakeDecider{
		tPatientName:     {Kind: ActRemoveUnlessRequired, Fallback: FallbackDummy},
		tStudyUID:        {Kind: ActRemoveUnlessRequired, Fallback: FallbackUID},
		tPatientComments: {Kind: ActRemoveUnlessRequired, Fallback: FallbackDummy},
		tStudyDate:       {Kind: ActDummy},
		tRefStudySeq:     {Kind: ActDummy},
		0x00081140:       {Kind: ActRemoveUnlessRequired, Fallback: FallbackUID},
	}
	en, err := newEngine(dec, testKeys, Options{})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(sop string) *dataset.Dataset {
		return build(t,
			el{tSOPClass, "UI", sop},
			el{tStudyDate, "DA", "20240101"},
			el{tRefStudySeq, "SQ", []*dataset.Dataset{build(t, el{tRefSOPInstance, "UI", "1.2.9"})}},
			el{0x00081140, "SQ", []*dataset.Dataset{build(t, el{tRefSOPInstance, "UI", "1.2.10"})}},
			el{tPatientName, "PN", "Doe^John"},
			el{tPatientComments, "LT", "a comment"},
			el{tStudyUID, "UI", "1.2.3"},
		)
	}
	ct := mk(ctImageStorage)
	if _, err := en.Apply(context.Background(), ct, Reception{}); err != nil {
		t.Fatal(err)
	}
	if v, ok := text(ct, tPatientName); !ok || v != "" {
		t.Errorf("Type 2 name: %q %v", v, ok)
	}
	if v := mustText(t, ct, tStudyUID); v != RemapUID(testKeys.UID, "1.2.3") {
		t.Errorf("Type 1 UID: %q", v)
	}
	if ct.Contains(tagOf(tPatientComments)) || ct.Contains(tagOf(0x00081140)) {
		t.Error("a Type 3 element is present")
	}
	if v := mustText(t, ct, tStudyDate); v != "19000101" {
		t.Errorf("dummy date %q", v)
	}
	if n, _ := ct.SequenceLength(tagOf(tRefStudySeq)); n != 0 {
		t.Errorf("a dummy sequence has %d items", n)
	}
	other := mk("1.2.3.4.5")
	if _, err := en.Apply(context.Background(), other, Reception{}); err != nil {
		t.Fatal(err)
	}
	if v := mustText(t, other, tPatientName); v != "DUMMY" {
		t.Errorf("unknown SOP class, name: %q", v)
	}
	if v := mustText(t, other, tPatientComments); v != "DUMMY" {
		t.Errorf("unknown SOP class, comments: %q", v)
	}
	if n, _ := other.SequenceLength(tagOf(0x00081140)); n != 1 {
		t.Errorf("unknown SOP class, a sequence kept for its UIDs has %d items", n)
	}
	readFile(t, writeFile(t, other))
}

// TestEveryDummyIsAValueOfItsVR guards the dummy table: every VR of PS3.5
// 6.2 but SQ has one, each text one passes the DICOM library's check of
// its VR, each binary one holds one value of its width (PS3.15 E.1.1: D
// is of non-zero length, so OB, OW and UN too), and the slice returned is
// the caller's.
func TestEveryDummyIsAValueOfItsVR(t *testing.T) {
	widths := map[string]int{"AT": 4, "FD": 8, "FL": 4, "SL": 4, "SS": 2, "SV": 8, "UL": 4, "US": 2, "UV": 8,
		"OB": 2, "OD": 8, "OF": 4, "OL": 4, "OV": 8, "OW": 2, "UN": 2}
	for _, vr := range []string{"AE", "AS", "AT", "CS", "DA", "DS", "DT", "FD", "FL", "IS", "LO", "LT", "OB", "OD", "OF", "OL", "OV", "OW", "PN", "SH", "SL", "SS", "ST", "SV", "TM", "UC", "UI", "UL", "UN", "UR", "US", "UT", "UV"} {
		d, ok := Dummy(vr)
		if !ok {
			t.Errorf("no dummy for %s", vr)
			continue
		}
		if w, binary := widths[vr]; binary {
			if len(d) != w {
				t.Errorf("%s dummy is %d bytes", vr, len(d))
			}
			continue
		}
		if len(d) == 0 {
			t.Errorf("%s dummy is empty", vr)
			continue
		}
		if err := valuerep.ValidateValue(vr, string(d)); err != nil {
			t.Errorf("%s dummy %q: %v", vr, d, err)
		}
		d[0] = 'x'
		if again, _ := Dummy(vr); again[0] == 'x' {
			t.Errorf("%s: the dummy table was changed through a returned slice", vr)
		}
	}
	if _, ok := Dummy("SQ"); ok {
		t.Error("SQ has a byte dummy")
	}
}

// TestDummyAndRemoveUnlessRequiredFromTheLanguage guards that the profile
// language's dummy and remove_unless_required reach the engine with their
// fallbacks: in a CT image, a Type 3 element is removed, a Type 2 one
// emptied, a Type 1 UID remapped, and a dummy date is the dummy.
func TestDummyAndRemoveUnlessRequiredFromTheLanguage(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("date", kw("StudyDate"), aDummy),
		rule("comments", kw("PatientComments"), aRemoveUnless(v1.RequiredFallback_REQUIRED_FALLBACK_DUMMY)),
		rule("referring", kw("ReferringPhysicianName"), aRemoveUnless(v1.RequiredFallback_REQUIRED_FALLBACK_EMPTY)),
		rule("study", kw("StudyInstanceUID"), aRemoveUnless(v1.RequiredFallback_REQUIRED_FALLBACK_UID)),
	)
	ds := build(t,
		el{tSOPClass, "UI", ctImageStorage},
		el{tStudyDate, "DA", "20240101"},
		el{tReferring, "PN", "Who^Doctor"},
		el{tPatientComments, "LT", "a comment"},
		el{tStudyUID, "UI", "1.2.3"},
	)
	apply(t, en, ds)
	if v := mustText(t, ds, tStudyDate); v != "19000101" {
		t.Errorf("dummy date %q", v)
	}
	if ds.Contains(tagOf(tPatientComments)) {
		t.Error("a Type 3 element is present")
	}
	if v, ok := text(ds, tReferring); !ok || v != "" {
		t.Errorf("a Type 2 element: %q %v", v, ok)
	}
	if v := mustText(t, ds, tStudyUID); v != RemapUID(testKeys.UID, "1.2.3") {
		t.Errorf("a Type 1 UID: %q", v)
	}
}
