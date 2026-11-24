package sidecar

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
)

// inst returns an instance of the scan s/e/c with UID u.
func inst(t *testing.T, u string, withPixels bool) Instance {
	t.Helper()
	k, err := layout.InstanceKey(label.Path{Subject: "s", Session: "e", Scan: "c"}, u)
	if err != nil {
		t.Fatal(err)
	}
	in := Instance{SOPInstanceUID: u, Key: k, Size: 1000, SHA256: strings.Repeat("ab", 32),
		Dataset: json.RawMessage(`{"00080018":{"vr":"UI","Value":["` + u + `"]}}`)}
	if withPixels {
		in.PixelData = &PixelData{Offset: 300, Length: 700}
		in.Frames = [][]Range{{{Offset: 312, Length: 688}}}
		in.FrameSHA256 = []string{strings.Repeat("cd", 32)}
	}
	return in
}

// readAll reads every instance of a sidecar.
func readAll(t *testing.T, b []byte) []Instance {
	t.Helper()
	r := NewReader(bytes.NewReader(b))
	var out []Instance
	for {
		in, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, in)
	}
}

// TestWriteAndReadBack guards the format: what is written reads back the
// same, in order, and the bytes are the one form.
func TestWriteAndReadBack(t *testing.T) {
	var b bytes.Buffer
	w := NewWriter(&b)
	for _, u := range []string{"1.2.3", "1.2.4", "1.2.5"} {
		if err := w.Add(inst(t, u, u != "1.2.4")); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got := readAll(t, b.Bytes())
	if len(got) != 3 || got[1].PixelData != nil || got[0].PixelData.Offset != 300 || got[2].SOPInstanceUID != "1.2.5" {
		t.Fatalf("read back %+v", got)
	}
	if !strings.HasPrefix(b.String(), header) || !strings.HasSuffix(b.String(), "\n"+footer) || strings.Count(b.String(), "\n") != 5 {
		t.Fatalf("not the one form:\n%s", b.String())
	}
}

// TestOrderAndBounds guards that an instance out of order is refused, and a
// data set over the bound is omitted, not kept and not refused.
func TestOrderAndBounds(t *testing.T) {
	w := NewWriter(io.Discard)
	if err := w.Add(inst(t, "1.2.5", false)); err != nil {
		t.Fatal(err)
	}
	if err := w.Add(inst(t, "1.2.4", false)); !errors.Is(err, ErrOrder) {
		t.Fatalf("out of order gave %v", err)
	}
	var b bytes.Buffer
	w = NewWriter(&b)
	big := inst(t, "1.2.3", false)
	big.Dataset = json.RawMessage(`"` + strings.Repeat("x", MaxDataset) + `"`)
	if err := w.Add(big); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	got := readAll(t, b.Bytes())
	if len(got) != 1 || got[0].Dataset != nil || !got[0].DatasetOmitted {
		t.Fatalf("an over-long data set was kept: %+v", got)
	}
	if err := NewWriter(io.Discard).Close(); err == nil {
		t.Fatal("an empty sidecar was written")
	}
}

// TestMergeReplacesAndDrops guards a late arrival's merge: the old
// instances stay in order, an added one replaces the old one under its UID,
// and a dropped one is left out.
func TestMergeReplacesAndDrops(t *testing.T) {
	var old bytes.Buffer
	w := NewWriter(&old)
	for _, u := range []string{"1.1", "1.3", "1.5"} {
		_ = w.Add(inst(t, u, false))
	}
	_ = w.Close()
	var out bytes.Buffer
	n, err := Merge(&out, bytes.NewReader(old.Bytes()), List([]Instance{inst(t, "1.2", true), inst(t, "1.3", true), inst(t, "1.6", true)}),
		func(u string) bool { return u == "1.5" })
	if err != nil {
		t.Fatal(err)
	}
	got := readAll(t, out.Bytes())
	var uids []string
	for _, in := range got {
		uids = append(uids, in.SOPInstanceUID)
	}
	if n != 4 || strings.Join(uids, " ") != "1.1 1.2 1.3 1.6" || got[2].PixelData == nil {
		t.Fatalf("merged %d: %v", n, uids)
	}
	n, err = Merge(io.Discard, nil, List([]Instance{inst(t, "1.1", false)}), nil)
	if err != nil || n != 1 {
		t.Fatalf("a merge into nothing gave %d, %v", n, err)
	}
}

// TestReaderRefusesOtherForms guards that a damaged sidecar is refused, not
// half read.
func TestReaderRefusesOtherForms(t *testing.T) {
	var good bytes.Buffer
	w := NewWriter(&good)
	_ = w.Add(inst(t, "1.1", false))
	_ = w.Close()
	for name, b := range map[string]string{
		"cut":     good.String()[:good.Len()-4],
		"version": strings.Replace(good.String(), `"version":2`, `"version":4`, 1),
		"field":   strings.Replace(good.String(), `"size":`, `"other":1,"size":`, 1),
		"after":   good.String() + "x",
	} {
		r := NewReader(strings.NewReader(b))
		var err error
		for err == nil {
			_, _, err = r.Next()
		}
		if !errors.Is(err, ErrFormat) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// TestTransferSyntaxRoundTrips guards the optional transfer_syntax field: a
// line that holds it reads back with it, a line written without it (an older
// sidecar) reads back with it empty and unchanged, and a value that is not a
// UID is refused.
func TestTransferSyntaxRoundTrips(t *testing.T) {
	with := inst(t, "1.2.3", false)
	with.TransferSyntax = "1.2.840.10008.1.2.1.99"
	without := inst(t, "1.2.4", true)
	var b bytes.Buffer
	w := NewWriter(&b)
	for _, in := range []Instance{with, without} {
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Count(b.String(), "transfer_syntax") != 1 {
		t.Errorf("the field is written for the line that has it only:\n%s", b.String())
	}
	got := readAll(t, b.Bytes())
	if got[0].TransferSyntax != with.TransferSyntax || got[1].TransferSyntax != "" {
		t.Errorf("transfer syntaxes %q and %q, want %q and none", got[0].TransferSyntax, got[1].TransferSyntax, with.TransferSyntax)
	}
	bad := inst(t, "1.2.5", false)
	bad.TransferSyntax = "not a uid"
	if err := NewWriter(&bytes.Buffer{}).Add(bad); !errors.Is(err, ErrFormat) {
		t.Errorf("a transfer syntax that is not a UID: %v, want ErrFormat", err)
	}
}

// TestIndexLocatesALine guards the index a large sidecar ends with: a
// sidecar of more than IndexEvery instances still reads whole, a reader
// started at the offset Locate gives for an instance reads that instance's
// line first or an earlier one of the same stretch and then the rest in
// order, a merge rewrites the index for what it holds, and a small sidecar
// has none.
func TestIndexLocatesALine(t *testing.T) {
	const n = 3*IndexEvery + 10
	uidOf := func(i int) string { return "1.2." + itoa(1000000+i) }
	var b bytes.Buffer
	w := NewWriter(&b)
	for i := 0; i < n; i++ {
		if err := w.Add(inst(t, uidOf(i), i%2 == 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, b.Bytes()); len(got) != n {
		t.Fatalf("%d instances read back, want %d", len(got), n)
	}
	for _, whole := range []bool{false, true} {
		tail := b.Bytes()[b.Len()-100:]
		if whole {
			tail = b.Bytes()
		}
		entries, complete := ParseIndex(tail, whole)
		if whole && (!complete || len(entries) != 4) {
			t.Fatalf("whole file: %d entries, complete %v, want 4 and true", len(entries), complete)
		}
		if !whole && complete {
			t.Fatalf("a tail inside the index line was taken as complete")
		}
	}
	entries, complete := ParseIndex(b.Bytes()[b.Len()-2000:], false)
	if !complete || len(entries) != 4 {
		t.Fatalf("%d entries, complete %v, want 4 and true", len(entries), complete)
	}
	for _, i := range []int{0, 1, IndexEvery, IndexEvery + 5, 3*IndexEvery + 9} {
		off := Locate(entries, uidOf(i))
		rd := NewReaderFrom(bytes.NewReader(b.Bytes()[off:]))
		first, _, err := rd.Next()
		if err != nil || first.SOPInstanceUID > uidOf(i) || i-IndexEvery >= 0 && first.SOPInstanceUID < uidOf(i-IndexEvery) {
			t.Fatalf("instance %d: started at %s, %v", i, first.SOPInstanceUID, err)
		}
		last, count := first.SOPInstanceUID, 1
		for {
			in, _, err := rd.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil || in.SOPInstanceUID <= last {
				t.Fatalf("instance %d: %v after %s", i, err, last)
			}
			last, count = in.SOPInstanceUID, count+1
		}
		if last != uidOf(n-1) {
			t.Errorf("instance %d: ended at %s, want %s", i, last, uidOf(n-1))
		}
	}
	var merged bytes.Buffer
	if _, err := Merge(&merged, bytes.NewReader(b.Bytes()), func() (Instance, bool, error) { return Instance{}, false, nil },
		func(u string) bool { return u == uidOf(0) }); err != nil {
		t.Fatal(err)
	}
	if got := readAll(t, merged.Bytes()); len(got) != n-1 {
		t.Fatalf("merge kept %d instances, want %d", len(got), n-1)
	}
	es, ok := ParseIndex(merged.Bytes(), true)
	if !ok || len(es) != 4 || es[0].UID != uidOf(1) {
		t.Errorf("the merged index is %v (complete %v), want 4 entries from %s", es, ok, uidOf(1))
	}
	var small bytes.Buffer
	sw := NewWriter(&small)
	if err := sw.Add(inst(t, "1.2.3", false)); err != nil {
		t.Fatal(err)
	}
	if err := sw.Close(); err != nil {
		t.Fatal(err)
	}
	if es, ok := ParseIndex(small.Bytes(), true); !ok || es != nil || strings.Contains(small.String(), `"index"`) {
		t.Errorf("a small sidecar has an index: %v", es)
	}
}

// itoa writes a small number.
func itoa(i int) string { return strconv.Itoa(i) }
