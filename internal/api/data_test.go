package api_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strconv"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// largeFile returns the MR file with its SOP Instance UID ending in last
// and n bytes of a pattern after its pixel data, as Data Set Trailing
// Padding (FFFC,FFFC) (PS3.5 7.1.3): an instance whose bytes span several
// chunks of a stream and of a download.
func largeFile(t testing.TB, n int, last byte) []byte {
	t.Helper()
	pad := make([]byte, n)
	for i := range pad {
		pad[i] = byte(i % 251)
	}
	return append(reUID(t, corpusFile(t, "MR_small.dcm"), last), explicitLong(0xFFFC, 0xFFFC, "OB", pad)...)
}

// dataFiles are the files the data tests have lab1 receive: the metadata
// tests' files (several subjects, sessions and scans, native and
// encapsulated) and an instance of 2.5 MiB, which a stream carries in
// three chunks.
func dataFiles(t *testing.T) [][]byte {
	t.Helper()
	files, _ := metaFiles(t)
	return append(files, largeFile(t, 5<<19, '5'))
}

// subjectsOf returns the subjects of the scans, each once, in order.
func subjectsOf(scans []label.Path) []string {
	var out []string
	for _, p := range scans {
		if !slices.Contains(out, p.Subject) {
			out = append(out, p.Subject)
		}
	}
	return out
}

// checkInstances holds what a stream carried to the archive: each scan's
// marker carries the SHA-256 of the scan's manifest on the storage and its
// count; each instance's announcement is its manifest's entry (key, place,
// length, SHA-256); its bytes are that many, have that SHA-256, and are
// the bytes received; and the instances come in the contract's order,
// (folded session label, folded scan label, instance id), with no
// instance twice.
func checkInstances(t *testing.T, a *archive, got []*streamed, markers []*v1.ScanMarker, announceOnly bool) {
	t.Helper()
	for _, m := range markers {
		p := label.Path{Subject: m.GetSubjectLabel(), Session: m.GetSessionLabel(), Scan: m.GetScanLabel()}
		man, sum := manifestOf(t, a, p)
		if m.GetManifestSha256() != sum || m.GetInstanceCount() != int64(len(man)) {
			t.Errorf("%v: marker %s and %d instances, the manifest on the storage has %s and %d", p, m.GetManifestSha256(), m.GetInstanceCount(), sum, len(man))
		}
	}
	var last string
	for _, s := range got {
		f := s.file
		p := label.Path{Subject: f.GetSubjectLabel(), Session: f.GetSessionLabel(), Scan: f.GetScanLabel()}
		if s.marker.GetScanLabel() != p.Scan || s.marker.GetSessionLabel() != p.Session {
			t.Errorf("%s comes under the marker of another scan", f.GetInstanceId())
		}
		order := label.Fold(p.Session) + "\x00" + label.Fold(p.Scan) + "\x00" + f.GetInstanceId()
		if order <= last {
			t.Errorf("%s comes out of order", f.GetInstanceId())
		}
		last = order
		man, _ := manifestOf(t, a, p)
		e, ok := man[f.GetInstanceId()]
		if !ok || f.GetSopInstanceUid() != f.GetInstanceId() || f.GetKey() != e.Key || f.GetOffset() != e.Offset || f.GetLength() != e.Length || f.GetSha256() != e.SHA256 {
			t.Errorf("%v: announcement %+v, the manifest lists %+v", p, f, e)
			continue
		}
		if announceOnly {
			if s.chunks != 0 {
				t.Errorf("%s: %d chunks with announce_only", f.GetInstanceId(), s.chunks)
			}
			continue
		}
		sum := sha256.Sum256(s.bytes)
		if int64(len(s.bytes)) != e.Length || hex.EncodeToString(sum[:]) != e.SHA256 || !bytes.Equal(s.bytes, a.sent[f.GetInstanceId()]) {
			t.Errorf("%s: %d bytes with SHA-256 %x, the manifest has %d and %s", f.GetInstanceId(), len(s.bytes), sum, e.Length, e.SHA256)
		}
	}
}

