package profile_test

import (
	"context"
	"testing"
	"unicode"

	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// TestFoldKeepsLetters guards the folding over every code point: a letter
// or a digit never folds to a rune that is neither, so a term's word edge
// is where its letters end (the Greek iota's orbit holds a combining mark
// less than every iota).
func TestFoldKeepsLetters(t *testing.T) {
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			if f := profile.Fold(r); !unicode.IsLetter(f) && !unicode.IsDigit(f) {
				t.Errorf("%U folds to %U, not a letter", r, f)
			}
		}
	}
	for _, r := range []rune{0x0399, 0x03B9, 0x1FBE} {
		if profile.Fold(r) != profile.Fold(0x0399) || !unicode.IsLetter(profile.Fold(r)) {
			t.Errorf("%U folds to %U", r, profile.Fold(r))
		}
	}
}

// find reports whether a list of term finds it standing in text.
func find(t *testing.T, term, text string) bool {
	t.Helper()
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{term}}})
	if problems != nil {
		t.Fatal(problems)
	}
	budget := int64(1 << 20)
	spans, err := l.Find(context.Background(), []rune(profile.NFC(text)), l.All(), &budget)
	if err != nil {
		t.Fatal(err)
	}
	return len(spans) > 0
}

// The invisible code points the tests write into text, by number so the
// source shows them.
var (
	softHyphen = string(rune(0x00AD))
	zwsp       = string(rune(0x200B))
	zwj        = string(rune(0x200D))
	wordJoiner = string(rune(0x2060))
	zwnbsp     = string(rune(0xFEFF))
)

// TestTermEdgesAndInvisibleCharacters guards how a term stands in text of
// other scripts and with characters a reader does not see: a Greek term
// is not taken out of a longer name ending in an iota, as "Ann" is not
// out of "Annual"; a soft hyphen or a zero-width space, joiner or word
// joiner inside a term does not split it, so it is found. And it records
// the spellings not taken for a term, a documented limit: full case
// folding (ß as ss), compatibility forms (the ligature ﬃ) and the Turkish
// dotted capital I; and an accented letter is another letter (José is not
// Jose).
func TestTermEdgesAndInvisibleCharacters(t *testing.T) {
	for _, c := range []struct {
		term, text string
		found      bool
	}{
		{"Αλεξ", "Dr Αλεξιος", false},
		{"Αλεξ", "Dr Αλεξ.", true},
		{"Zebulonquist", "by Zebu" + softHyphen + "lonquist", true},
		{"Zebulonquist", "by Zebu" + zwsp + "lonquist", true},
		{"Zebulonquist", "by Zebu" + zwj + "lon" + wordJoiner + "quist" + zwnbsp, true},
		{"Strauss", "seen by Strauß", false},
		{"Griffin", "seen by Griﬃn", false},
		{"Istanbul", "from İstanbul", false},
		{"Jose", "Dr José", false},
	} {
		if got := find(t, c.term, c.text); got != c.found {
			t.Errorf("%q in %q: found %v, want %v", c.term, c.text, got, c.found)
		}
	}
	if got := profile.FoldText("a" + softHyphen + "b" + zwsp + "c"); got != "ABC" {
		t.Errorf("folded %q", got)
	}
}
