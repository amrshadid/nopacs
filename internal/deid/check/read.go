package check

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader/attributes"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// pixelTags are the elements whose value is image data, never searched:
// Float Pixel Data, Double Float Pixel Data and Pixel Data (PS3.3
// C.7.6.3). Their bytes can hold any sequence of characters by chance.
var pixelTags = map[tag.Tag]bool{0x7FE00008: true, 0x7FE00009: true, 0x7FE00010: true}

// preambleLength is the length of the preamble of a Part 10 file (PS3.10
// 7.1).
const preambleLength = 128

// heldBinary is the longest value of a binary VR (or of no VR) the reader
// of the elements holds: a longer one stays in the file, and one that may
// be text (VR UN, OB, or none) is read from the file when its text is
// searched, one value at a time. It is the header reader's bound, so the
// elements held are about the header the engine held.
const heldBinary = 1024

// parsed is a file's elements, read for the search with their values in
// place: the image data and every binary value past heldBinary are left in
// the file.
type parsed struct {
	src  io.ReaderAt
	size int64
	file *attributes.File
	// segs are the ranges of the file outside the image data, in order.
	segs [][2]int64
	// binary are the ranges of the values of standard elements of a binary
	// VR (a LUT, an overlay, an encapsulated document), in order, where a
	// short raw form is not taken.
	binary [][2]int64
	// skipped are the ranges of the values the lists' terms are not
	// searched in (listsSkipped), in order.
	skipped [][2]int64
	// nestedPixels is the path of the first element of group 7FE0 inside
	// a sequence item, or "": de-identification keeps pixel data at the
	// top level only, so one found deeper is an image no pixel rule
	// filled and no search reads (a residual, whatever it holds).
	nestedPixels string
	// largestText is the longest text value the text search decodes, in
	// bytes of UTF-8 at most (textSpread), and largestLoose the longest
	// value of unknown kind it reads whole (at most looseMax), in bytes
	// read: what the check holds of one value.
	largestText, largestLoose int64
	// streams is set when a value of unknown kind is read as a stream.
	streams bool
	// written are the elements of the top level that hold the record of
	// the de-identification exactly as the engine wrote it in this run
	// (writtenElements): their text is not searched, nor the raw bytes of
	// the values compared (writtenValues).
	written map[*attributes.DataElementValue]bool
}

// parse reads the elements of the size bytes of src with the codec-free
// reader: text values within MaxValueBytes held, binary values past
// heldBinary and the image data left in the file. It refuses a file the
// reader did not read whole or whose sequences are not well formed. The
// elements of the top level that hold exactly what written says the engine
// wrote (writtenElements) are kept out of both searches.
func (c *Checker) parse(src io.ReaderAt, size int64, written engine.Written) (*parsed, error) {
	fr := newFileReader(src, size)
	f, err := attributes.Read(filebase.NewFileReader(fr), attributes.ReadOptions{
		SkipBulkData:      true,
		BulkDataThreshold: heldBinary,
		MaxValueLength:    c.opts.MaxValueBytes,
		Logger:            slog.New(slog.DiscardHandler),
	})
	if fr.err != nil {
		// The source failed, which says nothing of the file.
		return nil, fmt.Errorf("deid check: %w", fr.err)
	}
	if err != nil {
		if errors.Is(err, attributes.ErrValueTooLong) {
			return nil, &Refusal{Cause: CauseTooLarge}
		}
		return nil, &Refusal{Cause: CauseUnreadable}
	}
	if f.Deflated {
		// The positions of a deflated data set's elements count in the
		// inflated bytes, and its raw bytes on disk are compressed: what
		// a parser misses could not be searched for.
		return nil, &Refusal{Cause: CauseUnreadable}
	}
	if fr.size != size || !wholeRead(f, size) || !wellFormed(f.MetaElements) || !wellFormed(f.DataElements) {
		return nil, &Refusal{Cause: CauseUnreadable}
	}
	p := &parsed{src: src, size: size, file: f}
	var cut [][2]int64
	if err := p.survey(f.MetaElements, "", 0, &cut, c.opts.MaxValueBytes); err != nil {
		return nil, err
	}
	if err := p.survey(f.DataElements, "", 0, &cut, c.opts.MaxValueBytes); err != nil {
		return nil, err
	}
	p.written = writtenElements(f.DataElements, written)
	cut = append(cut, writtenValues(p.written)...)
	p.segs = outside(cut, size)
	sort.Slice(p.binary, func(i, j int) bool { return p.binary[i][0] < p.binary[j][0] })
	sort.Slice(p.skipped, func(i, j int) bool { return p.skipped[i][0] < p.skipped[j][0] })
	return p, nil
}

