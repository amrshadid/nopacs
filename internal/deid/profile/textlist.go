package profile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/charset"
	"github.com/amrshadid/go-dicom/config"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/pathname"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// Bounds of a profile's text lists. They keep a version within the API's
// message limit and the automaton of every term, built once per version
// and held while it runs, to a few tens of megabytes at most.
const (
	// MaxTextLists is the most lists a profile has; a rule's lists are a
	// mask of this many bits.
	MaxTextLists = 16
	// MaxListTerms is the most terms of one list.
	MaxListTerms = 10000
	// MaxTextListTerms is the most terms of every list together.
	MaxTextListTerms = 20000
	// MaxTermBytes is the longest term, as written.
	MaxTermBytes = 256
	// MaxTextListBytes is the most bytes of every term together, folded:
	// the automaton of the decoded text's search holds a node a byte.
	MaxTextListBytes = 512 << 10
	// MaxRawFormBytes is the most bytes of every term's raw forms
	// together, counted exactly in the character set that writes them
	// longest (rawFormBytes: each case and Unicode form in UTF-8, and for
	// a term outside ASCII its forms in that set), the automaton of the
	// raw bytes' search holding a node a byte. A version within it never
	// has a file refused for the character set it was received in.
	MaxRawFormBytes = 3 << 20
	// MinTermLength is the fewest characters of a term in a script written
	// with spaces between words; a term in a script written without them
	// (Han, Hiragana, Katakana, Hangul), where one character can be a
	// name, may be one character.
	MinTermLength = 2
	// MaxRuleLists is the most lists one rule names.
	MaxRuleLists = MaxTextLists
)

// The codes of a problem of a text list. Like every code, part of the
// API's contract.
const (
	// CodeBadTerm: a term is empty once folded, too short, holds a
	// control character or a backslash, or is not UTF-8.
	CodeBadTerm Code = "bad_term"
	// CodeDuplicateTerm: a term of a list is another term of it again once
	// folded.
	CodeDuplicateTerm Code = "duplicate_term"
	// CodeUnknownList: a rule names a list the profile does not have.
	CodeUnknownList Code = "unknown_list"
)

// TextList is a named, versioned list of terms (names of staff, of sites,
// of departments) that a clean or redact rule removes from free text
// where they stand as words, and that the check after de-identification
// searches every value for, so a term found again anywhere holds the
// file. Its terms are as sensitive as the values of a file: nothing prints
// them, and a description gives the list's name, version, count of terms
// and the SHA-256 of its folded terms only.
type TextList struct {
	// Name is the list's name: 1 to 63 of a-z, 0-9 and -, starting with a
	// letter.
	Name string
	// Version is the list's own version, from 1, which a person raises
	// when the terms change, so a description tells two lists of one name
	// apart.
	Version int32
	// Terms are the terms as written.
	Terms []string
}

// String names the list without its terms, for every verb.
func (l TextList) String() string {
	return fmt.Sprintf("%s v%d (%d terms)", l.Name, l.Version, len(l.Terms))
}

// GoString is String, so %#v prints no term either.
func (l TextList) GoString() string { return l.String() }

// Format prints String for every verb.
func (l TextList) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(l.String())) }

// TextListSummary is what may be said of a list: never a term.
type TextListSummary struct {
	// Name and Version are the list's.
	Name    string
	Version int32
	// Terms is the count of its terms.
	Terms int
	// SHA256 is the SHA-256 of its terms folded (FoldText), sorted and
	// each ended by a line feed, in lowercase hexadecimal: two lists with
	// one checksum hold the same terms however they were written.
	SHA256 string
}