// testReadFiles guards ReadFiles on an archive made by reception: a
// stream of each subject, then of a session, a scan and an instance,
// carries every instance of what it names exactly once, in order, each
// under its scan's marker and announced as its manifest lists it, with its
// bytes in chunks of at most 1 MiB that put together are the bytes
// received; announce_only carries the same announcements and no bytes; and
// a stream resumed after a position carries exactly the instances after it.
func testReadFiles(t *testing.T) {
	f := newPlaceFixture(t)
	files := dataFiles(t)
	a := receiveArchive(t, f, files)
	s := serveData(t, f.env)
	scans := a.scans(t)
	subjects := subjectsOf(scans)
	if len(subjects) < 2 || len(scans) < 4 {
		t.Fatalf("lab1 holds %d subjects and %d scans, want at least 2 and 4", len(subjects), len(scans))
	}
	total, chunked := 0, false
	var whole []*streamed
	for _, subject := range subjects {
		o := s.readFiles(f.lab1Reader, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: subject})
		if o.err != nil {
			t.Fatalf("subject %s: %v", subject, o.err)
		}
		got, markers := assemble(t, o.msgs)
		checkInstances(t, a, got, markers, false)
		var want []label.Path
		for _, p := range scans {
			if p.Subject == subject {
				want = append(want, p)
			}
		}
		if len(markers) != len(want) {
			t.Errorf("subject %s: %d markers, the subject has %d scans", subject, len(markers), len(want))
		}
		for _, g := range got {
			chunked = chunked || g.chunks > 1
		}
		total += len(got)
		if len(got) > len(whole) {
			whole = got
		}
	}
	if total != len(files) || !chunked {
		t.Fatalf("the subjects' streams carried %d instances (one in several chunks: %v), lab1 holds %d", total, chunked, len(files))
	}

	// A session, a scan and an instance carry what the subject's stream
	// carried of them.
	first := whole[0].file
	of := func(keep func(*v1.ArchivedFile) bool) []string {
		var ids []string
		for _, g := range whole {
			if keep(g.file) {
				ids = append(ids, g.file.GetInstanceId())
			}
		}
		return ids
	}
	ids := func(got []*streamed) []string {
		var out []string
		for _, g := range got {
			out = append(out, g.file.GetInstanceId())
		}
		return out
	}
	for _, c := range []struct {
		name string
		req  *v1.ReadFilesRequest
		want []string
	}{
		{"a session", &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: first.GetSubjectLabel(), SessionLabel: first.GetSessionLabel()},
			of(func(x *v1.ArchivedFile) bool { return x.GetSessionLabel() == first.GetSessionLabel() })},
		{"a scan", &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: first.GetSubjectLabel(), SessionLabel: first.GetSessionLabel(), ScanLabel: first.GetScanLabel()},
			of(func(x *v1.ArchivedFile) bool {
				return x.GetSessionLabel() == first.GetSessionLabel() && x.GetScanLabel() == first.GetScanLabel()
			})},
		{"an instance", &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: first.GetSubjectLabel(), SessionLabel: first.GetSessionLabel(), ScanLabel: first.GetScanLabel(), InstanceId: first.GetInstanceId()},
			[]string{first.GetInstanceId()}},
	} {
		o := s.readFiles(f.lab1Reader, c.req)
		if o.err != nil {
			t.Fatalf("%s: %v", c.name, o.err)
		}
		got, markers := assemble(t, o.msgs)
		checkInstances(t, a, got, markers, false)
		if !slices.Equal(ids(got), c.want) || len(markers) == 0 {
			t.Errorf("%s: %v under %d markers, want %v", c.name, ids(got), len(markers), c.want)
		}
	}

	// announce_only: the same announcements, no bytes.
	req := &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: first.GetSubjectLabel(), AnnounceOnly: true}
	o := s.readFiles(f.lab1Reader, req)
	if o.err != nil {
		t.Fatal(o.err)
	}
	got, markers := assemble(t, o.msgs)
	checkInstances(t, a, got, markers, true)
	if !slices.Equal(ids(got), ids(whole)) {
		t.Errorf("announce_only carried %v, want %v", ids(got), ids(whole))
	}

	// Resumed after each position, the stream carries exactly the rest.
	for k := range whole {
		at := whole[k].file
		o := s.readFiles(f.lab1Reader, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: first.GetSubjectLabel(),
			StartAfter: &v1.ReadPosition{SessionLabel: at.GetSessionLabel(), ScanLabel: at.GetScanLabel(), InstanceId: at.GetInstanceId()}})
		if o.err != nil {
			t.Fatalf("resumed after %d: %v", k, o.err)
		}
		got, markers := assemble(t, o.msgs)
		checkInstances(t, a, got, markers, false)
		if !slices.Equal(ids(got), ids(whole[k+1:])) {
			t.Errorf("resumed after %d: %v, want %v", k, ids(got), ids(whole[k+1:]))
		}
	}
	// A position need not be an instance held: a smaller id in the first
	// scan resumes from that scan's first instance.
	o = s.readFiles(f.lab1Reader, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: first.GetSubjectLabel(),
		StartAfter: &v1.ReadPosition{SessionLabel: first.GetSessionLabel(), ScanLabel: first.GetScanLabel(), InstanceId: "1"}})
	if got, _ := assemble(t, o.msgs); o.err != nil || !slices.Equal(ids(got), ids(whole)) {
		t.Errorf("resumed before the first instance: %v %v, want %v", o.err, ids(got), ids(whole))
	}
}