// survey records, for elements at depth under prefix, the ranges of image
// data to cut from the raw search, the ranges of standard binary values,
// the first pixel data nested in an item, and the longest values the text
// search will hold.
func (p *parsed) survey(elements []*attributes.DataElementValue, prefix string, depth int, cut *[][2]int64, maxValue uint32) error {
	if depth > maxDepth {
		return &Refusal{Cause: CauseTooLarge}
	}
	for _, e := range elements {
		pos := e.Position
		if depth > 0 && e.Tag.Group() == 0x7FE0 && p.nestedPixels == "" {
			p.nestedPixels = prefix + fmt.Sprintf("%08X", uint32(e.Tag))
		}
		if pixelTags[e.Tag] && depth == 0 {
			if pos.End > pos.ValueOffset {
				*cut = append(*cut, [2]int64{pos.ValueOffset, pos.End})
			}
			continue
		}
		if e.VR == "SQ" || e.Items != nil {
			for i, it := range e.Items {
				path := prefix + fmt.Sprintf("%08X[%d]/", uint32(e.Tag), i)
				if err := p.survey(it.Elements, path, depth+1, cut, maxValue); err != nil {
					return err
				}
			}
			continue
		}
		text := dataelem.IsTextVR(dataelem.VR(e.VR))
		if listsSkipped(e) && pos.End > pos.ValueOffset {
			p.skipped = append(p.skipped, [2]int64{pos.ValueOffset, pos.End})
		}
		if standardBinary(e) && pos.End > pos.ValueOffset {
			p.binary = append(p.binary, [2]int64{pos.ValueOffset, pos.End})
		}
		switch {
		case text:
			// Counted as the text the search holds: decoded into UTF-8, up
			// to textSpread bytes a byte that is not ASCII.
			n := int64(len(e.Value))
			if !plainASCII(e.Value) {
				n *= textSpread
			}
			p.largestText = max(p.largestText, n)
		case looseVR(e.VR) && e.Length <= maxValue && e.Length > looseMax:
			p.streams = true
		case looseVR(e.VR) && e.Length <= looseMax:
			p.largestLoose = max(p.largestLoose, int64(e.Length))
		}
	}
	return nil
}

// looseVR reports whether a value of VR vr may be text the file did not
// say was text (PS3.5 6.2.2): UN, OB, or no VR.
func looseVR(vr string) bool { return vr == "UN" || vr == "OB" || vr == "" }

// wholeRead reports whether the reader read the file to its end. The
// reader keeps what it read of a truncated file, and of one whose
// sequences nest past its maximum, and drops the rest; an element dropped
// so would not be searched as text, so such a file is not judged.
func wholeRead(f *attributes.File, size int64) bool {
	end := f.DataSetOffset
	if n := len(f.DataElements); n > 0 {
		end = f.DataElements[n-1].Position.End
	}
	return end == size
}

// wellFormed reports whether every sequence of elements, at any depth, was
// read whole (PS3.5 7.5): a sequence or item of undefined length ends with
// its delimitation item; an item of defined length ends where its length
// says, at the end of its last element; a sequence of defined length ends
// with its last item. The reader keeps the items before one cut short and
// says so only in words, so a file whose last item was dropped would
// otherwise pass with that item's text never searched.
func wellFormed(elements []*attributes.DataElementValue) bool {
	for _, e := range elements {
		if e.Items == nil && e.VR != "SQ" {
			continue
		}
		pos := e.Position
		if e.UndefinedLength {
			if pos.DelimiterOffset == 0 || pos.End != pos.DelimiterOffset+8 {
				return false
			}
		} else if n := len(e.Items); n > 0 && e.Items[n-1].End != pos.ValueOffset+int64(e.Length) {
			return false
		}
		for _, it := range e.Items {
			end := it.End
			if it.DeclaredLength == attributes.UndefinedLength {
				if it.DelimiterOffset == 0 || it.End != it.DelimiterOffset+8 {
					return false
				}
				end = it.DelimiterOffset
			} else if it.End != it.Offset+8+int64(it.DeclaredLength) {
				return false
			}
			last := it.Offset + 8
			if n := len(it.Elements); n > 0 {
				last = it.Elements[n-1].Position.End
			}
			if last != end || !wellFormed(it.Elements) {
				return false
			}
		}
	}
	return true
}

// outside returns the ranges of [0, n) outside cut.
func outside(cut [][2]int64, n int64) [][2]int64 {
	sort.Slice(cut, func(i, j int) bool { return cut[i][0] < cut[j][0] })
	var out [][2]int64
	var at int64
	for _, r := range cut {
		if r[0] > at {
			out = append(out, [2]int64{at, min(r[0], n)})
		}
		at = max(at, r[1])
	}
	if at < n {
		out = append(out, [2]int64{at, n})
	}
	return out
}

