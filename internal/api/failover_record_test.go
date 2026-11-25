package api_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// replicaRow returns the replica "copy"'s row of the scan at path.
func (r *replicaFixture) replicaRow(t *testing.T, path label.Path) tenant.ReplicaScan {
	t.Helper()
	ctx := context.Background()
	scan, err := r.a.p.CatalogScan(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	row, err := r.a.p.ReplicaScan(ctx, "copy", scan.SeriesUID)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

// webInstance returns the DICOMweb path of the instance uid of the scan at
// path.
func (r *replicaFixture) webInstance(t *testing.T, path label.Path, uid string) string {
	t.Helper()
	ctx := context.Background()
	scan, err := r.a.p.CatalogScan(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.a.p.CatalogSession(ctx, path.Subject, path.Session)
	if err != nil {
		t.Fatal(err)
	}
	return webPath("studies/" + sess.StudyUID + "/series/" + scan.SeriesUID + "/instances/" + uid)
}

// lastEntry returns the last entry of lab1's audit log made by method.
func (r *replicaFixture) lastEntry(t *testing.T, method string) master.AuditEntry {
	t.Helper()
	var last *master.AuditEntry
	for _, en := range r.f.tenantLog("lab1") {
		if en.Method == method {
			last = &en
		}
	}
	if last == nil {
		t.Fatalf("no entry of %s", method)
	}
	return *last
}

// TestMissingMetadataFileKeepsTheReplicaServing guards a scan whose
// metadata file neither the primary nor the replica in step holds (a scan
// archived before metadata files were written, which the replicator does
// not copy): the metadata read fails as data loss, the replica's row stays
// in step, and an instance the replica served before it is served after it
// as well.
func TestMissingMetadataFileKeepsTheReplicaServing(t *testing.T) {
	r := newReplicaFixture(t)
	big := largeFile(t, 5<<19, '5')
	path, in := r.instance(t, int64(len(big)))
	ctx := context.Background()
	key, err := layout.MetadataKey(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []interface {
		Delete(context.Context, string) error
	}{r.a.store, r.copy} {
		if err := st.Delete(ctx, key); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.a.store.Delete(ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	c := r.f.lab1Reader
	if got := r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), c); got.status != http.StatusOK || !bytes.Equal(got.body, big) {
		t.Fatalf("the download before: %d, %d bytes; want the archived bytes", got.status, len(got.body))
	}
	_, err = r.f.as(c).metadata.GetInstanceMetadata(ctx, instanceRequest(path, in.SOPInstanceUID))
	if connect.CodeOf(err) != connect.CodeDataLoss {
		t.Fatalf("the metadata with no metadata file: %v, want data loss", err)
	}
	if row := r.replicaRow(t, path); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after the metadata read the replica's row is %+v; want in step, nothing damaged", row)
	}
	got := r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), c)
	if got.status != http.StatusOK || got.readErr != nil || !bytes.Equal(got.body, big) {
		t.Errorf("the download after: %d, %d bytes, %v; want the archived bytes from the replica", got.status, len(got.body), got.readErr)
	}
}

// TestWholeRangeIsChecked guards a Range that covers every byte of the
// instance (0 to its last byte, or bytes=0-): it is the whole instance
// under 206, so it is checked against the catalog's SHA-256 as a 200 is.
// The replica's right bytes are answered whole and byte for byte; its
// wrong bytes cut the answer short and are recorded as its damage.
func TestWholeRangeIsChecked(t *testing.T) {
	r := newReplicaFixture(t)
	big := largeFile(t, 5<<19, '5')
	path, in := r.instance(t, int64(len(big)))
	ctx := context.Background()
	if err := r.a.store.Delete(ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	c := r.f.lab1Reader
	whole := "bytes=0-" + strconv.Itoa(len(big)-1)
	got := r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), c, "Range", whole)
	if got.status != http.StatusPartialContent || got.readErr != nil || !bytes.Equal(got.body, big) {
		t.Fatalf("%s from the replica: %d, %d bytes, %v; want 206 with the archived bytes", whole, got.status, len(got.body), got.readErr)
	}
	bad := bytes.Clone(big)
	bad[len(bad)-1] ^= 0xFF
	if _, err := r.copy.Put(ctx, in.Key, bytes.NewReader(bad)); err != nil {
		t.Fatal(err)
	}
	got = r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), c, "Range", "bytes=0-")
	if got.status != http.StatusPartialContent || got.readErr == nil || len(got.body) >= len(big) {
		t.Fatalf("bytes=0- of a replica's wrong bytes: %d, %d bytes, %v; want 206 cut short", got.status, len(got.body), got.readErr)
	}
	if row := r.replicaRow(t, path); row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("the replica's row is %+v; want damaged with one object", row)
	}
}