// TestReadFilesRefusesAndAudits guards what a stream does not answer: no
// key, no grant, a grant on another tenant, a tenant that does not exist,
// what the tenant does not hold, and a request that is not valid, each
// with its code; and that a stream that ran is audited in the tenant's log
// once when it opens and once when it ends, with the scan's path, the
// count of instances and bytes it sent, and never the instance id.
func TestReadFilesRefusesAndAudits(t *testing.T) {
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, dataFiles(t))
	s := serveData(t, f.env)
	path := a.scans(t)[0]
	held := a.held(t, path)
	id := held[0].SOPInstanceUID
	scan := func(mod func(*v1.ReadFilesRequest)) *v1.ReadFilesRequest {
		r := &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan}
		mod(r)
		return r
	}
	none := func(*v1.ReadFilesRequest) {}
	for _, c := range []struct {
		name string
		as   caller
		req  *v1.ReadFilesRequest
		code connect.Code
		msg  string
	}{
		{"no key", nobody, scan(none), connect.CodeUnauthenticated, ""},
		{"no grant", f.noGrant, scan(none), connect.CodePermissionDenied, ""},
		{"a grant on another tenant", f.lab2Admin, scan(none), connect.CodePermissionDenied, ""},
		{"a tenant that does not exist", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.Tenant = "nolab" }), connect.CodePermissionDenied, ""},
		{"a tenant that does not exist, to an installation admin", f.instAdmin, scan(func(r *v1.ReadFilesRequest) { r.Tenant = "nolab" }), connect.CodeNotFound, ""},
		{"a subject not held", f.lab1Reader, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: "nothere"}, connect.CodeNotFound, "no subject"},
		{"a session not held", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.ScanLabel, r.SessionLabel = "", "nothere" }), connect.CodeNotFound, "no session"},
		{"a scan not held", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.ScanLabel = "nothere" }), connect.CodeNotFound, "no scan"},
		{"an instance not held", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.InstanceId = "1.2.3.4.5" }), connect.CodeNotFound, "no instance"},
		{"no subject", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.SubjectLabel = "" }), connect.CodeInvalidArgument, "subject_label"},
		{"a label that is not one", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.ScanLabel = "Smith^John" }), connect.CodeInvalidArgument, "scan_label"},
		{"a scan without a session", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.SessionLabel = "" }), connect.CodeInvalidArgument, "scan_label"},
		{"an instance without a scan", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.ScanLabel, r.InstanceId = "", id }), connect.CodeInvalidArgument, "instance_id"},
		{"an instance id that is not a UID", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.InstanceId = "../x" }), connect.CodeInvalidArgument, "instance_id"},
		{"a position without an instance", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.StartAfter = &v1.ReadPosition{} }), connect.CodeInvalidArgument, "start_after.instance_id"},
		{"a position in another scan", f.lab1Reader, scan(func(r *v1.ReadFilesRequest) { r.StartAfter = &v1.ReadPosition{ScanLabel: "other", InstanceId: id} }), connect.CodeInvalidArgument, "start_after.scan_label"},
		{"a position without its session, for a subject", f.lab1Reader,
			&v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: path.Subject, StartAfter: &v1.ReadPosition{ScanLabel: path.Scan, InstanceId: id}}, connect.CodeInvalidArgument, "start_after.session_label"},
	} {
		o := s.readFiles(c.as, c.req)
		if connect.CodeOf(o.err) != c.code || o.err == nil || !strings.Contains(o.err.Error(), c.msg) {
			t.Errorf("%s: %v, want %v with %q", c.name, o.err, c.code, c.msg)
		}
		if len(o.msgs) != 0 {
			t.Errorf("%s: %d messages before the error", c.name, len(o.msgs))
		}
	}

	o := s.readFiles(f.lab1Reader, scan(func(r *v1.ReadFilesRequest) {
		r.StartAfter = &v1.ReadPosition{InstanceId: "1"}
	}))
	if o.err != nil {
		t.Fatal(o.err)
	}
	got, _ := assemble(t, o.msgs)
	var n int64
	for _, g := range got {
		n += int64(len(g.bytes))
	}
	entries := entriesOf(f.tenantLog("lab1"), o.requestID)
	if len(entries) != 2 {
		t.Fatalf("%d entries with the stream's request id in lab1's log, want 2", len(entries))
	}
	object := path.Subject + "/" + path.Session + "/" + path.Scan
	open, end := entries[0], entries[1]
	if open.Details["stream"] == "ended" {
		open, end = end, open
	}
	if open.Method != "/nopacs.v1.DataService/ReadFiles" || open.Object != object || open.Outcome != master.OutcomeDone ||
		open.Details["stream"] != "opened" || open.Details["start_after"] != "present" || open.Details["announce_only"] != "false" {
		t.Errorf("the opening entry is %+v", open)
	}
	if end.Object != object || end.Outcome != master.OutcomeDone || end.Details["stream"] != "ended" || end.Details["ended"] != "complete" ||
		end.Details["instances"] != strconv.Itoa(len(got)) || end.Details["bytes"] != strconv.FormatInt(n, 10) {
		t.Errorf("the closing entry is %+v, want %d instances and %d bytes", end, len(got), n)
	}
	for _, en := range f.tenantLog("lab1") {
		for k, v := range en.Details {
			if strings.Contains(v, id) || strings.Contains(v, "data/") {
				t.Errorf("an entry's detail %s holds an instance id or a key: %q", k, v)
			}
		}
		if strings.Contains(en.Object, id) || strings.Contains(en.Reason, id) {
			t.Errorf("an entry holds the instance id: %+v", en)
		}
	}
}

