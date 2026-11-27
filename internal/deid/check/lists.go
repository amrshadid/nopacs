package check

import (
	"context"
	"log/slog"
	"slices"
	"strings"
	"sync"

	"github.com/amrshadid/go-dicom/config"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/filereader/attributes"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// The terms of a profile's text lists are searched for in every file
// whatever it holds (engine.NeedleList): in the decoded text of every
// element, by the lists' own automaton of folded terms, as the needles'
// forms are; and in the raw bytes, by an automaton of their byte forms in
// UTF-8 and in the character set the file was received in. The text's
// automaton is built once per version (profile.TextLists, shared through
// the profile's cache of compiled lists), the raw one once per version and
// character set (rawListsFor); a file pays for the passes over its text,
// and its reservation counts the automata it searches with (listsBytes).
// A term stands as profile.TermStands says, the rule clean cuts by, so
// the check finds again what clean leaves and no more. A finding is
// KindListTerm at its path, never the term.
//
// A list's terms are words of text. What holds only numbers is not
// searched for them, in its loose readings or its raw bytes, since 10,000
// names of four letters each occur by chance in a megabyte of random
// bytes (listsSkipped): a standard value of a numeric VR the data
// dictionary agrees is numeric (OW, OF, OD, OL, OV, FL, FD, SL, SS, SV, UL,
// US, UV, AT), image data at any depth (which the pixel rules handle), an
// overlay's data and a waveform's. Everything else is searched: OB, which
// holds an encapsulated document (a CDA's XML, a PDF's text) as readily
// as bytes, UN, any private value, and a text element sent with a numeric
// VR.

// numericVRs are the VRs whose value is numbers, never text (PS3.5 6.2).
var numericVRs = map[string]bool{
	"OW": true, "OF": true, "OD": true, "OL": true, "OV": true, "FL": true, "FD": true,
	"SL": true, "SS": true, "SV": true, "UL": true, "US": true, "UV": true, "AT": true,
}

// listsSkipped reports whether e is left out of the lists' search: image
// data (Pixel Data, Float and Double Float Pixel Data, at any depth), an
// overlay's data (60xx,3000), a waveform's (5400,1010), or a standard value
// of a numeric VR as sent whose tag the dictionary gives numeric VRs only.
func listsSkipped(e *attributes.DataElementValue) bool {
	t := uint32(e.Tag)
	switch {
	case e.Tag.IsPrivate():
		return false
	case pixelTags[e.Tag], t&0xFF00FFFF == 0x60003000, t == 0x54001010:
		return true
	}
	if !numericVRs[e.VR] {
		return false
	}
	v := tag.Tag(t).GetVR()
	if v == "" {
		return false
	}
	for _, part := range strings.Split(v, " or ") {
		if !numericVRs[part] {
			return false
		}
	}
	return true
}

// standardBinary reports whether e is a standard binary value
// (parsed.binary holds their bytes): of a binary VR as declared, and of a
// tag the data dictionary gives only binary VRs. The declared VR is the
// sender's word: a Series Description sent as OB is text, and is
// searched.
func standardBinary(e *attributes.DataElementValue) bool {
	return !e.Tag.IsPrivate() && binaryVR(e.VR) && dictBinary(uint32(e.Tag))
}

// binaryVR reports whether vr is a VR of numbers or bytes: not text, not
// UN, not a sequence, and known.
func binaryVR(vr string) bool {
	return vr != "UN" && vr != "" && vr != "SQ" && !dataelem.IsTextVR(dataelem.VR(vr))
}

// dictBinary reports whether the data dictionary gives tag t binary VRs
// only (PS3.6: "OB or OW" is binary, "US or SS" too); false for a tag it
// does not know, which may be anything.
func dictBinary(t uint32) bool {
	v := tag.Tag(t).GetVR()
	if v == "" {
		return false
	}
	for _, part := range strings.Split(v, " or ") {
		if !binaryVR(part) {
			return false
		}
	}
	return true
}

// listsText searches the folded text hay of the value at path for the
// lists' terms.
func (s *searcher) listsText(path string, hay []byte) error {
	if s.listsOff {
		return nil
	}
	for _, l := range s.set.lists {
		m := l.Matcher()
		at, err := m.Scan(s.ctx, hay, &s.budget, func(pat, end int) {
			if start := end - m.Len(pat); l.Stands(hay, pat, start, end) {
				s.foundListTerm(path, false)
			}
		})
		if err != nil {
			return s.refused(s.read+at, err)
		}
	}
	return nil
}

// listsStream searches the value at [off, end) of the file, read as a
// stream as stream reads it, for the lists' terms, one list at a time.
func (s *searcher) listsStream(path string, off, end int64) error {
	if s.listsOff {
		return nil
	}
	for _, l := range s.set.lists {
		text := newFoldReader(profile.NFCReader(sectionOf(s.p.src, off, end)))
		var at int64
		var err error
		s.buf, at, err = scanStream(s.ctx, l.Matcher(), text, s.window(end-off), s.buf, &s.budget, nil, func(win []byte, pat, start, end int, _ int64) {
			if l.Stands(win, pat, start, end) {
				s.foundListTerm(path, false)
			}
		})
		if err != nil {
			return s.refused(s.read+int(at), err)
		}
	}
	return nil
}

// foundListTerm records a term of a list found at path.
func (s *searcher) foundListTerm(path string, raw bool) {
	s.listFound = true
	s.fs.add(Finding{Path: path, Kind: KindListTerm, Raw: raw})
}

// rawLists is the automaton of the byte forms of a version's list terms
// in one received character set.
type rawLists struct {
	m *textmatch.Matcher
	// targets are, per raw pattern, the list patterns it is a form of.
	targets [][]rawListTarget
	anyG0   bool
	// refused is set when the forms in this character set pass
	// profile.MaxRawFormBytes, or their writing its budget: a file
	// received in it with these lists is refused (CauseTooManyNeedles).
	refused bool
}

// rawListTarget is a term a raw pattern is a byte form of.
type rawListTarget struct {
	pat int
	// short is set for a form under DefaultMinLength bytes, of a script
	// written without spaces, matched only outside the values of standard
	// binary elements, as a needle's short form is; g0 for a form in a
	// 7-bit set of two-byte characters, matched only inside a run the file
	// shifted into it (rawForm).
	short, g0 bool
}

// maxListCharsets is how many received character sets one version's raw
// automata are kept for; a site's files come in a few. The memory they
// hold is counted against the cache of stored versions' lists
// (profile.TextLists.Retain).
const maxListCharsets = 8

// rawListCache is a version's raw automata by received character set.
type rawListCache struct {
	mu sync.Mutex
	// ascii reports lists of terms of ASCII only, whose raw forms are the
	// same in every character set: one set serves every file.
	ascii bool
	by    map[string]*rawEntry
	clock uint64
}

// rawEntry is one character set's raw automaton, or its build under way.
type rawEntry struct {
	done chan struct{}
	r    *rawLists
	used uint64
}

// rawListsFor returns the raw automaton of lists in the source encodings,
// built once per character set, by one caller while the others wait,
// outside the lock, and kept with the lists (profile.TextLists.Derived),
// the least recently used set dropped past maxListCharsets. A character
// set is known by its encodings, sorted and each once; lists of ASCII only
// have one set for every file.
func rawListsFor(l *profile.TextLists, source []string) *rawLists {
	cache, _ := l.Derived(func() any {
		c := &rawListCache{by: map[string]*rawEntry{}, ascii: true}
		l.EachTerm(func(_ int, folded string, _ bool) {
			if !plainASCII([]byte(folded)) {
				c.ascii = false
			}
		})
		return c
	}).(*rawListCache)
	key := ""
	if !cache.ascii {
		encs := slices.Clone(source)
		slices.Sort(encs)
		key = strings.Join(slices.Compact(encs), `\`)
	}
	cache.mu.Lock()
	cache.clock++
	if e, ok := cache.by[key]; ok {
		e.used = cache.clock
		cache.mu.Unlock()
		<-e.done
		return e.r
	}
	e := &rawEntry{done: make(chan struct{}), used: cache.clock}
	cache.by[key] = e
	var dropped []*rawEntry
	for len(cache.by) > maxListCharsets {
		oldest := ""
		for k, x := range cache.by {
			if k != key && (oldest == "" || x.used < cache.by[oldest].used) {
				oldest = k
			}
		}
		dropped = append(dropped, cache.by[oldest])
		delete(cache.by, oldest)
	}
	cache.mu.Unlock()
	for _, d := range dropped {
		<-d.done
		l.Retain(-d.r.bytes())
	}
	if cache.ascii {
		source = []string{utf8Encoding}
	}
	e.r = buildRawLists(l, source)
	l.Retain(e.r.bytes())
	close(e.done)
	return e.r
}

// bytes is the memory of the automaton.
func (r *rawLists) bytes() int64 {
	if r == nil || r.m == nil {
		return 0
	}
	return int64(r.m.Nodes()) * automatonNodeBytes
}

// buildRawLists writes each term in the byte forms the raw search reads,
// as a needle's are written (form.encodings): each case and Unicode form
// in UTF-8 (profile.RawForms), and in the received character set, across
// its encodings and in each alone, escape sequences taken out, ASCII
// letters upper-cased as the raw bytes are when read. A form under
// DefaultMinLength bytes is kept only for a term of a script written
// without spaces, as short. A term of ASCII has the same bytes in every
// character set a file may be received in (PS3.5 6.1), so only the terms
// outside ASCII add forms of a set; past profile.MaxRawFormBytes in all,
// or past the budget of writing them, the set is refused. It is built
// under no file's context, since what it builds is kept for every file:
// a file's check canceled must not leave a set refused for the others.
func buildRawLists(l *profile.TextLists, source []string) *rawLists {
	out := &rawLists{}
	index := map[string]int{}
	var patterns [][]byte
	total := 0
	budget := int64(workBudget)
	enc := newEncoder(config.WithLogger(context.Background(), slog.New(slog.DiscardHandler)), source, &budget)
	l.EachTerm(func(pat int, folded string, unspaced bool) {
		if out.refused {
			return
		}
		raws, err := form{text: folded, unspaced: unspaced}.encodings(enc, DefaultMinLength)
		if err != nil || budget < 0 {
			out.refused = true
			return
		}
		// The UTF-8 forms, and of the received set's forms at most the
		// bytes the version's bound counted for the term
		// (profile.CharsetCap), the whole within profile.MaxRawFormBytes:
		// no valid version has a file refused for its character set. In a
		// set of one encoding every form fits, as counted; in a set of
		// several (ISO 2022 code extensions) a form past either bound is
		// left to the search of the decoded text.
		utf := map[string]bool{}
		for _, b := range profile.RawForms(folded) {
			utf[string(b)] = true
		}
		other, charsetCap := 0, l.CharsetCap(pat)
		for _, r := range raws {
			at, ok := index[string(r.bytes)]
			if !utf[string(r.bytes)] {
				if other+len(r.bytes) > charsetCap || (!ok && total+len(r.bytes) > profile.MaxRawFormBytes) {
					continue
				}
				other += len(r.bytes)
			}
			if !ok {
				total += len(r.bytes)
				at = len(patterns)
				index[string(r.bytes)] = at
				patterns = append(patterns, r.bytes)
				out.targets = append(out.targets, nil)
			}
			out.targets[at] = append(out.targets[at], rawListTarget{pat: pat, short: r.short, g0: r.g0})
			out.anyG0 = out.anyG0 || r.g0
		}
	})
	if out.refused {
		return &rawLists{refused: true}
	}
	if len(patterns) > 0 {
		out.m = textmatch.New(patterns)
	}
	return out
}

// prepareLists builds, or takes from the lists' cache, the raw automata of
// the file's lists in its received character set, before the check takes
// its memory, so the reservation counts them; it refuses a set whose forms
// pass their bound.
func (s *searcher) prepareLists() error {
	s.listRaws = s.listRaws[:0]
	for _, l := range s.set.lists {
		rl := rawListsFor(l, s.source)
		if rl.refused {
			return &Refusal{Cause: CauseTooManyNeedles}
		}
		s.listRaws = append(s.listRaws, rl)
	}
	return nil
}

// automatonNodeBytes is what a node of an automaton holds: 19 bytes
// (textmatch.Matcher), and the room its slices were made with.
const automatonNodeBytes = 24

// listsBytes is the memory of the automata the file's lists are searched
// with: the text's and the raw bytes' in its character set.
func (s *searcher) listsBytes() int64 {
	var n int64
	for i, l := range s.set.lists {
		if m := l.Matcher(); m != nil {
			n += int64(m.Nodes())
		}
		if i < len(s.listRaws) && s.listRaws[i].m != nil {
			n += int64(s.listRaws[i].m.Nodes())
		}
	}
	return n * automatonNodeBytes
}

// rawListsSearch searches the raw bytes outside the image data and the
// standard binary values for the byte forms of the lists' terms, unless a
// term was already found in the text, which holds the file whatever else
// is found.
func (s *searcher) rawListsSearch() error {
	if s.listFound {
		return nil
	}
	for i, l := range s.set.lists {
		rl := s.listRaws[i]
		if rl.m == nil {
			continue
		}
		for _, seg := range s.p.segs {
			var sh *shifter
			if rl.anyG0 {
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
				if s.p.inListsSkipped(off) {
					return
				}
				for _, t := range rl.targets[pat] {
					if (t.g0 && !sh.in(off, seg[0]+base+int64(end))) || !l.Stands(win, t.pat, start, end) || s.composesAfter(win, start, end) {
						continue
					}
					s.fs.add(Finding{Path: s.p.locate(off), Kind: KindListTerm, Raw: true})
					s.listFound = true
					return
				}
			}
			var err error
			s.buf, _, err = scanStream(s.ctx, rl.m, sectionOf(s.p.src, seg[0], seg[1]), s.window(seg[1]-seg[0]), s.buf, &s.budget, prepare, found)
			if err != nil {
				return budgetRefusal(err)
			}
		}
	}
	return nil
}
