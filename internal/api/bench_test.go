package api_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	connect "connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connectinprocess"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// inProcess returns clients that reach e's handlers without the network:
// the same connect.Server and stand-in, through the in-process transport, so
// a benchmark measures the check and the handler, not HTTP.
func (e *env) inProcess(b *testing.B, c caller) clients {
	b.Helper()
	h, err := api.New(api.Config{Access: e.access, Registry: e.registry, Master: e.master, NewID: e.newID, Settings: e.known, Keys: e.keys, Now: e.now})
	if err != nil {
		b.Fatal(err)
	}
	srv := connect.NewServer(e.standIn)
	h.Register(srv)
	set := func(next connect.ClientFunc) connect.ClientFunc {
		return func(ctx context.Context, spec connect.Spec) (connect.ClientStream, error) {
			if info, ok := connect.CallInfoForClientContext(ctx); ok && c.key != "" {
				info.RequestHeader().Set("Authorization", "Bearer "+c.key)
			}
			return next(ctx, spec)
		}
	}
	cl := connect.NewClient(connectinprocess.New(srv), set)
	return clients{
		tenants:  nopacsv1connect.NewTenantServiceClient(cl),
		access:   nopacsv1connect.NewAccessServiceClient(cl),
		settings: nopacsv1connect.NewSettingsServiceClient(cl),
		audit:    nopacsv1connect.NewAuditServiceClient(cl),
		metadata: nopacsv1connect.NewMetadataServiceClient(cl),
		catalog:  nopacsv1connect.NewCatalogServiceClient(cl),
	}
}