// TestReadFilesDataLoss guards that a stream never passes on an instance
// its storage does not hold as archived: a stored file whose bytes were
// changed ends the stream with DATA_LOSS naming its scan, before its last
// chunk; one missing from its storage does too; and the closing audit
// entry says so.
func TestReadFilesDataLoss(t *testing.T) {
	f := newPlaceFixture(t)
	big := largeFile(t, 5<<19, '5')
	a := receiveArchive(t, f, [][]byte{big, corpusFile(t, "MR_small.dcm")})
	s := serveData(t, f.env)
	path := a.scans(t)[0]
	held := a.held(t, path)
	var bigIn, smallIn int
	for i, h := range held {
		if h.Size == int64(len(big)) {
			bigIn = i
		} else {
			smallIn = i
		}
	}
	changed := slices.Clone(big)
	changed[len(changed)-1] ^= 0xFF
	if _, err := a.store.Put(context.Background(), held[bigIn].Key, bytes.NewReader(changed)); err != nil {
		t.Fatal(err)
	}
	req := &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan, InstanceId: held[bigIn].SOPInstanceUID}
	o := s.readFiles(f.lab1Reader, req)
	scanPath := path.Subject + "/" + path.Session + "/" + path.Scan
	if connect.CodeOf(o.err) != connect.CodeDataLoss || !strings.Contains(o.err.Error(), scanPath) {
		t.Fatalf("a changed file: %v, want data_loss naming %s", o.err, scanPath)
	}
	got, _ := assemble(t, o.msgs)
	if len(got) != 1 || int64(len(got[0].bytes)) >= held[bigIn].Length {
		t.Errorf("a changed file: its bytes were passed whole (%d instances)", len(got))
	}
	en := entriesOf(f.tenantLog("lab1"), o.requestID)
	ended := slices.IndexFunc(en, func(e master.AuditEntry) bool { return e.Details["stream"] == "ended" })
	if len(en) != 2 || ended < 0 || en[ended].Outcome != master.OutcomeFailed || en[ended].Details["ended"] != "data_loss" {
		t.Errorf("the entries of a stream ended by data loss: %+v", en)
	}

	if err := a.store.Delete(context.Background(), held[smallIn].Key); err != nil {
		t.Fatal(err)
	}
	req.InstanceId = held[smallIn].SOPInstanceUID
	o = s.readFiles(f.lab1Reader, req)
	if connect.CodeOf(o.err) != connect.CodeDataLoss || !strings.Contains(o.err.Error(), "missing") {
		t.Errorf("a missing file: %v, want data_loss", o.err)
	}
}
