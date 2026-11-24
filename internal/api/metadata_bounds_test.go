package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// hugeHeader returns the MR file with a private sequence of n items before
// its pixel data, so its DICOM JSON is about 70 bytes an item, and the SOP
// Instance UID ending in last.
func hugeHeader(t testing.TB, n int, last byte) []byte {
	t.Helper()
	var items []byte
	elem := explicitShort(0x0008, 0x1155, "UI", []byte("1.2.840.10008.5.1.4.1.1.2\x00"))
	for range n {
		items = binary.LittleEndian.AppendUint16(items, 0xFFFE)
		items = binary.LittleEndian.AppendUint16(items, 0xE000)
		items = binary.LittleEndian.AppendUint32(items, uint32(len(elem))) // #nosec G115 -- a test's small value.
		items = append(items, elem...)
	}
	extra := append(explicitShort(0x7FDF, 0x0010, "LO", []byte("NOPACS TEST ")), explicitLong(0x7FDF, 0x1010, "SQ", items)...)
	return reUID(t, insertBeforePixelData(t, corpusFile(t, "MR_small.dcm"), extra), last)
}

// TestMetadataSizes guards the bounds of an answer: an instance whose DICOM
// JSON passes 1 MiB is answered without it and says so, never cut; a page
// stops before its DICOM JSON passes 3 MiB, with a token that resumes with
// the instance it stopped before, so no instance is lost or repeated.
func TestMetadataSizes(t *testing.T) {
	f := newPlaceFixture(t)
	// Four instances of about 0.85 MiB each: three fit a page, four do
	// not. One of 1.2 MiB is too large to answer.
	files := [][]byte{hugeHeader(t, 12500, '1'), hugeHeader(t, 12500, '2'), hugeHeader(t, 12500, '3'), hugeHeader(t, 12500, '4'), hugeHeader(t, 17000, '5')}
	a := receiveArchive(t, f, files)
	md := f.as(f.lab1Reader).metadata
	path := a.scans(t)[0]
	res := mustCall[*v1.GetScanMetadataResponse](t, "first page", call(md.GetScanMetadata, scanRequest(path)))
	total := 0
	for _, m := range res.GetInstances() {
		total += len(m.GetDicomJson())
	}
	if total > 3<<20 || len(res.GetInstances()) != 3 || res.GetNextPageToken() == "" {
		t.Fatalf("the first page holds %d instances and %d bytes of JSON with token %q, want 3, at most 3 MiB, and a token", len(res.GetInstances()), total, res.GetNextPageToken())
	}
	for _, m := range res.GetInstances() {
		if m.GetJsonTooLarge() || len(m.GetDicomJson()) < 700<<10 || len(m.GetDicomJson()) > 1<<20 {
			t.Errorf("%s: %d bytes of JSON, too_large %v", m.GetInstanceId(), len(m.GetDicomJson()), m.GetJsonTooLarge())
		}
	}
	r := scanRequest(path)
	r.PageToken = res.GetNextPageToken()
	rest := mustCall[*v1.GetScanMetadataResponse](t, "second page", call(md.GetScanMetadata, r))
	seen := map[string]bool{}
	for _, m := range append(slices.Clone(res.GetInstances()), rest.GetInstances()...) {
		if seen[m.GetInstanceId()] {
			t.Errorf("%s is answered twice", m.GetInstanceId())
		}
		seen[m.GetInstanceId()] = true
	}
	if len(seen) != 5 || rest.GetNextPageToken() != "" {
		t.Fatalf("the pages hold %d instances, next %q, want 5 and the end", len(seen), rest.GetNextPageToken())
	}
	tooLarge := 0
	for _, m := range rest.GetInstances() {
		if m.GetJsonTooLarge() {
			tooLarge++
			if m.GetDicomJson() != "" || m.GetTransferSyntaxUid() == "" {
				t.Errorf("%s: too large but holds %d bytes of JSON and transfer syntax %q", m.GetInstanceId(), len(m.GetDicomJson()), m.GetTransferSyntaxUid())
			}
		}
	}
	if tooLarge != 1 {
		t.Errorf("%d instances are too large, want 1", tooLarge)
	}
}

// TestMetadataDataLoss guards what the catalog holds and the storage does
// not: a scan whose metadata file is gone, and an instance whose file is
// gone, are data loss, never not found and never an empty answer.
func TestMetadataDataLoss(t *testing.T) {
	f := newPlaceFixture(t)
	files, _ := metaFiles(t)
	a := receiveArchive(t, f, files)
	md := f.as(f.lab1Reader).metadata
	var path label.Path
	for _, p := range a.scans(t) {
		if len(a.held(t, p)) > 1 {
			path = p
		}
	}
	held := a.held(t, path)
	// A file of the storage is removed behind the archive's back.
	root := filepath.Join(f.root, "lab1", "mri")
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(held[0].Key))); err != nil {
		t.Fatal(err)
	}
	if o := call(md.GetInstanceMetadata, instanceRequest(path, held[0].SOPInstanceUID)); o.code() != connect.CodeDataLoss {
		t.Errorf("an instance whose file is gone: %v, want data_loss", o.err)
	}
	if o := call(md.GetScanMetadata, scanRequest(path)); o.code() != connect.CodeDataLoss {
		t.Errorf("a scan one of whose files is gone: %v, want data_loss", o.err)
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(sidecarKey(t, path)))); err != nil {
		t.Fatal(err)
	}
	if o := call(md.GetScanMetadata, scanRequest(path)); o.code() != connect.CodeDataLoss || !strings.Contains(o.message(), "metadata file") {
		t.Errorf("a scan whose metadata file is gone: %v, want data_loss naming the metadata file", o.err)
	}
	if o := call(md.GetInstanceMetadata, instanceRequest(path, held[1].SOPInstanceUID)); o.code() != connect.CodeDataLoss {
		t.Errorf("an instance of a scan whose metadata file is gone: %v, want data_loss", o.err)
	}
}

