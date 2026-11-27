package check

import (
	"context"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/charset"
	"github.com/amrshadid/go-dicom/config"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// mode is how a form's edges are matched.
type mode int

const (
	// modePlain: a form beginning with a digit is not matched after a
	// digit, one ending with a digit not before one.
	modePlain mode = iota
	// modeFullDate: as plain, but digits may follow, as a time follows a
	// date in a DT value.
	modeFullDate
	// modeBounded: matched only standing alone as a date
	// (engine.EdgeDate).
	modeBounded
	// modeWord: matched only as a whole word (engine.EdgeWord).
	modeWord
	// modeDigits: matched only with no digit beside it
	// (engine.EdgeDigits).
	modeDigits
	// modeIdentifier: an identifier of words (engine.EdgeIdentifier,
	// engine.IdentifierEdge): on a side where it ends in a letter, no
	// letter or "_" beside it; on one where it ends in a digit, as plain.
	modeIdentifier
	// modeUID: as plain, and not preceded by a digit and a dot, which
	// would make it the end of another UID. A dot and digits after it are
	// a finding: a UID made by appending to the original shows the
	// original.
	modeUID
)

// form is one written form of a needle.
type form struct {
	// text is the form as written, for encoding into raw bytes.
	text string
	// folded is the form folded (foldText), for the text search.
	folded string
	kind   Kind
	mode   mode
	// unspaced is set for a form in a script written without spaces,
	// searched for in the raw bytes at any length.
	unspaced bool
}

// formSet is the forms of a file's needles.
type formSet struct {
	forms   []form
	skipped int
	// bytes is the bytes of the forms' folded texts together.
	bytes int
	// lists are the profile's text lists of the needles of kind
	// engine.NeedleList, each once: their terms are searched for by their
	// own automata, built once per profile version (lists.go).
	lists []*profile.TextLists
}

// forms returns the distinct forms of needles, refusing more needles than
// MaxNeedles, one longer than MaxNeedleBytes, or forms of more than
// MaxFormBytes together.
func (c *Checker) forms(needles engine.Needles) (formSet, error) {
	var set formSet
	if len(needles) > c.opts.MaxNeedles {
		return set, &Refusal{Cause: CauseTooManyNeedles}
	}
	type key struct {
		folded string
		mode   mode
	}
	seen := map[key]bool{}
	skippedSeen := map[string]bool{}
	add := func(text string, kind Kind, m mode) {
		text = strings.Trim(text, " \x00")
		if text == "" {
			return
		}
		folded := foldText(text)
		n := utf8.RuneCountInString(folded)
		un := engine.Unspaced(text)
		if (un && n < c.opts.MinUnspaced) || (!un && n < c.opts.MinLength) {
			if !skippedSeen[folded] {
				skippedSeen[folded] = true
				set.skipped++
			}
			return
		}
		k := key{folded, m}
		if seen[k] {
			return
		}
		seen[k] = true
		set.bytes += len(folded)
		set.forms = append(set.forms, form{text: text, folded: folded, kind: kind, mode: m, unspaced: un})
	}
	for _, n := range needles {
		if n.Kind == engine.NeedleList {
			if n.Lists.Matcher() != nil && !slices.Contains(set.lists, n.Lists) {
				set.lists = append(set.lists, n.Lists)
			}
			continue
		}
		if len(n.Value) > c.opts.MaxNeedleBytes {
			return formSet{}, &Refusal{Cause: CauseTooManyNeedles}
		}
		kind := kindOf(n.Kind)
		values := strings.Split(n.Value, `\`)
		if n.Whole {
			values = []string{n.Value}
		}
		if len(values) > 1 {
			add(n.Value, kind, modePlain)
		}
		for _, v := range values {
			v = strings.Trim(v, " \x00")
			if v == "" {
				continue
			}
			switch n.Kind {
			case engine.NeedleUID:
				if !engine.Registered(v) {
					add(v, kind, modeUID)
				}
			case engine.NeedleName:
				for _, f := range engine.NameForms(v) {
					add(f.Text, kind, modeOf(f.Edge, modePlain))
				}
			case engine.NeedleDate:
				forms, partial := engine.DateForms(v)
				if !partial {
					add(v, kind, modePlain)
				}
				for _, f := range forms {
					add(f.Text, kind, modeOf(f.Edge, modeFullDate))
				}
			default:
				add(v, kind, modeOf(n.Edge, modePlain))
			}
		}
		if set.bytes > c.opts.MaxFormBytes {
			return formSet{}, &Refusal{Cause: CauseTooManyNeedles}
		}
	}
	return set, nil
}

// modeOf returns the mode of a form's edge; any is the mode of a form
// taken anywhere.
func modeOf(e engine.Edge, anyMode mode) mode {
	switch e {
	case engine.EdgeDate:
		return modeBounded
	case engine.EdgeWord:
		return modeWord
	case engine.EdgeDigits:
		return modeDigits
	case engine.EdgeIdentifier:
		return modeIdentifier
	}
	return anyMode
}

// foldText returns s folded as profile.FoldText folds it: in NFC, every
// rune folded (case, and full-width ASCII), every run of white space one
// space. Bytes that are not UTF-8 become U+FFFD, which no form holds.
// Forms, list terms and texts are folded alike.
func foldText(s string) string { return profile.FoldText(s) }

// foldString appends s, already in NFC, to dst folded as foldText folds
// it; *space says whether what was appended before ended in white space.
func foldString(dst []byte, s string, space *bool) []byte {
	for _, r := range s {
		dst = profile.AppendFolded(dst, r, space)
	}
	return dst
}

// sourceEncodings returns the encodings of the character set a file was
// received in, as the DICOM library names them; the default repertoire
// read as ISO 8859-1 when it declared none, as the library reads such a
// file (PS3.5 6.1.2.3).
func sourceEncodings(characterSet *string) []string {
	cs := ""
	if characterSet != nil {
		cs = *characterSet
	}
	enc, err := charset.ConvertEncodings(strings.Split(cs, `\`))
	if err != nil || len(enc) == 0 {
		return []string{charset.DefaultEncoding}
	}
	return enc
}

// rawForm is one byte form of a form, for the raw search.
type rawForm struct {
	bytes []byte
	// short is set for an encoding under MinLength bytes (a name of one
	// or two characters of a script written without spaces), matched only
	// outside the values of standard binary elements, whose numbers can
	// spell any two or three bytes by chance.
	short bool
	// g0 is set for text written in a 7-bit set of two-byte characters
	// (ISO 2022 IR 87 or IR 159, PS3.5 6.1.2.5.3), whose bytes are
	// ordinary ASCII letters: 林 is "NS". It is matched only inside a run
	// the file shifted into such a set by its escape sequence, at an even
	// distance from it.
	g0 bool
}

// encoder writes forms in the received character sets. The library's
// encoder searches every prefix of a text it cannot write whole, which
// takes the square of the text's length, so a text is written in a set
// only once each of its runes is known to be writable there, rune by rune;
// each rune checked costs one unit of the budget, and ctx is read as it
// goes.
type encoder struct {
	ctx    context.Context
	source []string
	budget *int64
	// can holds, per encoding, whether a rune is writable in it.
	can map[string]map[rune]bool
	// library is the seam told of each call to the library's encoder.
	library func(encodings []string)
}

// encode calls the library's encoder, always with one encoding.
func (e *encoder) encode(text, enc string) ([]byte, error) {
	encs := []string{enc}
	if e.library != nil {
		e.library(encs)
	}
	return charset.EncodeStringWithContext(e.ctx, text, encs)
}

// newEncoder returns an encoder of the source encodings. It asks the
// library to fail on a text it cannot write (config.RAISE): otherwise the
// library answers such a text with its UTF-8 bytes and no error.
func newEncoder(ctx context.Context, source []string, budget *int64) *encoder {
	ctx = config.WithWritingValidationMode(ctx, config.RAISE)
	return &encoder{ctx: ctx, source: source, budget: budget, can: map[string]map[rune]bool{}}
}

// writable reports whether r can be written in enc.
func (e *encoder) writable(r rune, enc string) bool {
	m := e.can[enc]
	if m == nil {
		m = map[rune]bool{}
		e.can[enc] = m
	}
	ok, known := m[r]
	if !known {
		_, err := e.encode(string(r), enc)
		ok = err == nil
		m[r] = ok
	}
	return ok
}

// encodeRuns writes text in the source encodings: each run of runes that
// the first encoding writing its first rune writes too, in that encoding
// alone, the runs joined without the escapes a file writes between them.
// ok is false when a rune is writable in none. g0 reports a run of
// characters outside ASCII written as 7-bit bytes.
func (e *encoder) encodeRuns(text string, only string) (out []byte, g0, ok bool, err error) {
	encs := e.source
	if only != "" {
		encs = []string{only}
	}
	var run []rune
	runEnc := ""
	flush := func() bool {
		if len(run) == 0 {
			return true
		}
		b, err := e.encode(string(run), runEnc)
		if err != nil {
			return false
		}
		b = profile.StripEscapes(b)
		if !plainASCII([]byte(string(run))) && plainASCII(b) {
			g0 = true
		}
		out = append(out, b...)
		run = run[:0]
		return true
	}
	for i, r := range text {
		if i%4096 == 0 {
			if err := e.ctx.Err(); err != nil {
				return nil, false, false, err
			}
		}
		*e.budget--
		enc := ""
		if runEnc != "" && e.writable(r, runEnc) {
			enc = runEnc
		} else {
			for _, c := range encs {
				if e.writable(r, c) {
					enc = c
					break
				}
			}
		}
		if enc == "" {
			return nil, false, false, nil
		}
		if enc != runEnc {
			if !flush() {
				return nil, false, false, nil
			}
			runEnc = enc
		}
		run = append(run, r)
	}
	if !flush() {
		return nil, false, false, nil
	}
	return out, g0, true, nil
}

// encodings returns the raw forms of f: its text in upper case, lower case
// and as written (the raw bytes' ASCII letters are upper-cased, but no
// other letter: "müller" and "MÜLLER" are two byte strings), each in UTF-8
// as given, composed (NFC) and decomposed (NFD: raw bytes are not
// normalized, so a "ü" may be there as "u" and a combining mark), and
// composed in the source encodings, across all of them (a run in each) and
// in each alone, escape sequences taken out, ASCII letters in upper case.
// A form under minBytes is kept only when it is unspaced, as short.
func (f form) encodings(e *encoder, minBytes int) ([]rawForm, error) {
	var out []rawForm
	seen := map[string]bool{}
	put := func(b []byte, g0 bool) {
		if len(b) == 0 || (len(b) < minBytes && !f.unspaced) {
			return
		}
		upperASCII(b)
		if !seen[string(b)] {
			seen[string(b)] = true
			out = append(out, rawForm{bytes: b, short: len(b) < minBytes, g0: g0})
		}
	}
	// In UTF-8, the forms a list's term is searched for in too: one
	// implementation of them (profile.RawForms).
	for _, b := range profile.RawForms(f.text) {
		put(b, false)
	}
	for _, v := range []string{strings.ToUpper(f.text), strings.ToLower(f.text), f.text} {
		v = profile.NFC(v)
		alone := []string{""}
		if len(e.source) > 1 {
			alone = append(alone, e.source...)
		}
		for _, only := range alone {
			b, g0, ok, err := e.encodeRuns(v, only)
			if err != nil {
				return nil, err
			}
			if ok {
				put(b, g0)
			}
		}
	}
	return out, nil
}

// upperASCII changes the ASCII lower-case letters of b to upper case, in
// place.
func upperASCII(b []byte) {
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - 'a' + 'A'
		}
	}
}
