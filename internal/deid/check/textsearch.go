package check

import (
	"context"
	"fmt"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/filereader/attributes"

	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// searcher is one check's search of one file: the text of its elements,
// one value at a time, then its raw bytes, a window at a time.
type searcher struct {
	ctx context.Context
	// quiet is ctx with the library's logging discarded, for its decoder
	// and encoder.
	quiet  context.Context
	p      *parsed
	set    formSet
	source []string
	opts   Options
	fs     *findings
	seams  seams
	budget int64
	// done marks the forms found, each reported once.
	done []bool
	// raws are the raw forms of each form (encodeRaw), rawBytes the bytes
	// of the distinct ones together and rawLongest the longest.
	raws                 [][]rawForm
	rawBytes, rawLongest int

	// The text search: one pattern per distinct folded form.
	m       *textmatch.Matcher
	targets [][]int
	// read is the text searched so far, in folded bytes and a separator
	// after each text, for the seam told where a refusal came.
	read int
	// listFound is set once a term of a list is found (lists.go); listsOff
	// while the value searched is a standard binary one, which the lists'
	// terms are not searched in.
	listFound bool
	listsOff  bool
	// listRaws are the raw automata of the file's lists in its received
	// character set, by list (prepareLists).
	listRaws []*rawLists

	// Room reused from one value, and one window, to the next.
	hay   []byte
	value []byte
	buf   []byte
}

// newSearcher returns the search of p for set.
func newSearcher(ctx, quiet context.Context, p *parsed, set formSet, source []string, opts Options, fs *findings, sm seams) *searcher {
	return &searcher{ctx: ctx, quiet: quiet, p: p, set: set, source: source, opts: opts, fs: fs, seams: sm,
		budget: workBudget, done: make([]bool, len(set.forms))}
}

// text searches the decoded text of every element, as the library decodes
// it under its data set's character set, and the preamble, for the forms,
// one text at a time: each folded (foldText) into one buffer reused, a
// value of unknown kind too long to decode under a character set read
// from the file as a stream. The automaton starts afresh at each text, as
// a text's ends are edges.
func (s *searcher) text() error {
	index := map[string]int{}
	var patterns [][]byte
	for i, f := range s.set.forms {
		at, ok := index[f.folded]
		if !ok {
			at = len(patterns)
			index[f.folded] = at
			patterns = append(patterns, []byte(f.folded))
			s.targets = append(s.targets, nil)
		}
		s.targets[at] = append(s.targets[at], i)
	}
	s.m = textmatch.New(patterns)
	defer func() { s.m = nil }()
	f := s.p.file
	if f.HasPreamble && s.p.size >= preambleLength {
		pre := make([]byte, preambleLength)
		if _, err := s.p.src.ReadAt(pre, 0); err != nil {
			return fmt.Errorf("deid check: %w", err)
		}
		if err := s.texts("preamble", s.loose(pre, nil)); err != nil {
			return err
		}
	}
	if err := s.walk(f.MetaElements, nil, "", 0); err != nil {
		return err
	}
	return s.walk(f.DataElements, nil, "", 0)
}

// walk searches the text of elements, at depth, under prefix, decoded by
// the encodings in force for their data set (PS3.3 C.12.1.1.2: an item
// without its own Specific Character Set takes its parent's). The top
// level's image data is not text, and the record of the de-identification
// the engine wrote (parsed.written) is not searched.
func (s *searcher) walk(elements []*attributes.DataElementValue, inherited []string, prefix string, depth int) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	encodings := attributes.TextEncodingsWithContext(s.quiet, elements, inherited)
	mime := documentMIME(elements)
	for _, e := range elements {
		path := prefix + fmt.Sprintf("%08X", uint32(e.Tag))
		if (pixelTags[e.Tag] && depth == 0) || s.p.written[e] {
			continue
		}
		if e.VR == "SQ" || e.Items != nil {
			for i, it := range e.Items {
				if err := s.walk(it.Elements, encodings, fmt.Sprintf("%s[%d]/", path, i), depth+1); err != nil {
					return err
				}
			}
			continue
		}
		var err error
		// A binary value is not searched for the lists' terms
		// (listsSkipped); an encapsulated document they cannot be
		// searched in holds the file when the profile has lists, fail
		// closed (listsUnsearchable).
		s.listsOff = listsSkipped(e, mime)
		if len(s.set.lists) > 0 && listsUnsearchable(e, mime) {
			s.listFound = true
			s.fs.add(Finding{Path: path, Kind: KindListUnsearchable})
		}
		switch {
		case dataelem.IsTextVR(dataelem.VR(e.VR)):
			if len(e.Value) > 0 {
				err = s.texts(path, []string{attributes.DecodeTextWithContext(s.quiet, e.VR, e.Value, encodings)})
			}
		case !looseVR(e.VR) || e.Length == 0 || e.Length > s.opts.MaxValueBytes:
			// A binary value longer than MaxValueBytes is searched for
			// in the raw bytes only.
		case !e.ValueSkipped:
			err = s.texts(path, s.loose(e.Value, encodings))
		case e.Length <= looseMax:
			if s.value, err = s.p.readValue(e, s.value); err == nil {
				err = s.texts(path, s.loose(s.value, encodings))
			}
		default:
			off, end := e.Position.ValueOffset, e.Position.ValueOffset+int64(e.Length)
			if err = s.stream(path, off, end); err == nil {
				err = s.streamDecoded(path, off, end, encodings)
			}
		}
		s.listsOff = false
		if err != nil {
			return err
		}
	}
	return nil
}

