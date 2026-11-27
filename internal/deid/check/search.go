package check

import (
	"errors"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/filereader/attributes"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// workBudget bounds the work of a check that does not grow with the file:
// each form found ending at a position (whether or not its edges then
// fit) and each rune of a form checked against a character set costs one
// unit. The automaton's own steps, at most two a byte, are not charged.
// Normal files spend a few thousand units; a file whose text makes forms
// end on one another at every byte (a run of one letter against forms of
// every length) spends the budget within a second or two whatever its
// size, and is refused as too large rather than searched for minutes.
const workBudget = 1 << 25

// search finds the forms in p: in one pass over the text of each element,
// then one pass over the raw bytes outside the image data, a window at a
// time, for the forms the text did not hold. It adds at most one finding
// per form to fs. The raw forms are written first (encodeRaw), so the
// caller can take the memory of their automaton before either pass.
func (s *searcher) search() error {
	if err := s.text(); err != nil {
		return err
	}
	if err := s.raw(); err != nil {
		return err
	}
	return s.rawListsSearch()
}

// rawTarget is a form a raw pattern is a byte form of.
type rawTarget struct {
	form      int
	short, g0 bool
}

// encodeRaw writes the raw forms of every form (form.encodings), each
// rune checked against a character set charged to the budget, and keeps
// them for raw, with the bytes of the distinct ones together (rawBytes)
// and the longest (rawLongest), which the check's reservation counts.
func (s *searcher) encodeRaw() error {
	s.raws, s.rawBytes, s.rawLongest = make([][]rawForm, len(s.set.forms)), 0, 0
	enc := newEncoder(s.quiet, s.source, &s.budget)
	enc.library = s.seams.library
	seen := map[string]bool{}
	for i, f := range s.set.forms {
		raws, err := f.encodings(enc, s.opts.MinLength)
		if err != nil {
			return err
		}
		if s.budget < 0 {
			return &Refusal{Cause: CauseTooLarge}
		}
		s.raws[i] = raws
		for _, r := range raws {
			if !seen[string(r.bytes)] {
				seen[string(r.bytes)] = true
				s.rawBytes += len(r.bytes)
				s.rawLongest = max(s.rawLongest, len(r.bytes))
			}
		}
	}
	return nil
}

// raw searches the raw bytes outside the image data for every byte form of
// every form not found yet, the bytes of all forms within MaxFormBytes.
// Each range is read a window at a time (scanStream), its ASCII letters
// upper-cased as read; a match across the edge between two windows is in
// the second whole.
func (s *searcher) raw() error {
	forms := s.set.forms
	rawIndex := map[string]int{}
	var rawPatterns [][]byte
	var rawTargets [][]rawTarget
	total := s.set.bytes
	anyG0 := false
	for i := range forms {
		if s.done[i] {
			continue
		}
		for _, r := range s.raws[i] {
			at, ok := rawIndex[string(r.bytes)]
			if !ok {
				if total += len(r.bytes); total > s.opts.MaxFormBytes {
					return &Refusal{Cause: CauseTooManyNeedles}
				}
				at = len(rawPatterns)
				rawIndex[string(r.bytes)] = at
				rawPatterns = append(rawPatterns, r.bytes)
				rawTargets = append(rawTargets, nil)
			}
			rawTargets[at] = append(rawTargets[at], rawTarget{form: i, short: r.short, g0: r.g0})
			anyG0 = anyG0 || r.g0
		}
	}
	if len(rawPatterns) == 0 {
		return nil
	}
	rm := textmatch.New(rawPatterns)
	for _, seg := range s.p.segs {
		// A run of two-byte characters never crosses the image data: the
		// tag after it begins with a byte that is not such a character
		// (group 7FE0 and later), so each range is followed from its start.
		var sh *shifter
		if anyG0 {
			sh = newShifter()
		}
		prepare := func(win []byte, fresh, from, to int, base int64) {
			upperASCII(win[fresh:])
			if sh != nil {
				sh.feed(win[from:to], seg[0]+base+int64(from))
			}
		}
		found := func(win []byte, pat, start, end int, base int64) {
			off := seg[0] + base + int64(start)
			for _, t := range rawTargets[pat] {
				if s.done[t.form] || (t.short && s.p.inBinary(off)) || (t.g0 && !sh.in(off, seg[0]+base+int64(end))) ||
					!edgesFit(win, start, end, forms[t.form].mode) || s.composesAfter(win, start, end) {
					continue
				}
				s.done[t.form] = true
				s.fs.add(Finding{Path: s.p.locate(off), Kind: forms[t.form].kind, Raw: true})
			}
		}
		var err error
		s.buf, _, err = scanStream(s.ctx, rm, sectionOf(s.p.src, seg[0], seg[1]), s.window(seg[1]-seg[0]), s.buf, &s.budget, prepare, found)
		if err != nil {
			return budgetRefusal(err)
		}
	}
	return nil
}

// composesAfter reports whether the raw match win[start:end] is joined by
// Unicode composition (UAX #15) to what follows it, so that the text there,
// in NFC, no longer begins with the match: "RENE" followed by U+0301 (a
// combining acute, Unicode category M, which edgesFit reads as no letter)
// is "RENÉ" in NFC. Clean compares text in NFC, so it leaves such a text,
// and the check, which must agree with it, does not count it as a copy.
// The bytes are read in every character set the file was received in
// (UTF-8 byte by byte, any other by the library's decoder), and the edge
// is closed only when every reading composes: a byte that one set reads
// as a combining mark and another as a letter of its own is still a
// finding, as is any following byte below 0x80 (an ASCII character
// composes with nothing before it).
//
// This opens no leak. What it lets go is a text whose NFC form is a
// different word from the form searched for: "RENÉ" is not "RENE" to
// clean, to the text search, or to anyone reading the value, and an
// accented name is its own needle. When the patient's name is "René", the
// needle is "René": its composed bytes and its decomposed ones ("RENE",
// U+0301, encodings) are raw forms of their own, and the text search finds
// it in NFC however it was written. A mark that composes with nothing
// ("X" then U+0301 stays two characters in NFC) leaves the match standing
// in NFC, so it is found, as clean cuts it.
func (s *searcher) composesAfter(win []byte, start, end int) bool {
	if end >= len(win) || win[end] < utf8.RuneSelf {
		return false
	}
	after := win[end:min(len(win), end+edgeRoom)]
	for _, enc := range s.source {
		match, next := string(win[start:end]), string(after)
		if enc != utf8Encoding {
			match = attributes.DecodeTextWithContext(s.quiet, "LO", win[start:end], []string{enc})
			next = attributes.DecodeTextWithContext(s.quiet, "LO", after, []string{enc})
		}
		if strings.HasPrefix(profile.NFC(match+next), profile.NFC(match)) {
			return false
		}
	}
	return len(s.source) > 0
}

// utf8Encoding is the library's name of the encoding of ISO_IR 192
// (PS3.3 C.12.1.1.2), read here without its decoder.
const utf8Encoding = "UTF-8"

// budgetRefusal turns a pass past its budget into the refusal too_large,
// and returns any other error as it is.
func budgetRefusal(err error) error {
	var b textmatch.ErrBudget
	if errors.As(err, &b) {
		return &Refusal{Cause: CauseTooLarge}
	}
	return err
}

// edgesFit reports whether the match text[start:end] stands on its own as
// m says. Folded text and upper-cased raw bytes are judged alike: what is
// read beside a match is an ASCII digit, an ASCII letter, a separator, or
// a rune of another script.
func edgesFit(text []byte, start, end int, m mode) bool {
	switch m {
	case modeBounded:
		return dateEdge(text, start, -1) && dateEdge(text, end, 1)
	case modeWord:
		return wordEdge(text, start, -1) && wordEdge(text, end, 1)
	case modeDigits:
		return (start == 0 || !isDigit(text[start-1])) && (end == len(text) || !isDigit(text[end]))
	case modeIdentifier:
		if !identifierEdge(text, start, end) {
			return false
		}
	case modeUID:
		if isDigit(text[start]) && start > 0 && (isDigit(text[start-1]) ||
			(text[start-1] == '.' && start > 1 && isDigit(text[start-2]))) {
			return false
		}
		return !isDigit(text[end-1]) || end == len(text) || !isDigit(text[end])
	}
	if isDigit(text[start]) && start > 0 && isDigit(text[start-1]) {
		return false
	}
	if (m == modePlain || m == modeIdentifier) && isDigit(text[end-1]) && end < len(text) && isDigit(text[end]) {
		return false
	}
	return true
}

// runeAt returns the rune beside position i of text on side dir (-1
// before it, 1 after it), its size, and whether there is one.
func runeAt(text []byte, i, dir int) (rune, int, bool) {
	if dir < 0 {
		if i == 0 {
			return 0, 0, false
		}
		r, size := utf8.DecodeLastRune(text[:i])
		return r, size, true
	}
	if i >= len(text) {
		return 0, 0, false
	}
	r, size := utf8.DecodeRune(text[i:])
	return r, size, true
}

// wordEdge reports whether a word ends at position i of text on side dir:
// no letter or digit of any script beside it.
func wordEdge(text []byte, i, dir int) bool {
	r, _, ok := runeAt(text, i, dir)
	return !ok || (!unicode.IsLetter(r) && !unicode.IsDigit(r))
}

// identifierEdge reports whether the match text[start:end] of an
// identifier stands as engine.EdgeIdentifier says on each side where it
// ends in a letter: no letter of any script and no "_" beside it
// (engine.IdentifierWordRune).
// Bytes that are not UTF-8 (a form in another character set) read as no
// letter, so such a side is matched as plain: the search errs toward
// finding.
func identifierEdge(text []byte, start, end int) bool {
	if first, _, _ := runeAt(text, start, 1); unicode.IsLetter(first) {
		if r, _, ok := runeAt(text, start, -1); ok && engine.IdentifierWordRune(r) {
			return false
		}
	}
	if last, _, _ := runeAt(text, end, -1); unicode.IsLetter(last) {
		if r, _, ok := runeAt(text, end, 1); ok && engine.IdentifierWordRune(r) {
			return false
		}
	}
	return true
}

// dateEdge reports whether a date stands alone at position i of text on
// side dir (-1 before it, 1 after it): no ASCII letter or digit there (a
// letter of another script, as 年 in 1957年, does not join a date), and no
// date's separator ("-", "/", ".") with a digit beyond it, which would make
// it part of a longer date.
func dateEdge(text []byte, i, dir int) bool {
	r, size, ok := runeAt(text, i, dir)
	if !ok {
		return true
	}
	if r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
		return false
	}
	if r != '-' && r != '/' && r != '.' {
		return true
	}
	j := i + dir*size
	if dir < 0 {
		return j <= 0 || !isDigit(text[j-1])
	}
	return j >= len(text) || !isDigit(text[j])
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }
