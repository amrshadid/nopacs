package sidecar

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/tenant"
)

var (
	t1 = time.Date(2026, 11, 20, 8, 30, 0, 123456000, time.UTC)
	t2 = t1.Add(3 * time.Hour)
)

// head returns a valid scan header.
func head() ScanHeader {
	n := int32(3)
	return ScanHeader{StudyUID: "1.2.826.0.1", SeriesUID: "1.2.826.0.1.2", SessionReceiver: "scanner-a", Receiver: "scanner-b",
		Route:  &Route{Name: "trial", Profile: "research@3", ReplacesForwardTo: true, ForwardTo: []string{"archive"}},
		Values: Values{StudyDate: "20240131", Modality: "MR", SeriesDescription: "T1 axial", SeriesNumber: &n}, Created: t1, Updated: t2}
}

// v3inst returns an instance of the scan s/e/c with every field version 3
// adds.
func v3inst(t *testing.T, u string, withPixels bool) Instance {
	t.Helper()
	in := inst(t, u, withPixels)
	in.Reception, in.HeldAt = "rcp-"+strings.ReplaceAll(u, ".", "-"), t1
	return in
}

// full returns an instance with every optional field set.
func full(t *testing.T, u string) Instance {
	in := v3inst(t, u, true)
	in.ReceivedSHA256 = strings.Repeat("ef", 32)
	in.TransferSyntax, in.ReceivedSyntax = "1.2.840.10008.1.2.1", "1.2.840.10008.1.2.4.50"
	in.Profile, in.ProfileSHA256 = "research@3", strings.Repeat("12", 32)
	in.ReceivedForm, in.NopacsVersion = FormDIMSE, "v1.2.3-rc.1+build"
	return in
}