// TextLists are a profile's lists compiled: their summaries and one
// automaton of every distinct folded term, with the lists each term is
// in. It is never changed after CompileTextLists, so one serves every file
// at once. It prints as its summaries, never a term.
type TextLists struct {
	summaries []TextListSummary
	byName    map[string]int
	m         *textmatch.Matcher
	// pats are the automaton's patterns, by number: the folded term, the
	// lists it is in, and whether it is matched anywhere (unspaced) or
	// only standing as a word (TermStands).
	pats []listPattern
	// rawBytes is the bytes of the terms' raw forms in UTF-8 (RawForms)
	// and in the character set that writes them longest (rawFormBytes).
	rawBytes int
	// cacheKey is the key of the lists in the cache of stored versions'
	// lists, empty when they are a document's; retained the bytes of the
	// automata they and their users hold (Retain).
	cacheKey string
	retained atomic.Int64
	// derived is what a user of the lists builds from them once, such as
	// the check's automata of their raw forms (Derived).
	once    sync.Once
	derived any
}

// listPattern is one distinct folded term.
type listPattern struct {
	text     string
	mask     uint32
	unspaced bool
	// charsetCap is the bytes of its forms in the encoding that writes
	// them longest (rawFormBytes).
	charsetCap int
}

// String names the lists without their terms.
func (l *TextLists) String() string {
	if l == nil {
		return "no lists"
	}
	parts := make([]string, len(l.summaries))
	for i, s := range l.summaries {
		parts[i] = fmt.Sprintf("%s v%d (%d terms)", s.Name, s.Version, s.Terms)
	}
	return strings.Join(parts, ", ")
}

// GoString is String.
func (l *TextLists) GoString() string { return l.String() }

// Format prints String for every verb.
func (l *TextLists) Format(f fmt.State, _ rune) { _, _ = f.Write([]byte(l.String())) }

// CompileTextLists checks lists and compiles them. A list with a problem
// is refused with Problems, each naming the list's position and the
// term's (Field "textLists[2].terms[17]") and never a term. Nothing is
// dropped to fit a bound: a list past one is refused whole.
func CompileTextLists(lists []TextList) (*TextLists, Problems) {
	chk := newChecker(false)
	out := compileTextLists(at{c: chk}, lists, true)
	if len(chk.list) > 0 {
		return nil, chk.list
	}
	return out, nil
}

// textListsOf converts a document's text lists.
func textListsOf(pl []*v1.TextList) []TextList {
	out := make([]TextList, len(pl))
	for i, l := range pl {
		out[i] = TextList{Name: l.GetName(), Version: l.GetVersion(), Terms: l.GetTerms()}
	}
	return out
}

// DescribeTextLists returns the summaries of an expanded profile's text
// lists as a description answers them: name, version, count of terms and
// checksum, never a term. A stored form's lists were checked when it was
// made; lists that do not compile answer nothing. No automaton is built:
// a description reads the terms once, folded, and drops them.
func DescribeTextLists(e *v1.ExpandedProfile) []*v1.TextListSummary {
	if len(e.GetTextLists()) == 0 {
		return nil
	}
	chk := newChecker(false)
	l := compileTextLists(at{c: chk}, textListsOf(e.GetTextLists()), false)
	if len(chk.list) > 0 || l == nil {
		return nil
	}
	out := make([]*v1.TextListSummary, 0, l.Len())
	for _, s := range l.Summaries() {
		out = append(out, &v1.TextListSummary{Name: s.Name, Version: s.Version, Terms: int32(s.Terms), Sha256: s.SHA256}) //nolint:gosec // G115: at most MaxListTerms.
	}
	return out
}

