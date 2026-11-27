package check

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/amrshadid/go-dicom/charset"
	"github.com/amrshadid/go-dicom/filereader/attributes"
	"golang.org/x/text/unicode/norm"

	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// decodeChunk is the raw bytes a decodeReader reads at a time. A chunk
// that holds no place to cut it (lastCut: only in an encoding whose
// characters' widths are not known here, knownWidths) grows, doubling, up
// to looseMax, the longest value read whole: the reader holds at most
// that, and its text, beside the window (streamRoom).
const decodeChunk = 32 << 10

// decodeReader reads a long value of unknown kind as the text it is under
// a character set, decoded a chunk at a time and folded as foldText folds
// it, so a value too long to decode whole is still read as its sender
// wrote it: a word of a single-byte set written in its own letter case (a
// Greek name in Title Case in ISO 8859-7, which no byte form of the word
// in one case matches), a word an ISO 2022 escape sequence splits, a word
// of two scripts across a shift, a word of a set written without spaces.
//
// It reads a value as the library's decoder does (charset.DecodeBytes): a
// value is fragments, each from an escape sequence to the next (the bytes
// before the first are the first), a fragment of a known escape in that
// escape's encoding and any other in the character set's first one (its
// base), and a delimiter (LF, CR, TAB, FF, PS3.5 6.1.3) returning the rest
// of a fragment to the base. A chunk is cut only where a character of the
// encoding in force ends (lastCut, charWidth): never inside a character of
// two or four bytes, and so never misreading the rest; and the next chunk
// is read in the state the value was in there, the fragment's escape
// sequence written before it. A fragment of a single-byte encoding (ISO
// 8859, TIS 620) is decoded through its table (byteTable), a byte at a
// time; any other through the library's decoder. Its text comes out in
// NFC and folded.
type decodeReader struct {
	ctx       context.Context
	src       io.Reader
	encodings []string
	// base is the encoding a value begins in and a delimiter returns to.
	base string
	// raw is the room raw bytes are read into; carry of them the bytes
	// after the last cut, decoded with the next chunk.
	raw   []byte
	carry int
	// st is the state where the bytes not decoded yet begin.
	st decodeState
	// out is the folded text not read yet, in dec; space is set when the
	// text given last ended in white space.
	out, dec []byte
	space    bool
	// text is decoded text not yet in NFC and folded (flush).
	text   []byte
	tables map[string]*byteTable
	eof    bool
	err    error
}

// decodeState is where a value's reading stands: in a fragment of a known
// escape sequence (esc, of encoding enc) not returned to the base by a
// delimiter, or in the base (esc nil, or reset).
type decodeState struct {
	esc   []byte
	enc   string
	reset bool
}

// encoding returns the encoding in force in st, base the value's.
func (st decodeState) encoding(base string) string {
	if st.esc == nil || st.reset {
		return base
	}
	return st.enc
}

// newDecodeReader returns the folded text of src decoded under encodings,
// the library's names (sourceEncodings), with ctx its logging discarded.
func newDecodeReader(ctx context.Context, src io.Reader, encodings []string) *decodeReader {
	base := charset.DefaultEncoding
	if len(encodings) > 0 {
		base = encodings[0]
	}
	return &decodeReader{ctx: ctx, src: src, encodings: encodings, base: base, raw: make([]byte, decodeChunk)}
}

// Read gives the folded text.
func (d *decodeReader) Read(p []byte) (int, error) {
	for len(d.out) == 0 {
		if d.err != nil {
			return 0, d.err
		}
		if d.eof && d.carry == 0 {
			return 0, io.EOF
		}
		d.fill()
	}
	n := copy(p, d.out)
	d.out = d.out[n:]
	return n, nil
}

// fill reads a chunk, cuts it at the last place a character ends and
// decodes what comes before the cut. A chunk with no such place grows,
// up to looseMax, past which it is cut at its end.
func (d *decodeReader) fill() {
	n := d.carry
	for n < len(d.raw) && !d.eof {
		m, err := d.src.Read(d.raw[n:])
		n += m
		if errors.Is(err, io.EOF) {
			d.eof = true
		} else if err != nil {
			d.err = err
			return
		}
	}
	cut := n
	if !d.eof {
		cut = d.lastCut(d.raw[:n])
		if cut == 0 {
			if len(d.raw) < looseMax {
				grown := make([]byte, min(2*len(d.raw), looseMax))
				d.carry = copy(grown, d.raw[:n])
				d.raw = grown
				return
			}
			cut = n
		}
	}
	d.dec = d.dec[:0]
	d.decodePiece(d.raw[:cut])
	d.flush(d.eof && cut == n)
	d.out = d.dec
	d.carry = copy(d.raw, d.raw[cut:n])
}