// v3sidecar writes a sidecar of version 3 of the given instances.
func v3sidecar(t *testing.T, h ScanHeader, ins ...Instance) []byte {
	t.Helper()
	var b bytes.Buffer
	w := NewWriterV3(&b, h)
	for _, in := range ins {
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// drain reads a sidecar to its end, returning the version, the header and
// the first error that is not io.EOF.
func drain(b []byte) (int, *ScanHeader, []Instance, error) {
	r := NewReader(bytes.NewReader(b))
	var out []Instance
	for {
		in, _, err := r.Next()
		if errors.Is(err, io.EOF) {
			return r.Version(), r.Scan(), out, nil
		}
		if err != nil {
			return r.Version(), r.Scan(), out, err
		}
		out = append(out, in)
	}
}

// TestV3RoundTrip guards the form of version 3: its first line states the
// scan's header, every field of a line reads back as written, the version
// is known from the first bytes alone, and a line with only the required
// fields has none of the optional ones in its text.
func TestV3RoundTrip(t *testing.T) {
	plain := v3inst(t, "1.2.4", false)
	b := v3sidecar(t, head(), full(t, "1.2.3"), plain)
	v, h, got, err := drain(b)
	if err != nil || v != Version || h == nil || !reflect.DeepEqual(*h, head()) {
		t.Fatalf("version %d header %+v: %v", v, h, err)
	}
	if len(got) != 2 || !reflect.DeepEqual(got[0], full(t, "1.2.3")) || got[1].ReceivedSHA256 != "" || got[1].NopacsVersion != "" {
		t.Fatalf("read back %+v", got)
	}
	lines := strings.Split(string(b), "\n")
	if !strings.HasPrefix(lines[0], `{"version":3,"scan":{"study_uid":"1.2.826.0.1","series_uid":"1.2.826.0.1.2","session_receiver":"scanner-a","receiver":"scanner-b","route":{"name":"trial","profile":"research@3","replaces_forward_to":true,"forward_to":["archive"]},`) ||
		!strings.HasSuffix(lines[0], `"values":{"study_date":"20240131","modality":"MR","series_description":"T1 axial","series_number":3},"created":"2026-11-20T08:30:00.123456Z","updated":"2026-11-20T11:30:00.123456Z"},"instances":[`) {
		t.Fatalf("the first line is not the form:\n%s", lines[0])
	}
	for _, f := range []string{"received_sha256", "profile_sha256", "received_form", "received_syntax", "nopacs_version", `"revision"`} {
		if strings.Count(string(b), f) > 1 || (f == `"revision"` && strings.Contains(string(b), f)) {
			t.Errorf("%s is written for a line that has none", f)
		}
	}
	if VersionOf(b[:HeadLength]) != Version || VersionOf(b[:HeadLength-1]) != 0 {
		t.Errorf("VersionOf says %d", VersionOf(b[:HeadLength]))
	}
	noRoute := head()
	noRoute.Route = nil
	if _, h, _, err := drain(v3sidecar(t, noRoute, plain)); err != nil || h.Route != nil {
		t.Errorf("a header without a route: %+v %v", h, err)
	}
	if got := h; got != nil && !reflect.DeepEqual(got.Route.Tenant(), tenant.ReceptionRoute{Route: "trial", Profile: tenant.ProfileRef{Name: "research", Version: 3}, ReplacesForwardTo: true, ForwardTo: []string{"archive"}}) {
		t.Errorf("route as the catalog keeps it: %+v", got.Route.Tenant())
	}
	if got := h.Values.Tenant(); got != (tenant.ScanValues{StudyDate: "20240131", Modality: "MR", SeriesDescription: "T1 axial", SeriesNumber: 3, HasSeriesNumber: true}) {
		t.Errorf("values as the catalog keeps them: %+v", got)
	}
	// A scan the catalog holds no receivers or values for keeps none in its
	// header, and a series number of 0 is one the catalog holds.
	bare := head()
	bare.Receiver, bare.SessionReceiver, bare.Values = "", "", Values{}
	b = v3sidecar(t, bare, plain)
	if _, h, _, err := drain(b); err != nil || !reflect.DeepEqual(*h, bare) || !strings.Contains(string(b), `"session_receiver":"","receiver":"",`) || !strings.Contains(string(b), `"values":{},`) {
		t.Errorf("a header without receivers or values: %+v %v\n%s", h, err, b)
	}
	zero := head()
	zero.Values = ValuesOf(tenant.ScanValues{SeriesNumber: 0, HasSeriesNumber: true})
	if _, h, _, err := drain(v3sidecar(t, zero, plain)); err != nil || h.Values.SeriesNumber == nil || *h.Values.SeriesNumber != 0 {
		t.Errorf("a series number of 0: %+v %v", h, err)
	}
}

// TestV3RevisionKeys guards that a line of a later revision holds the
// revision key and the revision, both or neither, and reads back.
func TestV3RevisionKeys(t *testing.T) {
	p := label.Path{Subject: "s", Session: "e", Scan: "c"}
	k2, err := layout.RevisionKey(p, "1.2.3", 2)
	if err != nil {
		t.Fatal(err)
	}
	in := v3inst(t, "1.2.3", false)
	in.Key, in.Revision = k2, 2
	_, _, got, err := drain(v3sidecar(t, head(), in))
	if err != nil || len(got) != 1 || got[0].Key != k2 || got[0].Revision != 2 {
		t.Fatalf("read back %+v, %v", got, err)
	}
	for name, mod := range map[string]func(*Instance){
		"key without revision": func(i *Instance) { i.Revision = 0 },
		"revision 1 spelled":   func(i *Instance) { i.Revision = 1 },
		"other revision":       func(i *Instance) { i.Revision = 3 },
		"negative":             func(i *Instance) { i.Revision = -2 },
		"revision, plain key":  func(i *Instance) { i.Key, i.Revision = inst(t, "1.2.3", false).Key, 2 },
	} {
		bad := in
		mod(&bad)
		if err := NewWriterV3(io.Discard, head()).Add(bad); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: %v, want ErrFormat", name, err)
		}
	}
}

// TestV3Refuses guards each rule of a line and of the header, on writing and
// on reading: a line of version 3 without its reception or time, a checksum
// that is not one, a received checksum equal to the archived one, a profile
// checksum without a profile, an unknown form, a bad syntax or version
// text, a time not in UTC, and a header that is wrong in any part.
func TestV3Refuses(t *testing.T) {
	cases := map[string]func(*Instance){
		"no reception":       func(i *Instance) { i.Reception = "" },
		"bad reception":      func(i *Instance) { i.Reception = "a b" },
		"long reception":     func(i *Instance) { i.Reception = strings.Repeat("a", 65) },
		"no held_at":         func(i *Instance) { i.HeldAt = time.Time{} },
		"held_at zone":       func(i *Instance) { i.HeldAt = t1.In(time.FixedZone("x", 3600)) },
		"received short":     func(i *Instance) { i.ReceivedSHA256 = "abc" },
		"received upper":     func(i *Instance) { i.ReceivedSHA256 = strings.Repeat("AB", 32) },
		"received equal":     func(i *Instance) { i.ReceivedSHA256 = i.SHA256 },
		"profile sum alone":  func(i *Instance) { i.Profile = "" },
		"profile sum bad":    func(i *Instance) { i.Profile, i.ProfileSHA256 = "r@1", "xyz" },
		"form":               func(i *Instance) { i.ReceivedForm = "stow" },
		"received syntax":    func(i *Instance) { i.ReceivedSyntax = "1.02" },
		"version text":       func(i *Instance) { i.NopacsVersion = "v 1" },
		"version long":       func(i *Instance) { i.NopacsVersion = strings.Repeat("v", 65) },
		"version non-ascii":  func(i *Instance) { i.NopacsVersion = "vé" },
		"profile not a ref":  func(i *Instance) { i.Profile = "x" },
		"transfer syntax":    func(i *Instance) { i.TransferSyntax = "x" },
		"held_at far zone":   func(i *Instance) { i.HeldAt = t1.In(time.FixedZone("x", -3600)) },
		"received_form case": func(i *Instance) { i.ReceivedForm = "DIMSE" },
	}
	for name, mod := range cases {
		in := full(t, "1.2.3")
		mod(&in)
		if err := NewWriterV3(io.Discard, head()).Add(in); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: written, %v", name, err)
		}
	}
	good := string(v3sidecar(t, head(), full(t, "1.2.3")))
	for name, b := range map[string]string{
		"reception":      strings.Replace(good, `"reception":"rcp-1-2-3",`, "", 1),
		"held_at":        strings.Replace(good, `"held_at":"2026-11-20T08:30:00.123456Z",`, "", 1),
		"held_at offset": strings.Replace(good, `"held_at":"2026-11-20T08:30:00.123456Z"`, `"held_at":"2026-11-20T09:30:00.123456+01:00"`, 1),
		"unknown field":  strings.Replace(good, `"reception"`, `"other":1,"reception"`, 1),
		"cut":            good[:len(good)-4],
		"after":          good + "x",
	} {
		if _, _, _, err := drain([]byte(b)); !errors.Is(err, ErrFormat) {
			t.Errorf("read of %s: %v, want ErrFormat", name, err)
		}
	}
	hdr := map[string]func(*ScanHeader){
		"study":         func(h *ScanHeader) { h.StudyUID = "1.02" },
		"no study":      func(h *ScanHeader) { h.StudyUID = "" },
		"series":        func(h *ScanHeader) { h.SeriesUID = "x" },
		"receiver":      func(h *ScanHeader) { h.Receiver = "Bad Name" },
		"session rcv":   func(h *ScanHeader) { h.SessionReceiver = "a/b" },
		"study date":    func(h *ScanHeader) { h.Values.StudyDate = "20240230" },
		"modality":      func(h *ScanHeader) { h.Values.Modality = "mr" },
		"description":   func(h *ScanHeader) { h.Values.SeriesDescription = "T1 " },
		"created":       func(h *ScanHeader) { h.Created = time.Time{} },
		"created zone":  func(h *ScanHeader) { h.Created = t1.In(time.FixedZone("x", 60)) },
		"updated early": func(h *ScanHeader) { h.Updated = t1.Add(-time.Second) },
		"empty route":   func(h *ScanHeader) { h.Route = &Route{} },
		"route profile": func(h *ScanHeader) { h.Route = &Route{Profile: "x"} },
		"route name":    func(h *ScanHeader) { h.Route = &Route{Name: "A B"} },
		"route forward": func(h *ScanHeader) { h.Route = &Route{ForwardTo: []string{"a"}} },
	}
	for name, mod := range hdr {
		h := head()
		mod(&h)
		w := NewWriterV3(io.Discard, h)
		if err := w.Add(v3inst(t, "1.2.3", false)); !errors.Is(err, ErrFormat) {
			t.Errorf("header %s: written, %v", name, err)
		}
	}
	first := strings.SplitN(good, "\n", 2)
	for name, line := range map[string]string{
		"header unknown":  strings.Replace(first[0], `"receiver"`, `"other":1,"receiver"`, 1),
		"header reorder":  strings.Replace(first[0], `"session_receiver":"scanner-a","receiver":"scanner-b","route"`, `"receiver":"scanner-b","session_receiver":"scanner-a","route"`, 1),
		"header spaces":   strings.Replace(first[0], `"scan":{`, `"scan": {`, 1),
		"header version":  strings.Replace(first[0], `"version":3`, `"version":4`, 1),
		"header no tail":  strings.TrimSuffix(first[0], `,"instances":[`),
		"header no scan":  `{"version":3,"instances":[`,
		"header offset":   strings.Replace(first[0], `08:30:00.123456Z`, `09:30:00.123456+01:00`, 1),
		"header trailing": strings.Replace(first[0], `},"instances"`, `}x,"instances"`, 1),
	} {
		if _, _, _, err := drain([]byte(line + "\n" + first[1])); !errors.Is(err, ErrFormat) {
			t.Errorf("read of %s: %v, want ErrFormat", name, err)
		}
	}
	if err := NewWriterV3(io.Discard, ScanHeader{}).Close(); !errors.Is(err, ErrFormat) {
		t.Errorf("a writer of an invalid header closed: %v", err)
	}
}