// inBinary reports whether offset is in the value of a standard binary
// element.
func (p *parsed) inBinary(off int64) bool {
	i := sort.Search(len(p.binary), func(i int) bool { return p.binary[i][1] > off })
	return i < len(p.binary) && p.binary[i][0] <= off
}

// inListsSkipped reports whether offset is in a value the lists' terms are
// not searched in (listsSkipped).
func (p *parsed) inListsSkipped(off int64) bool {
	i := sort.Search(len(p.skipped), func(i int) bool { return p.skipped[i][1] > off })
	return i < len(p.skipped) && p.skipped[i][0] <= off
}

// locate returns the path of the innermost element whose bytes hold
// offset: "preamble" in the first 128 bytes of a file that has one, "file"
// between elements.
func (p *parsed) locate(off int64) string {
	if p.file.HasPreamble && off < preambleLength {
		return "preamble"
	}
	if path, ok := locateIn(p.file.MetaElements, off, ""); ok {
		return path
	}
	if path, ok := locateIn(p.file.DataElements, off, ""); ok {
		return path
	}
	return "file"
}

// locateIn finds the element of elements, at any depth, whose bytes hold
// off.
func locateIn(elements []*attributes.DataElementValue, off int64, prefix string) (string, bool) {
	for _, e := range elements {
		pos := e.Position
		if off < pos.Offset || off >= pos.End {
			continue
		}
		path := prefix + fmt.Sprintf("%08X", uint32(e.Tag))
		for i, it := range e.Items {
			if inner, ok := locateIn(it.Elements, off, fmt.Sprintf("%s[%d]/", path, i)); ok {
				return inner, true
			}
		}
		return path, true
	}
	return "", false
}

// shifter follows a stream of raw bytes, one byte at a time, for the runs
// a file wrote in a 7-bit set of two-byte characters (PS3.5 6.1.2.5.3: JIS
// X 0208 after ESC $ B or ESC $ @, JIS X 0212 after ESC $ ( D): from the
// byte after the escape sequence to the first byte that is not a character
// of such a set (0x21 to 0x7E), the next escape among them. Its state
// carries from one window to the next, so a run longer than a window is
// still one run from its start.
type shifter struct {
	// esc is how much of an escape sequence was read: 1 after ESC, 2
	// after ESC $, 3 after ESC $ ( or ESC $ ).
	esc int
	// start is the offset of the run in progress, or -1.
	start int64
	// runs are the runs that ended in the bytes fed since the last reset,
	// and the run in progress, to the last byte fed.
	runs [][2]int64
}

// newShifter returns a shifter at the start of a stream.
func newShifter() *shifter { return &shifter{start: -1} }

// feed follows b, whose first byte is at offset base, and sets runs to
// the runs that ended in b and the run in progress at its end.
func (s *shifter) feed(b []byte, base int64) {
	s.runs = s.runs[:0]
	for i, c := range b {
		off := base + int64(i)
		if s.start >= 0 {
			if c >= 0x21 && c <= 0x7E {
				continue
			}
			if off > s.start {
				s.runs = append(s.runs, [2]int64{s.start, off})
			}
			s.start = -1
		}
		switch {
		case s.esc == 2 && (c == '(' || c == ')'):
			s.esc = 3
		case (s.esc == 2 || s.esc == 3) && c >= 0x40 && c <= 0x7E:
			s.esc, s.start = 0, off+1
		case s.esc == 1 && c == '$':
			s.esc = 2
		case c == 0x1B:
			s.esc = 1
		default:
			s.esc = 0
		}
	}
	if end := base + int64(len(b)); s.start >= 0 && end > s.start {
		s.runs = append(s.runs, [2]int64{s.start, end})
	}
}

// in reports whether [off, end) lies in one run, at an even distance from
// its start, so a match falls on whole two-byte characters.
func (s *shifter) in(off, end int64) bool {
	i := sort.Search(len(s.runs), func(i int) bool { return s.runs[i][1] > off })
	return i < len(s.runs) && s.runs[i][0] <= off && end <= s.runs[i][1] && (off-s.runs[i][0])%2 == 0
}

// readValue reads the value of e from the file into buf, grown when it is
// too small, and returns it.
func (p *parsed) readValue(e *attributes.DataElementValue, buf []byte) ([]byte, error) {
	n := int(e.Length)
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	buf = buf[:n]
	if _, err := io.ReadFull(sectionOf(p.src, e.Position.ValueOffset, e.Position.ValueOffset+int64(n)), buf); err != nil {
		return nil, err
	}
	return buf, nil
}
