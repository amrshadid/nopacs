package engine_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// ownEngine is an engine over a document of rules and no base, which keeps
// every element no rule names.
func ownEngine(t *testing.T, rules ...*v1.ProfileRule) *engine.Engine {
	t.Helper()
	exp, problems := profile.Expand(&v1.ProfileDocument{Rules: rules}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	k := func(c byte) []byte { return bytes.Repeat([]byte{c}, 32) }
	en, err := engine.New(exp, engine.Keys{UID: k('u'), Date: k('d'), Hash: k('h')}, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// TestWhatAValuePatternOrRedactRemovesIsANeedle guards the needles of a
// person's rule that chooses values by a pattern: the text redact cuts,
// the whole value a whole match removes, and the text a search finds in a
// value it removes are each searched for after, so a copy kept elsewhere
// in the file holds it at the copy; without a copy the file passes.
func TestWhatAValuePatternOrRedactRemovesIsANeedle(t *testing.T) {
	redact := &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: `MRN\d{6}`}}}
	remove := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	vp := func(p string, search bool, vr string) *v1.ProfileSelector {
		return &v1.ProfileSelector{Kind: &v1.ProfileSelector_ValuePattern{ValuePattern: &v1.ValuePatternSelector{Pattern: p, Search: search, Vr: vr}}}
	}
	description := &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: "StudyDescription"}}
	for _, c := range []struct {
		name   string
		rule   *v1.ProfileRule
		source elem
		copied string
	}{
		{"redact's cut", &v1.ProfileRule{Name: "mrn", Selector: description, Action: redact},
			elem{0x00081030, "LO", "Head CT MRN123456 follow-up"}, "MRN123456"},
		{"a whole match removed", &v1.ProfileRule{Name: "acc", Selector: vp(`ACC-\d{4}`, false, ""), Action: remove},
			elem{0x00321060, "LO", "ACC-4321"}, "ACC-4321"},
		{"a search's match removed", &v1.ProfileRule{Name: "mrn-lo", Selector: vp(`MRN\d{6}`, true, "LO"), Action: remove},
			elem{0x00321060, "LO", "seen as MRN654321 today"}, "MRN654321"},
	} {
		t.Run(c.name, func(t *testing.T) {
			en := ownEngine(t, c.rule)
			// The copy is in Image Comments (LT), which no rule names and
			// none of the value patterns above reads.
			held := dataSet(t, elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "2.25.4323"},
				c.source, elem{0x00204000, "LT", "reviewed " + c.copied})
			out, needles := deidentify(t, en, part10(t, held))
			if !bytes.Contains(out, []byte(c.copied)) {
				t.Fatal("the result does not keep the copy the test needs")
			}
			var res *check.Residual
			if err := checkAfter(out, needles); !errors.As(err, &res) || res.First.Path != "00204000" || res.First.Kind != check.KindIdentifier {
				t.Errorf("a copy of what the rule removed kept in Image Comments: %v, want an identifier found at 00204000", err)
			}
			passes := dataSet(t, elem{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, elem{0x00080018, "UI", "2.25.4324"}, c.source)
			out, needles = deidentify(t, en, part10(t, passes))
			if err := checkAfter(out, needles); err != nil {
				t.Errorf("without a copy: %v", err)
			}
			if bytes.Contains(out, []byte(c.copied)) {
				t.Error("the matched text stayed in the result")
			}
		})
	}
}

// fileOf is a CT data set holding the elements.
func fileOf(t *testing.T, es ...elem) []byte {
	t.Helper()
	base := []elem{{0x00080016, "UI", "1.2.840.10008.5.1.4.1.1.2"}, {0x00080018, "UI", "2.25.4325"}}
	return part10(t, dataSet(t, append(base, es...)...))
}

// TestPatternNeedlesAreWholeAndBounded guards how a text a pattern takes
// out is searched for after: each distinct match is a needle however often
// it repeats, so the one after 300 repeats is not dropped and its copy
// holds the file; a cut that begins or ends with a letter is taken only as
// a whole word, so "Smith" holds a file that keeps "Smith" and neither
// holds one nor is cleaned out of one that keeps "Smithson"; and a cut of
// an LT value holding a backslash is one needle, so its halves kept apart
// hold nothing.
func TestPatternNeedlesAreWholeAndBounded(t *testing.T) {
	vp := func(p, vr string) *v1.ProfileSelector {
		return &v1.ProfileSelector{Kind: &v1.ProfileSelector_ValuePattern{ValuePattern: &v1.ValuePatternSelector{Pattern: p, Search: true, Vr: vr}}}
	}
	kw := func(k string) *v1.ProfileSelector {
		return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}
	}
	redact := func(p string) *v1.ProfileAction {
		return &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: p}}}
	}
	remove := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	clean := &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{}}}
	repeated := strings.Repeat("MRN111111 ", 300) + "MRN999999"
	for _, c := range []struct {
		name  string
		rules []*v1.ProfileRule
		els   []elem
		held  string
		kept  string
	}{
		{"the match after many repeats", []*v1.ProfileRule{{Name: "mrn", Selector: vp(`MRN\d{6}`, "LT"), Action: remove}},
			[]elem{{0x00204000, "LT", repeated}, {0x00181020, "LO", "build MRN999999"}}, "00181020", ""},
		{"a whole word", []*v1.ProfileRule{{Name: "smith", Selector: kw("StudyDescription"), Action: redact(`Smith`)}},
			[]elem{{0x00081030, "LO", "seen by Smith"}, {0x00181020, "LO", "Smith 2"}}, "00181020", ""},
		{"not inside a longer word", []*v1.ProfileRule{
			{Name: "smith", Selector: kw("StudyDescription"), Action: redact(`Smith`)},
			{Name: "series", Selector: kw("SeriesDescription"), Action: clean}},
			[]elem{{0x00081030, "LO", "seen by Smith"}, {0x00181020, "LO", "Smithson 2"}, {0x0008103E, "LO", "Smithson scan"}}, "", "Smithson scan"},
		{"a backslash of LT is text", []*v1.ProfileRule{{Name: "pair", Selector: kw("ImageComments"), Action: redact(`ABCD\\EFGH`)}},
			[]elem{{0x00204000, "LT", `xABCD\EFGHy`}, {0x00181020, "LO", "ABCD tool"}}, "", "ABCD tool"},
	} {
		t.Run(c.name, func(t *testing.T) {
			out, needles := deidentify(t, ownEngine(t, c.rules...), fileOf(t, c.els...))
			err := checkAfter(out, needles)
			var res *check.Residual
			switch {
			case c.held != "" && (!errors.As(err, &res) || res.First.Path != c.held):
				t.Errorf("want the copy found at %s, got %v", c.held, err)
			case c.held == "" && err != nil:
				t.Errorf("want the file passed, got %v", err)
			}
			if c.kept != "" && !bytes.Contains(out, []byte(c.kept)) {
				t.Errorf("%q was not kept as it was", c.kept)
			}
		})
	}
}
