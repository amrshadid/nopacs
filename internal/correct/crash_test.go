package correct

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// crashFiles are what the crash tests archive: one CT study of two series
// and one MR file, a subject each.
var crashFiles = studyFiles[:8]

// crashed runs fn with the engine stopping at step, then a new engine's
// Recover, as a restart; it answers that engine.
func crashed(t *testing.T, r *rig, step string, fn func(e *Engine) (tenant.Correction, error)) (tenant.Correction, *Engine) {
	t.Helper()
	e := r.engine()
	e.SetCrashAt(crashAt(step))
	c, err := fn(e)
	if !errors.Is(err, errCrash) {
		t.Fatalf("the engine did not stop at %s: %+v, %v", step, c, err)
	}
	restarted := r.engine()
	if err := restarted.Recover(r.ctx); err != nil {
		t.Fatalf("Recover after a stop at %s: %v", step, err)
	}
	return c, restarted
}

// TestDeleteCrashes guards the restart rules of a delete: one stopped
// before its publish is abandoned, nothing hidden, its record rewritten
// abandoned and its arrivals no longer held; one stopped after its publish
// stays committed and its record is rewritten committed.
func TestDeleteCrashes(t *testing.T) {
	for _, tt := range []struct {
		step string
		want record.State
	}{{"prepared", record.StateAbandoned}, {"records", record.StateAbandoned}, {"committed", record.StateCommitted}} {
		t.Run(tt.step, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			subject := r.scanWith(2).SubjectLabel
			c, _ := crashed(t, r, tt.step, func(e *Engine) (tenant.Correction, error) {
				return r.deleteNow(e, tenant.CorrectionTarget{Subject: subject})
			})
			got := r.state(c.ID)
			if got.State != tt.want || got.RecordsState != tt.want {
				t.Fatalf("after the restart the delete is %s, its records %s; want %s", got.State, got.RecordsState, tt.want)
			}
			if rec := r.correctionRecord(c.ID); rec.State != tt.want {
				t.Fatalf("the record on the storage is %s", rec.State)
			}
			_, err := r.p.CatalogSubject(r.ctx, subject)
			if visible := err == nil; visible != (tt.want == record.StateAbandoned) {
				t.Fatalf("after the restart the subject's visibility is %v (%v)", visible, err)
			}
			m, err := r.p.DeletedMatches(r.ctx, tenant.DeletedProbe{Subject: subject})
			if err != nil || (m.Subject != "") != (tt.want == record.StateCommitted) {
				t.Fatalf("after the restart the subject matches %q, %v", m.Subject, err)
			}
		})
	}
}

// TestUndoCrash guards an undo stopped before its records were rewritten:
// the delete stays undone and the restart rewrites its record undone.
func TestUndoCrash(t *testing.T) {
	r := newRig(t, false)
	r.archive(crashFiles...)
	subject := r.scanWith(2).SubjectLabel
	c, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: subject})
	if err != nil {
		t.Fatal(err)
	}
	crashed(t, r, "undone", func(e *Engine) (tenant.Correction, error) {
		return e.Undo(r.ctx, c.ID, "the wrong subject", r.audit(), nil)
	})
	if got := r.state(c.ID); got.State != record.StateUndone || r.correctionRecord(c.ID).State != record.StateUndone {
		t.Fatalf("after the restart the undo is %s", got.State)
	}
	if _, err := r.p.CatalogSubject(r.ctx, subject); err != nil {
		t.Fatalf("an undone subject is not read: %v", err)
	}
}

// TestPurgeCrashes guards a purge stopped at each of its steps: the
// restart goes on to purged, no object of the subject is left, every other
// object is as it was, and each object removed is recorded once.
func TestPurgeCrashes(t *testing.T) {
	for _, step := range []string{"purge-started", "purge-object", "purge-objects", "purged"} {
		t.Run(step, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			subject := r.scanWith(2).SubjectLabel
			gone := []string{"data/" + subject + "/", "meta/" + subject + "/"}
			before := r.objects()
			c, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: subject})
			if err != nil {
				t.Fatal(err)
			}
			crashed(t, r, step, func(e *Engine) (tenant.Correction, error) { return e.PurgeNow(r.ctx, c.ID, r.audit(), nil) })
			got := r.state(c.ID)
			after := r.objects()
			if got.State != record.StatePurged || r.correctionRecord(c.ID).State != record.StatePurged {
				t.Fatalf("after the restart the purge is %s", got.State)
			}
			if left := under(after, gone...); len(left) != 0 {
				t.Fatalf("after the restart the storage holds %q", left)
			}
			if want := int64(len(under(before, gone...))); got.Counts.ObjectsRemoved != want {
				t.Fatalf("the purge recorded %d objects removed, want %d", got.Counts.ObjectsRemoved, want)
			}
			r.wantUnchanged(before, after, gone...)
		})
	}
}