// compileTextLists checks lists and compiles them, recording each problem
// at top; nil when there is one, or when there are no lists. With build
// false only the summaries are made, and no automaton.
func compileTextLists(top at, lists []TextList, build bool) *TextLists {
	if len(lists) == 0 {
		return nil
	}
	chk := top.c
	before := len(chk.list)
	if len(lists) > MaxTextLists {
		top.in("textLists").add(CodeTooMany, "a profile has at most %d lists", MaxTextLists)
		return nil
	}
	out := &TextLists{byName: map[string]int{}}
	index := map[string]int{}
	total, bytesTotal := 0, 0
	for i, list := range lists {
		a := top.index("textLists", i)
		switch {
		case !pathname.ValidName(list.Name):
			a.in("name").add(CodeBadName, "a list's name is 1 to 63 of a-z, 0-9 and -, starting with a letter")
		case out.byName[list.Name] != 0:
			a.in("name").add(CodeDuplicateName, "list %d already has this name", out.byName[list.Name])
		default:
			out.byName[list.Name] = i + 1
		}
		if list.Version < 1 || list.Version > MaxVersions {
			a.in("version").add(CodeOutOfRange, "a list's version is 1 to %d", MaxVersions)
		}
		switch {
		case len(list.Terms) == 0:
			a.in("terms").add(CodeRequired, "a list needs at least one term")
			continue
		case len(list.Terms) > MaxListTerms:
			a.in("terms").add(CodeTooMany, "a list has at most %d terms", MaxListTerms)
			continue
		}
		if total += len(list.Terms); total > MaxTextListTerms {
			a.in("terms").add(CodeTooMany, "the lists together have at most %d terms", MaxTextListTerms)
			return nil
		}
		seen := map[string]int{}
		folded := make([]string, 0, len(list.Terms))
		for j, term := range list.Terms {
			ta := a.index("terms", j)
			f, ok := checkTerm(ta, term)
			if !ok {
				continue
			}
			if k := seen[f]; k != 0 {
				ta.add(CodeDuplicateTerm, "term %d is term %d again once folded (case, width and white space)", j+1, k)
				continue
			}
			seen[f] = j + 1
			folded = append(folded, f)
			bytesTotal += len(f)
		}
		if bytesTotal > MaxTextListBytes {
			a.in("terms").add(CodeTooLarge, "the lists' terms together are at most %d bytes folded", MaxTextListBytes)
			return nil
		}
		slices.Sort(folded)
		h := sha256.New()
		for _, f := range folded {
			h.Write([]byte(f))
			h.Write([]byte{'\n'})
		}
		out.summaries = append(out.summaries, TextListSummary{Name: list.Name, Version: list.Version, Terms: len(list.Terms), SHA256: hex.EncodeToString(h.Sum(nil))})
		bit := uint32(1) << uint(i) //nolint:gosec // G115: i is under MaxTextLists.
		for _, f := range folded {
			p, ok := index[f]
			if !ok {
				p = len(out.pats)
				index[f] = p
				out.pats = append(out.pats, listPattern{text: f, unspaced: unspacedTerm(f)})
			}
			out.pats[p].mask |= bit
		}
	}
	if len(chk.list) > before {
		return nil
	}
	raw := rawFormBytes(out.pats)
	if raw > MaxRawFormBytes {
		top.in("textLists").add(CodeTooLarge, "the lists' terms written in every case and Unicode form, in UTF-8 and at most in the character set a file is received in, come to %d bytes, more than %d: the search of a file's raw bytes holds an automaton of them; shorten or split the lists", raw, MaxRawFormBytes)
		return nil
	}
	out.rawBytes = raw
	if build && len(out.pats) > 0 {
		patterns := make([][]byte, len(out.pats))
		for i, p := range out.pats {
			patterns[i] = []byte(p.text)
		}
		out.m = textmatch.New(patterns)
	}
	return out
}

// RawForms returns the byte forms of a folded term a search of raw bytes
// reads, in UTF-8: upper case, lower case and as folded, each composed
// (NFC) and decomposed (NFD: raw bytes are not normalized), ASCII letters
// upper-cased as the raw bytes are when read; each distinct one once.
func RawForms(folded string) [][]byte {
	var out [][]byte
	seen := map[string]bool{}
	for _, v := range []string{strings.ToUpper(folded), strings.ToLower(folded), folded} {
		for _, f := range []string{NFC(v), NFD(v)} {
			b := []byte(f)
			for i, c := range b {
				if 'a' <= c && c <= 'z' {
					b[i] = c - 'a' + 'A'
				}
			}
			if !seen[string(b)] {
				seen[string(b)] = true
				out = append(out, b)
			}
		}
	}
	return out
}

