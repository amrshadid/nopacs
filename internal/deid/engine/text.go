package engine

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// hashValue is the keyed hash of a value as the naming rule's {#...}
// placeholders make it (naming.HashText), so a profile's hash of Patient ID
// equals the subject's label code. A CS value holds only upper-case letters
// (PS3.5 6.2), so there the code is in upper case.
func hashValue(key []byte, vr, value string) string {
	h := naming.HashText(key, []byte(value))
	if vr == "CS" {
		return strings.ToUpper(h)
	}
	return h
}

// renderTemplate writes a template of the expanded form: literal text, "{{"
// and "}}" for a brace, and "{#GGGGEEEE}" for the keyed hash of a
// top-level element's value as received, which value gives. A placeholder
// whose element is absent or empty fails: the empty value would hash to one
// code for every file without it.
func renderTemplate(tmpl string, key []byte, value func(uint32) (string, bool)) (string, bool) {
	var out strings.Builder
	for i := 0; i < len(tmpl); i++ {
		switch {
		case strings.HasPrefix(tmpl[i:], "{{"):
			out.WriteByte('{')
			i++
		case strings.HasPrefix(tmpl[i:], "}}"):
			out.WriteByte('}')
			i++
		case strings.HasPrefix(tmpl[i:], "{#"):
			end := strings.IndexByte(tmpl[i:], '}')
			if end != 10 {
				return "", false
			}
			t, err := strconv.ParseUint(tmpl[i+2:i+10], 16, 32)
			if err != nil {
				return "", false
			}
			v, ok := value(uint32(t))
			if !ok || v == "" {
				return "", false
			}
			out.WriteString(naming.HashText(key, []byte(v)))
			i += end
		default:
			out.WriteByte(tmpl[i])
		}
	}
	return out.String(), true
}

// placeholders returns the tags of a template's placeholders.
func placeholders(tmpl string) []uint32 {
	var out []uint32
	for i := 0; i+11 <= len(tmpl); i++ {
		if strings.HasPrefix(tmpl[i:], "{{") {
			i++
			continue
		}
		if strings.HasPrefix(tmpl[i:], "{#") && tmpl[i+10] == '}' {
			if t, err := strconv.ParseUint(tmpl[i+2:i+10], 16, 32); err == nil {
				out = append(out, uint32(t))
			}
		}
	}
	return out
}

// minCleanLength is the shortest value clean removes from free text
// anywhere it appears, in a script that separates words with spaces: a
// shorter one matches inside ordinary words. A name's part of one or two
// letters (an initial, "Al", "Li") is removed too, but only as a whole word.
// In a script written without spaces between words (Han, Hiragana,
// Katakana, and Hangul, whose names are of one to three syllables), every
// value and name part is removed wherever it appears, whatever its length:
// a family name of two characters is a name.
const minCleanLength = 3

// Bounds of clean. maxCleanValue bounds a value clean reads, maxCleanTerms
// the terms of one file, and maxCleanWork the work of all the file's
// cleans together, counted as the characters searched times the characters
// of the terms searched for, each pass: past any of them the file is
// refused (CauseBadValue for a value, CauseTooLarge otherwise), so no file
// can make clean run for minutes, and no term is ever dropped to fit.
const (
	maxCleanValue = 64 << 10
	maxCleanTerms = 4096
	maxCleanWork  = 1 << 27
)

// errCleanTooLarge is a file whose cleans would pass maxCleanWork.
var errCleanTooLarge = errors.New("engine: the cleans of the file pass their bound")

// cleanTerm is one value clean removes, its runes folded (Fold).
type cleanTerm struct {
	runes []rune
	// word is set for a term matched only as a whole word: not preceded
	// or followed by a letter or a digit.
	word bool
	// partial is set for a date of only a year, or a year and a month,
	// matched only as a date of that precision: a whole word, and not
	// part of a longer date (2024 in 2024-03-15, or 2024-03 in
	// 2024-03-15), so the year of a date is not taken for it.
	partial bool
	// digits is set for a run of digits matched only with no digit
	// beside it.
	digits bool
	// ident is set for an identifier of words (EdgeIdentifier), matched on each such side only at a word's edge,
	// "_" part of a word; a side ending in a digit is matched anywhere, so
	// clean removes at least what the check after looks for.
	ident bool
}