// TestInstancePurgeCrash guards an instance's purge stopped after it wrote
// the scan's manifest without the instance and before its end: the restart
// ends it, and the catalog holds the checksum of the manifest on the
// storage, though the catalog still held the one from before the delete.
func TestInstancePurgeCrash(t *testing.T) {
	for _, step := range []string{"purge-meta", "purge-objects"} {
		t.Run(step, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			path := pathOf(r.scanWith(2))
			victim := r.held(path)[0]
			c, err := r.deleteNow(r.engine(), tenant.CorrectionTarget{Subject: path.Subject, Session: path.Session, Scan: path.Scan,
				Instance: victim.SOPInstanceUID})
			if err != nil {
				t.Fatal(err)
			}
			crashed(t, r, step, func(e *Engine) (tenant.Correction, error) { return e.PurgeNow(r.ctx, c.ID, r.audit(), nil) })
			if got := r.state(c.ID); got.State != record.StatePurged {
				t.Fatalf("after the restart the purge is %s", got.State)
			}
			mk, _ := layout.ManifestKey(path)
			raw, err := readAll(r.ctx, r.storage, mk, int64(manifest.MaxBytes))
			if err != nil {
				t.Fatal(err)
			}
			sc, err := r.p.CatalogScan(r.ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			if sc.ManifestSHA256 != tenant.SHA256(sha256.Sum256(raw)) {
				t.Fatal("after the restart the catalog does not hold the stored manifest's checksum")
			}
		})
	}
}

// TestRelabelCrashes guards that a relabel left prepared at any step goes
// on at the restart: copied, published, the subject under its new label
// with every object it names stored, its record committed.
func TestRelabelCrashes(t *testing.T) {
	for _, step := range []string{"prepared", "records", "relabel-object", "copied", "committed"} {
		t.Run(step, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			subject := r.scanWith(2).SubjectLabel
			c, _ := crashed(t, r, step, func(e *Engine) (tenant.Correction, error) {
				c, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: subject}, ToLabel: "renamed-1",
					Reason: "wrong subject id", Audit: r.audit()})
				if err != nil {
					return c, err
				}
				return e.ContinueRelabel(r.ctx, c.ID)
			})
			if got := r.state(c.ID); got.State != record.StateCommitted || r.correctionRecord(c.ID).State != record.StateCommitted {
				t.Fatalf("after the restart the relabel is %s", got.State)
			}
			objs := r.objects()
			for _, s := range r.scans() {
				if s.SubjectLabel == subject {
					t.Fatalf("a scan is still under %s", subject)
				}
				for _, in := range r.held(pathOf(s)) {
					if _, ok := objs[in.Key]; !ok {
						t.Fatalf("the catalog names %s, which is not stored", in.Key)
					}
				}
			}
		})
	}
}

// TestReplaceCrashes guards that a replace left prepared at any step is
// abandoned at the restart and leaves the archive as it was: the scan's
// manifest byte for byte, the metadata naming the old key, no object at
// the new revision's key, the catalog's instance untouched; one stopped
// after its publish stays committed.
func TestReplaceCrashes(t *testing.T) {
	for _, step := range []string{"prepared", "records", "object", "meta", "committed"} {
		t.Run(step, func(t *testing.T) {
			r := newRig(t, false)
			r.archive(crashFiles...)
			s := r.scanWith(2)
			path := pathOf(s)
			in := r.held(path)[0]
			old, err := readAll(r.ctx, r.storage, in.Key, 1<<30)
			if err != nil {
				t.Fatal(err)
			}
			corrected := append(append([]byte(nil), old...), 1)
			q := r.holdDifferent(s, in, corrected)
			mk, _ := layout.ManifestKey(path)
			beforeManifest, err := readAll(r.ctx, r.storage, mk, int64(manifest.MaxBytes))
			if err != nil {
				t.Fatal(err)
			}
			line := r.lineFor(path, in.SOPInstanceUID)
			c, _ := crashed(t, r, step, func(e *Engine) (tenant.Correction, error) {
				return e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
					Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: line,
					Reason: "corrected re-send", Audit: r.audit()})
			})
			got := r.state(c.ID)
			key, _ := layout.RevisionKey(path, in.SOPInstanceUID, 2)
			if step == "committed" {
				if got.State != record.StateCommitted || r.correctionRecord(c.ID).State != record.StateCommitted {
					t.Fatalf("after the restart the replace is %s", got.State)
				}
				return
			}
			if got.State != record.StateAbandoned || r.correctionRecord(c.ID).State != record.StateAbandoned {
				t.Fatalf("after the restart the replace is %s", got.State)
			}
			afterManifest, err := readAll(r.ctx, r.storage, mk, int64(manifest.MaxBytes))
			if err != nil || !bytes.Equal(afterManifest, beforeManifest) {
				t.Fatalf("the manifest was not written back as it was: %v", err)
			}
			if l := r.lineFor(path, in.SOPInstanceUID); l.Key != in.Key || l.SHA256 != line.SHA256 {
				t.Fatalf("the metadata's line is %+v, want the old revision's", l)
			}
			if _, err := r.storage.Stat(r.ctx, key); fault.KindOf(err) != fault.NotFound {
				t.Fatalf("the new revision's object is still stored: %v", err)
			}
			h, err := r.p.HeldInstance(r.ctx, path, in.SOPInstanceUID)
			if err != nil || h.Key != in.Key {
				t.Fatalf("the catalog's instance is %+v, %v", h, err)
			}
		})
	}
}