// looseMax is the longest value of unknown kind decoded whole under a
// character set: the library's decoder is slow on long binary values (some
// 90 ms a megabyte, against a few for the search) and holds the text
// whole, and a longer value is searched as UTF-8, decoded a chunk at a
// time (streamDecoded), and in its raw bytes, all the same.
const looseMax = 256 << 10

// loose returns the texts a value of unknown kind may be (PS3.5 6.2.2): as
// UTF-8, and as LO under the data set's encodings and under the character
// set the file was received in; each once. A value of ASCII without an
// escape reads the same in all of them, and one past looseMax is taken as
// UTF-8 only.
func (s *searcher) loose(value []byte, encodings []string) []string {
	out := []string{string(value)}
	if len(value) > looseMax || plainASCII(value) {
		return out
	}
	for _, enc := range [][]string{encodings, s.source} {
		if enc == nil {
			continue
		}
		t := attributes.DecodeTextWithContext(s.quiet, "LO", value, enc)
		if t != out[0] && (len(out) < 2 || t != out[1]) {
			out = append(out, t)
		}
	}
	return out
}

// plainASCII reports whether b is ASCII without the escape that begins an
// ISO 2022 code extension (PS3.5 6.1.2.5.3).
func plainASCII(b []byte) bool {
	for _, c := range b {
		if c >= 0x80 || c == 0x1B {
			return false
		}
	}
	return true
}

// texts searches each of texts, the readings of the value at path, in NFC
// and folded.
func (s *searcher) texts(path string, texts []string) error {
	for _, t := range texts {
		space := false
		t = profile.NFC(t)
		if cap(s.hay) < len(t) {
			// Sized once: grown by append, a long text's buffer would be
			// copied over and over (Go grows a large slice by a quarter),
			// holding two copies at once at each step.
			s.hay = make([]byte, 0, len(t))
		}
		s.hay = foldString(s.hay[:0], t, &space)
		at, err := s.m.Scan(s.ctx, s.hay, &s.budget, func(pat, end int) {
			s.foundText(s.hay, end-s.m.Len(pat), end, pat, path)
		})
		if err != nil {
			return s.refused(s.read+at, err)
		}
		if err := s.listsText(path, s.hay); err != nil {
			return err
		}
		s.read += len(s.hay) + 1
	}
	return nil
}

