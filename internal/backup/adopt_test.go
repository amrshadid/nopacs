package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/correct"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// storedKeys returns every key on the rig's storage, in order.
func (r *rig) storedKeys() []string {
	r.t.Helper()
	var out []string
	token := ""
	for {
		page, err := r.storage.List(r.ctx, blob.ListOptions{Token: token})
		if err != nil {
			r.t.Fatal(err)
		}
		out = append(out, page.Keys...)
		if token = page.Next; token == "" {
			slices.Sort(out)
			return out
		}
	}
}

// engineOver returns a corrections engine over the rig's storage whose
// database is store, with a grace period of two days.
func (r *rig) engineOver(store tenant.Corrections) *correct.Engine {
	r.t.Helper()
	e, err := correct.New(correct.Config{Tenant: "lab", TenantID: tenantID, Keys: r.keys, Store: store,
		OpenStorage: func(context.Context, string) (blob.Store, error) { return r.storage, nil },
		Grace:       func(context.Context) (time.Duration, error) { return 48 * time.Hour, nil }})
	if err != nil {
		r.t.Fatal(err)
	}
	return e
}

// beforeCommit is the tenant's database with a step run before each
// correction's publish, or in its place when the step fails.
type beforeCommit struct {
	tenant.Corrections
	step func() error
}

// CommitCorrection runs the step, then the publish unless the step failed.
func (b beforeCommit) CommitCorrection(ctx context.Context, cc tenant.CorrectionCommit) (tenant.Correction, error) {
	if err := b.step(); err != nil {
		return tenant.Correction{}, err
	}
	return b.Corrections.CommitCorrection(ctx, cc)
}

// TestRestoreReadsEachDeleteRecordAsItSays guards the one rule by which a
// restore reads the records of deletes made after its backup, which a
// rebuild reads a record with no row by: a delete committed hides its
// subject, committed and unresolved; one undone and one abandoned (its
// publish failed) show their subjects, undone and abandoned, naming
// nothing an arrival is held for; one still prepared hides its subject,
// committed and unresolved, on each database type.
func TestRestoreReadsEachDeleteRecordAsItSays(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		all := r.subjects(r.p)
		if len(all) < 4 {
			t.Fatalf("the corpus made %d subjects, want 4", len(all))
		}
		path, _ := r.backup()
		e := r.engine()
		committed := r.deleteSubject(e, all[0])
		undone := r.deleteSubject(e, all[1])
		if _, err := e.Undo(r.ctx, undone.ID, "deleted by mistake", r.audit(), nil); err != nil {
			t.Fatal(err)
		}
		failing := r.engineOver(beforeCommit{Corrections: r.p, step: func() error { return fault.New(fault.Unavailable, "the database stopped") }})
		target := tenant.CorrectionTarget{Subject: all[2]}
		p, err := r.p.PreviewDelete(r.ctx, target, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		abandoned, err := failing.Delete(r.ctx, correct.DeleteRequest{Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Audit: r.audit()})
		if err == nil || abandoned.State != record.StateAbandoned {
			t.Fatalf("the delete whose publish failed is %s (%v), want abandoned", abandoned.State, err)
		}
		prepared := r.deleteSubject(e, all[3])
		r.forgeRecord(prepared.ID, func(c *record.Correction) {
			c.State, c.CommittedAt, c.PurgeAfter = record.StatePrepared, time.Time{}, time.Time{}
		})
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		want := []string{all[1], all[2]}
		want = append(want, all[4:]...)
		slices.Sort(want)
		if got := r.subjects(r.p); !slices.Equal(got, want) {
			t.Fatalf("the restored catalog holds %v, want %v", got, want)
		}
		for _, c := range []struct {
			id         string
			subject    string
			state      record.State
			unresolved bool
		}{
			{committed.ID, all[0], record.StateCommitted, true},
			{undone.ID, all[1], record.StateUndone, false},
			{abandoned.ID, all[2], record.StateAbandoned, false},
			{prepared.ID, all[3], record.StateCommitted, true},
		} {
			got, err := r.p.Correction(r.ctx, c.id)
			if err != nil || got.State != c.state || got.Unresolved != c.unresolved {
				t.Errorf("the delete of %s is %s, unresolved %v (%v); want %s, unresolved %v", c.subject, got.State, got.Unresolved, err,
					c.state, c.unresolved)
			}
			match, err := r.p.DeletedMatches(r.ctx, tenant.DeletedProbe{Subject: c.subject, StudyUID: "1.2.3"})
			if err != nil || (match.Subject == c.id) != (c.state == record.StateCommitted) {
				t.Errorf("the delete of %s (%s) names its subject: %+v (%v)", c.subject, c.state, match, err)
			}
		}
		if res := rep.Tenants[0]; res.Added != 4 || res.Moved != 0 || res.Disagreed != 0 {
			t.Errorf("the tenant's reconcile is %+v", res)
		}
	})
}