// BenchmarkGetTenant measures one read, GetTenant by a key with reader on
// the tenant, through the access check and the handler without the network:
// the check's reads of the key, the tenant and the roles, the handler's read
// of the tenant, and its audit entry handed to the audit writer.
func BenchmarkGetTenant(b *testing.B) {
	e := newEnv(b)
	e.tenant("lab1")
	reader, _ := e.key(e.onTenant("lab1", master.RoleReader))
	cl := e.inProcess(b, reader)
	req := &v1.GetTenantRequest{Name: "lab1"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cl.tenants.GetTenant(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSetSetting measures one change, SetSetting on the installation by
// an installation admin, through the check and the handler without the
// network: the change and its audit entry are one synced transaction of the
// SQLite master, which is most of the cost.
func BenchmarkSetSetting(b *testing.B) {
	e := newEnv(b)
	admin, _ := e.key(onInstallation(master.RoleAdmin))
	cl := e.inProcess(b, admin)
	req := &v1.SetSettingRequest{Setting: &v1.Setting{Name: testRetention, Scope: installation(), Value: "90d"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cl.settings.SetSetting(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkGetTenantSetting measures a read on a tenant's scope, whose audit
// entry goes to the tenant's log through the access package's batching
// writer, so no synced commit is on the request's path: it should cost about
// what a read of the installation does.
func BenchmarkGetTenantSetting(b *testing.B) {
	e := newEnv(b)
	e.tenant("lab1")
	reader, _ := e.key(e.onTenant("lab1", master.RoleReader))
	cl := e.inProcess(b, reader)
	req := &v1.GetSettingRequest{Name: testRetention, Scope: tenantScope("lab1")}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cl.settings.GetSetting(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSetTenantSetting is BenchmarkSetSetting on a tenant's override,
// by the tenant's admin: the change goes to the tenant's database, after a
// read of the installation's default from the master.
func BenchmarkSetTenantSetting(b *testing.B) {
	e := newEnv(b)
	e.tenant("lab1")
	admin, _ := e.key(e.onTenant("lab1", master.RoleAdmin))
	cl := e.inProcess(b, admin)
	req := &v1.SetSettingRequest{Setting: &v1.Setting{Name: testRetention, Scope: tenantScope("lab1"), Value: "90d"}}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := cl.settings.SetSetting(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// readBench is lab1 of a fresh installation after reception of files to a
// directory storage, served through the stand-ins of the download and of
// DICOMweb, with a key that reads it: what the benchmarks of reading need.
type readBench struct {
	e      *env
	p      tenant.Pipeline
	s      *dataServer
	reader caller
}

// newReadBench receives files into lab1 of a new installation.
func newReadBench(b *testing.B, files [][]byte) *readBench {
	b.Helper()
	e := newEnv(b)
	e.tenant("lab1")
	root := filepath.Join(e.dir, "roots", "fast")
	if err := os.MkdirAll(root, 0o700); err != nil {
		b.Fatal(err)
	}
	e.setSetting(installation(), settings.StorageDirectoryRoots, "fast="+root)
	e.setSetting(tenantScope("lab1"), settings.ReceiverPorts, "11112")
	e.setSetting(tenantScope("lab1"), settings.ReceiverAllowAsReceived, "true")
	for _, o := range []outcome{addStorage("lab1", dirStorage("main", "fast", "mri"))(e.as(local)), addReceiver("lab1", dimseReceiver("scanner1", "main", 11112))(e.as(local))} {
		if o.err != nil {
			b.Fatal(o.err)
		}
	}
	reader, _ := e.key(e.onTenant("lab1", master.RoleReader))
	p := receiveInto(b, e, filepath.Join(root, "lab1", "mri"), files)
	return &readBench{e: e, p: p, s: serveWeb(b, e), reader: reader}
}

// firstScan returns the path of lab1's first scan by label.
func (r *readBench) firstScan(b *testing.B) label.Path {
	b.Helper()
	scans, err := r.p.ListScans(context.Background(), tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1})
	if err != nil || len(scans.Items) != 1 {
		b.Fatalf("the scan: %v", err)
	}
	sc := scans.Items[0]
	return label.Path{Subject: sc.SubjectLabel, Session: sc.SessionLabel, Scan: sc.Label}
}

// fetch makes one GET of the path with the headers given in pairs, reads
// the whole body and fails unless the status is want and the body holds
// size bytes (any size when size is negative). It returns the bytes read.
func (r *readBench) fetch(b *testing.B, path string, want int, size int64, headers ...string) int64 {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, r.s.url+path, nil)
	if err != nil {
		b.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+r.reader.key)
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	res, err := r.s.client.Do(req)
	if err != nil {
		b.Fatal(err)
	}
	n, err := io.Copy(io.Discard, res.Body)
	_ = res.Body.Close()
	if err != nil || res.StatusCode != want || (size >= 0 && n != size) {
		b.Fatalf("%d, %d bytes, %v", res.StatusCode, n, err)
	}
	return n
}

// numbered returns MR_small.dcm of the corpus with the last four digits of
// its SOP Instance UID, and of its Series Instance UID when series is not
// zero, replaced by those of 1000 plus each number: a file of its own,
// in a series of its own when asked, of the same length.
func numbered(b *testing.B, sop, series int) []byte {
	b.Helper()
	file := corpusFile(b, "MR_small.dcm")
	u := uidsOfFile(b, file)
	swap := func(file []byte, uid string, n int) []byte {
		return bytes.ReplaceAll(file, []byte(uid), []byte(uid[:len(uid)-4]+strconv.Itoa(1000+n)))
	}
	file = swap(file, u.sop, sop)
	if series != 0 {
		file = swap(file, u.series, series)
	}
	return file
}

// The instances of the benchmarks of bytes: three of 16 MiB in one series
// of one study, so a study's retrieve carries 48 MiB in three parts.
const benchLarge = 16 << 20

// largeFiles returns the three instances.
func largeFiles(b *testing.B) [][]byte {
	return [][]byte{largeFile(b, benchLarge, '5'), largeFile(b, benchLarge, '6'), largeFile(b, benchLarge, '8')}
}

// BenchmarkDownload measures the plain HTTP download of one whole
// instance of 16 MiB from a directory storage, in MB/s, over a loopback
// connection: the check, the catalog's lookup, the read, the SHA-256
// checked as the bytes pass, and the client reading them.
// The object is in the system's cache after the first round, so this is
// the server's own cost, not the disk's.
func BenchmarkDownload(b *testing.B) {
	files := largeFiles(b)
	r := newReadBench(b, files)
	path := downloadPath(r.firstScan(b), uidOf(b, files[0]))
	b.SetBytes(int64(len(files[0])))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.fetch(b, path, http.StatusOK, int64(len(files[0])))
	}
}

// BenchmarkDownloadRange is BenchmarkDownload with a Range of every byte
// but the first: a range is checked for its length only, not against the
// SHA-256 of the whole instance, so nothing in the server has to see its
// bytes and the platform's copy from file to socket may carry it.
func BenchmarkDownloadRange(b *testing.B) {
	files := largeFiles(b)
	r := newReadBench(b, files)
	path := downloadPath(r.firstScan(b), uidOf(b, files[0]))
	size := int64(len(files[0])) - 1
	b.SetBytes(size)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.fetch(b, path, http.StatusPartialContent, size, "Range", "bytes=1-")
	}
}

// BenchmarkReadFiles measures ReadFiles of a scan of three instances of
// 16 MiB over a loopback connection, in MB/s: 1 MiB chunks, each checked
// against its instance's SHA-256 as it passes, marshaled and sent, and
// the client receiving every message.
func BenchmarkReadFiles(b *testing.B) {
	files := largeFiles(b)
	r := newReadBench(b, files)
	path := r.firstScan(b)
	req := &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan}
	total := int64(0)
	for _, f := range files {
		total += int64(len(f))
	}
	cl := r.e.as(r.reader)
	b.SetBytes(total)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		st, err := cl.data.ReadFiles(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
		got := int64(0)
		for {
			m, err := st.Receive()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				b.Fatal(err)
			}
			got += int64(len(m.GetChunk().GetData()))
		}
		_ = st.Close()
		if got != total {
			b.Fatalf("%d bytes, want %d", got, total)
		}
	}
}

// BenchmarkRetrieveStudy measures the WADO-RS retrieve of a study of
// three instances of 16 MiB (PS3.18 10.4), a multipart body of 48 MiB,
// in MB/s, each instance checked against its SHA-256 as it passes.
func BenchmarkRetrieveStudy(b *testing.B) {
	files := largeFiles(b)
	r := newReadBench(b, files)
	path := webPath("studies/" + uidsOfFile(b, files[0]).study)
	total := int64(0)
	for _, f := range files {
		total += int64(len(f))
	}
	b.SetBytes(total)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if n := r.fetch(b, path, http.StatusOK, -1, "Accept", `multipart/related; type="application/dicom"`); n < total {
			b.Fatalf("%d bytes, want at least %d", n, total)
		}
	}
}

// BenchmarkRetrieveFrames measures the WADO-RS retrieve of the one frame
// of an instance (PS3.18 10.4.1.1.4), 8 KiB of native pixel data, in
// ns/op: the catalog's lookup, the frame table of the sidecar, and the
// ranged read of the storage.
func BenchmarkRetrieveFrames(b *testing.B) {
	files := largeFiles(b)
	r := newReadBench(b, files)
	u := uidsOfFile(b, files[0])
	path := webPath("studies/" + u.study + "/series/" + u.series + "/instances/" + u.sop + "/frames/1")
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r.fetch(b, path, http.StatusOK, -1, "Accept", `multipart/related; type="application/octet-stream"`)
	}
}

// BenchmarkGetScanMetadata measures GetScanMetadata of a scan of 300
// instances, a page of all of them, through the check and the handler
// without the network: the catalog's page, each instance's sidecar read
// from the storage, and its DICOM JSON written.
func BenchmarkGetScanMetadata(b *testing.B) {
	files := make([][]byte, 300)
	for i := range files {
		files[i] = numbered(b, i, 0)
	}
	r := newReadBench(b, files)
	req := scanRequest(r.firstScan(b))
	req.PageSize = 1000
	cl := r.e.inProcess(b, r.reader)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		res, err := cl.metadata.GetScanMetadata(context.Background(), req)
		if err != nil || len(res.GetInstances()) != len(files) {
			b.Fatalf("%d instances, %v", len(res.GetInstances()), err)
		}
	}
}

// BenchmarkListScans measures a page of 100 scans of ListScans by label,
// through the check and the handler without the network: the catalog's
// indexed page, the conversion of each row and the audit entry.
func BenchmarkListScans(b *testing.B) {
	files := make([][]byte, 100)
	for i := range files {
		files[i] = numbered(b, i, i+1)
	}
	r := newReadBench(b, files)
	req := &v1.ListScansRequest{Tenant: "lab1", Order: v1.CatalogOrder_CATALOG_ORDER_LABEL, PageSize: 100}
	cl := r.e.inProcess(b, r.reader)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		res, err := cl.catalog.ListScans(context.Background(), req)
		if err != nil || len(res.GetScans()) != len(files) {
			b.Fatalf("%d scans, %v", len(res.GetScans()), err)
		}
	}
}
