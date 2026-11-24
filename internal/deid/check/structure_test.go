package check

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader/attributes"
	"github.com/amrshadid/go-dicom/filewriter"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// readBytes reads the file at path.
func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// checkBytes checks b, given as size bytes, for needles.
func checkBytes(t *testing.T, opts Options, b []byte, size int64, needles ...engine.Needle) (Report, error) {
	t.Helper()
	return New(opts).Check(context.Background(), bytes.NewReader(b), size, needles, nil)
}

// wantRefusal fails unless err is a refusal of cause.
func wantRefusal(t *testing.T, err error, cause Cause) {
	t.Helper()
	var r *Refusal
	if !errors.As(err, &r) || r.Cause != cause {
		t.Fatalf("want a refusal %s, got %v", cause, err)
	}
}

// explicitLE writes one element in Explicit VR Little Endian (PS3.5 7.1.2)
// with a 2-byte length, the form of LO and SH.
func explicitLE(group, elem uint16, vr string, value string) []byte {
	b := binary.LittleEndian.AppendUint16(nil, group)
	b = binary.LittleEndian.AppendUint16(b, elem)
	b = append(b, vr[0], vr[1])
	b = binary.LittleEndian.AppendUint16(b, uint16(len(value))) //nolint:gosec // G115: the test's values are a few bytes.
	return append(b, value...)
}

// undefinedSequence writes (0040,0275) as a sequence of undefined length
// whose items are of undefined length (PS3.5 7.5.2), each closed by its
// delimitation item and the sequence by its own.
func undefinedSequence(items ...[]byte) []byte {
	b := []byte{0x40, 0x00, 0x75, 0x02, 'S', 'Q', 0, 0, 0xFF, 0xFF, 0xFF, 0xFF}
	for _, it := range items {
		b = append(b, 0xFE, 0xFF, 0x00, 0xE0, 0xFF, 0xFF, 0xFF, 0xFF)
		b = append(b, it...)
		b = append(b, 0xFE, 0xFF, 0x0D, 0xE0, 0, 0, 0, 0)
	}
	b = append(b, 0xFE, 0xFF, 0xDD, 0xE0, 0, 0, 0, 0)
	return b
}

// TestLastItemCutShortIsRefused guards the structure of sequences: a file
// cut right after the header of an element in the last item of a
// sequence, which the reader reads as a sequence of the items before it
// with no error, is refused, for a sequence and items of undefined length
// and for ones of defined length; the whole file finds the name in that
// item.
func TestLastItemCutShortIsRefused(t *testing.T) {
	n := needle(engine.NeedleName, "王^XY")
	last := explicitLE(0x0040, 0x0009, "SH", "ABCDEFGHIJ")
	seq := undefinedSequence(explicitLE(0x0040, 0x0007, "LO", "first "),
		append(explicitLE(0x0040, 0x0007, "LO", "王^小明 "), last...))
	head := readBytes(t, writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}))
	whole := append(append([]byte{}, head...), seq...)
	at := bytes.Index(whole, last) + 8
	t.Run("undefined length", func(t *testing.T) {
		if _, err := checkBytes(t, Options{}, whole, int64(len(whole)), n); err == nil {
			t.Fatal("the whole file passed")
		}
		cut := whole[:at]
		f, err := attributes.Read(filebase.NewFileReader(bytes.NewReader(cut)), attributes.ReadOptions{})
		if err != nil {
			t.Fatalf("the reader refused the cut file itself (%v): this test no longer shows a silent cut", err)
		}
		if s := f.DataElements[len(f.DataElements)-1]; s.Position.End != int64(len(cut)) {
			t.Fatalf("the cut file does not end with its last element: %d of %d", s.Position.End, len(cut))
		}
		_, err = checkBytes(t, Options{}, cut, int64(len(cut)), n)
		wantRefusal(t, err, CauseUnreadable)
	})
	t.Run("defined length", func(t *testing.T) {
		items := []*dataset.Dataset{
			build(t, el{0x00400007, "LO", "first"}),
			build(t, el{0x00400007, "LO", "王^小明"}, el{0x00400009, "SH", "ABCDEFGHIJ"}),
		}
		b := readBytes(t, writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}, el{0x00400275, "SQ", items}))
		if _, err := checkBytes(t, Options{}, b, int64(len(b)), n); err == nil {
			t.Fatal("the whole file passed")
		}
		cut := b[:bytes.Index(b, []byte("ABCDEFGHIJ"))]
		_, err := checkBytes(t, Options{}, cut, int64(len(cut)), n)
		wantRefusal(t, err, CauseUnreadable)
	})
}

// TestDeflatedIsRefused guards that a file in the Deflated Explicit VR
// Little Endian transfer syntax (PS3.5 A.5), whose raw bytes are
// compressed, is refused rather than searched in part, even when its
// inflated data set reads to the end of the file: here the file is padded
// to the length the inflated elements' positions reach, and its text holds
// the needle, so only the refusal keeps it from being judged.
func TestDeflatedIsRefused(t *testing.T) {
	// A long run of one letter deflates to a few bytes, so the inflated
	// data set is longer than the file.
	p := writeDS(t, build(t, base(el{0x00204000, "LT", "MRN-778812"}, el{0x00204001, "LT", strings.Repeat("x", 8000)})...),
		filewriter.FileMetaInfo{TransferSyntaxUID: "1.2.840.10008.1.2.1.99"})
	b := readBytes(t, p)
	if bytes.Contains(b, []byte("MRN-778812")) {
		t.Fatal("the file was not deflated")
	}
	f, err := attributes.Read(filebase.NewFileReader(bytes.NewReader(b)), attributes.ReadOptions{})
	if err != nil || !f.Deflated {
		t.Fatalf("read: %v", err)
	}
	end := f.DataElements[len(f.DataElements)-1].Position.End
	if end <= int64(len(b)) {
		t.Fatalf("the inflated data set ends at %d, within the file's %d bytes", end, len(b))
	}
	b = append(b, make([]byte, end-int64(len(b)))...)
	_, err = checkBytes(t, Options{}, b, int64(len(b)), needle(engine.NeedleText, "MRN-778812"))
	wantRefusal(t, err, CauseUnreadable)
}