// TestMetadataNamesTheStoragesThatServed guards the audit entries of the
// metadata service: with the scan's metadata file missing from the
// primary, GetInstanceMetadata and GetScanMetadata answer from the replica
// in step, and each entry names the replica as having served it.
func TestMetadataNamesTheStoragesThatServed(t *testing.T) {
	r := newReplicaFixture(t)
	big := largeFile(t, 5<<19, '5')
	path, in := r.instance(t, int64(len(big)))
	ctx := context.Background()
	key, err := layout.MetadataKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.a.store.Delete(ctx, key); err != nil {
		t.Fatal(err)
	}
	m := r.f.as(r.f.lab1Reader).metadata
	if _, err := m.GetInstanceMetadata(ctx, instanceRequest(path, in.SOPInstanceUID)); err != nil {
		t.Fatalf("GetInstanceMetadata: %v", err)
	}
	if en := r.lastEntry(t, nopacsv1connect.MetadataServiceGetInstanceMetadataProcedure); !strings.Contains(en.Details["served_by"], "copy") {
		t.Errorf("GetInstanceMetadata's entry is %+v, want served by copy", en)
	}
	if _, err := m.GetScanMetadata(ctx, scanRequest(path)); err != nil {
		t.Fatalf("GetScanMetadata: %v", err)
	}
	if en := r.lastEntry(t, nopacsv1connect.MetadataServiceGetScanMetadataProcedure); !strings.Contains(en.Details["served_by"], "copy") || en.Details["instances"] == "" {
		t.Errorf("GetScanMetadata's entry is %+v, want served by copy, with its count of instances", en)
	}
}

// TestFramesTurnToTheReplica guards the DICOMweb retrieval of a frame when
// the primary's object is missing: the frame's bytes are those the primary
// answered before, and the entry names the replica.
func TestFramesTurnToTheReplica(t *testing.T) {
	r := newReplicaFixture(t)
	ctx := context.Background()
	c := r.f.lab1Reader
	const accept = `multipart/related; type="application/octet-stream"`
	for _, p := range r.a.scans(t) {
		for _, h := range r.a.held(t, p) {
			url := r.webInstance(t, p, h.SOPInstanceUID) + "/frames/1"
			before := r.s.get(t, http.MethodGet, url, c, "Accept", accept)
			if before.status != http.StatusOK {
				continue
			}
			want := partsOf(t, before, "application/octet-stream")
			if err := r.a.store.Delete(ctx, h.Key); err != nil {
				t.Fatal(err)
			}
			after := r.s.get(t, http.MethodGet, url, c, "Accept", accept)
			if after.status != http.StatusOK || after.readErr != nil {
				t.Fatalf("the frame from the replica: %d, %v", after.status, after.readErr)
			}
			got := partsOf(t, after, "application/octet-stream")
			if len(got) != 1 || len(want) != 1 || !bytes.Equal(got[0].body, want[0].body) {
				t.Errorf("the frame from the replica is not the frame the primary answered")
			}
			if en := entriesOf(r.f.tenantLog("lab1"), after.header.Get(api.RequestIDHeader)); len(en) != 1 || !strings.Contains(en[0].Details["served_by"], "copy") {
				t.Errorf("the frame's entry is %+v, want served by copy", en)
			}
			return
		}
	}
	t.Fatal("no instance answers a frame")
}

// TestHeadAndAReplicaNotThereRecordNothing guards what a read that finds no
// replica's bytes records: HEAD with the object missing on both storages
// fails and leaves the replica's row in step (it asks for no byte; the GET
// records), and a GET while the replica's storage cannot be opened fails
// and records nothing either, since a storage not answering says nothing of
// what it holds.
func TestHeadAndAReplicaNotThereRecordNothing(t *testing.T) {
	r := newReplicaFixture(t)
	big := largeFile(t, 5<<19, '5')
	path, in := r.instance(t, int64(len(big)))
	ctx := context.Background()
	if err := r.a.store.Delete(ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	c := r.f.lab1Reader
	root := filepath.Join(r.f.root, "lab1", "copy")
	if err := os.Rename(root, root+".moved"); err != nil {
		t.Fatal(err)
	}
	if got := r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), c); got.status == http.StatusOK {
		t.Errorf("a GET with the replica not there: %d, want a failure", got.status)
	}
	if row := r.replicaRow(t, path); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after a GET with the replica not there, the row is %+v; want in step", row)
	}
	if err := os.Rename(root+".moved", root); err != nil {
		t.Fatal(err)
	}
	if err := r.copy.Delete(ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	if head := r.s.get(t, http.MethodHead, downloadPath(path, in.SOPInstanceUID), c); head.status == http.StatusOK {
		t.Errorf("HEAD with the object nowhere: %d, want a failure", head.status)
	}
	if row := r.replicaRow(t, path); row.State != tenant.ReplicaInStep || row.Damaged != 0 {
		t.Errorf("after HEAD the row is %+v; want in step", row)
	}
	if got := r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), c); got.status == http.StatusOK {
		t.Errorf("a GET with the object nowhere: %d, want a failure", got.status)
	}
	if row := r.replicaRow(t, path); row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("after the GET the row is %+v; want the replica's miss recorded", row)
	}
}
