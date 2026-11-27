package profile

import (
	"unicode"
	"unicode/utf8"
)

// Fold returns the rune every rune of r's case folding orbit maps to
// (unicode.SimpleFold), its least of the same kind (a letter for a letter,
// anything for anything else), after a full-width form of an ASCII
// character (U+FF01 to U+FF5E, written in East Asian text) is taken as
// that character: "K", "k", "ｋ" and the Kelvin sign are one rune, and
// "１２３" is "123". It maps a rune to a rune, so positions in a folded
// text are positions in the text. A letter stays a letter: the orbit of
// the Greek iota holds U+0345, a combining mark less than every iota,
// which taken as the iota's fold would make an iota a word's edge.
func Fold(r rune) rune {
	if r >= 0xFF01 && r <= 0xFF5E {
		r -= 0xFEE0
	}
	if r < utf8.RuneSelf {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}
	letter := unicode.IsLetter(r)
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < least && unicode.IsLetter(f) == letter {
			least = f
		}
	}
	return least
}

// Ignorable reports whether r is a default-ignorable code point a text
// may hold inside a word without showing it (UAX #44): the soft hyphen
// (U+00AD), the zero-width space, non-joiner and joiner (U+200B to
// U+200D), the word joiner (U+2060) and the zero-width no-break space
// (U+FEFF). Folding drops it, in clean and in the check after alike, so it
// cannot split a value or a term into two words no search takes for it.
func Ignorable(r rune) bool {
	return r == 0x00AD || (r >= 0x200B && r <= 0x200D) || r == 0x2060 || r == 0xFEFF
}

// FoldText returns s in NFC (a letter written with combining marks as its
// composed character) with every rune folded (Fold: case, and full-width
// ASCII) and every run of white space made one space, so "MRN  778812" and
// "mrn 778812" are one text, and "Müller" is one text however its "ü" was
// written; an ignorable code point (Ignorable) is dropped. Bytes that are
// not UTF-8 become U+FFFD. The check after
// de-identification folds the text it searches so, and a list's terms are
// folded so, so a term and the text it is searched in are compared alike.
func FoldText(s string) string {
	space := false
	dst := make([]byte, 0, len(s))
	if !asciiOnly(s) {
		// Text of ASCII is in NFC already (UAX #15).
		s = NFC(s)
	}
	for _, r := range s {
		dst = AppendFolded(dst, r, &space)
	}
	return string(dst)
}

// asciiOnly reports whether s is of ASCII bytes only.
func asciiOnly(s string) bool {
	for i := range len(s) {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// AppendFolded appends r folded as FoldText folds it, to a text already in
// NFC: one space for a run of white space, *space saying whether the rune
// appended before was white space; nothing for an ignorable code point
// (Ignorable).
func AppendFolded(dst []byte, r rune, space *bool) []byte {
	if Ignorable(r) {
		return dst
	}
	if unicode.IsSpace(r) {
		if !*space {
			dst = append(dst, ' ')
		}
		*space = true
		return dst
	}
	*space = false
	return utf8.AppendRune(dst, Fold(r))
}