// cleanTerms returns the terms clean removes for a file's needles: each
// value, for a person's name its forms (NameForms), and for a date the
// forms people write it in (DateForms), by the lengths minCleanLength
// gives; longest first, then by text, so the result is the same every run.
// ok is false when there are more than maxCleanTerms.
func cleanTerms(needles []Needle) ([]cleanTerm, bool) {
	seen := map[string]bool{}
	var out []cleanTerm
	add := func(v string, part bool, edge Edge) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		runes := foldString(v)
		key := string(runes)
		if len(runes) == 0 || seen[key] {
			return
		}
		t := cleanTerm{runes: runes, word: edge == EdgeWord, partial: edge == EdgeDate, digits: edge == EdgeDigits,
			ident: edge == EdgeIdentifier}
		switch {
		case t.partial, t.word, t.digits:
		case Unspaced(v), len(runes) >= minCleanLength:
		case part:
			t.word = true
		default:
			return
		}
		seen[key] = true
		out = append(out, t)
	}
	for _, n := range needles {
		switch n.Kind {
		case NeedleList:
			// The lists' terms are cut by their own automaton
			// (cleaner.cleanWith), where a rule names them.
		case NeedleDate:
			forms, partial := DateForms(n.Value)
			if !partial {
				add(n.Value, false, EdgeAny)
			}
			for _, f := range forms {
				add(f.Text, false, f.Edge)
			}
		case NeedleName:
			for _, f := range NameForms(n.Value) {
				add(f.Text, true, f.Edge)
			}
		default:
			add(n.Value, false, n.Edge)
		}
	}
	slices.SortFunc(out, func(a, b cleanTerm) int {
		if len(a.runes) != len(b.runes) {
			return len(b.runes) - len(a.runes)
		}
		return slices.Compare(a.runes, b.runes)
	})
	return out, len(out) <= maxCleanTerms
}

// foldString returns the runes of s in NFC (profile.NFC), each folded
// (Fold), so a term written with combining marks is the term written
// composed, without its ignorable code points (profile.Ignorable).
func foldString(s string) []rune {
	runes := []rune(profile.NFC(s))
	out := runes[:0]
	for _, r := range runes {
		if !profile.Ignorable(r) {
			out = append(out, Fold(r))
		}
	}
	return out
}

// cleaner removes a file's terms from its free text, within a budget of
// work shared by every clean of the file.
type cleaner struct {
	terms []cleanTerm
	// weight is the characters of every term together: the work of
	// searching one character of text for all of them, at most.
	weight int
	budget int
	// listBudget is the work left for the lists' terms: a unit for each
	// character searched and each term found ending there
	// (profile.TextLists.Find), whatever the number of terms, since one
	// automaton searches for them all.
	listBudget int64
}

// newCleaner returns a cleaner of terms with a budget of work, for the
// file's needles and for the lists' terms each.
func newCleaner(terms []cleanTerm, budget int) *cleaner {
	c := &cleaner{terms: terms, budget: budget, listBudget: int64(budget)}
	for _, t := range terms {
		c.weight += len(t.runes)
	}
	return c
}

// affords reports whether one pass over n characters of text fits the
// budget, so a file far past it is refused before any work.
func (c *cleaner) affords(n int) bool {
	return c.weight == 0 || n <= c.budget/c.weight
}

// clean removes every term from s, in any letter case, comparing rune by
// rune, so no byte offset of one form is used in another. What is removed
// is replaced by nothing; runs of spaces left are kept, since the text's
// shape says nothing of what was there. Removing a term can join the text
// around it into another occurrence ("PAPAT001T001" holds "PAT001" again
// once it is cut), so the passes repeat until one removes nothing. Every
// pass is charged to the budget; ctx is read each pass.
//
// The text is searched in NFC (profile.NFC), as the terms are, so a name
// written with combining marks is removed as its composed form is; a text
// from which something is removed is returned in NFC, one from which
// nothing is removed as it came.
func (c *cleaner) clean(ctx context.Context, s string) (string, error) {
	return c.cleanWith(ctx, s, nil, 0)
}

