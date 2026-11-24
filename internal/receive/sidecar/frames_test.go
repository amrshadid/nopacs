package sidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
)

// v1Line returns the line a version 1 sidecar holds for inst(t, u, true):
// the same fields without a frame table.
func v1Line(t *testing.T, u string) string {
	t.Helper()
	in := inst(t, u, true)
	in.Frames, in.FrameSHA256 = nil, nil
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "frames") {
		t.Fatalf("a line without a table names one: %s", b)
	}
	return string(b)
}

// v1Sidecar returns a sidecar as a version that kept no frame table wrote
// it: its header, each instance's line without a table, its footer.
func v1Sidecar(t *testing.T, uids ...string) []byte {
	t.Helper()
	lines := make([]string, len(uids))
	for i, u := range uids {
		lines[i] = v1Line(t, u)
	}
	return []byte(headerV1 + strings.Join(lines, ",\n") + "\n" + footer)
}

// filled returns the line of uid u with a frame table of two frames, the
// second of two fragments.
func filled(t *testing.T, u string) Instance {
	t.Helper()
	in := inst(t, u, true)
	in.Frames = [][]Range{{{Offset: 320, Length: 100}}, {{Offset: 428, Length: 50}, {Offset: 486, Length: 60}}}
	in.FrameSHA256 = []string{strings.Repeat("01", 32), strings.Repeat("23", 32)}
	return in
}