// TestShortSourceIsRefused guards that a source holding fewer bytes than
// the size given is refused, not searched as the bytes it has.
func TestShortSourceIsRefused(t *testing.T) {
	b := readBytes(t, writeFile(t))
	_, err := checkBytes(t, Options{}, b, int64(len(b))+16, needle(engine.NeedleText, "MRN-778812"))
	wantRefusal(t, err, CauseUnreadable)
}

// TestPanicIsRefused guards that a panic while a file is checked is a
// refusal, never a pass and never a crash.
func TestPanicIsRefused(t *testing.T) {
	b := readBytes(t, writeFile(t))
	c := New(Options{})
	c.seams.afterParse = func() { panic("a fault of the reader") }
	_, err := c.Check(context.Background(), bytes.NewReader(b), int64(len(b)), engine.Needles{needle(engine.NeedleText, "MRN-778812")}, nil)
	wantRefusal(t, err, CauseUnreadable)
}

// TestDenseMatchesPassTheBudget guards the bound on the search's work: a
// run of one letter against forms of every length ends on as many forms
// at each byte, and is refused as too large instead of searched for long.
func TestDenseMatchesPassTheBudget(t *testing.T) {
	var needles engine.Needles
	for i := 4; i < 600; i++ {
		needles = append(needles, needle(engine.NeedleText, strings.Repeat("A", i)))
	}
	p := writeFile(t, el{0x00204000, "UT", strings.Repeat("A", 200000)})
	_, err := run(t, p, needles...)
	wantRefusal(t, err, CauseTooLarge)
}

// TestLongNeedleOutsideTheSourceIsQuick guards the raw forms of a needle
// the received character sets cannot write (a rune cut in two, read as
// U+FFFD), under a list of sets: the library's encoder is only ever asked
// to write in one set, and only text each of whose runes that set writes,
// so its search of every prefix of a text a list of sets cannot write,
// which takes the square of the length (a name of 64 KB took minutes, one
// of 8 KB as here a second), is never reached; and the runes checked are
// charged to the budget.
func TestLongNeedleOutsideTheSourceIsQuick(t *testing.T) {
	long := strings.Repeat("A", 8<<10) + "\xe7\x8e" + "^B"
	p := writeFile(t)
	c := New(Options{})
	calls, lists := 0, 0
	var spent int64
	c.seams.library = func(encs []string) {
		calls++
		if len(encs) != 1 {
			lists++
		}
	}
	c.seams.work = func(n int64) { spent = n }
	cs := `\ISO 2022 IR 87`
	if _, err := c.checkPath(t, context.Background(), p, engine.Needles{needle(engine.NeedleName, long)}, &cs); err != nil {
		t.Fatalf("got %v", err)
	}
	if calls == 0 || lists != 0 {
		t.Errorf("%d calls to the encoder, %d with a list of sets", calls, lists)
	}
	if spent < int64(len(long)) {
		t.Errorf("%d units spent for a needle of %d runes", spent, len(long))
	}
}

// TestFormsPastTheirBoundAreRefused guards the bound on the bytes of the
// forms searched for, which bounds the automata's memory: needles whose
// forms pass MaxFormBytes are refused, never searched, before the file is
// read (so an unreadable file is refused for the needles); and forms within
// the bound as text that pass it with their raw forms are refused too.
func TestFormsPastTheirBoundAreRefused(t *testing.T) {
	var needles engine.Needles
	for i := range 40 {
		needles = append(needles, needle(engine.NeedleText, fmt.Sprintf("%02d", i)+strings.Repeat("X", 60<<10)))
	}
	_, err := checkBytes(t, Options{}, []byte("not a DICOM file"), 16, needles...)
	wantRefusal(t, err, CauseTooManyNeedles)
	_, err = run(t, writeFile(t), needles[:20]...)
	wantRefusal(t, err, CauseTooManyNeedles)
}

// TestDenseMatchesAreRefusedQuickly guards that the budget of a dense file
// does not grow with its size: a run of one letter against forms of every
// length is refused at the same point of its text whether the run is 1 MB
// or 16 MB long, well before the end of the shorter.
func TestDenseMatchesAreRefusedQuickly(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a 16 MB file")
	}
	var needles engine.Needles
	for i := 4; i < 600; i++ {
		needles = append(needles, needle(engine.NeedleText, strings.Repeat("A", i)))
	}
	refusedAt := func(size int) int {
		p := writeFile(t, el{0x00204000, "UT", strings.Repeat("A", size)})
		c := New(Options{MaxValueBytes: 32 << 20})
		at := -1
		c.seams.refusedAt = func(n int) { at = n }
		_, err := c.checkPath(t, context.Background(), p, needles, nil)
		wantRefusal(t, err, CauseTooLarge)
		return at
	}
	small, large := refusedAt(1<<20), refusedAt(16<<20)
	if small < 0 || small != large || small > 1<<19 {
		t.Errorf("refused at %d of 1 MB and %d of 16 MB", small, large)
	}
}