// replaceRequest holds a corrected copy of the first instance of a scan of
// two or more as a different file, and returns the replace of it and the
// instance's new revision key.
func (r *rig) replaceRequest() (correct.ReplaceRequest, string) {
	r.t.Helper()
	page, err := r.p.ListScans(r.ctx, tenant.ScanFilter{Order: tenant.OrderLabel}, master.Page{Limit: 1000})
	if err != nil {
		r.t.Fatal(err)
	}
	var s tenant.CatalogScan
	for _, x := range page.Items {
		if x.Instances >= 2 {
			s = x
			break
		}
	}
	path := label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
	held, _, err := r.p.ListHeldInstances(r.ctx, path, master.Page{Limit: 1000})
	if err != nil || len(held) == 0 {
		r.t.Fatalf("the scan %v holds no instance: %v", path, err)
	}
	in := held[0]
	rc, err := r.storage.Read(r.ctx, in.Key, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	old, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		r.t.Fatal(err)
	}
	corrected := append(slices.Clone(old), 0)
	recv, _, err := r.p.Reception(r.ctx, in.ReceptionID)
	if err != nil {
		r.t.Fatal(err)
	}
	q := tenant.QuarantinedFile{ID: "qreplace0001", Reason: tenant.QuarantineDifferentFile, Receiver: "dimse", ReceptionID: in.ReceptionID,
		Size: int64(len(corrected)), SHA256: tenant.SHA256(sha256.Sum256(corrected)), KeptSHA256: in.SHA256,
		SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: in.SOPInstanceUID, StudyUID: recv.StudyUID, SeriesUID: s.SeriesUID,
		TransferSyntaxUID: "1.2.840.10008.1.2.1", Incoming: "quarantine/qreplace0001", ContentKept: true,
		Quarantined: time.Now().UTC().Truncate(time.Microsecond)}
	if _, err := r.p.HoldFiles(r.ctx, []tenant.QuarantinedFile{q}); err != nil {
		r.t.Fatal(err)
	}
	key, err := layout.RevisionKey(path, in.SOPInstanceUID, 2)
	if err != nil {
		r.t.Fatal(err)
	}
	return correct.ReplaceRequest{Quarantine: q.ID, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil },
		Line: r.lineOf(path, in.SOPInstanceUID), Reason: "corrected re-send", Audit: r.audit()}, key
}

// lineOf returns the instance's line of the scan's metadata.
func (r *rig) lineOf(p label.Path, uid string) sidecar.Instance {
	r.t.Helper()
	key, err := layout.MetadataKey(p)
	if err != nil {
		r.t.Fatal(err)
	}
	rc, err := r.storage.Read(r.ctx, key, 0, -1)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	rd := sidecar.NewReader(rc)
	for {
		in, _, err := rd.Next()
		if errors.Is(err, io.EOF) {
			r.t.Fatalf("the metadata of %v holds no line of %s", p, uid)
		}
		if err != nil {
			r.t.Fatal(err)
		}
		if in.SOPInstanceUID == uid {
			return in
		}
	}
}

// TestRestoreMovesAReplaceCommittedAfterItsBackup guards a backup taken
// between a replace's prepare and its publish, the replace published after
// it: the restore moves the row the backup holds prepared to committed, as
// the replace's record says, unresolved so its old revision is purged only
// by an admin; the rebuilt catalog names the new revision; and the start
// after the restore (Recover) leaves the new revision on the storage, where
// a row left prepared would be rolled back and its object removed, on each
// database type.
func TestRestoreMovesAReplaceCommittedAfterItsBackup(t *testing.T) {
	onBoth(t, func(t *testing.T, pg bool) {
		r := newRig(t, pg)
		r.archive(studyFiles...)
		req, key := r.replaceRequest()
		var path string
		e := r.engineOver(beforeCommit{Corrections: r.p, step: func() error {
			path, _ = r.backup()
			return nil
		}})
		c, err := e.Replace(r.ctx, req)
		if err != nil || c.State != record.StateCommitted || path == "" {
			t.Fatalf("the replace is %s (%v), the backup %q; want committed after a backup", c.State, err, path)
		}
		rep, err := r.restoreFresh(path)
		if err != nil {
			t.Fatalf("the restore failed: %v", err)
		}
		got, err := r.p.Correction(r.ctx, c.ID)
		if err != nil || got.State != record.StateCommitted || !got.Unresolved || got.StateReason != tenant.StateReasonFromRecord {
			t.Fatalf("after the restore the replace is %s, unresolved %v, %q (%v); want committed, unresolved", got.State, got.Unresolved,
				got.StateReason, err)
		}
		if res := rep.Tenants[0]; res.Moved != 1 || res.Disagreed != 0 {
			t.Errorf("the tenant's reconcile is %+v", res)
		}
		var out strings.Builder
		if err := rep.Print(&out); err != nil || !strings.Contains(out.String(), "1 whose row the backup holds behind its record") {
			t.Errorf("the report reads %q (%v)", out.String(), err)
		}
		if err := r.engine().Recover(r.ctx); err != nil {
			t.Fatalf("the start after the restore failed: %v", err)
		}
		if !slices.Contains(r.storedKeys(), key) {
			t.Fatal("the start after the restore removed the replace's new revision")
		}
		held, _, err := r.p.ListHeldInstances(r.ctx, req.Path, master.Page{Limit: 1000})
		if err != nil {
			t.Fatal(err)
		}
		for _, in := range held {
			if in.SOPInstanceUID == req.UID && in.Key != key {
				t.Errorf("the catalog names %s for the replaced instance, want its new revision %s", in.Key, key)
			}
		}
		if after, err := r.p.Correction(r.ctx, c.ID); err != nil || after.State != record.StateCommitted {
			t.Errorf("after the start the replace is %s (%v), want committed", after.State, err)
		}
	})
}
