package check

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/config"
	"github.com/amrshadid/go-dicom/filereader/attributes"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// longValue is a value past looseMax, read as a stream: filler, then text,
// padded to an even length.
func longValue(text string) []byte {
	b := []byte(strings.Repeat("ab ", 100_000) + "by " + text + " ")
	if len(b)%2 == 1 {
		b = append(b, ' ')
	}
	return b
}

// TestLongValuesAreReadInTheirCharacterSet guards the search of a value of
// unknown kind too long to decode whole (past looseMax): it is decoded a
// chunk at a time under the character set, with the shift state carried
// from chunk to chunk, and searched as the text of a shorter value is, for
// the lists' terms and for the values the profile removed (as a name and
// as an identifier). Each case is a word no byte form in one letter case
// matches: a Greek name in Title Case in ISO 8859-7; a name an escape
// sequence splits in ISO 2022 IR 100; a name of Latin and kanji across a
// shift between two code extensions; two names in 300 KB of kanji with no
// space or escape after its first ESC $ B, at some 33 KiB and 200 KiB,
// past many chunks, which no chunk may be cut inside a character of; two
// names in GB 18030, an odd byte first, across the places a chunk of
// 32 KiB would be cut at its end; and a Greek name after an escape in
// a value of ISO 8859-1, a set of one value, read as the library reads it.
func TestLongValuesAreReadInTheirCharacterSet(t *testing.T) {
	// 亜 in JIS X 0208, with 山田Anne at 33 KiB and 鈴木Anne at 200 KiB, a
	// name in Title Case beside each, which only the decoded text holds:
	// no raw form of the term, in one letter case, matches "Anne".
	kanji := []byte("\x1b$B" + strings.Repeat("\x30\x21", 33<<9) + "\x3b\x33\x45\x44\x1b(BAnne\x1b$B" +
		strings.Repeat("\x30\x21", 167<<9) + "\x4e\x6b\x4c\x5a\x1b(BAnne\x1b$B" + strings.Repeat("\x30\x21", 50<<10) + "\x1b(B")
	// 啊 in GB 18030 after an odd byte, with 张伟Éé at byte 32767 and
	// 王芳Éé at byte 196607, each across a multiple of 32 KiB, where a
	// chunk cut at its end would split the character (the decoder finds
	// its way back after one bad character, so only a term the cut splits
	// is lost). "Éé", a capital and a small letter outside ASCII, is in no
	// raw form of the term, each of one letter case.
	gb := []byte("x" + strings.Repeat("\xb0\xa1", 16383) + "\xd5\xc5\xce\xb0\x81\x30\x87\x37\xa8\xa6" +
		strings.Repeat("\xb0\xa1", 81915) + "\xcd\xf5\xb7\xbc\x81\x30\x87\x37\xa8\xa6" + strings.Repeat("\xb0\xa1", 50<<10) + " ")
	for _, c := range []struct {
		name, term, name2, cs, written string
		value                          []byte
	}{
		{"Greek in Title Case", "Αλέξανδρος", "Αλέξανδρος^Νίκος", "ISO_IR 126", "\xc1\xeb\xdd\xee\xe1\xed\xe4\xf1\xef\xf2", nil},
		{"an escape inside a word", "Müllerstein", "Müllerstein^Ann", `\ISO 2022 IR 100`, "M\x1b-A\xfcllerstein", nil},
		{"a shift inside a word", "Müller山田", "Müller山田^Ann", `ISO 2022 IR 100\ISO 2022 IR 87`, "M\x1b-A\xfcller\x1b$B\x3b\x33\x45\x44\x1b(B", nil},
		{"kanji at 33 KiB", "山田Anne", "山田Anne", `\ISO 2022 IR 87`, "", kanji},
		{"kanji at 200 KiB", "鈴木Anne", "鈴木Anne", `\ISO 2022 IR 87`, "", kanji},
		{"GB 18030 at 32 KiB", "张伟Éé", "张伟Éé", "GB18030", "", gb},
		{"GB 18030 at 192 KiB", "王芳Éé", "王芳Éé", "GB18030", "", gb},
		{"an escape in a set of one value", "Αλέξανδρος", "Αλέξανδρος^Νίκος", "ISO_IR 100", "\x1b-F\xc1\xeb\xdd\xee\xe1\xed\xe4\xf1\xef\xf2", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{c.term}}})
			if problems != nil {
				t.Fatal(problems)
			}
			cs := c.cs
			v := c.value
			if v == nil {
				v = longValue(c.written)
			}
			path := writeFile(t, el{0x00080005, "CS", cs}, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OB", v})
			for _, n := range []engine.Needle{
				{Kind: engine.NeedleList, Lists: l},
				{Kind: engine.NeedleName, Value: c.name2, Path: "00100010"},
				{Kind: engine.NeedleText, Value: c.term, Path: "00100020"},
			} {
				_, err := New(Options{}).checkPath(t, context.Background(), path, engine.Needles{n}, &cs)
				var res *Residual
				if !errors.As(err, &res) || res.First.Path != "00111003" {
					t.Errorf("%v %q in a long value of %s: %v", n.Kind, c.term, cs, err)
				}
			}
		})
	}
}