// TestOlderVersionsRefuseV3Fields guards that a sidecar of version 1 or 2
// holds none of the fields version 3 adds, so a line has one meaning in
// each version.
func TestOlderVersionsRefuseV3Fields(t *testing.T) {
	line := full(t, "1.2.3")
	for _, c := range []struct {
		name string
		w    *Writer
	}{{"version 2", NewWriter(io.Discard)}} {
		if err := c.w.Add(line); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: a line of version 3 written: %v", c.name, err)
		}
	}
	good := string(v3sidecar(t, head(), full(t, "1.2.3")))
	body := strings.SplitN(good, "\n", 2)[1]
	for name, h := range map[string]string{"version 2": header, "version 1": headerV1} {
		if _, _, _, err := drain([]byte(h + body)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s: a line of version 3 read: %v", name, err)
		}
	}
	// Read from the middle of a sidecar, a reader that does not know the
	// version checks what is there and requires nothing.
	rd := NewReaderFrom(strings.NewReader(body))
	if in, _, err := rd.Next(); err != nil || in.Reception == "" {
		t.Errorf("a reader that began at a line: %+v %v", in, err)
	}
}

// TestMergeScan guards the merges that write version 3: into nothing, with
// the header given; into version 3, with the header given and nothing of
// the old one's (whose attempt may never have committed); refusing another
// series; and into an older version, which stays what it is, with the added
// lines losing the fields it has no place for.
func TestMergeScan(t *testing.T) {
	var fresh bytes.Buffer
	if n, err := MergeScan(&fresh, nil, head(), List([]Instance{v3inst(t, "1.2.3", true), v3inst(t, "1.2.5", false)}), nil); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	if v, h, _, err := drain(fresh.Bytes()); err != nil || v != Version || !reflect.DeepEqual(*h, head()) {
		t.Fatalf("a merge into nothing: %d %+v %v", v, h, err)
	}

	later := head()
	later.SessionReceiver, later.Receiver, later.Route = "other-receiver", "other-receiver", &Route{Name: "elsewhere"}
	later.Created, later.Updated = t2.Add(time.Hour), t2.Add(2*time.Hour)
	var merged bytes.Buffer
	if _, err := MergeScan(&merged, bytes.NewReader(fresh.Bytes()), later, List([]Instance{v3inst(t, "1.2.4", false)}), func(u string) bool { return u == "1.2.5" }); err != nil {
		t.Fatal(err)
	}
	v, h, got, err := drain(merged.Bytes())
	want := later
	if err != nil || v != Version || !reflect.DeepEqual(*h, want) || len(got) != 2 || got[1].SOPInstanceUID != "1.2.4" {
		t.Fatalf("a merge into version 3: %d %+v %v\n%+v", v, h, err, got)
	}
	var kept bytes.Buffer
	if _, err := Merge(&kept, bytes.NewReader(merged.Bytes()), List(nil), nil); err != nil || !bytes.Equal(kept.Bytes(), merged.Bytes()) {
		t.Errorf("a merge without a header changed a sidecar of version 3: %v", err)
	}

	other := head()
	other.SeriesUID = "1.2.9"
	if _, err := MergeScan(io.Discard, bytes.NewReader(fresh.Bytes()), other, List(nil), nil); !errors.Is(err, ErrFormat) {
		t.Errorf("another series: %v, want ErrFormat", err)
	}

	for name, old := range map[string][]byte{"version 2": []byte(header + lineOf(t, inst(t, "1.2.3", true)) + "\n" + footer), "version 1": v1Sidecar(t, "1.2.3")} {
		var out bytes.Buffer
		if _, err := MergeScan(&out, bytes.NewReader(old), head(), List([]Instance{full(t, "1.2.4")}), nil); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		v, h, got, err := drain(out.Bytes())
		wantV := map[string]int{"version 2": VersionFrames, "version 1": 1}[name]
		if err != nil || v != wantV || h != nil || len(got) != 2 || got[1].Reception != "" || got[1].ReceivedSHA256 != "" {
			t.Errorf("%s: kept as %d, %+v, %v\n%+v", name, v, h, err, got)
		}
	}
}

