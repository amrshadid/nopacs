package correct

import (
	"bytes"
	"crypto/sha256"
	"io"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// wantMetaDigest fails unless the catalog keeps, for the scan at p, the
// digest of the metadata file the primary holds, and the replica, once the
// replicator ran (which checks the file it puts against that digest),
// holds the same file.
func (r *replicaRig) wantMetaDigest(t *testing.T, what string, p label.Path) {
	t.Helper()
	sc, err := r.p.CatalogScan(r.ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	sk, _ := layout.MetadataKey(p)
	onPrimary := tenant.SHA256(sha256.Sum256(readKey(t, r.storage, sk)))
	if sc.SidecarSHA256.IsZero() || sc.SidecarSHA256 != onPrimary {
		t.Fatalf("after %s the catalog keeps the metadata digest %s, the primary's file is %s", what, sc.SidecarSHA256, onPrimary)
	}
	r.replicate()
	if got := tenant.SHA256(r.copyObjects()[sk]); got != onPrimary {
		t.Fatalf("after %s the replica's metadata file is %s, want the primary's %s", what, got, onPrimary)
	}
}

// TestCorrectionsKeepTheMetadataDigest guards the digest of a scan's
// metadata file through the corrections that write it again: a replace, a
// relabel and an instance delete's purge each leave the catalog with the
// digest of the file they wrote on the primary, not none, so reads,
// scrubs and the replicator still check it; and the replicator then puts
// the file on the replica against that digest.
func TestCorrectionsKeepTheMetadataDigest(t *testing.T) {
	r := newReplicaRig(t)
	e := r.engine()
	r.archive(studyFiles[:7]...)
	r.attach()

	s := r.scanWith(2)
	path := pathOf(s)
	in := r.held(path)[0]
	old := readKey(t, r.storage, in.Key)
	corrected := append(append([]byte(nil), old...), 0)
	q := r.holdDifferent(s, in, corrected)
	c, err := e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: r.lineFor(path, in.SOPInstanceUID),
		Reason: "corrected re-send", Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the replace = %s, %v", c.State, err)
	}
	r.wantMetaDigest(t, "a replace", path)
	if p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
		t.Fatalf("the replace's purge = %s, %v", p.State, err)
	}

	s = r.scanWith(3)
	from := pathOf(s)
	to := label.Path{Subject: from.Subject, Session: from.Session, Scan: "renamed"}
	c, err = e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: from.Subject, Session: from.Session, Scan: from.Scan},
		ToLabel: "renamed", Reason: "wrong series name", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	if c, err = e.ContinueRelabel(r.ctx, c.ID); err != nil || c.State != record.StateCommitted {
		t.Fatalf("the relabel = %s, %v", c.State, err)
	}
	r.wantMetaDigest(t, "a relabel", to)

	if p, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
		t.Fatalf("the relabel's purge = %s, %v", p.State, err)
	}
	in = r.held(to)[0]
	d, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: to.Subject, Session: to.Session, Scan: to.Scan, Instance: in.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := e.PurgeNow(r.ctx, d.ID, r.audit(), nil); err != nil || p.State != record.StatePurged {
		t.Fatalf("the instance delete's purge = %s, %v", p.State, err)
	}
	r.wantMetaDigest(t, "an instance delete's purge", to)
}

// TestAReplaceKeepsTheDigestOfTheFileItLeaves guards a replace's metadata
// digest against a pass that rewrites the scan's metadata file between the
// replace's rewrite (after which it lets the scan's lock go) and its
// publish: the pass forgets the digest, puts its file and records it, as
// the frames pass does. While the replace is prepared the pass's forget is
// refused and it puts nothing, so the digest the publish keeps is that of
// the file the scan holds. A pass that put its file would leave the file
// checked against the replace's digest of another.
func TestAReplaceKeepsTheDigestOfTheFileItLeaves(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles[:7]...)
	e := r.engine()
	s := r.scanWith(2)
	path := pathOf(s)
	in := r.held(path)[0]
	old := readKey(t, r.storage, in.Key)
	corrected := append(append([]byte(nil), old...), 0)
	q := r.holdDifferent(s, in, corrected)
	rec := r.p.(tenant.SidecarRewrites)
	sk, _ := layout.MetadataKey(path)
	var refused error
	e.SetCrashAt(func(step string) error {
		if step != "meta" {
			return nil
		}
		if _, err := rec.ForgetSidecar(r.ctx, s.SeriesUID, "main"); err != nil {
			refused = err
			return nil
		}
		b := append(readKey(t, r.storage, sk), '\n')
		w, err := r.storage.Put(r.ctx, sk, bytes.NewReader(b))
		if err != nil {
			return err
		}
		_, err = rec.RecordSidecarRewritten(r.ctx, s.SeriesUID, "main", tenant.SHA256(w.SHA256), time.Now())
		return err
	})
	c, err := e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: r.lineFor(path, in.SOPInstanceUID),
		Reason: "corrected re-send", Audit: r.audit()})
	if err != nil || c.State != record.StateCommitted {
		t.Fatalf("the replace = %s, %v", c.State, err)
	}
	if fault.KindOf(refused) != fault.Unavailable {
		t.Errorf("the pass's forget while the replace was prepared answered %v; want it refused as unavailable", refused)
	}
	sc, err := r.p.CatalogScan(r.ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	onStorage := tenant.SHA256(sha256.Sum256(readKey(t, r.storage, sk)))
	if sc.SidecarSHA256.IsZero() || sc.SidecarSHA256 != onStorage {
		t.Fatalf("after the replace the catalog keeps the metadata digest %s; the file is %s", sc.SidecarSHA256, onStorage)
	}
}