// stream searches the value at [off, end) of the file, of unknown kind and
// too long to decode under a character set, as UTF-8 in NFC, folded, a
// window at a time: the value is never held whole.
func (s *searcher) stream(path string, off, end int64) error {
	text := newFoldReader(profile.NFCReader(sectionOf(s.p.src, off, end)))
	var at int64
	var err error
	s.buf, at, err = scanStream(s.ctx, s.m, text, s.window(end-off), s.buf, &s.budget, nil, func(win []byte, pat, start, end int, _ int64) {
		s.foundText(win, start, end, pat, path)
	})
	if err != nil {
		return s.refused(s.read+int(at), err)
	}
	if err := s.listsStream(path, off, end); err != nil {
		return err
	}
	s.read += int(at) + 1
	return nil
}

// streamDecoded searches the value at [off, end) of the file, as stream
// does, in the text it is under the data set's encodings and under the set
// the file was received in (decodedPasses), decoded a chunk at a time
// (decodeReader), for the needles' forms and the lists' terms, in one pass
// a character set: the matcher with the longest pattern reads the windows,
// which keep that pattern's length of overlap, and the others search each
// window as it comes (scanStream's prepare).
func (s *searcher) streamDecoded(path string, off, end int64, encodings []string) error {
	type pass struct {
		m     *textmatch.Matcher
		found func(win []byte, pat, start, end int)
	}
	passes := []pass{{m: s.m, found: func(win []byte, pat, start, end int) { s.foundText(win, start, end, pat, path) }}}
	if !s.listsOff {
		for _, l := range s.set.lists {
			passes = append(passes, pass{m: l.Matcher(), found: func(win []byte, pat, start, end int) {
				if l.Stands(win, pat, start, end) {
					s.foundListTerm(path, false)
				}
			}})
		}
	}
	first := 0
	for i, p := range passes {
		if p.m.Longest() > passes[first].m.Longest() {
			first = i
		}
	}
	for _, enc := range decodedPasses(encodings, s.source) {
		var others error
		prepare := func(win []byte, _, from, to int, _ int64) {
			for i, p := range passes {
				if i == first || others != nil {
					continue
				}
				_, others = p.m.ScanRange(s.ctx, win, from, to, &s.budget, func(pat, end int) {
					p.found(win, pat, end-p.m.Len(pat), end)
				})
			}
		}
		// The reader gives the text in NFC and folded.
		text := newDecodeReader(s.quiet, sectionOf(s.p.src, off, end), enc)
		var at int64
		var err error
		s.buf, at, err = scanStream(s.ctx, passes[first].m, text, s.window(end-off), s.buf, &s.budget, prepare, func(win []byte, pat, start, end int, _ int64) {
			passes[first].found(win, pat, start, end)
		})
		if err == nil {
			err = others
		}
		if err != nil {
			return s.refused(s.read+int(at), err)
		}
		s.read += int(at) + 1
	}
	return nil
}

// window returns the bytes of a stream of n bytes read at a time.
func (s *searcher) window(n int64) int {
	w := int64(window)
	if s.seams.window > 0 {
		w = int64(s.seams.window)
	}
	return int(min(w, n))
}

// foundText records the forms of the pattern pat matched at
// text[start:end] whose edges fit, not found before.
func (s *searcher) foundText(text []byte, start, end, pat int, path string) {
	for _, i := range s.targets[pat] {
		if !s.done[i] && edgesFit(text, start, end, s.set.forms[i].mode) {
			s.done[i] = true
			s.fs.add(Finding{Path: path, Kind: s.set.forms[i].kind})
		}
	}
}

// refused tells the seam how far into the text the search had read when it
// stopped, and returns a pass past the budget as the refusal too_large;
// any other error as it is.
func (s *searcher) refused(at int, err error) error {
	if s.seams.refusedAt != nil {
		s.seams.refusedAt(at)
	}
	return budgetRefusal(err)
}
