package engine

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataset"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/repattern"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// Tags of the redact tests.
const (
	tagStudyDescription = 0x00081030 // LO
	tagStationName      = 0x00081010 // SH
	tagImageComments    = 0x00204000 // LT
	tagTextValue        = 0x0040A160 // UT
	tagRequestedProc    = 0x00321060 // LO, Requested Procedure Description
	tagReferringName    = 0x00080090 // PN
)

// aRedact is a redact action.
func aRedact(p, repl string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: p, Replacement: repl}}}
}

// valueSel is a value pattern selector.
func valueSel(p string, search bool, vr string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_ValuePattern{ValuePattern: &v1.ValuePatternSelector{Pattern: p, Search: search, Vr: vr}}}
}

// entryAt returns the record's entry for the element at path.
func entryAt(t *testing.T, rec *Record, path string) Entry {
	t.Helper()
	for _, e := range rec.Entries {
		if e.Path == path {
			return e
		}
	}
	t.Fatalf("no entry at %s", path)
	return Entry{}
}

// TestRedactCutsAnMRNFromFreeText guards redact on a description: only the
// match is cut, the rest kept as it was, the entry counts the cut, and the
// text cut is an identifier needle.
func TestRedactCutsAnMRNFromFreeText(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("mrn", kw("StudyDescription"), aRedact(`MRN\d{6}`, "")))
	ds := build(t, el{tagStudyDescription, "LO", "Head CT MRN123456 follow-up"})
	rec := apply(t, en, ds)
	if got, _ := text(ds, tagStudyDescription); got != "Head CT  follow-up" {
		t.Errorf("redacted to %q", got)
	}
	e := entryAt(t, rec, "00081030")
	if e.Action != "redact" || e.Redactions != 1 || !e.Changed {
		t.Errorf("entry %+v", e)
	}
	if !slices.ContainsFunc(rec.Needles, func(n Needle) bool { return n.Kind == NeedleText && n.Value == "MRN123456" }) {
		t.Errorf("the text cut is not a needle: %d needles", len(rec.Needles))
	}
	// A value with nothing to cut is left as it is, and makes no needle.
	ds = build(t, el{tagStudyDescription, "LO", "Head CT"})
	rec = apply(t, en, ds)
	if got, _ := text(ds, tagStudyDescription); got != "Head CT" || len(rec.Needles) != 0 || entryAt(t, rec, "00081030").Changed {
		t.Errorf("a value with nothing to cut became %q, %d needles", got, len(rec.Needles))
	}
}

// TestRedactValueKeepsTheStructure guards how a value is cut: each value
// of a multi-valued one apart, a person's name component by component so
// no "^" or "=" is ever cut, again until nothing matches, the longest
// match at each place (a prefix alternation cuts the whole MRN), and a
// backslash of an LT value as text. Each text cut is answered once; every
// match is counted.
func TestRedactValueKeepsTheStructure(t *testing.T) {
	for _, c := range []struct {
		name, pattern, repl, vr, value, want string
		cuts                                 []string
		count                                int
	}{
		{"free text", `MRN\d{6}`, "", "LO", "Head CT MRN123456 follow-up", "Head CT  follow-up", []string{"MRN123456"}, 1},
		{"with a replacement", `MRN\d{6}`, "[MRN]", "LO", "x MRN123456", "x [MRN]", []string{"MRN123456"}, 1},
		{"the longest alternative", `MRN|MRN\d+`, "", "LO", "x MRN123456", "x ", []string{"MRN123456"}, 1},
		{"each value apart", `\d+`, "", "LO", `A1\B22`, `A\B`, []string{"1", "22"}, 2},
		{"no match across values", `A\\B`, "", "LO", `A\B`, `A\B`, nil, 0},
		{"a backslash of LT is text", `A\\B`, "", "LT", `xA\By`, `xy`, []string{`A\B`}, 1},
		{"name components", `Dr\.?`, "", "PN", "Dr.Smith^DrJohn==Dr^", "Smith^John==^", []string{"Dr.", "Dr"}, 3},
		{"no match across a caret", `h\^J`, "", "PN", "Smith^John", "Smith^John", nil, 0},
		{"no match across groups", `n=Y`, "", "PN", "Smith^John=Yamada^Taro", "Smith^John=Yamada^Taro", nil, 0},
		{"cut again until clean", `ab`, "", "LO", "aabbc", "c", []string{"ab"}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, cuts, count, cause := redactValue(compileSearch(t, c.pattern), c.repl, c.vr, c.value, func(int) bool { return true }, nil)
			if cause != "" || got != c.want || !slices.Equal(cuts, c.cuts) || count != c.count {
				t.Errorf("got %q, cuts %q (%d), cause %q; want %q, cuts %q (%d)", got, cuts, count, cause, c.want, c.cuts, c.count)
			}
		})
	}
}

