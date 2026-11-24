package engine

import (
	"context"
	"slices"
	"testing"
)

// The two ways Unicode writes the name Müller: the "ü" as one character
// (NFC), and as "u" followed by the combining diaeresis U+0308 (NFD).
const (
	mullerNFC = "Müller"
	mullerNFD = "Müller"
)

// TestDecomposedNamesAreCleaned guards clean in NFC: a name removed from
// Patient's Name is cut from a description that writes it with a combining
// mark, and one removed written with the mark is cut where the description
// writes it composed; what is left is in NFC.
func TestDecomposedNamesAreCleaned(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("names", vrSel("PN"), aRemove),
		rule("desc", kw("StudyDescription"), aClean),
	)
	for _, c := range []struct{ name, desc string }{
		{mullerNFC + "^Hans", "seen by Dr " + mullerNFD + " with Jörg"},
		{mullerNFD + "^Hans", "seen by Dr " + mullerNFC + " with Jörg"},
	} {
		ds := build(t,
			el{tSpecificCharacterSet, "CS", "ISO_IR 192"},
			el{tPatientName, "PN", c.name},
			el{tStudyDesc, "LO", c.desc},
		)
		rec := apply(t, en, ds)
		if got, want := mustText(t, ds, tStudyDesc), "seen by Dr  with Jörg"; got != want {
			t.Errorf("name %q: cleaned %q, want %q", c.name, got, want)
		}
		if !slices.ContainsFunc(rec.Needles, func(n Needle) bool { return n.Value == mullerNFC+"^Hans" }) {
			t.Errorf("name %q: no needle of its NFC form", c.name)
		}
	}
}

// TestCleanLeavesATextWithNothingToCut guards that clean changes nothing
// in a text with nothing to remove, written with combining marks or in
// ASCII: it is not rewritten in NFC, so a file with no name in its text is
// written as it came.
func TestCleanLeavesATextWithNothingToCut(t *testing.T) {
	terms, _ := cleanTerms([]Needle{{Kind: NeedleName, Value: mullerNFC + "^Hans"}})
	for _, in := range []string{"Jörg was here", "plain ASCII text"} {
		got, err := newCleaner(terms, maxCleanWork).clean(context.Background(), in)
		if err != nil || got != in {
			t.Errorf("clean %q = %q, %v", in, got, err)
		}
	}
}

// TestCleanJudgesACombiningMarkInNFC guards what the check after agrees
// with: a name followed by a combining mark that composes with its last
// letter ("Rene" then U+0301 is "René" in NFC) is another word and is
// left, while one followed by a mark that composes with nothing ("Marq"
// then U+0301 stays two characters) is cut.
func TestCleanJudgesACombiningMarkInNFC(t *testing.T) {
	for _, c := range []struct{ name, in, want string }{
		{"RENE^XAVIER", "seen by René today", "seen by René today"},
		{"MARQ^PAUL", "seen by Marq́ today", "seen by ́ today"},
	} {
		terms, _ := cleanTerms([]Needle{{Kind: NeedleName, Value: c.name}})
		got, err := newCleaner(terms, maxCleanWork).clean(context.Background(), c.in)
		if err != nil || got != c.want {
			t.Errorf("clean %q for %s = %q, %v; want %q", c.in, c.name, got, err, c.want)
		}
	}
}

// TestRedactCutsDecomposedText guards redact in NFC: a pattern written
// composed cuts the name a value writes with a combining mark, the text
// cut is the composed name, and what is left is in NFC.
func TestRedactCutsDecomposedText(t *testing.T) {
	got, cuts, count, cause := redactValue(compileSearch(t, mullerNFC), "", "LO", "Dr "+mullerNFD+" Jörg", func(int) bool { return true })
	if cause != "" || got != "Dr  Jörg" || !slices.Equal(cuts, []string{mullerNFC}) || count != 1 {
		t.Errorf("got %q, cuts %q (%d), cause %q", got, cuts, count, cause)
	}
}
