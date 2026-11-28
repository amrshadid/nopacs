package correct

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// quickPurge is the rig's storage with a purge that touches no disk, so a
// measure of a purge is of the database's side alone: a directory storage
// syncs its directory after each removal, which dominates on a laptop.
type quickPurge struct{ blob.Store }

// Purge implements blob.Store: removed, at once.
func (quickPurge) Purge(context.Context, string) ([]blob.Removal, error) {
	return []blob.Removal{{Disposition: blob.Removed}}, nil
}

// TestScaleSubjectDelete measures, on SQLite, the delete and the purge of
// one subject of NOPACS_SCALE_INSTANCES instances (scans of a thousand)
// through the engine, the storage's removals taking no time: what the
// statements cost as the subject grows (every one is over the delete's own
// rows, none lists every id, and no step per object reads what the purge
// recorded before). It checks the purge removed and recorded every
// instance. It is skipped unless NOPACS_SCALE_INSTANCES is set.
func TestScaleSubjectDelete(t *testing.T) {
	n := testkit.ScaleInstances(t)
	r := newRig(t, false)
	subject := "sub-scale"
	study := "2.25.1000"
	rid := "scalereception0001"
	opened := time.Now().UTC().Truncate(time.Microsecond)
	rc, _, err := r.p.OpenReception(r.ctx, "dimse", study, rid, opened)
	if err != nil {
		t.Fatal(err)
	}
	type scan struct {
		uid   string
		files []tenant.ReceivedFile
	}
	var scans []scan
	for i := int64(0); i < n; i++ {
		if i%1000 == 0 {
			scans = append(scans, scan{uid: fmt.Sprintf("2.25.2%06d", len(scans))})
		}
		s := &scans[len(scans)-1]
		uid := fmt.Sprintf("2.25.3%09d", i)
		s.files = append(s.files, tenant.ReceivedFile{ReceptionID: rid, SOPInstanceUID: uid, SeriesUID: s.uid, StudyUID: study,
			SOPClassUID: "1.2.840.10008.5.1.4.1.1.4", TransferSyntaxUID: "1.2.840.10008.1.2.1", Size: 1000,
			SHA256: tenant.SHA256(sha256.Sum256([]byte(uid))), Incoming: fmt.Sprintf("dimse/%02x/%030x", i%256, i)})
	}
	start := time.Now()
	for _, s := range scans {
		for b := 0; b < len(s.files); b += tenant.MaxReceivedBatch {
			if _, err := r.p.AddReceivedFiles(r.ctx, s.files[b:min(b+tenant.MaxReceivedBatch, len(s.files))], opened, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	step := func(ev reception.Event) {
		st := tenant.ReceptionStep{ID: rid, From: rc.State, Event: ev, At: opened.Add(time.Minute)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = subject, "ses-scale", "main"
		}
		if rc, err = r.p.StepReception(r.ctx, st, nil); err != nil {
			t.Fatal(err)
		}
	}
	step(reception.Quiet)
	step(reception.Identify)
	for i, s := range scans {
		path := label.Path{Subject: subject, Session: "ses-scale", Scan: fmt.Sprintf("scan-%03d", i)}
		for b := 0; b < len(s.files); b += tenant.MaxClaimBatch {
			part := s.files[b:min(b+tenant.MaxClaimBatch, len(s.files))]
			claims := make([]tenant.InstanceClaim, len(part))
			c := tenant.ScanCommit{ReceptionID: rid, StudyUID: study, SeriesUID: s.uid, SubjectLabel: subject, SessionLabel: path.Session,
				ScanLabel: path.Scan, Storage: "main", At: opened.Add(time.Minute)}
			for j, f := range part {
				claims[j] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: study, SeriesUID: s.uid, SHA256: f.SHA256}
				key, _ := layout.InstanceKey(path, f.SOPInstanceUID)
				c.Instances = append(c.Instances, tenant.ConfirmedInstance{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size,
					Size: f.Size, ArchivedSHA256: f.SHA256})
			}
			if _, err := r.p.ClaimInstances(r.ctx, rid, claims, opened.Add(time.Minute), nil); err != nil {
				t.Fatal(err)
			}
			if _, err := r.p.CommitScan(r.ctx, c); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, ev := range []reception.Event{reception.Archive, reception.Verify, reception.Finish} {
		step(ev)
	}
	t.Logf("a subject of %d instances in %d scans written in %v", n, len(scans), time.Since(start).Round(time.Millisecond))

	e := r.engineWith(func(c *Config) {
		c.OpenStorage = func(context.Context, string) (blob.Store, error) { return quickPurge{r.storage}, nil }
	})
	start = time.Now()
	c, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	deleted := time.Since(start)
	start = time.Now()
	p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil)
	if err != nil {
		t.Fatal(err)
	}
	purged := time.Since(start)
	if p.State != record.StatePurged || p.Counts.ObjectsRemoved != n || p.Counts.ObjectsOwned != n {
		t.Fatalf("the purge is %s with %d removed of %d owned, want %d", p.State, p.Counts.ObjectsRemoved, p.Counts.ObjectsOwned, n)
	}
	t.Logf("SQLite, %d instances: preview, prepare and publish %v; purge %v (%v an object)", n, deleted.Round(time.Millisecond),
		purged.Round(time.Millisecond), (purged / time.Duration(n)).Round(time.Microsecond))
}
