package api_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
)

// metaFiles are the files the metadata tests have lab1 receive: a native
// and two encapsulated images of one series (the same public file with its
// pixels stored three ways), CT slices of two series, an image of another
// subject, and an image that holds a binary value over 1 KiB and a text
// value over 64 KiB before its pixel data and trailing padding after it,
// none of which the metadata file keeps the place of.
func metaFiles(t *testing.T) (files [][]byte, synthetic []byte) {
	t.Helper()
	extra, _, _ := privateBulk()
	synthetic = reUID(t, insertBeforePixelData(t, corpusFile(t, "MR_small.dcm"), extra), '6')
	synthetic = append(synthetic, explicitLong(0xFFFC, 0xFFFC, "OB", bytes.Repeat([]byte{0}, 64))...)
	files = [][]byte{
		corpusFile(t, "MR_small.dcm"),
		reUID(t, corpusFile(t, "MR_small_RLE.dcm"), '8'),
		reUID(t, corpusFile(t, "MR_small_jp2klossless.dcm"), '9'),
		synthetic,
		corpusFile(t, "ct2n-6293.dcm"),
		corpusFile(t, "ct5n-2062.dcm"),
		corpusFile(t, "liver_1frame.dcm"),
	}
	return files, synthetic
}

// dataset decodes a DICOM JSON answer.
func dataset(t testing.TB, text string) map[string]map[string]any {
	t.Helper()
	var m map[string]map[string]any
	if err := json.Unmarshal([]byte(text), &m); err != nil {
		t.Fatalf("the answer is not DICOM JSON: %v", err)
	}
	return m
}

// uidOf returns the SOP Instance UID of a file.
func uidOf(t testing.TB, file []byte) string {
	t.Helper()
	h, err := header.Read(bytes.NewReader(file), int64(len(file)))
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := h.UID(header.SOPInstanceUID)
	return uid
}

// bulkOf returns the bytes a BulkDataURI of an answer names in the file
// the instance was received as.
func bulkOf(t testing.TB, el map[string]any, p label.Path, id string, file []byte) []byte {
	t.Helper()
	s, _ := el["BulkDataURI"].(string)
	u := parseURI(t, s, p, id)
	if u.offset < 0 || u.length < 0 || u.offset+u.length > int64(len(file)) {
		t.Fatalf("BulkDataURI %q lies outside a file of %d bytes", s, len(file))
	}
	return file[u.offset : u.offset+u.length]
}