// lineOf returns the line of an instance as a writer writes it.
func lineOf(t *testing.T, in Instance) string {
	t.Helper()
	var b bytes.Buffer
	w := NewWriter(&b)
	if err := w.Add(in); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return strings.Split(b.String(), "\n")[1]
}

// TestUpgrade guards the rewrite to version 3: every line of a sidecar of
// version 2 goes through the filler and comes out in order with what it
// added, the header is the one given, a second rewrite of the result
// changes no byte, a filler that fails fails the rewrite, and an index is
// written for a large scan and points at its lines.
func TestUpgrade(t *testing.T) {
	const n = IndexEvery + 30
	var old bytes.Buffer
	w := NewWriter(&old)
	uids := make([]string, n)
	for i := range uids {
		uids[i] = "1.2." + itoa(1000+i)
		if err := w.Add(inst(t, uids[i], i%3 == 0)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	fill := func(in Instance) (Instance, error) {
		in.Reception, in.HeldAt = "r1", t1
		return in, nil
	}
	var out bytes.Buffer
	if got, err := Upgrade(&out, bytes.NewReader(old.Bytes()), head(), fill); err != nil || got != n {
		t.Fatal(got, err)
	}
	v, h, ins, err := drain(out.Bytes())
	if err != nil || v != Version || !reflect.DeepEqual(*h, head()) || len(ins) != n {
		t.Fatalf("%d %+v %d lines: %v", v, h, len(ins), err)
	}
	for i, in := range ins {
		if in.SOPInstanceUID != uids[i] || in.Reception != "r1" || !in.HeldAt.Equal(t1) || (i%3 == 0) != (in.PixelData != nil) || in.Dataset == nil {
			t.Fatalf("line %d: %+v", i, in)
		}
	}
	entries, ok := ParseIndex(out.Bytes(), true)
	if !ok || len(entries) != 2 {
		t.Fatalf("index %v %v", entries, ok)
	}
	for _, e := range entries {
		in, _, err := NewReaderFrom(bytes.NewReader(out.Bytes()[e.Offset:])).Next()
		if err != nil || in.SOPInstanceUID != e.UID {
			t.Errorf("the index puts %s at %d: %s, %v", e.UID, e.Offset, in.SOPInstanceUID, err)
		}
	}
	var again bytes.Buffer
	if _, err := Upgrade(&again, bytes.NewReader(out.Bytes()), head(), fill); err != nil || !bytes.Equal(again.Bytes(), out.Bytes()) {
		t.Errorf("a second rewrite changed the sidecar: %v", err)
	}
	boom := errors.New("boom")
	if _, err := Upgrade(io.Discard, bytes.NewReader(old.Bytes()), head(), func(Instance) (Instance, error) { return Instance{}, boom }); !errors.Is(err, boom) {
		t.Errorf("a failing filler: %v", err)
	}
	if _, err := Upgrade(io.Discard, bytes.NewReader(old.Bytes()), head(), func(in Instance) (Instance, error) { return in, nil }); !errors.Is(err, ErrFormat) {
		t.Errorf("a line left without its reception: %v, want ErrFormat", err)
	}
	if _, err := Upgrade(io.Discard, bytes.NewReader(v1Sidecar(t, "1.2.3")), head(), fill); !errors.Is(err, ErrIncomplete) {
		t.Errorf("a sidecar of version 1 with pixel data and no table: %v, want ErrIncomplete", err)
	}
}
