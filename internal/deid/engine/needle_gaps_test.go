package engine_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/check"
)

// implicitVR is Implicit VR Little Endian (PS3.5 A.1): no element carries
// its VR, so a reader gives one its data dictionary does not know VR UN.
const implicitVR = "1.2.840.10008.1.2"

// withSeq adds to ds a sequence of tag t holding the items.
func withSeq(t *testing.T, ds *dataset.Dataset, tg uint32, items ...*dataset.Dataset) *dataset.Dataset {
	t.Helper()
	seq := sequence.New()
	for _, it := range items {
		if err := seq.Append(it); err != nil {
			t.Fatal(err)
		}
	}
	if err := ds.AddSequence(tag.Tag(tg), seq); err != nil {
		t.Fatal(err)
	}
	return ds
}

// probeFile is a CT data set holding the elements, and a copy of a value
// in Software Versions (0018,1020), which the basic profile keeps.
func probeFile(t *testing.T, copied string, es ...elem) *dataset.Dataset {
	t.Helper()
	base := []elem{
		{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, {0x00080018, "UI", "2.25.4323"},
		{0x00100020, "LO", "PID12345"}, {0x00181020, "LO", copied},
	}
	return dataSet(t, append(base, es...)...)
}

// expectHeld de-identifies ds, written in the transfer syntax ts, under
// the basic profile, and requires the check after to find the copy kept
// in Software Versions.
func expectHeld(t *testing.T, name string, ds *dataset.Dataset, ts, copied string) {
	t.Helper()
	out, needles := deidentify(t, basicEngine(t), part10As(t, ds, ts))
	if !bytes.Contains(out, []byte(copied)) {
		t.Fatalf("%s: the result does not keep the copy the test needs", name)
	}
	var res *check.Residual
	if err := checkAfter(out, needles); !errors.As(err, &res) || res.First.Path != "00181020" {
		t.Errorf("%s: a removed value kept in Software Versions: %v, want it found at 00181020", name, err)
	}
}

// TestARemovedValueTheDictionaryDoesNotKnowIsANeedle guards the attributes
// PS3.15 Table E.1-1 lists that this build's data dictionary does not know
// (Name to Use, Effective Start DateTime), which an implicit VR file gives
// VR UN: a value the basic profile removes is searched for as text, at the
// top level and inside a removed sequence the dictionary does not know
// either (Person Names to Use Sequence), so its copy in a kept element
// holds the file.
func TestARemovedValueTheDictionaryDoesNotKnowIsANeedle(t *testing.T) {
	expectHeld(t, "name to use", probeFile(t, "Janie Quill", elem{0x00100012, "LO", "Janie Quill"}), implicitVR, "Janie Quill")
	nested := withSeq(t, probeFile(t, "Janie Quill"), 0x00100011, dataSet(t, elem{0x00100012, "LO", "Janie Quill"}))
	expectHeld(t, "name to use in its sequence", nested, implicitVR, "Janie Quill")
	expectHeld(t, "effective start", probeFile(t, "20240315101500", elem{0x0040A034, "DT", "20240315101500"}), implicitVR, "20240315101500")
}

// TestAddedIdentifiersAreNeedles guards the identifying rows that are no
// name, date or UID: a trial's protocol name, an issuer of a trial's time
// point, the system that modified attributes, a modifying device's ID
// (CS), a hanging protocol's selector value, a display address, and a GPS
// coordinate, the one numeric value that locates a place by itself. Each
// value the basic profile removes is searched for, so its copy holds the
// file.
func TestAddedIdentifiersAreNeedles(t *testing.T) {
	for _, c := range []struct {
		name string
		e    elem
	}{
		{"trial protocol name", elem{0x00120021, "LO", "HEARTSTUDY-77"}},
		{"issuer of a time point", elem{0x00120055, "LO", "RIVERSIDE TRIALS"}},
		{"modifying system", elem{0x04000563, "LO", "PACSYS-77"}},
		{"modifying device", elem{0x00203401, "CS", "DEV_4471"}},
		{"selector value", elem{0x00720066, "LO", "RIVERSIDE-CT"}},
		{"display address", elem{0x0040E012, "UR", "https://pacs.riverside.example/view"}},
		{"gps latitude", elem{0x00160072, "DS", "51.507351"}},
	} {
		expectHeld(t, c.name, probeFile(t, c.e.v, c.e), "1.2.840.10008.1.2.1", c.e.v)
	}
}

// TestSequencesCoveredWholeGiveEveryValue guards the sequences whose every
// value is a needle when removed: a Person Identification Code Sequence
// inside another removed sequence (Verifying Observer Sequence), at any
// depth; a person's identification code sequence (Content Creator's
// Identification Code Sequence), whose code value is a staff number; and
// an issuer's sequence (Issuer of Admission ID Sequence), whose Universal
// Entity ID names the issuer. Each copy kept elsewhere holds the file; the
// coding scheme beside the code is no needle.
func TestSequencesCoveredWholeGiveEveryValue(t *testing.T) {
	code := func(v string) *dataset.Dataset {
		return dataSet(t, elem{0x00080100, "SH", v}, elem{0x00080102, "SH", "99LOCAL"}, elem{0x00080104, "LO", "staff number"})
	}
	observer := withSeq(t, dataSet(t, elem{0x0040A075, "PN", "Roe^Sam"}), 0x00401101, code("EMP-55123"))
	nested := withSeq(t, probeFile(t, "EMP-55123"), 0x0040A073, observer)
	expectHeld(t, "a floor sequence nested", nested, "1.2.840.10008.1.2.1", "EMP-55123")
	creator := withSeq(t, probeFile(t, "STAFF-90817"), 0x00700086, code("STAFF-90817"))
	expectHeld(t, "content creator's code", creator, "1.2.840.10008.1.2.1", "STAFF-90817")
	issuer := withSeq(t, probeFile(t, "1.2.840.99999.4242"), 0x00380014,
		dataSet(t, elem{0x00400032, "UT", "1.2.840.99999.4242"}, elem{0x00400033, "CS", "ISO"}))
	expectHeld(t, "issuer of admission", issuer, "1.2.840.10008.1.2.1", "1.2.840.99999.4242")

	scheme := withSeq(t, probeFile(t, "99LOCAL"), 0x00700086, code("STAFF-90817"))
	out, needles := deidentify(t, basicEngine(t), part10(t, scheme))
	if valueIsNeedle(needles, "99LOCAL") {
		t.Error("a coding scheme is a needle")
	}
	if err := checkAfter(out, needles); err != nil {
		t.Errorf("a coding scheme kept elsewhere holds the file: %v", err)
	}
}