// TestMetadataAnswersEveryInstanceOfAScan guards the whole answer on an
// archive made by reception: each scan's page holds exactly the instances
// the catalog holds, in order, with the transfer syntax its file states,
// the DICOM JSON of its header with a BulkDataURI in place of its pixel
// data (whose bytes, read at that offset of the file that was received,
// start where the contract says), no file meta information and no trailing
// padding, and the checksum of the scan's manifest on the storage.
func TestMetadataAnswersEveryInstanceOfAScan(t *testing.T) {
	f := newPlaceFixture(t)
	files, _ := metaFiles(t)
	a := receiveArchive(t, f, files)
	md := f.as(f.lab1Reader).metadata
	if len(a.scans(t)) < 3 {
		t.Fatalf("lab1 holds %d scans, want at least 3", len(a.scans(t)))
	}
	for _, path := range a.scans(t) {
		o := call(md.GetScanMetadata, scanRequest(path))
		if o.err != nil {
			t.Fatalf("%v: %v", path, o.err)
		}
		res := o.res.(*v1.GetScanMetadataResponse)
		held := a.held(t, path)
		if len(res.GetInstances()) != len(held) || res.GetNextPageToken() != "" {
			t.Fatalf("%v: %d instances and token %q, want %d and none", path, len(res.GetInstances()), res.GetNextPageToken(), len(held))
		}
		sum := sha256.Sum256(a.object(t, mustKey(t, path)))
		if res.GetManifestSha256() != hex.EncodeToString(sum[:]) {
			t.Errorf("%v: manifest_sha256 %s, the manifest on the storage has %x", path, res.GetManifestSha256(), sum)
		}
		for i, m := range res.GetInstances() {
			id := held[i].SOPInstanceUID
			file := a.sent[id]
			if m.GetInstanceId() != id || m.GetSopInstanceUid() != id {
				t.Fatalf("%v: instance %d is %q / %q, want %q", path, i, m.GetInstanceId(), m.GetSopInstanceUid(), id)
			}
			h, err := header.Read(bytes.NewReader(file), int64(len(file)))
			if err != nil {
				t.Fatal(err)
			}
			if m.GetTransferSyntaxUid() != h.TransferSyntaxUID || m.GetJsonTooLarge() || m.GetBulkDataOmitted() {
				t.Errorf("%v %s: transfer syntax %q too_large %v omitted %v, want %q false false", path, id, m.GetTransferSyntaxUid(), m.GetJsonTooLarge(), m.GetBulkDataOmitted(), h.TransferSyntaxUID)
			}
			set := dataset(t, m.GetDicomJson())
			for _, key := range []string{"00080018", "0020000D", "0020000E"} {
				if set[key] == nil {
					t.Errorf("%v %s: the answer has no %s", path, id, key)
				}
			}
			if set["00020010"] != nil || set["FFFCFFFC"] != nil {
				t.Errorf("%v %s: the answer holds file meta information or trailing padding", path, id)
			}
			px := set["7FE00010"]
			if px == nil {
				t.Fatalf("%v %s: the answer has no Pixel Data", path, id)
			}
			if _, inline := px["InlineBinary"]; inline || px["Value"] != nil || px["BulkDataURI"] == nil {
				t.Fatalf("%v %s: Pixel Data is %v, want a BulkDataURI alone", path, id, px)
			}
			checkPixelBytes(t, px, path, id, file, h.TransferSyntaxUID)
		}
	}
}

