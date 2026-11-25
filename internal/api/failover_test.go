package api_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/dir"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// replicaFixture is lab1 after reception of the data tests' files, with
// the directory storage "copy" a replica of its storage "main", holding a
// copy of every object and in step for every scan, and the download and
// DICOMweb stand-ins over it.
type replicaFixture struct {
	f    *placeFixture
	a    *archive
	s    *dataServer
	copy blob.Store
}

// newReplicaFixture receives the files and makes the replica.
func newReplicaFixture(t *testing.T) *replicaFixture {
	t.Helper()
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{largeFile(t, 5<<19, '5'), corpusFile(t, "MR_small.dcm"), corpusFile(t, "ct2n-6293.dcm")})
	root := filepath.Join(f.root, "lab1", "copy")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cp, err := dir.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cp.Close() })
	ctx := context.Background()
	audit := func() master.AuditEntry {
		return master.AuditEntry{ID: "a" + f.newID(), Time: time.Now(), KeyID: "K1234567890a", RemoteAddress: "192.0.2.10:50000",
			Method: "Test", TenantName: "lab1", Outcome: master.OutcomeDone, RequestID: "r1"}
	}
	if _, err := a.p.AddStorage(ctx, tenant.Storage{Name: "copy", Type: tenant.StorageDirectory, Tier: tenant.TierOnline,
		Directory: tenant.DirectoryStorage{Root: "fast", Path: "copy"}}, time.Now(), audit(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := a.p.AttachReplica(ctx, "copy", "main", time.Now(), audit(), nil); err != nil {
		t.Fatal(err)
	}
	opts := blob.ListOptions{}
	for {
		page, err := a.store.List(ctx, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range page.Keys {
			if _, err := cp.Put(ctx, k, bytes.NewReader(a.object(t, k))); err != nil {
				t.Fatal(err)
			}
		}
		if page.Next == "" {
			break
		}
		opts.Token = page.Next
	}
	// In step at the catalog's digest, as the replicator records a copy it
	// verified.
	now := time.Now()
	claims, err := a.p.ClaimReplicaScans(ctx, "test", now, time.Minute, tenant.MaxReplicaClaim)
	if err != nil || len(claims) == 0 {
		t.Fatalf("claims: %d, %v", len(claims), err)
	}
	for _, c := range claims {
		if state, err := a.p.EndReplicaScan(ctx, c.Fence, c.ManifestSHA256, now); err != nil || state != tenant.ReplicaInStep {
			t.Fatalf("in step: %s, %v", state, err)
		}
	}
	return &replicaFixture{f: f, a: a, s: serveWeb(t, f.env), copy: cp}
}

// instance returns the held instance of size bytes and its scan's path.
func (r *replicaFixture) instance(t *testing.T, size int64) (label.Path, tenant.ScanInstance) {
	t.Helper()
	for _, p := range r.a.scans(t) {
		for _, h := range r.a.held(t, p) {
			if h.Size == size {
				return p, h
			}
		}
	}
	t.Fatalf("no instance of %d bytes", size)
	return label.Path{}, tenant.ScanInstance{}
}

// TestReadsTurnToTheReplicaInStep guards the readers of the API when the
// primary's object is missing: the download (GET and HEAD), the DICOMweb
// retrieval of the instance and ReadFiles each answer the archived bytes
// whole from the replica in step, and the audit entry of each names the
// storages that served it: the replica, and the primary for what it still
// served (the scan's metadata file, the scan's other instances).
func TestReadsTurnToTheReplicaInStep(t *testing.T) {
	r := newReplicaFixture(t)
	big := largeFile(t, 5<<19, '5')
	path, in := r.instance(t, int64(len(big)))
	if err := r.a.store.Delete(context.Background(), in.Key); err != nil {
		t.Fatal(err)
	}
	c := r.f.lab1Reader
	got := r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), c)
	if got.status != http.StatusOK || got.readErr != nil || !bytes.Equal(got.body, big) {
		t.Fatalf("the download: %d, %d bytes, %v; want the archived bytes", got.status, len(got.body), got.readErr)
	}
	head := r.s.get(t, http.MethodHead, downloadPath(path, in.SOPInstanceUID), c)
	if head.status != http.StatusOK {
		t.Errorf("HEAD: %d, want 200", head.status)
	}
	for _, id := range []string{got.header.Get(api.RequestIDHeader), head.header.Get(api.RequestIDHeader)} {
		en := entriesOf(r.f.tenantLog("lab1"), id)
		if len(en) != 1 || en[0].Outcome != master.OutcomeDone || en[0].Details["served_by"] != "copy" {
			t.Errorf("the download's entry is %+v, want served by copy", en)
		}
	}

	scan, err := r.a.p.CatalogScan(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := r.a.p.CatalogSession(context.Background(), path.Subject, path.Session)
	if err != nil {
		t.Fatal(err)
	}
	web := r.s.get(t, http.MethodGet, webPath("studies/"+sess.StudyUID+"/series/"+scan.SeriesUID+"/instances/"+in.SOPInstanceUID), c,
		"Accept", "application/dicom")
	if web.status != http.StatusOK || !bytes.Equal(web.body, big) {
		t.Errorf("the DICOMweb retrieval: %d, %d bytes; want the archived bytes", web.status, len(web.body))
	}
	// The scan's metadata file is read from the primary, which holds it;
	// the instance's bytes from the replica.
	if en := entriesOf(r.f.tenantLog("lab1"), web.header.Get(api.RequestIDHeader)); len(en) != 1 || en[0].Details["served_by"] != "main,copy" {
		t.Errorf("the DICOMweb entry is %+v, want served by main and copy", en)
	}

	fs := r.s.readFiles(c, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan})
	if fs.err != nil {
		t.Fatalf("ReadFiles: %v", fs.err)
	}
	files, _ := assemble(t, fs.msgs)
	whole := false
	for _, f := range files {
		if f.file.GetInstanceId() == in.SOPInstanceUID {
			whole = bytes.Equal(f.bytes, big)
		}
	}
	if !whole {
		t.Error("ReadFiles did not send the instance whole from the replica")
	}
	ended := false
	for _, e := range entriesOf(r.f.tenantLog("lab1"), fs.requestID) {
		if e.Details["stream"] == "ended" {
			ended = true
			if e.Details["served_by"] != "main,copy" && e.Details["served_by"] != "copy,main" {
				t.Errorf("the stream's end names %q as served by, want main and copy", e.Details["served_by"])
			}
		}
	}
	if !ended {
		t.Error("no entry of the stream's end")
	}
}

