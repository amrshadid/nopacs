package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/scrub"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// scrubTenant is lab1 alone, as the scrub supervisor reaches it.
type scrubTenant struct{ p tenant.Pipeline }

// Names answers lab1.
func (s scrubTenant) Names(context.Context) ([]string, error) { return []string{"lab1"}, nil }

// Store answers lab1's database.
func (s scrubTenant) Store(string) (tenant.Store, error) { return s.p, nil }

// scrubSettings answer a scrub's settings: unpaced, never scheduled.
type scrubSettings struct{}

// FailuresKept answers job.failures-kept.
func (scrubSettings) FailuresKept(context.Context) (int, error) {
	return tenant.DefaultFailuresKept, nil
}

// ScrubRate answers scrub.rate.
func (scrubSettings) ScrubRate(context.Context) (int64, error) { return 64 << 30, nil }

// ScrubInterval answers scrub.interval, "0".
func (scrubSettings) ScrubInterval(context.Context) (time.Duration, error) { return 0, nil }

// ArchiveSample answers scrub.archive-sample: no object of an archive tier
// read.
func (scrubSettings) ArchiveSample(context.Context) (float64, error) { return 0, nil }

// oneStorage opens lab1's storage for every name, and keeps it open.
type oneStorage struct{ s blob.Store }

// Get answers the storage.
func (o oneStorage) Get(context.Context, string) (blob.Store, error) { return o.s, nil }

// Close keeps it open: the test closes it.
func (oneStorage) Close() {}

// hashCheck is the outside check of a storage: Python's hashlib reads each
// object the catalog names straight from the storage's directory and
// answers which are missing and which have another size or SHA-256 than the
// catalog's, without any of NoPACs' code.
const hashCheck = `
import hashlib, json, os, sys
job = json.load(sys.stdin)
missing, damaged = [], []
for o in job["objects"]:
    path = os.path.join(job["root"], *o["key"].split("/"))
    if not os.path.isfile(path):
        missing.append(o["key"])
        continue
    with open(path, "rb") as f:
        data = f.read()
    if len(data) != o["size"] or hashlib.sha256(data).hexdigest() != o["sha256"]:
        damaged.append(o["key"])
print(json.dumps({"missing": sorted(missing), "damaged": sorted(damaged)}))
`

// outsideHashes runs hashCheck over the objects of the storage at root, or
// skips the test when no Python is there.
func outsideHashes(t *testing.T, root string, held []tenant.ScanInstance) (missing, damaged []string) {
	t.Helper()
	py := os.Getenv("NOPACS_PYTHON")
	if py == "" {
		var err error
		if py, err = exec.LookPath("python3"); err != nil {
			t.Skip("no Python: set NOPACS_PYTHON or put python3 on the PATH for the outside check of a scrub")
		}
	}
	type object struct {
		Key    string `json:"key"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	}
	in := struct {
		Root    string   `json:"root"`
		Objects []object `json:"objects"`
	}{Root: root}
	for _, h := range held {
		in.Objects = append(in.Objects, object{Key: h.Key, Size: h.Size, SHA256: h.ArchivedSHA256.String()})
	}
	body, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-I", "-c", hashCheck) // #nosec G204 G702 -- a test's own interpreter and script.
	cmd.Stdin = bytes.NewReader(body)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the outside check failed: %v", err)
	}
	var got struct {
		Missing []string `json:"missing"`
		Damaged []string `json:"damaged"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("the outside check answered %q: %v", out, err)
	}
	return got.Missing, got.Damaged
}

