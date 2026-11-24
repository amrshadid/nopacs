package engine

import (
	"context"
	"testing"
)

// TestSubjectGivenByTheCaller guards Reception.Subject: a file without a
// Patient ID whose caller names its subject has its dates shifted by that
// subject's shift (ShiftDays over the subject given), exactly as a file
// whose Patient ID is that subject; the subject is written nowhere; and a
// Reception without one, as a reception passes, still refuses the file
// no_subject (TestDatesShiftPerSubject).
func TestSubjectGivenByTheCaller(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("dates", vrSel("DA"), aShift))
	noID := build(t, el{tStudyDate, "DA", "20200610"})
	rec, err := en.Apply(context.Background(), noID, Reception{Subject: "sub-01"})
	if err != nil {
		t.Fatalf("a file whose caller names its subject: %v", err)
	}
	want, err := shiftDA("20200610", ShiftDays(testKeys.Date, "sub-01", en.dec.DateShiftDays()))
	if err != nil {
		t.Fatal(err)
	}
	if got := mustText(t, noID, tStudyDate); got != want {
		t.Errorf("the date was shifted to %s, want %s", got, want)
	}
	withID := build(t, el{tPatientID, "LO", "sub-01"}, el{tStudyDate, "DA", "20200610"})
	apply(t, en, withID)
	if got := mustText(t, withID, tStudyDate); got != want {
		t.Errorf("the subject given and the same Patient ID shift differently: %s and %s", want, got)
	}
	if _, ok := text(noID, tPatientID); ok {
		t.Error("the subject given was written into the file as its Patient ID")
	}
	for _, s := range allText(noID) {
		if s == "sub-01" {
			t.Error("the subject given was written into the file")
		}
	}
	for _, n := range rec.Needles {
		if n.Value == "sub-01" {
			t.Error("the subject given became a needle")
		}
	}
}
