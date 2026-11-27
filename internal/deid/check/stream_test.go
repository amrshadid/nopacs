package check

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// TestScanStreamFindsAMatchAcrossEveryEdge guards the windows of a stream
// on their own: a pattern placed at every offset of a text read in windows
// of 16 bytes, so it lies across an edge between two windows at many of
// them, is reported exactly once, at its offset, with the bytes its edges
// are judged by beside it in the window that reports it.
func TestScanStreamFindsAMatchAcrossEveryEdge(t *testing.T) {
	const needle = "NEEDLE77"
	m := textmatch.New([][]byte{[]byte(needle)})
	for at := 0; at < 120; at++ {
		text := []byte(strings.Repeat(".", at) + needle + strings.Repeat(".", 120-at))
		budget := int64(workBudget)
		var got []int64
		fits := true
		_, _, err := scanStream(context.Background(), m, bytes.NewReader(text), 16, nil, &budget, nil,
			func(win []byte, pat, start, end int, base int64) {
				got = append(got, base+int64(start))
				if !edgesFit(win, start, end, modeWord) || (at > 0 && start == 0) || (end == len(win) && int(base)+end != len(text)) {
					fits = false
				}
			})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 1 || got[0] != int64(at) || !fits {
			t.Errorf("a pattern at %d: reported at %v (edges in the window: %v)", at, got, fits)
		}
	}
}

// privateOW is the tag of a private binary value the tests put bytes in:
// searched in the raw bytes only, never decoded as text.
const privateOW = 0x00111003

// TestRawNeedleAcrossAWindowEdgeIsFound guards the raw search read a window
// at a time: an identifier in a private binary value, placed at every
// offset across two edges between windows of 64 bytes, is found in the
// raw bytes at each, as it is when the file is read in one window.
func TestRawNeedleAcrossAWindowEdgeIsFound(t *testing.T) {
	n := needle(engine.NeedleText, "MRN-778812")
	for at := 300; at < 440; at++ {
		v := []byte(strings.Repeat(".", at) + "MRN-778812" + strings.Repeat(".", 600-at))
		if len(v)%2 == 1 {
			v = append(v, '.')
		}
		p := writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OW", v})
		for _, w := range []int{64, 0} {
			c := New(Options{})
			c.seams.window = w
			rep, err := c.checkPath(t, context.Background(), p, engine.Needles{n}, nil)
			var res *Residual
			if !errors.As(err, &res) || rep.Findings[0].Path != "00111003" || !rep.Findings[0].Raw {
				t.Fatalf("an identifier at %d of a binary value, windows of %d: %v %+v", at, w, err, rep.Findings)
			}
		}
	}
}

// TestISO2022ShiftAcrossWindowsIsFollowed guards the shift state of the raw
// search across windows (PS3.5 6.1.2.5.3): a name in JIS X 0208 (ISO 2022
// IR 87), whose bytes are ASCII letters and count only inside a run the
// file shifted into by its escape sequence, is placed in a private binary
// value at every offset from 0 to 199 and read in windows of 16 bytes, so
// the escape sequence is cut by a window's edge at some offsets and the
// name lies across one at others; alone in its run, and at the end of a
// run of 28 bytes, longer than a window, so the escape that opened it is
// windows behind. It is found at every offset, in the raw bytes.
func TestISO2022ShiftAcrossWindowsIsFollowed(t *testing.T) {
	n := needle(engine.NeedleName, "山田^XYZW")
	for _, text := range []string{"山田", "患者患者患者患者患者患者山田"} {
		run := mustEncode(t, text, "", "ISO 2022 IR 87")
		if run[0] != 0x1B || !bytes.Contains(run, []byte(";3ED")) {
			t.Fatalf("%s written as % X", text, run)
		}
		for at := range 200 {
			v := append([]byte(strings.Repeat(".", at)), run...)
			v = append(v, strings.Repeat(".", 40)...)
			if len(v)%2 == 1 {
				v = append(v, '.')
			}
			p := writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OW", v})
			c := New(Options{})
			c.seams.window = 16
			cs := `\ISO 2022 IR 87`
			rep, err := c.checkPath(t, context.Background(), p, engine.Needles{n}, &cs)
			var res *Residual
			if !errors.As(err, &res) || rep.Findings[0].Path != "00111003" || !rep.Findings[0].Raw {
				t.Fatalf("%s at %d of a binary value, windows of 16: %v %+v", text, at, err, rep.Findings)
			}
		}
	}
}