// TestFrameTableRoundTrips guards the frames field: a table reads back
// equal, written as [offset,length] pairs, with its frames' checksums; an empty table (frames that are
// not byte ranges) reads back empty and not absent; and a line without
// pixel data names no table.
func TestFrameTableRoundTrips(t *testing.T) {
	a := filled(t, "1.2.3")
	b := inst(t, "1.2.4", true)
	b.Frames, b.FrameSHA256 = [][]Range{}, nil
	c := inst(t, "1.2.5", false)
	var buf bytes.Buffer
	w := NewWriter(&buf)
	for _, in := range []Instance{a, b, c} {
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	text := buf.String()
	if !strings.Contains(text, `"frames":[[[320,100]],[[428,50],[486,60]]]`) || !strings.Contains(text, `"frames":[],`) ||
		strings.Count(text, `"frames"`) != 2 {
		t.Fatalf("the tables are not in their form:\n%s", text)
	}
	got := readAll(t, buf.Bytes())
	if len(got[0].Frames) != 2 || got[0].Frames[1][1] != (Range{Offset: 486, Length: 60}) {
		t.Errorf("table read back %+v", got[0].Frames)
	}
	if !slices.Equal(got[0].FrameSHA256, a.FrameSHA256) || got[1].FrameSHA256 != nil || got[2].FrameSHA256 != nil {
		t.Errorf("frame checksums read back %v, %v, %v", got[0].FrameSHA256, got[1].FrameSHA256, got[2].FrameSHA256)
	}
	if got[1].Frames == nil || len(got[1].Frames) != 0 || got[1].NeedsFrames() {
		t.Errorf("an empty table read back as %#v", got[1].Frames)
	}
	if got[2].Frames != nil || got[2].NeedsFrames() {
		t.Errorf("a line without pixel data read back a table %#v", got[2].Frames)
	}
}

// TestFrameNumbersCountFromOne guards Frame: frames are numbered from 1, a
// number outside the table is ErrNoSuchFrame, and a line without a table
// or with an empty one is ErrNoFrames.
func TestFrameNumbersCountFromOne(t *testing.T) {
	in := filled(t, "1.2.3")
	f, err := in.Frame(2)
	if err != nil || len(f) != 2 || f[0].Offset != 428 {
		t.Fatalf("frame 2: %+v, %v", f, err)
	}
	for _, n := range []int{0, 3, -1} {
		if _, err := in.Frame(n); !errors.Is(err, ErrNoSuchFrame) {
			t.Errorf("frame %d: %v, want ErrNoSuchFrame", n, err)
		}
	}
	in.Frames = nil
	if _, err := in.Frame(1); !errors.Is(err, ErrNoFrames) {
		t.Errorf("no table: %v", err)
	}
	in.Frames = [][]Range{}
	if _, err := in.Frame(1); !errors.Is(err, ErrNoFrames) {
		t.Errorf("an empty table: %v", err)
	}
}

// TestFrameTableIsChecked guards the rules of a table: a range outside the
// pixel data element, a range before the one ahead of it, a frame with no
// range, a table without pixel data, a table without one checksum of 64
// lowercase hex digits a frame, checksums beside an empty table, and a
// pair that is not two numbers are refused; a table over MaxFrameRanges is
// left empty, with its checksums, not refused.
func TestFrameTableIsChecked(t *testing.T) {
	for name, frames := range map[string][][]Range{
		"before":   {{{Offset: 299, Length: 10}}},
		"past end": {{{Offset: 900, Length: 101}}},
		"negative": {{{Offset: 400, Length: -1}}},
		"backward": {{{Offset: 500, Length: 10}}, {{Offset: 400, Length: 10}}},
		"empty":    {{}},
	} {
		in := inst(t, "1.2.3", true)
		in.Frames, in.FrameSHA256 = frames, nil
		for range frames {
			in.FrameSHA256 = append(in.FrameSHA256, strings.Repeat("ef", 32))
		}
		if err := NewWriter(io.Discard).Add(in); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v, want ErrFormat", name, err)
		}
	}
	for name, sums := range map[string][]string{
		"no checksums":       nil,
		"one too few":        {strings.Repeat("ef", 32)},
		"not hex":            {strings.Repeat("zz", 32), strings.Repeat("ef", 32)},
		"upper case":         {strings.Repeat("EF", 32), strings.Repeat("ef", 32)},
		"short":              {"ef", strings.Repeat("ef", 32)},
		"one more than kept": {strings.Repeat("ef", 32), strings.Repeat("ef", 32), strings.Repeat("ef", 32)},
	} {
		in := filled(t, "1.2.3")
		in.FrameSHA256 = sums
		if err := NewWriter(io.Discard).Add(in); !errors.Is(err, ErrFormat) {
			t.Errorf("a table with checksums %s: %v, want ErrFormat", name, err)
		}
	}
	emptyWithSums := inst(t, "1.2.3", true)
	emptyWithSums.Frames = [][]Range{}
	if err := NewWriter(io.Discard).Add(emptyWithSums); !errors.Is(err, ErrFormat) {
		t.Errorf("checksums with an empty table: %v, want ErrFormat", err)
	}
	noPixels := inst(t, "1.2.3", false)
	noPixels.Frames = [][]Range{}
	if err := NewWriter(io.Discard).Add(noPixels); !errors.Is(err, ErrFormat) {
		t.Errorf("a table without pixel data: %v", err)
	}
	var buf bytes.Buffer
	w := NewWriter(&buf)
	big := inst(t, "1.2.3", true)
	big.Frames = make([][]Range, MaxFrameRanges+1)
	for i := range big.Frames {
		big.Frames[i] = []Range{{Offset: 312 + int64(i)/200, Length: 0}}
	}
	if err := w.Add(big); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	if got := readAll(t, buf.Bytes()); got[0].Frames == nil || len(got[0].Frames) != 0 {
		t.Errorf("a table over the bound was kept as %d frames", len(got[0].Frames))
	}
	for _, bad := range []string{`[1,2,3]`, `[1]`, `null`, `{"offset":1}`, `["1",2]`} {
		var r Range
		if err := json.Unmarshal([]byte(bad), &r); err == nil {
			t.Errorf("%s was read as a range %+v", bad, r)
		}
	}
}

