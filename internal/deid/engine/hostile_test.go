package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
)

// nested returns a data set of the elements top whose Content Sequence
// nests depth levels deep, a Text Value at the bottom.
func nested(t testing.TB, depth int, top ...el) *dataset.Dataset {
	ds := build(t, el{0x0040A160, "UT", "bottom"})
	for i := range depth {
		var outer []el
		if i == depth-1 {
			outer = top
		}
		ds = build(t, append(outer, el{tContentSequence, "SQ", []*dataset.Dataset{ds}})...)
	}
	return ds
}

// tContentSequence is the Content Sequence (0040,A730).
const tContentSequence = 0x0040A730

// TestHostileInputIsBounded guards the bounds: sequences nested past
// MaxDepth fail as too_deep and at MaxDepth do not; a value past
// MaxValueLength an action must read fails as bad_value while keeping it
// reads nothing; a long text under clean fails; and a failed file is left
// exactly as it was.
func TestHostileInputIsBounded(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("id", kw("PatientID"), aHash), rule("text", kw("TextValue"), aReplace("x")))
	apply(t, en, nested(t, MaxDepth, el{tPatientID, "LO", "PAT001"}))

	deep := nested(t, MaxDepth+1, el{tPatientID, "LO", "PAT001"})
	var f *Failure
	if _, err := en.Apply(context.Background(), deep, Reception{}); !errors.As(err, &f) || f.Cause != CauseTooDeep {
		t.Fatalf("%d levels: %v", MaxDepth+1, err)
	}
	if v := mustText(t, deep, tPatientID); v != "PAT001" {
		t.Errorf("a file too deep was changed: %q", v)
	}

	huge := strings.Repeat("A", MaxValueLength+2)
	big := build(t, el{tPatientID, "LO", huge})
	if _, err := en.Apply(context.Background(), big, Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue {
		t.Errorf("a value of %d bytes hashed: %v", len(huge), err)
	}
	keep := newTestEngine(t, testKeys, Options{})
	apply(t, keep, build(t, el{tPatientComments, "LT", huge}))

	cl := newTestEngine(t, testKeys, Options{}, rule("c", kw("PatientComments"), aClean))
	if _, err := cl.Apply(context.Background(), build(t, el{tPatientComments, "UT", strings.Repeat("b", maxCleanValue+1)}), Reception{}); !errors.As(err, &f) || f.Cause != CauseBadValue {
		t.Errorf("a long text cleaned: %v", err)
	}
}

// TestCanceledContextChangesNothing guards that a canceled context is
// an error that is not a file's failure, and leaves the data set as it
// was.
func TestCanceledContextChangesNothing(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("id", kw("PatientID"), aHash))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ds := build(t, el{tPatientID, "LO", "PAT001"})
	_, err := en.Apply(ctx, ds, Reception{})
	var f *Failure
	if err == nil || errors.As(err, &f) {
		t.Fatalf("canceled: %v", err)
	}
	if v := mustText(t, ds, tPatientID); v != "PAT001" {
		t.Errorf("changed: %q", v)
	}
}

// FuzzDates guards the date actions against any text: none panics, and a
// value shifted or generalized without error is a valid DA or DT again.
func FuzzDates(f *testing.F) {
	for _, s := range []string{"20240301", "2024", "20240301101500.123456+0100", `20240101\20240102`, "00010101", "9999", ""} {
		f.Add(s, 30)
	}
	f.Fuzz(func(t *testing.T, s string, days int) {
		days = 1 + abs(days)%3650
		if out, err := shiftDA(s, days); err == nil && !validText("DA", out) {
			t.Errorf("DA %q became %q", s, out)
		}
		if out, err := shiftDT(s, days); err == nil && !validText("DT", out) {
			t.Errorf("DT %q became %q", s, out)
		}
		_, _ = generalizeDate(s)
		_, _ = generalizeAge(s)
	})
}

// abs is the absolute value of an int, the least one taken as its
// successor.
func abs(n int) int {
	if n < 0 {
		return -(n + 1)
	}
	return n
}