// rawFormBytes returns, counted exactly, the bytes of the raw search's
// forms of the lists' terms in the character set that writes them longest:
// every term's UTF-8 forms (RawForms), and the largest, over the encodings
// of the DICOM library, of the bytes of every term's forms written in that
// encoding (charsetBytes). It sets each pattern's charsetCap to its own
// forms' bytes in the encoding that writes it longest. A file received in
// a set of one encoding has exactly those forms searched, so a version
// within MaxRawFormBytes never has such a file refused; in a set of
// several (ISO 2022 code extensions) the check keeps a term's forms within
// its cap and the whole within MaxRawFormBytes, leaving a form past either
// to the search of the decoded text, never refusing the file.
func rawFormBytes(pats []listPattern) int {
	utf := 0
	perEnc := make([]int, len(libraryEncodings()))
	for i := range pats {
		for _, f := range RawForms(pats[i].text) {
			utf += len(f)
		}
		most := 0
		for e, n := range charsetBytes(pats[i].text) {
			perEnc[e] += n
			most = max(most, n)
		}
		pats[i].charsetCap = most
	}
	most := 0
	for _, n := range perEnc {
		most = max(most, n)
	}
	return utf + most
}

// charsetBytes returns, for each encoding of libraryEncodings, the bytes a
// folded term's forms take written in it, as the check writes them: each
// of its letter cases (upper, lower, as folded) in NFC that the encoding
// writes whole, its escape sequences taken out. A term of ASCII is its
// UTF-8 bytes in every set, already counted: nil.
func charsetBytes(folded string) []int {
	if asciiOnly(folded) {
		return nil
	}
	var variants []string
	seen := map[string]bool{}
	for _, v := range []string{strings.ToUpper(folded), strings.ToLower(folded), folded} {
		if v = NFC(v); !seen[v] {
			seen[v] = true
			variants = append(variants, v)
		}
	}
	out := make([]int, len(libraryEncodings()))
	for e := range out {
		for _, v := range variants {
			n, ok := 0, true
			for _, r := range v {
				b := runeEncodedBytes(r)[e]
				if b < 0 {
					ok = false
					break
				}
				n += b
			}
			if ok {
				out[e] += n
			}
		}
	}
	return out
}

// runeBytes holds runeEncodedBytes's answers.
var runeBytes sync.Map

// runeEncodedBytes returns the bytes each encoding of libraryEncodings
// writes r in, its escape sequences taken out (StripEscapes); -1 for an
// encoding that cannot write it.
func runeEncodedBytes(r rune) []int {
	if v, ok := runeBytes.Load(r); ok {
		return v.([]int) //nolint:forcetypeassert // only []int is stored.
	}
	ctx := config.WithLogger(config.WithWritingValidationMode(context.Background(), config.RAISE), slog.New(slog.DiscardHandler))
	encs := libraryEncodings()
	out := make([]int, len(encs))
	for i, enc := range encs {
		out[i] = -1
		if b, err := charset.EncodeStringWithContext(ctx, string(r), []string{enc}); err == nil {
			out[i] = len(StripEscapes(b))
		}
	}
	runeBytes.Store(r, out)
	return out
}

// StripEscapes removes ISO 2022 escape sequences (ECMA-35 13: ESC,
// intermediate bytes 0x20-0x2F, a final byte 0x30-0x7E) from b, so the
// bytes of a value written in a code extension match between the escapes
// a file wrote around a longer run (PS3.5 6.1.2.5.3). It returns a new
// slice.
func StripEscapes(b []byte) []byte {
	out := b[:0:0]
	for i := 0; i < len(b); i++ {
		if b[i] != 0x1B {
			out = append(out, b[i])
			continue
		}
		j := i + 1
		for j < len(b) && b[j] >= 0x20 && b[j] <= 0x2F {
			j++
		}
		if j < len(b) && b[j] >= 0x30 && b[j] <= 0x7E {
			i = j
			continue
		}
		out = append(out, b[i])
	}
	return out
}

// CharsetCap returns the most bytes of pattern i's forms, beyond its UTF-8
// ones, the check writes in a received character set: their bytes in the
// encoding that writes them longest, what the version's bound counted
// (rawFormBytes).
func (l *TextLists) CharsetCap(i int) int { return l.pats[i].charsetCap }

