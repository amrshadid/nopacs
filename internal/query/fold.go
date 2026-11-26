package query

import (
	"strings"
	"sync"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// folders holds case folders for reuse: a cases.Caser keeps state while it
// works and is not shared between goroutines.
var folders = sync.Pool{New: func() any {
	c := cases.Fold()
	return &c
}}

// Fold returns the form of a text that folded comparisons compare: letters
// case folded in full (Unicode's CaseFolding.txt, statuses C and F: "ß"
// folds to "ss", a final sigma to "σ"), accents removed (the text
// decomposed into its canonical form, NFD, its combining marks dropped,
// and composed again, NFC), and spaces at either end trimmed, so "Müller",
// "MULLER" and "muller " fold alike. Folding is done once more on what it
// returns with no change. A search table keeps the folded form of each text
// attribute beside its exact one, made by this function when a scan is
// written; a condition's value is folded by it when compared, in routing
// and in search alike, so the two never disagree on what folds to what.
// Bytes that are not UTF-8 fold to U+FFFD.
func Fold(s string) string {
	c, _ := folders.Get().(*cases.Caser)
	if c == nil {
		f := cases.Fold()
		c = &f
	}
	// Bytes that are not UTF-8 become U+FFFD first: case folding and
	// normalization each read them their own way, and a second fold of
	// what they made of them would differ from the first.
	folded := c.String(strings.ToValidUTF8(s, "�"))
	folders.Put(c)
	d := norm.NFD.String(folded)
	var b strings.Builder
	b.Grow(len(d))
	for _, r := range d {
		if unicode.Is(unicode.Mn, r) {
			continue
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(norm.NFC.String(b.String()))
}
