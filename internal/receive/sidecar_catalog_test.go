package receive

import (
	"bytes"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRetryAfterRefusedCommitWritesTheCatalogsHeader guards the header of a
// scan whose first commit was refused after its sidecar was put: the step
// tried again writes the times the catalog committed, not those of the
// attempt that never committed, so the scan's created time is the earliest
// time one of its lines was held.
func TestRetryAfterRefusedCommitWritesTheCatalogsHeader(t *testing.T) {
	e := newEnv(t, envOptions{db: func(s tenant.Pipeline) tenant.Pipeline {
		db := &faultyDB{Pipeline: s}
		db.commitFails.Store(1)
		return db
	}})
	e.send(studyFiles...)
	rs := e.settle(1)
	if !done(rs[0]) || rs[0].Counts.FilesArchived != int64(len(studyFiles)) {
		t.Fatalf("reception %+v", rs[0])
	}
	checkAgainstCatalog(t, e, scanPaths(t, e, rs), sidecar.FormDIMSE)
}

// TestSidecarOfAReceptionThatNeverCommittedIsNotKept guards a scan whose
// sidecar a reception that failed for good put on the storage, with another
// receiver, routing and times: the reception that archives the scan later
// writes the header from the catalog, with its own receiver and times, and
// keeps no line of the stale sidecar.
func TestSidecarOfAReceptionThatNeverCommittedIsNotKept(t *testing.T) {
	first := newEnv(t, envOptions{})
	first.send(studyFiles...)
	paths := scanPaths(t, first, first.settle(1))
	stale := map[label.Path][]byte{}
	for _, path := range paths {
		h, lines := sidecarOf(t, first, path)
		h.SessionReceiver, h.Receiver, h.Route = "stale-receiver", "stale-receiver", &sidecar.Route{Name: "stale"}
		h.Created, h.Updated = t0.Add(-24*time.Hour), t0.Add(-24*time.Hour)
		h.Values = sidecar.Values{Modality: "XA"}
		var b bytes.Buffer
		w := sidecar.NewWriterV3(&b, *h)
		for _, in := range lines {
			in.Reception = "stale-reception"
			if err := w.Add(in); err != nil {
				t.Fatal(err)
			}
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		stale[path] = b.Bytes()
	}

	e := newEnv(t, envOptions{})
	for path, b := range stale {
		key, _ := layout.MetadataKey(path)
		if _, err := e.storage.Put(e.ctx, key, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
	}
	e.send(studyFiles...)
	rs := e.settle(1)
	if !done(rs[0]) {
		t.Fatalf("reception %+v", rs[0])
	}
	checkAgainstCatalog(t, e, paths, sidecar.FormDIMSE)
	for _, path := range paths {
		h, lines := sidecarOf(t, e, path)
		if h.Receiver != "dimse" || h.SessionReceiver != "dimse" || h.Route != nil || h.Values.Modality != "CT" {
			t.Errorf("scan %s: header %+v keeps the stale sidecar's", path.Scan, h)
		}
		for _, in := range lines {
			if in.Reception != rs[0].ID {
				t.Errorf("scan %s: line %s keeps reception %q", path.Scan, in.SOPInstanceUID, in.Reception)
			}
		}
	}
}

// labelClash is a naming rule that gives every series of one modality the
// same scan label, so the two CT series of studyFiles meet in scan-CT.
var labelClash = naming.Spec{Subject: "sub-{PatientID}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{Modality}"}

// TestScanLabelClashHoldsTheFiles guards a series whose scan label the
// catalog gives another series: its files are held as a label conflict
// (held_by_another_scan) before anything is put, keeping their incoming
// copies, and the other scan's sidecar, of the current version or of
// version 2, and its manifest are the same bytes after as before.
func TestScanLabelClashHoldsTheFiles(t *testing.T) {
	for name, v2 := range map[string]bool{"version 3": false, "version 2": true} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, envOptions{naming: &labelClash})
			e.send(studyFiles[:2]...)
			rs := e.settle(1)
			paths := scanPaths(t, e, rs)
			if !done(rs[0]) || len(paths) != 1 || paths[0].Scan != "scan-CT" {
				t.Fatalf("reception %+v, scans %v", rs[0], paths)
			}
			if v2 {
				downgrade(t, e, paths[0])
			}
			metaKey, _ := layout.MetadataKey(paths[0])
			manifestKey, _ := layout.ManifestKey(paths[0])
			metaBefore, manifestBefore := readObject(t, e.storage, metaKey), readObject(t, e.storage, manifestKey)

			e.send(studyFiles[2:]...)
			rs = e.settle(2)
			var second tenant.Reception
			for _, r := range rs {
				if r.ID != scanReception(t, e, paths[0]) {
					second = r
				}
			}
			if second.State != reception.Quarantined || second.Counts.FilesQuarantined != int64(len(studyFiles[2:])) || second.Counts.FilesArchived != 0 {
				t.Fatalf("the second reception %+v", second)
			}
			n := 0
			for _, q := range e.held() {
				if q.ReceptionID != second.ID {
					continue
				}
				n++
				if q.Reason != tenant.QuarantineLabelConflict || q.Details["fault"] != tenant.FaultHeldByAnotherScan || q.Details["label"] != "scan" || q.Incoming == "" {
					t.Errorf("held %+v", q)
				}
			}
			if n != len(studyFiles[2:]) {
				t.Errorf("%d files held, want %d", n, len(studyFiles[2:]))
			}
			if !bytes.Equal(readObject(t, e.storage, metaKey), metaBefore) || !bytes.Equal(readObject(t, e.storage, manifestKey), manifestBefore) {
				t.Error("the other scan's sidecar or manifest changed")
			}
			if rows := heldRows(t, e, paths[0]); len(rows) != 2 {
				t.Errorf("the other scan holds %d instances, want 2", len(rows))
			}
		})
	}
}

// TestScanLabelClashInOneReception guards two series of one reception that
// the naming rule gives one label: the first archived holds the scan, the
// other's files are held as a label conflict, the reception ends rather
// than failing, and the scan's sidecar holds exactly the catalog's
// instances.
func TestScanLabelClashInOneReception(t *testing.T) {
	e := newEnv(t, envOptions{naming: &labelClash})
	e.send(studyFiles...)
	rs := e.settle(1)
	r := rs[0]
	if !done(r) || r.Counts.FilesArchived+r.Counts.FilesQuarantined != int64(len(studyFiles)) || r.Counts.FilesQuarantined == 0 {
		t.Fatalf("reception %+v", r)
	}
	for _, q := range e.held() {
		if q.Reason != tenant.QuarantineLabelConflict || q.Details["fault"] != tenant.FaultHeldByAnotherScan {
			t.Errorf("held %+v", q)
		}
	}
	paths := scanPaths(t, e, rs)
	if n := checkAgainstCatalog(t, e, paths, sidecar.FormDIMSE); int64(n) != r.Counts.FilesArchived {
		t.Errorf("%d lines, %d archived", n, r.Counts.FilesArchived)
	}
}

// scanReception returns the reception of the first line of a scan's
// sidecar.
func scanReception(t *testing.T, e *env, path label.Path) string {
	t.Helper()
	for _, row := range heldRows(t, e, path) {
		return row.ReceptionID
	}
	t.Fatalf("scan %s holds no instance", path.Scan)
	return ""
}