// libraryEncodings returns the encodings of every character set the DICOM
// library supports (PS3.3 C.12.1.1.2's defined terms), as its encoder
// names them, each once.
var libraryEncodings = sync.OnceValue(func() []string {
	var out []string
	seen := map[string]bool{}
	for _, term := range charset.GetSupportedEncodings() {
		encs, err := charset.ConvertEncodings([]string{term})
		if err != nil {
			continue
		}
		for _, e := range encs {
			if !seen[e] {
				seen[e] = true
				out = append(out, e)
			}
		}
	}
	return out
})

// RawBytes returns the bytes of every term's raw forms together
// (RawForms), at most MaxRawFormBytes.
func (l *TextLists) RawBytes() int {
	if l == nil {
		return 0
	}
	return l.rawBytes
}

// checkTerm checks a term as written and returns it folded (FoldText,
// trimmed). The problems name the term's position only.
func checkTerm(a at, term string) (string, bool) {
	switch {
	case len(term) > MaxTermBytes:
		a.add(CodeTooLong, "a term is at most %d bytes", MaxTermBytes)
		return "", false
	case !utf8.ValidString(term):
		a.add(CodeBadTerm, "a term is UTF-8")
		return "", false
	}
	for _, r := range term {
		switch {
		case r == '\\':
			// A backslash separates the values of an element (PS3.5 6.4):
			// a term holding one would never stand inside one value.
			a.add(CodeBadTerm, "a term holds no backslash, which separates the values of an element")
			return "", false
		case unicode.IsControl(r):
			a.add(CodeBadTerm, "a term holds no control character")
			return "", false
		}
	}
	f := strings.TrimSpace(FoldText(term))
	n := utf8.RuneCountInString(f)
	switch {
	case n == 0:
		a.add(CodeBadTerm, "a term holds at least one character that is not white space")
		return "", false
	case n < MinTermLength && !unspacedTerm(f):
		a.add(CodeBadTerm, "a term in a script written with spaces is at least %d characters", MinTermLength)
		return "", false
	}
	return f, true
}