// TestVersionOneSidecarsStayReadable guards the sidecars written before
// frame tables: one reads back with its lines and no tables, a merge into
// it stays version 1 (the lines it keeps have none), a merge into nothing
// or into a sidecar of this version is this version, and this version's
// reader refuses a line with pixel data and no table.
func TestVersionOneSidecarsStayReadable(t *testing.T) {
	old := v1Sidecar(t, "1.1", "1.3")
	rd := NewReader(bytes.NewReader(old))
	n := 0
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if !in.NeedsFrames() {
			t.Errorf("%s has a table", in.SOPInstanceUID)
		}
		n++
	}
	if n != 2 || rd.Version() != 1 {
		t.Fatalf("read %d lines of version %d", n, rd.Version())
	}
	var merged bytes.Buffer
	if _, err := Merge(&merged, bytes.NewReader(old), List([]Instance{filled(t, "1.2")}), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(merged.String(), headerV1) || len(readAll(t, merged.Bytes())) != 3 {
		t.Fatalf("a merge into version 1 wrote:\n%s", merged.String())
	}
	var fresh bytes.Buffer
	if _, err := Merge(&fresh, nil, List([]Instance{filled(t, "1.2")}), nil); err != nil || !strings.HasPrefix(fresh.String(), header) {
		t.Fatalf("a merge into nothing: %v\n%s", err, fresh.String())
	}
	var again bytes.Buffer
	if _, err := Merge(&again, bytes.NewReader(fresh.Bytes()), List([]Instance{filled(t, "1.3")}), nil); err != nil ||
		!strings.HasPrefix(again.String(), header) {
		t.Fatalf("a merge into this version: %v\n%s", err, again.String())
	}
	if _, err := Merge(io.Discard, nil, List([]Instance{func() Instance { in := filled(t, "1.2"); in.Frames, in.FrameSHA256 = nil, nil; return in }()}), nil); !errors.Is(err, ErrIncomplete) {
		t.Errorf("a line added without a table: %v, want ErrIncomplete", err)
	}
	forged := strings.Replace(string(old), headerV1, header, 1)
	r := NewReader(strings.NewReader(forged))
	if _, _, err := r.Next(); !errors.Is(err, ErrFormat) || !errors.Is(err, ErrIncomplete) {
		t.Errorf("a line of this version without a table: %v", err)
	}
}

// TestCompleteGivesEveryLineItsTable guards the rewrite of a version 1
// sidecar: every line that lacked a table takes the one added, the lines
// without pixel data stay as they were, the result is this version, its
// index points at the lines where they now are, a second rewrite of the
// result changes no byte, and a line left without a table fails the
// rewrite.
func TestCompleteGivesEveryLineItsTable(t *testing.T) {
	const n = IndexEvery + 20
	uids := make([]string, n)
	for i := range uids {
		uids[i] = "1.2." + itoa(1000+i)
	}
	lines := make([]string, n)
	for i, u := range uids {
		if i%5 == 0 {
			b, _ := json.Marshal(inst(t, u, false))
			lines[i] = string(b)
		} else {
			lines[i] = v1Line(t, u)
		}
	}
	old := []byte(headerV1 + strings.Join(lines, ",\n") + "\n" + footer)
	var fills []Instance
	for i, u := range uids {
		if i%5 != 0 {
			fills = append(fills, filled(t, u))
		}
	}
	var out bytes.Buffer
	written, err := Complete(&out, bytes.NewReader(old), List(fills))
	if err != nil || written != n {
		t.Fatalf("complete wrote %d: %v", written, err)
	}
	got := readAll(t, out.Bytes())
	for i, in := range got {
		if in.SOPInstanceUID != uids[i] || (i%5 == 0) != (in.PixelData == nil) || in.NeedsFrames() || (i%5 != 0 && len(in.Frames) != 2) {
			t.Fatalf("line %d: %+v", i, in)
		}
	}
	entries, complete := ParseIndex(out.Bytes(), true)
	if !complete || len(entries) != 2 {
		t.Fatalf("index %v, %v", entries, complete)
	}
	for _, e := range entries {
		rd := NewReaderFrom(bytes.NewReader(out.Bytes()[e.Offset:]))
		in, _, err := rd.Next()
		if err != nil || in.SOPInstanceUID != e.UID {
			t.Errorf("the index puts %s at %d, which reads %s, %v", e.UID, e.Offset, in.SOPInstanceUID, err)
		}
	}
	var again bytes.Buffer
	if _, err := Merge(&again, bytes.NewReader(out.Bytes()), List(nil), nil); err != nil || !bytes.Equal(again.Bytes(), out.Bytes()) {
		t.Errorf("rewriting a complete sidecar changed it: %v", err)
	}
	if _, err := Complete(io.Discard, bytes.NewReader(old), List(fills[1:])); !errors.Is(err, ErrIncomplete) {
		t.Errorf("a line left without a table: %v, want ErrIncomplete", err)
	}
}