// cleanWith is clean that also removes the terms of the lists in mask
// wherever they stand as words (profile.TermStands), in the same passes,
// so a cut of one can never leave the other joined whole.
func (c *cleaner) cleanWith(ctx context.Context, s string, lists *profile.TextLists, mask uint32) (string, error) {
	if lists == nil {
		mask = 0
	}
	if len(c.terms) == 0 && mask == 0 {
		return s, nil
	}
	runes := []rune(profile.NFC(s))
	cutAny := false
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if !c.affords(len(runes)) {
			return "", errCleanTooLarge
		}
		c.budget -= len(runes) * c.weight
		cut := make([]bool, len(runes))
		found := false
		if mask != 0 {
			if c.listBudget -= int64(len(runes)); c.listBudget < 0 {
				return "", errCleanTooLarge
			}
			spans, err := lists.Find(ctx, runes, mask, &c.listBudget)
			if err != nil {
				if errors.As(err, new(textmatch.ErrBudget)) {
					return "", errCleanTooLarge
				}
				return "", err
			}
			for _, sp := range spans {
				for j := sp[0]; j < sp[1]; j++ {
					cut[j] = true
				}
				found = true
			}
		}
		// The text folded without its ignorable code points (profile.Ignorable),
		// as the check after reads it; at is where each folded rune is in
		// runes, so a cut spans what it skipped.
		folded := make([]rune, 0, len(runes))
		at := make([]int, 0, len(runes))
		for i, r := range runes {
			if !profile.Ignorable(r) {
				folded = append(folded, Fold(r))
				at = append(at, i)
			}
		}
		for _, t := range c.terms {
			m := len(t.runes)
			for i := 0; i+m <= len(folded); i++ {
				if folded[i] != t.runes[0] || !slices.Equal(folded[i:i+m], t.runes) {
					continue
				}
				if t.word && (!wordEdge(folded, i-1) || !wordEdge(folded, i+m)) {
					continue
				}
				if t.partial && (!dateEdge(folded, i-1, -1) || !dateEdge(folded, i+m, 1)) {
					continue
				}
				if t.digits && (digitAt(folded, i-1) || digitAt(folded, i+m)) {
					continue
				}
				if t.ident && !identEdges(folded, i, i+m) {
					continue
				}
				for j := at[i]; j <= at[i+m-1]; j++ {
					cut[j] = true
				}
				found = true
			}
		}
		if !found {
			if !cutAny {
				return string([]rune(s)), nil
			}
			return string(runes), nil
		}
		cutAny = true
		kept := runes[:0]
		for i, r := range runes {
			if !cut[i] {
				kept = append(kept, r)
			}
		}
		runes = kept
	}
}

// wordEdge reports whether position i of a text is outside a word: before
// its start, past its end, or not a letter or a digit.
func wordEdge(runes []rune, i int) bool {
	return i < 0 || i >= len(runes) || (!unicode.IsLetter(runes[i]) && !unicode.IsDigit(runes[i]))
}

// identEdges reports whether the match runes[start:end] of an identifier
// stands as EdgeIdentifier says on the sides where it ends in a letter:
// no letter or "_" beside it there (IdentifierWordRune).
func identEdges(runes []rune, start, end int) bool {
	if unicode.IsLetter(runes[start]) && start > 0 && IdentifierWordRune(runes[start-1]) {
		return false
	}
	return !unicode.IsLetter(runes[end-1]) || end >= len(runes) || !IdentifierWordRune(runes[end])
}

// dateEdge reports whether position i of a text, on the side dir (-1
// before a term, 1 after it), ends a date there: no ASCII letter or digit
// (a letter of another script, as 年 in 1957年, does not join a date), and
// not a date's separator ("-", "/", ".") with a digit beyond it, which
// would make the term part of a longer date.
func dateEdge(runes []rune, i, dir int) bool {
	if i < 0 || i >= len(runes) {
		return true
	}
	if asciiAlnum(runes[i]) {
		return false
	}
	if !strings.ContainsRune("-/.", runes[i]) {
		return true
	}
	j := i + dir
	return j < 0 || j >= len(runes) || !unicode.IsDigit(runes[j])
}

// digitAt reports whether position i of a text holds an ASCII digit.
func digitAt(runes []rune, i int) bool {
	return i >= 0 && i < len(runes) && '0' <= runes[i] && runes[i] <= '9'
}

// asciiAlnum reports whether r is an ASCII letter or digit.
func asciiAlnum(r rune) bool {
	return ('0' <= r && r <= '9') || ('A' <= r && r <= 'Z') || ('a' <= r && r <= 'z')
}

// isASCII reports whether s is only of the default character repertoire's
// bytes, which every character set of PS3.5 6.1 reads the same.
func isASCII(s string) bool {
	for i := range len(s) {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}