// unspacedTerm reports whether a term holds a letter of a script written
// without spaces between words (Han, Hiragana, Katakana, Hangul), which
// is matched wherever it stands: such text has no word edges.
func unspacedTerm(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

// Summaries returns what may be said of each list, in the profile's order.
func (l *TextLists) Summaries() []TextListSummary {
	if l == nil {
		return nil
	}
	return slices.Clone(l.summaries)
}

// Len returns the number of lists.
func (l *TextLists) Len() int {
	if l == nil {
		return 0
	}
	return len(l.summaries)
}

// Mask returns the bits of the named lists, and false when one is not a
// list of these.
func (l *TextLists) Mask(names []string) (uint32, bool) {
	var mask uint32
	for _, n := range names {
		i := 0
		if l != nil {
			i = l.byName[n]
		}
		if i == 0 {
			return 0, false
		}
		mask |= 1 << uint(i-1) //nolint:gosec // G115: i is at most MaxTextLists.
	}
	return mask, true
}

// All returns the bits of every list.
func (l *TextLists) All() uint32 {
	if l == nil || len(l.summaries) == 0 {
		return 0
	}
	return uint32(1)<<uint(len(l.summaries)) - 1 //nolint:gosec // G115: at most MaxTextLists.
}

// Matcher returns the automaton of every distinct folded term; pattern i
// is the term Stands and Pattern speak of. Nil when there is no term.
func (l *TextLists) Matcher() *textmatch.Matcher {
	if l == nil {
		return nil
	}
	return l.m
}

// Pattern returns the lists pattern i of Matcher is in, and whether it is
// matched wherever it stands (a term of a script written without spaces).
func (l *TextLists) Pattern(i int) (mask uint32, unspaced bool) {
	return l.pats[i].mask, l.pats[i].unspaced
}

// EachTerm calls fn with each distinct folded term and its pattern
// number, for a user that writes the terms in other forms (the check's raw
// bytes). It is the one way a term leaves this type, into a function that
// must never print it.
func (l *TextLists) EachTerm(fn func(i int, folded string, unspaced bool)) {
	if l == nil {
		return
	}
	for i, p := range l.pats {
		fn(i, p.text, p.unspaced)
	}
}

// Derived returns what build makes of the lists, built on the first call
// and kept for every later one, whatever build they pass: for the one user
// that derives something of its own from the terms, once per version.
func (l *TextLists) Derived(build func() any) any {
	l.once.Do(func() { l.derived = build() })
	return l.derived
}

// Stands reports whether pattern i of Matcher, found at text[start:end] of
// a folded text, stands as a term is taken: TermStands, with the
// pattern's script.
func (l *TextLists) Stands(text []byte, i, start, end int) bool {
	return TermStands(text, start, end, l.pats[i].unspaced)
}

// TermStands reports whether a term found at text[start:end] of a folded
// text stands as a term is taken: on each side where the term ends in a
// letter or a digit, no letter or digit beside it, so a list's "Ann" is
// not taken out of "Annual" and "Lee" not out of "Leeds"; on a side where
// it ends in anything else (a full stop, a hyphen), anything may stand. A
// term of a script written without spaces (unspaced) stands anywhere,
// since such text has no word edges. Bytes that are not UTF-8 read as no
// letter, so a side next to them is an edge: the search errs toward
// finding. Clean and the check after both judge a term so, so the check
// finds again exactly what clean leaves, and no more.
//
// PS3.15 E.3.3's Clean Descriptors option asks that identifying
// information be removed from descriptors while what does not identify is
// kept; a term cut out of the middle of another word would change the
// word and remove what does not identify, which the MIDI-B evaluation
// scores as a failure as it scores a leak.
func TermStands(text []byte, start, end int, unspaced bool) bool {
	if unspaced {
		return true
	}
	first, _ := utf8.DecodeRune(text[start:end])
	last, _ := utf8.DecodeLastRune(text[start:end])
	if wordRune(first) && start > 0 {
		if r, _ := utf8.DecodeLastRune(text[:start]); wordRune(r) {
			return false
		}
	}
	if wordRune(last) && end < len(text) {
		if r, _ := utf8.DecodeRune(text[end:]); wordRune(r) {
			return false
		}
	}
	return true
}

// wordRune reports whether r is a letter or a digit of any script.
func wordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// Find returns the spans of runes of text, a text in NFC, that the terms
// of the lists in mask stand at (TermStands), each as [start, end) in
// runes, in the order they end. Each pattern found, whether or not it
// stands, costs a unit of *budget, as textmatch.Matcher.Scan charges; past
// it Find returns textmatch.ErrBudget. The text is folded as the terms are
// (FoldText), so a run of white space in it matches the one space of a
// term, and a span covers the whole run.
func (l *TextLists) Find(ctx context.Context, text []rune, mask uint32, budget *int64) ([][2]int, error) {
	if l == nil || l.m == nil || mask == 0 || len(text) == 0 {
		return nil, nil
	}
	folded := make([]byte, 0, len(text))
	// runeAt is the rune each byte of folded was made from: a run of white
	// space is one space, made from the run's first rune.
	runeAt := make([]int32, 0, len(text))
	space := false
	for i, r := range text {
		before := len(folded)
		folded = AppendFolded(folded, r, &space)
		for range len(folded) - before {
			runeAt = append(runeAt, int32(i)) //nolint:gosec // G115: a text is bounded far below 2^31 runes.
		}
	}
	var spans [][2]int
	_, err := l.m.Scan(ctx, folded, budget, func(pat, end int) {
		p := l.pats[pat]
		start := end - len(p.text)
		if p.mask&mask == 0 || !TermStands(folded, start, end, p.unspaced) {
			return
		}
		// A term begins and ends with a character that is not white space
		// (it is trimmed), so its last byte is of one rune.
		spans = append(spans, [2]int{int(runeAt[start]), int(runeAt[end-1]) + 1})
	})
	if err != nil {
		return nil, err
	}
	return spans, nil
}