// TestTextNeedleAcrossAWindowEdgeIsFound guards the text of a long value of
// unknown kind, read as a stream of UTF-8 a window at a time: a name
// written with a combining mark, placed at every offset across edges
// between windows of 64 bytes deep in a private UN value longer than the
// values decoded whole, is found in its text (not its raw bytes) at each.
func TestTextNeedleAcrossAWindowEdgeIsFound(t *testing.T) {
	n := needle(engine.NeedleName, "MÜLLER^HANS")
	const head = looseMax + 1000
	for at := head; at < head+140; at += 3 {
		v := []byte(strings.Repeat(".", at) + "seen by Müller." + strings.Repeat(".", 700))
		if len(v)%2 == 1 {
			v = append(v, '.')
		}
		p := writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{0x00111002, "UN", v})
		c := New(Options{})
		c.seams.window = 64
		rep, err := c.checkPath(t, context.Background(), p, engine.Needles{n}, nil)
		var res *Residual
		if !errors.As(err, &res) || rep.Findings[0].Path != "00111002" || rep.Findings[0].Raw {
			t.Fatalf("a name at %d of a long value: %v %+v", at, err, rep.Findings)
		}
	}
}

// largeFile writes a file of about size bytes: the base elements, a
// private binary value searched in the raw bytes (all but 8 MiB of it) and
// 8 MiB of Pixel Data, written a MiB at a time so the test holds neither.
func largeFile(t *testing.T, size int64) string {
	t.Helper()
	p := writeFile(t)
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	const pixels = 8 << 20
	chunk := make([]byte, 1<<20)
	for i := range chunk {
		chunk[i] = byte(i * 7 % 251)
	}
	header := func(group, elem uint16, vr string, n uint32) []byte {
		b := binary.LittleEndian.AppendUint16(nil, group)
		b = binary.LittleEndian.AppendUint16(b, elem)
		b = append(b, vr...)
		if vr == "LO" {
			return binary.LittleEndian.AppendUint16(b, uint16(n)) //nolint:gosec // G115: a short value of the test.
		}
		return binary.LittleEndian.AppendUint32(append(b, 0, 0), n)
	}
	write := func(b []byte) {
		if _, err := f.Write(b); err != nil {
			t.Fatal(err)
		}
	}
	values := func(n int64) {
		for ; n > 0; n -= int64(len(chunk)) {
			write(chunk[:min(n, int64(len(chunk)))])
		}
	}
	write(append(header(0x0011, 0x0010, "LO", 8), "ACME 1.0"...))
	binaryBytes := (size - pixels) &^ 1
	write(header(0x0011, 0x1003, "OW", uint32(binaryBytes))) //nolint:gosec // G115: at most 128 MiB.
	values(binaryBytes)
	write(header(0x7FE0, 0x0010, "OW", pixels))
	values(pixels)
	return p
}

// TestCheckMemoryIsFlat guards that the check holds a window of the file,
// never the file: checking a 128 MiB file allocates no more than checking
// a 16 MiB one (within a MiB) and less than 8 MiB in all, and the
// reservation it takes from the server's bound is the same for both and
// about a window (the file used to be read whole, and that much taken).
func TestCheckMemoryIsFlat(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a file of 128 MiB")
	}
	n := needle(engine.NeedleText, "MRN-778812")
	measure := func(size int64) (alloc uint64, reserved int64) {
		p := largeFile(t, size)
		b := NewBudget(func(context.Context) (int64, error) { return 1 << 30, nil }, 0)
		c := New(Options{Memory: b})
		c.seams.afterParse = func() { reserved = b.Used() }
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		start := time.Now()
		if _, err := c.checkPath(t, context.Background(), p, engine.Needles{n}, nil); err != nil {
			t.Fatalf("%d bytes: %v", size, err)
		}
		took := time.Since(start)
		runtime.ReadMemStats(&after)
		alloc = after.TotalAlloc - before.TotalAlloc
		t.Logf("a file of %d MiB: %.2f MiB allocated, %.2f MiB reserved, %v", size>>20,
			float64(alloc)/(1<<20), float64(reserved)/(1<<20), took)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
		return alloc, reserved
	}
	small, smallReserved := measure(16 << 20)
	large, largeReserved := measure(128 << 20)
	if large > 8<<20 || large > small+1<<20 {
		t.Errorf("checking 128 MiB allocated %d bytes, 16 MiB %d: not flat", large, small)
	}
	if largeReserved != smallReserved || largeReserved > 2<<20 {
		t.Errorf("reserved %d bytes for 128 MiB and %d for 16 MiB, want one window for both", largeReserved, smallReserved)
	}
	if got := Reservation(context.Background(), 128<<20, 4<<10, engine.Needles{n}, nil); got > 8<<20 {
		t.Errorf("Reservation(128 MiB) = %d", got)
	}
}