// storedInstance puts file on a directory storage under the key of the
// instance it holds and returns the catalog's row for it and the line of
// the metadata file for it, which holds no data set, as one does that was
// archived before the file kept it.
func storedInstance(t *testing.T, file []byte) (blob.Store, label.Path, tenant.ScanInstance, sidecar.Instance) {
	t.Helper()
	store, err := dir.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	path := label.Path{Subject: "S1", Session: "E1", Scan: "T1"}
	uid := uidOf(t, file)
	key, err := layout.Key{Kind: layout.Instance, Path: path, SOPInstanceUID: uid}.String()
	if err != nil {
		t.Fatal(err)
	}
	w, err := store.Put(context.Background(), key, bytes.NewReader(file))
	if err != nil {
		t.Fatal(err)
	}
	held := tenant.ScanInstance{SOPInstanceUID: uid, Key: key, Length: w.Size, Size: w.Size, SHA256: tenant.SHA256(w.SHA256)}
	return store, path, held, sidecar.Instance{SOPInstanceUID: uid, Key: key, Size: w.Size, SHA256: held.SHA256.String(), DatasetOmitted: true}
}

// TestMetadataHeaderLimit guards the bound of a header read from a file:
// with receiver.header-limit under what the file's header takes, the
// instance is answered without its JSON, with json_too_large set and its
// transfer syntax, and with the limit raised it is answered whole.
func TestMetadataHeaderLimit(t *testing.T) {
	store, path, held, line := storedInstance(t, corpusFile(t, "MR_small.dcm"))
	small, err := api.MetaInstance(context.Background(), store, "lab1", path, 1024, held, line)
	if err != nil {
		t.Fatal(err)
	}
	if !small.GetJsonTooLarge() || small.GetDicomJson() != "" || small.GetTransferSyntaxUid() != "1.2.840.10008.1.2.1" {
		t.Errorf("under the bound: too large %v, %d bytes of JSON, transfer syntax %q", small.GetJsonTooLarge(), len(small.GetDicomJson()), small.GetTransferSyntaxUid())
	}
	whole, err := api.MetaInstance(context.Background(), store, "lab1", path, 1<<20, held, line)
	if err != nil || whole.GetJsonTooLarge() || !strings.Contains(whole.GetDicomJson(), "7FE00010") {
		t.Errorf("within the bound: %v, too large %v", err, whole.GetJsonTooLarge())
	}
}

// TestMetadataAuditFacts guards what the audit entries of the two methods
// name before the handler runs: reader on the tenant the request names, the
// scan's path of labels as the object with a label that is not one written
// "invalid", and an instance id recorded only as present, never as given.
func TestMetadataAuditFacts(t *testing.T) {
	var m api.Methods
	const uid = "2.25.4711"
	scan, instance := nopacsv1connect.MetadataServiceGetScanMetadataProcedure, nopacsv1connect.MetadataServiceGetInstanceMetadataProcedure
	need, f := m.Requirement(instance, &v1.GetInstanceMetadataRequest{Tenant: "lab1", SubjectLabel: "S01", SessionLabel: "E01", ScanLabel: "T1", InstanceId: uid})
	if need != access.OnTenant("lab1", master.RoleReader) || f.Object != "S01/E01/T1" || f.Details["instance"] != "present" {
		t.Errorf("an instance's requirement %+v and facts %+v", need, f)
	}
	for k, v := range f.Details {
		if strings.Contains(v, uid) {
			t.Errorf("the detail %s holds the instance id: %q", k, v)
		}
	}
	need, f = m.Requirement(scan, &v1.GetScanMetadataRequest{Tenant: "lab1", SubjectLabel: "Smith^John", SessionLabel: "E01", ScanLabel: "T1"})
	if need != access.OnTenant("lab1", master.RoleReader) || f.Object != "invalid/E01/T1" {
		t.Errorf("a scan's requirement %+v and facts %+v", need, f)
	}
	if need, _ := m.Requirement(scan, &v1.GetScanMetadataRequest{Tenant: "Lab 1"}); need != access.OnInstallation(master.RoleReader) {
		t.Errorf("a malformed tenant needs %+v, want reader on the installation", need)
	}
}