// compileSearch compiles a pattern as a profile's redact does.
func compileSearch(t *testing.T, p string) *regexp.Regexp {
	t.Helper()
	re, _, err := repattern.CompileSearch(p)
	if err != nil {
		t.Fatal(err)
	}
	return re
}

// TestRedactRefusesWhatCannotStayValid guards the refusals: a value left
// too long for its VR, or too long for its element's length field though
// each of its values is valid (redact_invalid); more distinct matches than
// the bound (too_many_matches); and a value past MaxPatternValue
// (too_large). The
// file is refused, never written with a part cut or the element dropped.
func TestRedactRefusesWhatCannotStayValid(t *testing.T) {
	for _, c := range []struct {
		name  string
		rule  *v1.ProfileRule
		el    el
		cause Cause
	}{
		{"too long for SH", rule("long", kw("StationName"), aRedact(`\d+`, "REDACTEDREDACTED")),
			el{tagStationName, "SH", "A1B2"}, CauseRedactInvalid},
		{"too many matches", rule("many", kw("ImageComments"), aRedact(`MRN\d{6}`, "")),
			el{tagImageComments, "LT", distinctMRNs(profile.MaxRedactMatches + 1)}, CauseTooManyMatches},
		{"past the bound", rule("big", kw("TextValue"), aRedact(`MRN\d{6}`, "")),
			el{tagTextValue, "UT", strings.Repeat("x", profile.MaxPatternValue) + " MRN123456"}, CauseTooLarge},
		{"past the length field", rule("wide", kw("RequestedProcedureDescription"), aRedact(`a`, "XXX")),
			el{tagRequestedProc, "LO", strings.TrimSuffix(strings.Repeat(`a\`, 30000), `\`)}, CauseRedactInvalid},
	} {
		t.Run(c.name, func(t *testing.T) {
			en := newTestEngine(t, testKeys, Options{}, c.rule)
			ds := build(t, c.el)
			_, err := en.Apply(context.Background(), ds, Reception{})
			var f *Failure
			if !errors.As(err, &f) || f.Cause != c.cause || f.Rule != c.rule.GetName() {
				t.Fatalf("got %v, want %s by %s", err, c.cause, c.rule.GetName())
			}
			if got, _ := text(ds, c.el.tag); got != strings.TrimRight(c.el.value.(string), " ") {
				t.Errorf("the refused file's value changed")
			}
		})
	}
}

// TestRedactOfANameIsLoweredByTheFloor guards that redact never keeps part
// of a person's name: like keep and clean, it leaves what it does not cut,
// so on a PN the engine removes the element unless it is required, and
// gives it a dummy.
func TestRedactOfANameIsLoweredByTheFloor(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("titles", vrSel("PN"), aRedact(`Dr\.?`, "")))
	ds := build(t, el{tagReferringName, "PN", "Dr.Smith^John"})
	rec := apply(t, en, ds)
	e := entryAt(t, rec, "00080090")
	if e.Action == "redact" || e.Rule != -1 || !e.Changed {
		t.Errorf("entry %+v", e)
	}
	if got, present := text(ds, tagReferringName); present && strings.Contains(got, "Smith") {
		t.Errorf("part of the name stayed: %q", got)
	}
}

// distinctMRNs returns n distinct MRNs separated by spaces.
func distinctMRNs(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "MRN%06d ", 100000+i)
	}
	return b.String()
}

// TestAValueAPatternCannotReadRefusesTheFile guards that a value a value
// pattern would read and cannot is never passed unread: one past
// MaxPatternValue (too_large) or holding an undecoded ISO 2022 escape
// (bad_value), where it stands and inside a sequence a rule removes; and
// that a pattern matching more distinct texts than the bound refuses the
// file (too_many_matches) rather than keep only some as needles. A value
// within the bound is selected as usual.
func TestAValueAPatternCannotReadRefusesTheFile(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("mrn", valueSel(`MRN\d{6}`, true, ""), aRemove),
		rule("drop-sequence", kw("ReferencedStudySequence"), aRemove))
	long := strings.Repeat("x", profile.MaxPatternValue+1) + " MRN123456"
	for _, c := range []struct {
		name  string
		els   []el
		cause Cause
	}{
		{"past the bound", []el{{tagTextValue, "UT", long}}, CauseTooLarge},
		{"an undecoded escape", []el{{tagRequestedProc, "LO", "MRN123456 \x1b$B"}}, CauseBadValue},
		{"past the bound in a removed sequence", []el{{0x00081110, "SQ", []*dataset.Dataset{build(t, el{tagTextValue, "UT", long})}}}, CauseTooLarge},
		{"more distinct matches than the bound", []el{{tagImageComments, "LT", distinctMRNs(profile.MaxRedactMatches + 1)}}, CauseTooManyMatches},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, err := en.Apply(context.Background(), build(t, c.els...), Reception{})
			var f *Failure
			if !errors.As(err, &f) || f.Cause != c.cause {
				t.Fatalf("got %v, want %s", err, c.cause)
			}
		})
	}
	ds := build(t, el{tagTextValue, "UT", strings.Repeat("x", 1024) + " MRN123456"})
	rec := apply(t, en, ds)
	if _, present := text(ds, tagTextValue); present || entryAt(t, rec, "0040A160").PatternMatches != 1 {
		t.Errorf("a value within the bound was not removed")
	}
}

// TestAValuePatternMatchesEachValueTrimmed guards that each value of a
// multi-valued element is matched with its padding removed as its VR
// allows, so a whole-value pattern finds "123456" in "HEAD\ 123456".
func TestAValuePatternMatchesEachValueTrimmed(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, rule("ids", valueSel(`\d{6}`, false, "LO"), aRemove))
	ds := build(t, el{tagRequestedProc, "LO", `HEAD\ 123456`})
	rec := apply(t, en, ds)
	if _, present := text(ds, tagRequestedProc); present {
		t.Error("the padded second value was not matched")
	}
	if !slices.ContainsFunc(rec.Needles, func(n Needle) bool { return n.Value == "123456" && n.Whole }) {
		t.Errorf("the trimmed value is not a needle: %d needles", len(rec.Needles))
	}
}

// TestRedactIsFastOnAHostileValue guards that redact with a pattern
// within the bounds stays fast on a 64 KiB value, matched or not.
func TestRedactIsFastOnAHostileValue(t *testing.T) {
	for _, c := range []struct{ pattern, value string }{
		{`(a|aa)+(a+)+b`, strings.Repeat("a", profile.MaxPatternValue)},
		{`(x+x+)+y`, strings.Repeat("x", profile.MaxPatternValue-1) + "y"},
	} {
		en := newTestEngine(t, testKeys, Options{}, rule("hostile", kw("TextValue"), aRedact(c.pattern, "")))
		ds := build(t, el{tagTextValue, "UT", c.value})
		start := time.Now()
		_, _ = en.Apply(context.Background(), ds, Reception{})
		if took := time.Since(start); took > time.Second {
			t.Errorf("%s over %d bytes took %v", c.pattern, len(c.value), took)
		}
	}
}

// TestValuePatternPrecedenceInTheEngine guards the precedence as the
// engine applies it, and as Explain answers it: a tag rule keeps a
// description a value pattern would remove; a value pattern removes what
// a VR rule would keep, the VR rule answered as less specific.
func TestValuePatternPrecedenceInTheEngine(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("keep-lo", vrSel("LO"), aKeep),
		rule("mrn", valueSel(`MRN\d{6}`, true, ""), aRemove),
		rule("keep-description", kw("StudyDescription"), aKeep),
	)
	ds := build(t,
		el{tagStudyDescription, "LO", "Head CT MRN123456"},
		el{tagRequestedProc, "LO", "follow-up MRN654321"},
	)
	ex, err := en.Explain(context.Background(), ds, Reception{})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range ex {
		switch x.Entry.Path {
		case "00081030":
			if x.Entry.Action != "keep" || x.SelectedBy != "tag" || len(x.Others) != 2 || x.Others[0].SelectedBy != "value_pattern" || x.Others[0].WhyNot != WhyLessSpecific {
				t.Errorf("the description: %+v", x)
			}
		case "00321060":
			if x.Entry.Action != "remove" || x.SelectedBy != "value_pattern" || len(x.Others) != 1 || x.Others[0].SelectedBy != "vr" || x.Others[0].WhyNot != WhyLessSpecific {
				t.Errorf("the requested procedure: %+v", x)
			}
		}
	}
	if got, _ := text(ds, tagStudyDescription); got != "Head CT MRN123456" {
		t.Errorf("the tag rule's keep was overridden: %q", got)
	}
	if _, present := text(ds, tagRequestedProc); present {
		t.Error("the value pattern's remove did not apply over the VR rule")
	}
}

// TestPatternWorkIsBoundedPerFile guards the bound on what a file's value
// patterns and redacts may run: RE2's time is the value's length times
// the pattern's compiled size, so a costly pattern over a few values at
// the bound would take seconds each. Past maxPatternWork the file is
// refused as too_large, before the work is done, so the refusal is fast.
func TestPatternWorkIsBoundedPerFile(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector slows matching some twenty times; the bound is measured without it")
	}
	const costly = `(a*)*(a|b){1,100}c` // about 500 instructions and ranges
	value := strings.Repeat("a", profile.MaxPatternValue)
	three := []el{{tagImageComments, "LT", value}, {0x00104000, "LT", value}, {0x001021B0, "LT", value}}
	// Three values are within the bound for Apply, and past it when
	// explained, since the explanation runs the patterns again.
	if _, err := newTestEngine(t, testKeys, Options{}, rule("costly", valueSel(costly, true, ""), aRemove)).Apply(context.Background(), build(t, three...), Reception{}); err != nil {
		t.Fatalf("three values within the bound: %v", err)
	}
	_, err := newTestEngine(t, testKeys, Options{}, rule("costly", valueSel(costly, true, ""), aRemove)).Explain(context.Background(), build(t, three...), Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CauseTooLarge {
		t.Errorf("explaining three values: %v, want too_large", err)
	}
	for _, c := range []struct {
		name string
		rule *v1.ProfileRule
		els  []el
	}{
		{"value patterns", rule("costly", valueSel(costly, true, ""), aRemove),
			[]el{{tagImageComments, "LT", value}, {tagTextValue, "UT", value}, {0x00104000, "LT", value}, {0x001021B0, "LT", value}, {0x00324000, "LT", value}}},
		{"redact", rule("costly", vrSel("LT"), aRedact(costly, "")),
			[]el{{tagImageComments, "LT", value}, {0x00104000, "LT", value}, {0x001021B0, "LT", value}, {0x00324000, "LT", value}, {0x00184000, "LT", value}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			en := newTestEngine(t, testKeys, Options{}, c.rule)
			start := time.Now()
			_, err := en.Apply(context.Background(), build(t, c.els...), Reception{})
			var f *Failure
			if !errors.As(err, &f) || f.Cause != CauseTooLarge {
				t.Fatalf("got %v, want too_large", err)
			}
			if took := time.Since(start); took > 3*time.Second {
				t.Errorf("the refusal took %v", took)
			}
		})
	}
}