// mustKey returns the key of a scan's manifest.
func mustKey(t testing.TB, p label.Path) string {
	t.Helper()
	k, err := layout.ManifestKey(p)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// checkPixelBytes checks a Pixel Data reference against the file that was
// received, as the contract defines it: the bytes from offset are the
// value field of the element, whose header ends there; an encapsulated
// value starts with its first item and its length runs through the
// Sequence Delimitation Item.
func checkPixelBytes(t testing.TB, px map[string]any, p label.Path, id string, file []byte, ts string) {
	t.Helper()
	s, _ := px["BulkDataURI"].(string)
	u := parseURI(t, s, p, id)
	hdr := int64(12)
	if ts == "1.2.840.10008.1.2" {
		hdr = 8
	}
	if !bytes.Equal(file[u.offset-hdr:u.offset-hdr+4], []byte{0xE0, 0x7F, 0x10, 0x00}) {
		t.Errorf("%s: the bytes before offset %d are not the header of Pixel Data", id, u.offset)
	}
	encapsulated := strings.HasPrefix(ts, "1.2.840.10008.1.2.4.") || ts == "1.2.840.10008.1.2.5"
	value := bulkOf(t, px, p, id, file)
	if encapsulated {
		if !bytes.HasPrefix(value, []byte{0xFE, 0xFF, 0x00, 0xE0}) || !bytes.HasSuffix(value, []byte{0xFE, 0xFF, 0xDD, 0xE0, 0, 0, 0, 0}) {
			t.Errorf("%s: an encapsulated value does not run from its first item through its delimiter", id)
		}
	} else if want := binary.LittleEndian.Uint32(file[u.offset-4 : u.offset]); int64(want) != u.length && hdr == 12 {
		t.Errorf("%s: length %d, the element states %d", id, u.length, want)
	}
}

// TestMetadataReadsFromTheFileWhatTheSidecarDoesNotHold guards the bounded
// read of an instance's header: a binary value over 1 KiB and a text value
// over 64 KiB, whose place the metadata file does not keep, are answered
// as BulkDataURIs naming exactly their bytes in the file, and an element
// after the Pixel Data (trailing padding) is left out.
func TestMetadataReadsFromTheFileWhatTheSidecarDoesNotHold(t *testing.T) {
	f := newPlaceFixture(t)
	files, synthetic := metaFiles(t)
	a := receiveArchive(t, f, files)
	_, bin, text := privateBulk()
	id := uidOf(t, synthetic)
	var path label.Path
	for _, p := range a.scans(t) {
		for _, h := range a.held(t, p) {
			if h.SOPInstanceUID == id {
				path = p
			}
		}
	}
	o := call(f.as(f.lab1Reader).metadata.GetInstanceMetadata, instanceRequest(path, id))
	if o.err != nil {
		t.Fatal(o.err)
	}
	m := o.res.(*v1.GetInstanceMetadataResponse).GetInstance()
	set := dataset(t, m.GetDicomJson())
	for _, c := range []struct {
		key  string
		vr   string
		want []byte
	}{{"7FDF1001", "OB", bin}, {"7FDF1002", "UT", text}} {
		el := set[c.key]
		if el == nil || el["vr"] != c.vr || el["Value"] != nil || el["InlineBinary"] != nil {
			t.Fatalf("%s: %v, want a BulkDataURI alone", c.key, el)
		}
		if got := bulkOf(t, el, path, id, synthetic); !bytes.Equal(got, c.want) {
			t.Errorf("%s: the bytes at the URI are not the value that was sent", c.key)
		}
	}
	if set["7FDF0010"] == nil || set["7FDF0010"]["Value"] == nil {
		t.Errorf("the private creator, a short value, is not inlined: %v", set["7FDF0010"])
	}
	if set["FFFCFFFC"] != nil {
		t.Error("trailing padding is answered")
	}
	checkPixelBytes(t, set["7FE00010"], path, id, synthetic, m.GetTransferSyntaxUid())
}

// TestMetadataPagesAreWholeAndOrdered guards paging: pages of three over a
// scan of four join, in order of instance id, to the one answer of the
// scan, a page token made for another scan is invalid, and a page size of
// 1001 is refused.
func TestMetadataPagesAreWholeAndOrdered(t *testing.T) {
	f := newPlaceFixture(t)
	files, _ := metaFiles(t)
	a := receiveArchive(t, f, files)
	md := f.as(f.lab1Reader).metadata
	var big, other label.Path
	for _, p := range a.scans(t) {
		if len(a.held(t, p)) == 4 {
			big = p
		} else {
			other = p
		}
	}
	if big == (label.Path{}) {
		t.Fatal("no scan holds four instances")
	}
	whole := mustCall[*v1.GetScanMetadataResponse](t, "whole", call(md.GetScanMetadata, scanRequest(big)))
	var got []*v1.InstanceMetadata
	req := scanRequest(big)
	req.PageSize = 3
	for i := 0; ; i++ {
		res := mustCall[*v1.GetScanMetadataResponse](t, "page", call(md.GetScanMetadata, req))
		got = append(got, res.GetInstances()...)
		if res.GetNextPageToken() == "" {
			if i != 1 {
				t.Errorf("the scan ended after %d pages, want 2", i+1)
			}
			break
		}
		if i > 2 {
			t.Fatal("the pages do not end")
		}
		req.PageToken = res.GetNextPageToken()
	}
	if len(got) != 4 || len(whole.GetInstances()) != 4 {
		t.Fatalf("%d instances paged, %d whole, want 4", len(got), len(whole.GetInstances()))
	}
	ids := make([]string, 0, 4)
	for i, m := range got {
		ids = append(ids, m.GetInstanceId())
		if m.GetDicomJson() != whole.GetInstances()[i].GetDicomJson() {
			t.Errorf("instance %d differs between the pages and the whole answer", i)
		}
	}
	if !slices.IsSorted(ids) {
		t.Errorf("instance ids %v are not in byte order", ids)
	}
	first := scanRequest(big)
	first.PageSize = 1
	token := mustCall[*v1.GetScanMetadataResponse](t, "first", call(md.GetScanMetadata, first)).GetNextPageToken()
	foreign := scanRequest(other)
	foreign.PageToken = token
	if o := call(md.GetScanMetadata, foreign); o.code() != connect.CodeInvalidArgument || !strings.Contains(o.message(), "page_token") {
		t.Errorf("a token made for another scan: %v, want invalid_argument naming page_token", o.err)
	}
	for _, size := range []int32{1001, -1} {
		r := scanRequest(big)
		r.PageSize = size
		if o := call(md.GetScanMetadata, r); o.code() != connect.CodeInvalidArgument || !strings.Contains(o.message(), "page_size") {
			t.Errorf("page_size %d: %v, want invalid_argument naming page_size", size, o.err)
		}
	}
}

// testGetInstanceMetadata guards GetInstanceMetadata: the
// header of one instance is what its scan's page holds for it, and the
// instance named by its id in another scan is not found.
func testGetInstanceMetadata(t *testing.T) {
	f := newPlaceFixture(t)
	files, _ := metaFiles(t)
	a := receiveArchive(t, f, files)
	md := f.as(f.lab1Reader).metadata
	scans := a.scans(t)
	for _, path := range scans {
		whole := mustCall[*v1.GetScanMetadataResponse](t, "scan", call(md.GetScanMetadata, scanRequest(path)))
		for _, want := range whole.GetInstances() {
			got := mustCall[*v1.GetInstanceMetadataResponse](t, "instance", call(md.GetInstanceMetadata, instanceRequest(path, want.GetInstanceId())))
			if got.GetInstance().GetDicomJson() != want.GetDicomJson() || got.GetInstance().GetTransferSyntaxUid() != want.GetTransferSyntaxUid() {
				t.Errorf("%v %s: the instance's answer differs from its entry in the scan's", path, want.GetInstanceId())
			}
		}
	}
	other := a.held(t, scans[1])[0].SOPInstanceUID
	if o := call(md.GetInstanceMetadata, instanceRequest(scans[0], other)); o.code() != connect.CodeNotFound {
		t.Errorf("an instance of another scan: %v, want not_found", o.err)
	}
}

// testGetScanMetadata guards what is not answered: no key, no grant, a
// scan or an instance the tenant does not hold, and a request that is not
// valid; and that a read that answers is audited in the tenant's log with
// the scan's path and the count of instances.
func testGetScanMetadata(t *testing.T) {
	f := newPlaceFixture(t)
	files, _ := metaFiles(t)
	a := receiveArchive(t, f, files)
	path := a.scans(t)[0]
	id := a.held(t, path)[0].SOPInstanceUID
	scan := func(mod func(*v1.GetScanMetadataRequest)) func(caller) outcome {
		return func(c caller) outcome {
			r := scanRequest(path)
			mod(r)
			return call(f.as(c).metadata.GetScanMetadata, r)
		}
	}
	instance := func(mod func(*v1.GetInstanceMetadataRequest)) func(caller) outcome {
		return func(c caller) outcome {
			r := instanceRequest(path, id)
			mod(r)
			return call(f.as(c).metadata.GetInstanceMetadata, r)
		}
	}
	none := func(*v1.GetScanMetadataRequest) {}
	for _, c := range []struct {
		name string
		as   caller
		do   func(caller) outcome
		code connect.Code
		msg  string
	}{
		{"scan without a key", nobody, scan(none), connect.CodeUnauthenticated, ""},
		{"scan without a grant", f.noGrant, scan(none), connect.CodePermissionDenied, ""},
		{"scan with a grant on another tenant", f.lab2Admin, scan(none), connect.CodePermissionDenied, ""},
		{"instance without a grant", f.noGrant, instance(func(*v1.GetInstanceMetadataRequest) {}), connect.CodePermissionDenied, ""},
		{"a scan that is not there", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.ScanLabel = "nothere" }), connect.CodeNotFound, "no scan"},
		{"a session that is not there", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.SessionLabel = "nothere" }), connect.CodeNotFound, "no scan"},
		{"a subject that is not there", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.SubjectLabel = "nothere" }), connect.CodeNotFound, "no scan"},
		{"a label in another case", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.SubjectLabel = strings.ToLower(r.SubjectLabel) + "x" }), connect.CodeNotFound, "no scan"},
		{"an instance that is not there", f.lab1Reader, instance(func(r *v1.GetInstanceMetadataRequest) { r.InstanceId = "1.2.3.4.5" }), connect.CodeNotFound, "no instance"},
		{"a scan label that is not one", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.ScanLabel = "Smith^John" }), connect.CodeInvalidArgument, "label"},
		{"no subject", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.SubjectLabel = "" }), connect.CodeInvalidArgument, "label"},
		{"an instance id that is not a UID", f.lab1Reader, instance(func(r *v1.GetInstanceMetadataRequest) { r.InstanceId = "../x" }), connect.CodeInvalidArgument, "instance_id"},
		{"no instance id", f.lab1Reader, instance(func(r *v1.GetInstanceMetadataRequest) { r.InstanceId = "" }), connect.CodeInvalidArgument, "instance_id"},
		{"a page token that is not one", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.PageToken = "x" }), connect.CodeInvalidArgument, "page_token"},
		{"a page token over 1 KiB", f.lab1Reader, scan(func(r *v1.GetScanMetadataRequest) { r.PageToken = strings.Repeat("x", 1025) }), connect.CodeInvalidArgument, "page_token"},
		{"a tenant that is not a name", f.lab1Reader, func(c caller) outcome {
			r := scanRequest(path)
			r.Tenant = "Lab 1"
			return call(f.as(c).metadata.GetScanMetadata, r)
		}, connect.CodePermissionDenied, ""},
	} {
		o := c.do(c.as)
		if o.code() != c.code || !strings.Contains(o.message(), c.msg) {
			t.Errorf("%s: %v, want %v with %q", c.name, o.err, c.code, c.msg)
		}
	}
	// An admin who may list every tenant is told a tenant does not exist.
	r := scanRequest(path)
	r.Tenant = "nolab"
	if o := call(f.as(f.instAdmin).metadata.GetScanMetadata, r); o.code() != connect.CodeNotFound {
		t.Errorf("a tenant that does not exist, for an installation admin: %v, want not_found", o.err)
	}

	o := scan(none)(f.lab1Reader)
	n := len(mustCall[*v1.GetScanMetadataResponse](t, "scan", o).GetInstances())
	f.checkAudit(t, "GetScanMetadata", o.requestID, wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1",
		object: path.Subject + "/" + path.Session + "/" + path.Scan, details: map[string]string{"instances": strconv.Itoa(n)}})
	o = instance(func(*v1.GetInstanceMetadataRequest) {})(f.lab1Reader)
	mustCall[*v1.GetInstanceMetadataResponse](t, "instance", o)
	f.checkAudit(t, "GetInstanceMetadata", o.requestID, wantAudit{log: "lab1", outcome: master.OutcomeDone, tenant: "lab1",
		object: path.Subject + "/" + path.Session + "/" + path.Scan, details: map[string]string{"instance": "present"}})
	for _, en := range f.tenantLog("lab1") {
		for k, v := range en.Details {
			if strings.Contains(v, id) {
				t.Errorf("an entry's detail %s holds the instance id", k)
			}
		}
	}
}