// TestLongValueWithoutTheWordPasses guards against what reading a long
// value in chunks could invent: the same values with the word changed by a
// letter, and a word that only ends where a chunk is cut, pass.
func TestLongValueWithoutTheWordPasses(t *testing.T) {
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{"Αλέξανδρος", "Annabel"}}})
	if problems != nil {
		t.Fatal(problems)
	}
	cs := "ISO_IR 126"
	// "Annabelle" with its "le" past a chunk's cut: no space for a chunk
	// to end at, so the cut falls inside the word, which the window across
	// both chunks still reads whole.
	run := strings.Repeat("x", decodeChunk-7) + "Annabelle " + strings.Repeat("y", 64)
	for _, v := range [][]byte{
		longValue("\xc1\xeb\xdd\xee\xe5\xed\xe4\xf1\xef\xf2"),
		[]byte(run + string(longValue("nothing"))),
	} {
		if len(v)%2 == 1 {
			v = append(v, ' ')
		}
		path := writeFile(t, el{0x00080005, "CS", cs}, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OB", v})
		if _, err := New(Options{}).checkPath(t, context.Background(), path, engine.Needles{{Kind: engine.NeedleList, Lists: l}}, &cs); err != nil {
			t.Errorf("a long value without a term: %v", err)
		}
	}
}