// maxHeld is the most decoded bytes held back with no NFC boundary in
// them: a run of combining marks that long is normalized as it is, as
// NFC itself bounds a run it composes (UAX #15, the stream-safe format).
const maxHeld = 4 << 10

// flush puts the decoded text gathered (text) in NFC and folds it into
// dec, but for the part after its last NFC boundary, held back for the
// next chunk (norm.Form.LastBoundary): a chunk may end between a letter
// and the combining mark that follows it ("Rene" and U+0301 in GB 18030),
// which NFC composes only read together. The value's last chunk (final)
// is flushed whole.
func (d *decodeReader) flush(final bool) {
	b := len(d.text)
	if !final {
		b = norm.NFC.LastBoundary(d.text)
		if b < 0 || len(d.text)-b > maxHeld {
			b = len(d.text)
		}
	}
	if b > 0 {
		d.dec = foldString(d.dec, profile.NFC(string(d.text[:b])), &d.space)
	}
	d.text = d.text[:copy(d.text, d.text[b:])]
}

// lastCut returns the last offset of chunk, past 0, where it may be cut:
// before an escape sequence, which begins a fragment read on its own, or
// where a character of the encoding in force ends (charWidth). It is 0
// when there is none: an encoding whose characters' widths are not known
// here, with no escape and no byte of 0x20 or less to end at.
func (d *decodeReader) lastCut(chunk []byte) int {
	st := d.st
	good := 0
	for i := 0; i < len(chunk); {
		b := chunk[i]
		if b == 0x1B {
			if i > 0 {
				good = i
			}
			if len(chunk)-i < 4 {
				// The escape may go on past the chunk.
				break
			}
			if esc, enc := knownEscape(chunk[i:]); esc != nil {
				st = decodeState{esc: esc, enc: enc}
				i += len(esc)
			} else {
				st = decodeState{}
				i++
			}
			continue
		}
		enc := st.encoding(d.base)
		if singleByte(enc) {
			// Every byte ends a character: the run goes to the next escape,
			// or to a delimiter that returns it to the base.
			end := len(chunk)
			if j := bytes.IndexByte(chunk[i:], 0x1B); j >= 0 {
				end = i + j
			}
			if st.esc != nil && !st.reset {
				if j := bytes.IndexAny(chunk[i:end], "\n\r\t\f"); j >= 0 {
					st.reset = true
					i += j + 1
					good = i
					continue
				}
			}
			i, good = end, end
			continue
		}
		if delimiter(b) {
			// A character of its own in every set (PS3.5 6.1.3).
			st.reset = true
			i++
			good = i
			continue
		}
		w := charWidth(enc, st.esc, chunk[i:])
		switch {
		case w < 0:
			// A character the chunk does not hold whole.
			return good
		case w == 0:
			i++
			if b <= 0x20 {
				good = i
			}
		default:
			i += w
			good = i
		}
	}
	return good
}

// decodePiece appends the folded text of piece, read from d.st, to d.dec
// and leaves d.st where piece ends.
func (d *decodeReader) decodePiece(piece []byte) {
	for len(piece) > 0 {
		next := len(piece)
		if i := bytes.IndexByte(piece[1:], 0x1B); i >= 0 {
			next = i + 1
		}
		frag := piece[:next]
		piece = piece[next:]
		if frag[0] == 0x1B {
			if esc, enc := knownEscape(frag); esc != nil {
				// Copied: raw's bytes are moved at the cut.
				d.st = decodeState{esc: bytes.Clone(esc), enc: enc}
				frag = frag[len(esc):]
			} else {
				// An escape the library does not know: its fragment, the
				// escape's bytes too, is read in the base.
				d.st = decodeState{}
			}
		}
		d.decodeRun(frag)
	}
}

// decodeRun appends the folded text of data, the bytes of one fragment
// without its escape sequence, read in d.st: in the fragment's encoding up
// to the first delimiter, and in the base after it.
func (d *decodeReader) decodeRun(data []byte) {
	if d.st.esc == nil || d.st.reset {
		d.decodeAs(d.base, nil, data)
		return
	}
	p := firstDelimiter(d.st.enc, d.st.esc, data)
	if p < 0 {
		d.decodeAs(d.st.enc, d.st.esc, data)
		return
	}
	d.decodeAs(d.st.enc, d.st.esc, data[:p])
	d.st.reset = true
	d.text = append(d.text, data[p])
	d.decodeAs(d.base, nil, data[p+1:])
}