// TestReplicaWithWrongBytesCutsTheDownload guards a replica whose bytes are
// not the catalog's: the download is cut short as for a damaged primary,
// the replica's row of the scan is recorded damaged for the replicator to
// copy again, and no wrong byte is passed as whole.
func TestReplicaWithWrongBytesCutsTheDownload(t *testing.T) {
	r := newReplicaFixture(t)
	big := largeFile(t, 5<<19, '5')
	path, in := r.instance(t, int64(len(big)))
	ctx := context.Background()
	if err := r.a.store.Delete(ctx, in.Key); err != nil {
		t.Fatal(err)
	}
	bad := bytes.Clone(big)
	bad[len(bad)-1] ^= 0xFF
	if _, err := r.copy.Put(ctx, in.Key, bytes.NewReader(bad)); err != nil {
		t.Fatal(err)
	}
	got := r.s.get(t, http.MethodGet, downloadPath(path, in.SOPInstanceUID), r.f.lab1Reader)
	if got.status != http.StatusOK || got.readErr == nil || len(got.body) >= len(big) {
		t.Fatalf("a replica's wrong bytes: %d, %d bytes, %v; want a body cut short", got.status, len(got.body), got.readErr)
	}
	scan, err := r.a.p.CatalogScan(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	row, err := r.a.p.ReplicaScan(ctx, "copy", scan.SeriesUID)
	if err != nil || row.State != tenant.ReplicaDamaged || row.Damaged != 1 {
		t.Errorf("the replica's row is %+v, %v; want damaged with one object", row, err)
	}
}