// TestScrubOfARealArchiveAgreesWithHashlib guards a scrub end to end, from
// an archive made by reception of public corpus files on a directory
// storage: one byte flipped in one archived file and another file deleted,
// a scrub finds exactly those two, damaged and missing, and Python's
// hashlib, reading the storage's files itself, finds exactly the same; the
// scrub writes nothing to the storage (every object and checksum as before
// but for the test's own changes); and once marked, a download of the
// damaged instance fails as data loss before its first byte (no status of
// success, no header of the instance, no byte), as does its metadata (the
// metadata service's and DICOMweb's); a read of files announces a marked
// instance and ends as data loss before any of its bytes, so a client
// resuming after the announced instance reads every other instance whole;
// and the instance read good answers as before.
func TestScrubOfARealArchiveAgreesWithHashlib(t *testing.T) {
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, [][]byte{corpusFile(t, "MR_small.dcm"), corpusFile(t, "ct2n-6293.dcm"), corpusFile(t, "ct5n-2062.dcm")})
	ctx := context.Background()
	var held []tenant.ScanInstance
	paths := map[string]label.Path{}
	for _, p := range a.scans(t) {
		for _, h := range a.held(t, p) {
			held = append(held, h)
			paths[h.SOPInstanceUID] = p
		}
	}
	if len(held) != 3 {
		t.Fatalf("the archive holds %d instances, want 3", len(held))
	}
	sort.Slice(held, func(i, j int) bool { return held[i].Key < held[j].Key })
	damagedIn, missingIn, wholeIn := held[0], held[1], held[2]
	b := a.object(t, damagedIn.Key)
	b[len(b)/3] ^= 0x01
	if _, err := a.store.Put(ctx, damagedIn.Key, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Delete(ctx, missingIn.Key); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(f.root, "lab1", "mri")
	pyMissing, pyDamaged := outsideHashes(t, root, held)
	if !slices.Equal(pyMissing, []string{missingIn.Key}) || !slices.Equal(pyDamaged, []string{damagedIn.Key}) {
		t.Fatalf("hashlib finds missing %v and damaged %v, want the two changed", pyMissing, pyDamaged)
	}
	before := storageHashes(t, a)

	// The job's moment is after every commit of the reception, whose clock
	// is the wall's: the instances it scrubs are those held by then.
	f.advance(time.Since(t0) + time.Minute)
	sup, err := scrub.New(scrub.Config{Tenants: scrubTenant{p: a.p}, TenantSettings: func(tenant.Store) scrub.TenantSettings { return scrubSettings{} },
		OpenStorages: func(string, tenant.Pipeline) scrub.Storages { return oneStorage{s: a.store} },
		Reads:        &checkedread.Slots{}, NewID: f.newID, Now: f.now, Interval: time.Second, PollInterval: 20 * time.Millisecond,
		Keys: f.keys, TenantID: func(ctx context.Context, name string) (string, error) {
			row, err := f.master.TenantByName(ctx, name)
			return row.ID, err
		}})
	if err != nil {
		t.Fatal(err)
	}
	made, err := f.as(f.lab1Operator).jobs.CreateJob(ctx, &v1.CreateJobRequest{Tenant: "lab1",
		Kind: &v1.CreateJobRequest_Scrub{Scrub: &v1.ScrubJob{Storage: "main"}}})
	if err != nil {
		t.Fatalf("CreateJob of a scrub: %v", err)
	}
	if sj := made.GetJob().GetScrub(); sj.GetStorage() != "main" || sj.GetScheduled() {
		t.Errorf("the scrub made reads %+v", made.GetJob())
	}
	if err := sup.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sup.Stop(context.Background()) })
	id := made.GetJob().GetId()
	var j tenant.Job
	for deadline := time.Now().Add(time.Minute); ; {
		if j, err = a.p.Job(ctx, id); err != nil {
			t.Fatal(err)
		}
		if j.State.Ended() || time.Now().After(deadline) {
			break
		}
		sup.Poke("lab1")
		time.Sleep(20 * time.Millisecond)
	}
	if j.State != tenant.JobPartial || j.Counts.InstancesSent != 1 || j.Counts.InstancesRefused != 2 {
		t.Fatalf("the scrub ended as %+v", j)
	}
	findings, _, err := a.p.ListScrubFindings(ctx, tenant.ScrubFindingFilter{}, master.Page{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	var gotMissing, gotDamaged, other []string
	for _, fd := range findings {
		switch fd.Kind {
		case tenant.FindingMissing:
			gotMissing = append(gotMissing, fd.Key)
		case tenant.FindingDamaged:
			gotDamaged = append(gotDamaged, fd.Key)
		default:
			other = append(other, string(fd.Kind)+" "+fd.Key)
		}
	}
	if !slices.Equal(gotMissing, pyMissing) || !slices.Equal(gotDamaged, pyDamaged) || len(other) != 0 {
		t.Errorf("the scrub found missing %v, damaged %v and %v; hashlib found missing %v and damaged %v", gotMissing, gotDamaged, other,
			pyMissing, pyDamaged)
	}
	if after := storageHashes(t, a); !maps.Equal(before, after) {
		t.Errorf("the scrub changed the storage: %d objects before, %d after", len(before), len(after))
	}

	s := serveData(t, f.env)
	bad := s.get(t, http.MethodGet, downloadPath(paths[damagedIn.SOPInstanceUID], damagedIn.SOPInstanceUID), f.lab1Reader)
	if bad.status != http.StatusInternalServerError || len(bad.body) != 0 || bad.header.Get("ETag") != "" ||
		bad.header.Get("Content-Type") == "application/dicom" {
		t.Errorf("a download of the marked instance: %d, %d bytes, headers %v; want data loss before its first byte", bad.status,
			len(bad.body), bad.header)
	}
	ranged := s.get(t, http.MethodGet, downloadPath(paths[damagedIn.SOPInstanceUID], damagedIn.SOPInstanceUID), f.lab1Reader,
		"Range", "bytes=0-9")
	if ranged.status != http.StatusInternalServerError || len(ranged.body) != 0 {
		t.Errorf("a range of the marked instance: %d, %d bytes; want data loss", ranged.status, len(ranged.body))
	}
	good := s.get(t, http.MethodGet, downloadPath(paths[wholeIn.SOPInstanceUID], wholeIn.SOPInstanceUID), f.lab1Reader)
	if good.status != http.StatusOK || !bytes.Equal(good.body, a.object(t, wholeIn.Key)) {
		t.Errorf("a download of the instance read good: %d, %d bytes", good.status, len(good.body))
	}
	p := paths[damagedIn.SOPInstanceUID]
	sc, err := a.p.CatalogScan(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	se, err := a.p.CatalogSession(ctx, p.Subject, p.Session)
	if err != nil {
		t.Fatal(err)
	}
	web := serveWeb(t, f.env)
	instance := "studies/" + se.StudyUID + "/series/" + sc.SeriesUID + "/instances/" + damagedIn.SOPInstanceUID
	for _, r := range []struct{ path, accept string }{
		{instance, `multipart/related; type="application/dicom"`},
		{instance + "/frames/1", `multipart/related; type="application/octet-stream"`},
		{"studies/" + se.StudyUID + "/series/" + sc.SeriesUID, `multipart/related; type="application/dicom"`},
	} {
		got := web.get(t, http.MethodGet, webPath(r.path), f.lab1Reader, "Accept", r.accept)
		if got.status != http.StatusInternalServerError || len(got.body) != 0 {
			t.Errorf("a DICOMweb retrieve of %s: %d, %d bytes; want data loss before its first byte", r.path, got.status, len(got.body))
		}
	}
	stream := s.readFiles(f.lab1Reader, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: p.Subject, SessionLabel: p.Session,
		ScanLabel: p.Scan, InstanceId: damagedIn.SOPInstanceUID})
	if connect.CodeOf(stream.err) != connect.CodeDataLoss {
		t.Errorf("a read of files naming the marked instance ended with %v, want data loss", stream.err)
	}
	announced := 0
	for _, m := range stream.msgs {
		if m.GetChunk() != nil {
			t.Errorf("a read of files sent bytes of the marked instance: %d", len(m.GetChunk().GetData()))
		}
		if fl := m.GetFile(); fl != nil {
			announced++
			if fl.GetInstanceId() != damagedIn.SOPInstanceUID {
				t.Errorf("a read of files naming the marked instance announced %s", fl.GetInstanceId())
			}
		}
	}
	if announced != 1 {
		t.Errorf("a read of files naming the marked instance announced it %d times, want once before the data loss", announced)
	}
	var whole []string
	endings := 0
	subjects := map[string]bool{}
	for _, sp := range paths {
		subjects[sp.Subject] = true
	}
	for sub := range subjects {
		w, n := resumeThrough(t, s, f, sub)
		whole, endings = append(whole, w...), endings+n
	}
	if !slices.Equal(whole, []string{wholeIn.SOPInstanceUID}) || endings != 2 {
		t.Errorf("reading the subject, resumed after each marked instance announced, received %v whole after %d data losses; "+
			"want the instance read good, after one data loss for each marked instance", whole, endings)
	}

	// The headers of a marked instance are not answered as the archive's
	// either: data loss, by the metadata service and by DICOMweb.
	_, err = f.as(f.lab1Reader).metadata.GetInstanceMetadata(ctx, &v1.GetInstanceMetadataRequest{Tenant: "lab1",
		SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan, InstanceId: damagedIn.SOPInstanceUID})
	if connect.CodeOf(err) != connect.CodeDataLoss {
		t.Errorf("the metadata of the marked instance answered %v, want data loss", err)
	}
	_, err = f.as(f.lab1Reader).metadata.GetScanMetadata(ctx, &v1.GetScanMetadataRequest{Tenant: "lab1",
		SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan})
	if connect.CodeOf(err) != connect.CodeDataLoss {
		t.Errorf("the metadata of the marked instance's scan answered %v, want data loss", err)
	}
	meta := web.get(t, http.MethodGet, webPath(instance+"/metadata"), f.lab1Reader, "Accept", "application/dicom+json")
	if meta.status != http.StatusInternalServerError || len(meta.body) != 0 {
		t.Errorf("the DICOMweb metadata of the marked instance: %d, %d bytes; want data loss before its first byte", meta.status, len(meta.body))
	}
	wp := paths[wholeIn.SOPInstanceUID]
	if _, err := f.as(f.lab1Reader).metadata.GetInstanceMetadata(ctx, &v1.GetInstanceMetadataRequest{Tenant: "lab1",
		SubjectLabel: wp.Subject, SessionLabel: wp.Session, ScanLabel: wp.Scan, InstanceId: wholeIn.SOPInstanceUID}); err != nil {
		t.Errorf("the metadata of the instance read good: %v", err)
	}
}

// resumeThrough reads every instance of the subject with ReadFiles,
// calling again with start_after at the last instance announced each time
// a stream ends as data loss, and returns the instances received whole and
// the number of streams that ended so.
func resumeThrough(t *testing.T, s *dataServer, f *placeFixture, subject string) ([]string, int) {
	t.Helper()
	var whole []string
	endings := 0
	var after *v1.ReadPosition
	for range 10 {
		st := s.readFiles(f.lab1Reader, &v1.ReadFilesRequest{Tenant: "lab1", SubjectLabel: subject, StartAfter: after})
		var last *v1.ArchivedFile
		var got int64
		for _, m := range st.msgs {
			if fl := m.GetFile(); fl != nil {
				if last != nil && got == last.GetLength() {
					whole = append(whole, last.GetInstanceId())
				}
				last, got = fl, 0
			}
			if c := m.GetChunk(); c != nil {
				got += int64(len(c.GetData()))
			}
		}
		if st.err == nil {
			if last != nil && got == last.GetLength() {
				whole = append(whole, last.GetInstanceId())
			}
			return whole, endings
		}
		if connect.CodeOf(st.err) != connect.CodeDataLoss || last == nil || got != 0 {
			t.Fatalf("a read of files ended with %v after %d bytes of the last instance announced", st.err, got)
		}
		endings++
		after = &v1.ReadPosition{SessionLabel: last.GetSessionLabel(), ScanLabel: last.GetScanLabel(), InstanceId: last.GetInstanceId()}
	}
	t.Fatal("the reads of files did not end")
	return nil, 0
}

// storageHashes returns every object of the archive's storage with its
// SHA-256, read straight from the store.
func storageHashes(t *testing.T, a *archive) map[string]string {
	t.Helper()
	out := map[string]string{}
	opts := blob.ListOptions{}
	for {
		page, err := a.store.List(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, k := range page.Keys {
			out[k] = tagOf(a.object(t, k))
		}
		if page.Next == "" {
			return out
		}
		opts.Token = page.Next
	}
}