// decodeAs adds the text of data in encoding enc, after its escape
// sequence esc when it has one: through enc's table, or the library's
// decoder, which reads esc as the start of a fragment. The text is
// gathered (text) for flush; a table whose characters compose with nothing
// before them (byteTable.nfc) folds its bytes straight into dec, after the
// text gathered, all but its last, which stays gathered for a combining
// mark that may follow it in another fragment.
func (d *decodeReader) decodeAs(enc string, esc, data []byte) {
	if len(data) == 0 {
		return
	}
	if t := d.tableOf(enc); t != nil {
		if t.nfc {
			// The table's first character composes with nothing before
			// it, so the text gathered ends at a boundary.
			d.flush(true)
			last := data[len(data)-1]
			data = data[:len(data)-1]
			for _, c := range data {
				f := &t.fold[c]
				switch {
				case f.ignorable:
				case f.space:
					if !d.space {
						d.dec = append(d.dec, ' ')
					}
					d.space = true
				default:
					d.space = false
					d.dec = append(d.dec, f.folded...)
				}
			}
			d.text = t.appendText(d.text, last)
			return
		}
		for _, c := range data {
			d.text = t.appendText(d.text, c)
		}
		return
	}
	if esc != nil {
		d.text = append(d.text, attributes.DecodeTextWithContext(d.ctx, "LO", append(append([]byte(nil), esc...), data...), d.encodings)...)
	} else {
		d.text = append(d.text, attributes.DecodeTextWithContext(d.ctx, "LO", data, []string{enc})...)
	}
}

// appendText appends the text of byte c to dst.
func (t *byteTable) appendText(dst []byte, c byte) []byte {
	if c < 0x80 {
		return append(dst, c)
	}
	return append(dst, t.text[c-0x80]...)
}

// tableOf returns enc's byteTable, remembered for the value: a value of
// random bytes holds an escape every few hundred bytes, each beginning a
// fragment.
func (d *decodeReader) tableOf(enc string) *byteTable {
	if t, ok := d.tables[enc]; ok {
		return t
	}
	if d.tables == nil {
		d.tables = map[string]*byteTable{}
	}
	t := byteTableFor(d.ctx, enc)
	d.tables[enc] = t
	return t
}

// knownEscape returns the escape sequence b begins with, of three bytes or
// of four, and its encoding, as the library knows them; nil when it knows
// none.
func knownEscape(b []byte) ([]byte, string) {
	for _, n := range []int{3, 4} {
		if len(b) >= n {
			if enc := charset.EscapeToGoEncoding(b[:n]); enc != "" {
				return b[:n], enc
			}
		}
	}
	return nil, ""
}

// delimiter reports whether b returns a fragment to the base: LF, CR, TAB
// or FF (PS3.5 6.1.3), each a character of its own in every set.
func delimiter(b byte) bool { return b == '\n' || b == '\r' || b == '\t' || b == '\f' }

// firstDelimiter returns the offset of the first delimiter in data, of
// encoding enc after escape esc, that stands as a character of its own;
// -1 when there is none.
func firstDelimiter(enc string, esc, data []byte) int {
	for i := 0; i < len(data); {
		if delimiter(data[i]) {
			return i
		}
		w := charWidth(enc, esc, data[i:])
		if w <= 0 {
			w = 1
		}
		i += w
	}
	return -1
}

// charWidth returns the bytes of the character b begins with in encoding
// enc, entered by escape esc, as the library's decoder reads it: -1 when b
// does not hold it whole, 0 when the widths of enc's characters are not
// known here. A byte of 0x20 or less is a character of its own in every
// set DICOM names, but in a run of JIS X 0208 or 0212.
func charWidth(enc string, esc, b []byte) int {
	c := b[0]
	need := func(n int) int {
		if len(b) < n {
			return -1
		}
		return n
	}
	if enc == "ISO-2022-JP" && len(esc) > 1 && esc[1] == '$' {
		// JIS X 0208 and 0212, entered by ESC $ B, ESC $ @ or ESC $ ( D,
		// are two bytes of 0x21 to 0x7E (PS3.5 6.1.2.5.3). The decoder
		// (x/text's ISO-2022-JP) reads every byte below 0x80 but LF as
		// half of a pair, a space or a control too, and a byte from 0x80
		// alone.
		if c >= 0x80 || c == '\n' {
			return 1
		}
		return need(2)
	}
	if c <= 0x20 {
		return 1
	}
	switch enc {
	case "ISO-2022-JP":
		// ASCII, JIS X 0201 Roman or katakana: a byte a character.
		return 1
	case utf8Encoding:
		if !utf8.FullRune(b) {
			return -1
		}
		_, n := utf8.DecodeRune(b)
		return n
	case "EUC-KR", "GBK":
		if c >= 0x81 && c <= 0xFE {
			return need(2)
		}
		return 1
	case "GB18030", "ISO-2022-CN":
		// GB 18030: a lead byte, and a second of 0x30 to 0x39 for a
		// character of four bytes, else of two.
		if c >= 0x81 && c <= 0xFE {
			if len(b) < 2 {
				return -1
			}
			if b[1] >= 0x30 && b[1] <= 0x39 {
				return need(4)
			}
			return 2
		}
		return 1
	case "Shift_JIS":
		if (c >= 0x81 && c <= 0x9F) || (c >= 0xE0 && c <= 0xFC) {
			return need(2)
		}
		return 1
	case "EUC-JP":
		switch {
		case c == 0x8F:
			return need(3)
		case c == 0x8E || (c >= 0xA1 && c <= 0xFE):
			return need(2)
		}
		return 1
	}
	if singleByte(enc) {
		return 1
	}
	return 0
}