// TestDecodeReaderReadsAsTheDecoderDoes guards decodeReader against the
// library's decoder of a whole value, in NFC and folded, each many chunks
// long: text of two code extensions switched every few words; ASCII;
// ISO 8859-7 and ISO 8859-1 with every byte, read through the table of a
// single-byte set; GB 18030 with spaces, and without any (an odd byte
// first, so a cut at a chunk's end would fall inside a character); kanji
// with no space or escape for 300 KB after its first ESC $ B; kanji
// returned to the base by a line feed; and an escape inside a value of a
// set of one value, which the library honors. A reader that fails is
// told apart from the value's end.
func TestDecodeReaderReadsAsTheDecoderDoes(t *testing.T) {
	ctx := config.WithLogger(context.Background(), slog.New(slog.DiscardHandler))
	encodings := func(cs string) []string { return sourceEncodings(&cs) }
	var mixed, every, gb, lines bytes.Buffer
	for i := range 20_000 {
		mixed.WriteString("word M\x1b-A\xfcller ")
		if i%3 == 0 {
			mixed.WriteString("\x1b$B\x3b\x33\x45\x44\x1b(B ")
		}
		every.WriteByte(byte(0x20 + i%224))
		gb.WriteString("\xc9\xbd\xcc\xef ")
		lines.WriteString("\x1b$B\x3b\x33\x45\x44\nab \xe9 ")
	}
	cases := []struct {
		cs   string
		text []byte
	}{
		{`ISO 2022 IR 100\ISO 2022 IR 87`, mixed.Bytes()},
		{"ISO_IR 100", []byte(strings.Repeat("plain words ", 20_000))},
		{"ISO_IR 126", every.Bytes()},
		{"ISO_IR 100", every.Bytes()},
		{"GB18030", gb.Bytes()},
		{"GB18030", []byte("x" + strings.Repeat("\xb0\xa1\x81\x30\x81\x30", 50_000))},
		{`\ISO 2022 IR 87`, []byte("\x1b$B" + strings.Repeat("\x30\x21", 150_000) + "\x1b(B")},
		{`\ISO 2022 IR 87`, lines.Bytes()},
		{"ISO_IR 100", []byte(strings.Repeat("ab ", 20_000) + "\x1b-F\xc1\xeb\xdd\xee\xe1\xed\xe4\xf1\xef\xf2 " + strings.Repeat("\xe9 ", 20_000))},
		{"ISO_IR 100", bytes.Repeat([]byte("x\xe9"), 100_000)},
	}
	for _, c := range cases {
		enc := encodings(c.cs)
		space := false
		want := string(foldString(nil, profile.NFC(attributes.DecodeTextWithContext(ctx, "LO", c.text, enc)), &space))
		got, err := io.ReadAll(newDecodeReader(ctx, bytes.NewReader(c.text), enc))
		if err != nil || string(got) != want {
			at := 0
			for at < min(len(got), len(want)) && got[at] == want[at] {
				at++
			}
			t.Errorf("%s: decoded %d bytes (%v), want %d; first difference at %d", c.cs, len(got), err, len(want), at)
		}
	}
	if byteTableFor(ctx, "ISO-8859-7") == nil || byteTableFor(ctx, "GB18030") != nil || byteTableFor(ctx, "ISO-2022-JP") != nil {
		t.Error("a single-byte set has no table, or a multi-byte set has one")
	}
	// An encoding whose characters' widths are not known here (HZ, read
	// by the library for GB2312): a chunk with no byte to end at grows to
	// the next space, at 100 KB, and then to looseMax, past which it is
	// cut at its end; the text is the decoder's all the same.
	hz := []byte(strings.Repeat("~{<V~}", 100_000/6) + " " + strings.Repeat("ab", 200_000))
	r := newDecodeReader(ctx, bytes.NewReader(hz), []string{"GB2312"})
	space := false
	want := string(foldString(nil, profile.NFC(attributes.DecodeTextWithContext(ctx, "LO", hz, []string{"GB2312"})), &space))
	if got, err := io.ReadAll(r); err != nil || string(got) != want || len(r.raw) != looseMax {
		t.Errorf("an encoding of unknown widths: %d bytes (%v), want %d; room %d", len(got), err, len(want), len(r.raw))
	}
	// Every encoding the library reads a DICOM character set in has known
	// widths, so no chunk grows and the reservation holds no room for one.
	if chunksGrow() || knownWidths("GB2312") || !knownWidths("ISO-8859-11") || !knownWidths("ISO-2022-JP") {
		t.Errorf("chunks grow for a DICOM character set: %v", chunksGrow())
	}
	b := mixed
	enc := encodings(`ISO 2022 IR 100\ISO 2022 IR 87`)
	broken := errors.New("disk")
	if _, err := io.ReadAll(newDecodeReader(ctx, io.MultiReader(bytes.NewReader(b.Bytes()[:100_000]), errReader{broken}), enc)); !errors.Is(err, broken) {
		t.Errorf("a failing source: %v", err)
	}
}

// TestCombiningMarkAcrossAChunkEdge guards the reading of a long value in
// chunks against a term written decomposed: "Rene" and U+0301 in GB 18030
// (81 30 BC 37), which NFC composes into "René" only when the letter and
// the mark are read together, placed at every offset from 8 bytes before
// to 8 after the ends of the first and second chunks, is found as a term
// of a list and as a removed name.
func TestCombiningMarkAcrossAChunkEdge(t *testing.T) {
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{"René"}}})
	if problems != nil {
		t.Fatal(problems)
	}
	cs := "GB18030"
	for _, edge := range []int{decodeChunk, 2 * decodeChunk} {
		for at := edge - 8; at <= edge+8; at++ {
			v := []byte(strings.Repeat("a", at-1) + " Rene\x81\x30\xbc\x37 " + strings.Repeat("b ", 150_000))
			if len(v)%2 == 1 {
				v = append(v, ' ')
			}
			path := writeFile(t, el{0x00080005, "CS", cs}, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OB", v})
			for _, n := range []engine.Needle{
				{Kind: engine.NeedleList, Lists: l},
				{Kind: engine.NeedleName, Value: "René", Path: "00100010"},
			} {
				_, err := New(Options{}).checkPath(t, context.Background(), path, engine.Needles{n}, &cs)
				var res *Residual
				if !errors.As(err, &res) || res.First.Path != "00111003" {
					t.Errorf("%v at byte %d: %v", n.Kind, at, err)
				}
			}
		}
	}
}

// errReader fails every read with err.
type errReader struct{ err error }

// Read fails.
func (e errReader) Read([]byte) (int, error) { return 0, e.err }
