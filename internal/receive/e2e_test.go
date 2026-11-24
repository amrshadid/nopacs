package receive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// readObject reads a whole object from a storage.
func readObject(t *testing.T, s blob.Store, key string) []byte {
	t.Helper()
	rc, err := s.Read(context.Background(), key, 0, -1)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// checkArchive checks everything a done reception left on the storage
// against what was sent, with facts that do not come from the pipeline:
// each file's bytes, the manifest's checksums recomputed from the objects,
// and the sidecar's instances and pixel data offsets read in the objects
// themselves. sent maps each SOP Instance UID to the bytes handed in.
func checkArchive(t *testing.T, e *env, r tenant.Reception, sent map[string][]byte) {
	t.Helper()
	scans, _, err := e.store.ReceptionScans(e.ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, sc := range scans {
		path := label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: sc.Label}
		mk, _ := layout.ManifestKey(path)
		m, err := manifest.Decode(bytes.NewReader(readObject(t, e.storage, mk)))
		if err != nil {
			t.Fatalf("manifest of %s: %v", sc.Label, err)
		}
		for _, f := range m.Files {
			obj := readObject(t, e.storage, f.Key)
			sum := sha256.Sum256(obj)
			if hex.EncodeToString(sum[:]) != f.SHA256 || int64(len(obj)) != f.Size {
				t.Errorf("manifest entry %s does not match its object", f.Key)
			}
			want, ok := sent[f.SOPInstanceUID]
			if !ok {
				continue
			}
			if !bytes.Equal(obj, want) {
				t.Errorf("archived %s is not byte-identical with what was sent", f.Key)
			}
			seen[f.SOPInstanceUID] = true
		}
		sk, _ := layout.MetadataKey(path)
		rd := sidecar.NewReader(bytes.NewReader(readObject(t, e.storage, sk)))
		n := 0
		for {
			in, _, err := rd.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				t.Fatalf("sidecar of %s: %v", sc.Label, err)
			}
			n++
			if in.PixelData == nil {
				t.Errorf("sidecar entry %s has no pixel data", in.SOPInstanceUID)
				continue
			}
			obj := readObject(t, e.storage, in.Key)
			off := in.PixelData.Offset
			if g, el := binary.LittleEndian.Uint16(obj[off:]), binary.LittleEndian.Uint16(obj[off+2:]); g != 0x7FE0 || el != 0x0010 {
				t.Errorf("sidecar offset of %s points at (%04X,%04X)", in.SOPInstanceUID, g, el)
			}
			if in.Dataset == nil || !bytes.Contains(in.Dataset, []byte(in.SOPInstanceUID)) {
				t.Errorf("sidecar entry %s has no data set naming it", in.SOPInstanceUID)
			}
		}
		if n != len(m.Files) {
			t.Errorf("scan %s: sidecar lists %d instances, manifest %d", sc.Label, n, len(m.Files))
		}
	}
	for u := range sent {
		if !seen[u] {
			t.Errorf("instance %s was sent and is not on the storage", u)
		}
	}
}

// checkCatalogValues checks what the catalog keeps of the study of
// studyFiles from the files' headers against the values pydicom reads in
// those files (StudyDate 20010101, Modality CT; two series: "Scout",
// number 4, two files, and "SmartScore - Gated 0.5 sec", number 5, five
// files), and the receiver and totals the commits left.
func checkCatalogValues(t *testing.T, e *env, r tenant.Reception) {
	t.Helper()
	ses, err := e.store.CatalogSession(e.ctx, r.SubjectLabel, r.SessionLabel)
	if err != nil {
		t.Fatal(err)
	}
	if ses.StudyDate != "20010101" || len(ses.Modalities) != 1 || ses.Modalities[0] != "CT" || ses.Receiver != r.Receiver ||
		ses.Scans != 2 || ses.Instances != 7 || ses.Bytes != r.Counts.BytesArchived {
		t.Errorf("the session as the catalog keeps it: %+v", ses)
	}
	page, err := e.store.ListScans(e.ctx, tenant.ScanFilter{Order: tenant.OrderSeriesDescription}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := []tenant.ScanValues{
		{Modality: "CT", SeriesDescription: "Scout", SeriesNumber: 4, HasSeriesNumber: true},
		{Modality: "CT", SeriesDescription: "SmartScore - Gated 0.5 sec", SeriesNumber: 5, HasSeriesNumber: true},
	}
	if len(page.Items) != len(want) {
		t.Fatalf("scans by description: %+v", page.Items)
	}
	for i, sc := range page.Items {
		if sc.Values != want[i] || sc.Instances != int64(2+3*i) || sc.Receiver != r.Receiver {
			t.Errorf("scan %d: %+v, want values %+v", i, sc, want[i])
		}
	}
}

// sentMap returns the bytes of the named corpus files by SOP Instance UID.
func sentMap(t *testing.T, names ...string) map[string][]byte {
	out := map[string][]byte{}
	for _, n := range names {
		b := corpus(t, n)
		out[uidOf(t, b)] = b
	}
	return out
}

// TestStudyIsArchivedByteForByte guards the whole pipeline on the real
// SQLite driver and a directory storage: a study of public files handed in
// ends done, every file is on the storage byte-identical, its manifest's
// checksums match the objects, its sidecar points at each pixel data
// element, the counts agree with what was sent, the incoming area is
// empty, and the step that made the reception done told the senders
// (Config.Forwarded) once.
func TestStudyIsArchivedByteForByte(t *testing.T) {
	var forwarded atomic.Int64
	e := newEnv(t, envOptions{forwarded: func() { forwarded.Add(1) }})
	e.send(studyFiles...)
	rs := e.settle(1)
	if len(rs) != 1 || !done(rs[0]) {
		t.Fatalf("receptions %+v", rs)
	}
	r := rs[0]
	c := r.Counts
	if c.FilesReceived != 7 || c.FilesArchived != 7 || c.FilesDuplicate != 0 || c.FilesQuarantined != 0 || c.BytesArchived != c.BytesReceived {
		t.Fatalf("counts %+v", c)
	}
	checkArchive(t, e, r, sentMap(t, studyFiles...))
	totals, err := e.store.CatalogTotals(e.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if totals != (tenant.CatalogCounts{Subjects: 1, Sessions: 1, Scans: 2, Instances: 7}) {
		t.Fatalf("catalog totals %+v", totals)
	}
	checkCatalogValues(t, e, r)
	if n := e.incomingFiles(); n != 0 {
		t.Fatalf("the incoming area holds %d files after the reception is done", n)
	}
	if len(e.held()) != 0 {
		t.Fatalf("files were held: %+v", e.held())
	}
	if n := forwarded.Load(); n != 1 {
		t.Errorf("the senders were told of %d finished receptions, want 1", n)
	}
}
