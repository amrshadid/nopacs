package engine

import (
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
)

// TestARemovedPrivateValueOfUnknownKindIsNoNeedle guards the needles of
// a file that gives private elements no VR (Implicit VR Little Endian): a
// private value a rule removes is a needle only when it has a name's form
// (a name, which the floor covers), since a private element's meaning is
// not known and its values are mostly a vendor's settings. So the
// private name is a needle and clean takes its copy out of a
// description; the private identifier, of no known kind, is no needle and
// its copy stays; a binary value removed is no needle either.
func TestARemovedPrivateValueOfUnknownKindIsNoNeedle(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("vendor", privSel("ACME", "0009", "xx"), aRemove),
		rule("desc", kw("StudyDescription"), aClean))
	ds := readFile(t, writeFileAs(t, privateBlockSample(t), implicitVR))
	rec := apply(t, en, ds)
	if !hasNeedle(rec, NeedleName, "ROE^RICHARD") {
		t.Error("the removed private name is not a needle of a name")
	}
	if got := mustText(t, ds, tStudyDesc); strings.Contains(got, "ROE") || !strings.Contains(got, "MRN998877") {
		t.Errorf("clean left %q, want the name cut and the private identifier kept", got)
	}
	for _, n := range rec.Needles {
		if strings.HasSuffix(n.Path, "00091012") || strings.HasSuffix(n.Path, "00091011") {
			t.Errorf("a private value of unknown kind is a needle: %q", n.Value)
		}
	}
}

// TestStrippedSequencesGiveOnlyRemovedValuesAsNeedles guards the needles
// of the sequences removed whole: of the Original Attributes Sequence,
// which the pipeline removes from every file, the values the profile or
// the floor removes where they belong (a former name and Patient ID) are
// needles, and those it keeps (a former description, the system and the
// reason of the change) are not; of a sequence a rule removes, a code
// meaning the profile keeps is not. So clean leaves the descriptions the
// file keeps as they are, and takes out the former name and identifier.
func TestStrippedSequencesGiveOnlyRemovedValuesAsNeedles(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("study", kw("StudyDescription"), aClean),
		rule("series", kw("SeriesDescription"), aClean),
		rule("procedure", kw("ProcedureCodeSequence"), aRemove))
	modified := build(t, el{tStudyDesc, "LO", "CT HEAD"}, el{tPatientName, "PN", "OLDNAME^FORMER"}, el{tPatientID, "LO", "OLD-ID-9"})
	original := build(t,
		el{0x04000550, "SQ", []*dataset.Dataset{modified}},
		el{0x04000565, "CS", "CORRECT"},
		el{0x04000563, "LO", "PACSYS"},
		el{0x04000564, "LO", "RADIOLOGY"},
	)
	ds := build(t,
		el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4"}, el{tPatientID, "LO", "PAT12345"},
		el{tStudyDesc, "LO", "CT HEAD W"},
		el{0x0008103E, "LO", "CORRECT RECON by PACSYS in RADIOLOGY for OLDNAME OLD-ID-9"},
		el{0x00081032, "SQ", []*dataset.Dataset{build(t, el{0x00080100, "SH", "P1"}, el{0x00080102, "SH", "LOCAL"}, el{0x00080104, "LO", "CT HEAD"})}},
		el{0x04000561, "SQ", []*dataset.Dataset{original}},
	)
	rec := apply(t, en, ds)
	if got := mustText(t, ds, tStudyDesc); got != "CT HEAD W" {
		t.Errorf("Study Description: %q", got)
	}
	if got := mustText(t, ds, 0x0008103E); got != "CORRECT RECON by PACSYS in RADIOLOGY for" {
		t.Errorf("Series Description: %q", got)
	}
	for _, v := range []string{"CT HEAD", "CORRECT", "PACSYS", "RADIOLOGY"} {
		for _, n := range rec.Needles {
			if n.Value == v {
				t.Errorf("%q, kept by the profile, is a needle (%s)", v, n.Path)
			}
		}
	}
	if !hasNeedle(rec, NeedleName, "OLDNAME^FORMER") || !hasNeedle(rec, NeedleText, "OLD-ID-9") {
		t.Error("the former name and identifier are not needles")
	}
}

// TestPartialDatesAreMatchedAsDatesOfTheirPrecision guards the dates of
// only a year or a year and month: a shift that leaves one as it was
// empties it rather than keep the value as received; its needle is
// matched by clean only as a date of that precision, never as the year of
// a full date; and a generalized one keeps only its year.
func TestPartialDatesAreMatchedAsDatesOfTheirPrecision(t *testing.T) {
	short, long := "", ""
	for i := 0; short == "" || long == ""; i++ {
		id := fmt.Sprint("SUBJ", i)
		switch d := ShiftDays(testKeys.Date, id, 365); {
		case d < 100 && short == "":
			short = id
		case d > 200 && long == "":
			long = id
		}
	}
	en := newTestEngine(t, testKeys, Options{},
		rule("datetimes", vrSel("DT"), aShift),
		rule("desc", kw("StudyDescription"), aClean))
	for id, want := range map[string]string{short: "", long: "2023"} {
		ds := build(t, el{tPatientID, "LO", id}, el{tAcqDateTime, "DT", "2024"},
			el{tStudyDesc, "LO", "baseline 2024; seen 2024-03-15, 20240315 and 15.03.2024; 12024 2024a"})
		rec := apply(t, en, ds)
		if got, _ := text(ds, tAcqDateTime); got != want {
			t.Errorf("a year shifted by %d days: %q, want %q", ShiftDays(testKeys.Date, id, 365), got, want)
		}
		if !hasNeedle(rec, NeedleDate, "2024") {
			t.Error("the year is not a needle")
		}
		if got := mustText(t, ds, tStudyDesc); got != "baseline ; seen 2024-03-15, 20240315 and 15.03.2024; 12024 2024a" {
			t.Errorf("clean of a year: %q", got)
		}
	}
	terms, _ := cleanTerms([]Needle{{Kind: NeedleDate, Value: "202403"}})
	cl := newCleaner(terms, maxCleanWork)
	got, err := cl.clean(t.Context(), "in 2024-03, 03/2024 and 202403; on 2024-03-15 and 2024.03.15")
	if err != nil || got != "in ,  and ; on 2024-03-15 and 2024.03.15" {
		t.Errorf("clean of a year and month: %q, %v", got, err)
	}
	for in, want := range map[string]string{"202403": "2024", "2024": "2024", "20240315101500": "20240101", `2024\202405`: `2024\2024`} {
		if got, err := generalizeDate(in); err != nil || got != want {
			t.Errorf("generalize %q = %q, %v; want %q", in, got, err, want)
		}
	}
}

// TestAGeneralizedDateAlreadyGeneralIsNoNeedle guards a birth date that is
// already the first of January: generalizing it writes the same value, so
// it must not become a needle, or the check after de-identification would
// find the "removed" value in the output and hold every such file.
func TestAGeneralizedDateAlreadyGeneralIsNoNeedle(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("birth", kw("PatientBirthDate"), aGeneralize))
	ds := build(t, el{tPatientID, "LO", "P1"}, el{tBirthDate, "DA", "19500101"})
	rec := apply(t, en, ds)
	if got := mustText(t, ds, tBirthDate); got != "19500101" {
		t.Errorf("birth date = %q, want 19500101 kept", got)
	}
	for _, n := range rec.Needles {
		if n.Value == "19500101" {
			t.Errorf("the unchanged birth date is a needle at %s", n.Path)
		}
	}
}