// knownWidths reports whether charWidth knows the widths of enc's
// characters, so a chunk of it is always cut where a character ends.
func knownWidths(enc string) bool {
	return charWidth(enc, nil, []byte{'A'}) != 0 && charWidth(enc, []byte("\x1b$B"), []byte{'A', 'A'}) != 0
}

// singleByte reports whether enc is a set of one byte a character the
// library decodes byte by byte: an ISO 8859 part (ISO 8859-11 read as
// TIS 620).
func singleByte(enc string) bool { return strings.HasPrefix(enc, "ISO-8859-") }

// byteTables holds byteTableFor's answers, by encoding.
var byteTables sync.Map

// byteTableFor returns, for a single-byte encoding (singleByte), the text
// of each byte past 0x7F as the library decodes it, so a fragment of it is
// decoded a byte at a time, with no allocation, as fast as it is read: the
// library's decoder takes some 90 ms a megabyte and allocates its text
// several times. nil for any other encoding. It is judged once per
// encoding: the 128 bytes decoded together must read as the 128 decoded
// one at a time, each as one character.
func byteTableFor(ctx context.Context, enc string) *byteTable {
	if !singleByte(enc) {
		return nil
	}
	if v, ok := byteTables.Load(enc); ok {
		return v.(*byteTable) //nolint:forcetypeassert // only *byteTable is stored.
	}
	encodings := []string{enc}
	table := &byteTable{nfc: true}
	all := make([]byte, 0, 128)
	var joined strings.Builder
	for i := range 128 {
		b := byte(0x80 + i)
		all = append(all, b)
		c := attributes.DecodeTextWithContext(ctx, "LO", []byte{b}, encodings)
		if utf8.RuneCountInString(c) != 1 {
			table = nil
			break
		}
		table.text[i] = c
		p := norm.NFC.PropertiesString(c)
		table.nfc = table.nfc && norm.NFC.IsNormalString(c) && p.BoundaryBefore()
		joined.WriteString(c)
	}
	if table != nil && attributes.DecodeTextWithContext(ctx, "LO", all, encodings) != joined.String() {
		table = nil
	}
	if table != nil {
		table.setFolds()
	}
	v, _ := byteTables.LoadOrStore(enc, table)
	return v.(*byteTable) //nolint:forcetypeassert // only *byteTable is stored.
}

// byteTable is the text of each byte past 0x7F of a single-byte set.
type byteTable struct {
	text [128]string
	// nfc is set when each of those characters is in NFC and combines
	// with nothing before it (UAX #15: a boundary before it; a combining
	// mark has none). ASCII never combines with what is before it either,
	// so no two characters of the set's text compose and its text is in
	// NFC as decoded, each byte folded on its own (fold), as foldInto
	// folds a text in NFC.
	nfc  bool
	fold [256]foldedByte
}

// foldedByte is a byte's character folded as profile.AppendFolded folds
// it: white space (one space for a run of it), ignorable (nothing), or
// its folded bytes.
type foldedByte struct {
	space, ignorable bool
	folded           []byte
}

// setFolds fills t.fold from the characters of the bytes.
func (t *byteTable) setFolds() {
	for b := range 256 {
		r := rune(b)
		if b >= 0x80 {
			r, _ = utf8.DecodeRuneInString(t.text[b-0x80])
		}
		t.fold[b] = foldedByte{space: unicode.IsSpace(r), ignorable: profile.Ignorable(r), folded: utf8.AppendRune(nil, profile.Fold(r))}
	}
}

// decodedPasses returns the character sets a long value of unknown kind is
// read in beside UTF-8: the data set's encodings and the set the file was
// received in, each once, and neither when it is UTF-8 alone.
func decodedPasses(encodings, source []string) [][]string {
	var out [][]string
	for _, enc := range [][]string{encodings, source} {
		if len(enc) == 0 || (len(enc) == 1 && enc[0] == utf8Encoding) {
			continue
		}
		if len(out) == 1 && sameStrings(out[0], enc) {
			continue
		}
		out = append(out, enc)
	}
	return out
}

// sameStrings reports whether a and b hold the same strings in order.
func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
