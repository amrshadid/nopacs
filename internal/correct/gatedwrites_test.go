package correct

import (
	"bytes"
	"context"
	"io"
	"testing"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Put records the write, then puts.
func (s *placeStore) Put(ctx context.Context, key string, r io.Reader) (blob.Written, error) {
	s.seen.add(s.gate, s.place, key)
	return s.Store.Put(ctx, key, r)
}

// Delete records the removal, then deletes.
func (s *placeStore) Delete(ctx context.Context, key string) error {
	s.seen.add(s.gate, s.place, key)
	return s.Store.Delete(ctx, key)
}

// RemoveEmptyDirs records the removal, then removes through the storage
// when it removes directories.
func (s *placeStore) RemoveEmptyDirs(ctx context.Context, dir string) error {
	s.seen.add(s.gate, s.place, dir+"/")
	if pr, ok := s.Store.(blob.DirPruner); ok {
		return pr.RemoveEmptyDirs(ctx, dir)
	}
	return nil
}

// forget clears the writes seen so far.
func (rs *removals) forget() {
	rs.mu.Lock()
	rs.list = nil
	rs.mu.Unlock()
}

// TestEveryCorrectionWriteLandsOnThePlaceTheGateAdmitted guards the
// corrections' writes against a switch: a gate that swaps the storage's
// place each time it admits a write sees every write of the engine (a
// record, an instance purge's manifest and metadata, a relabel's copies
// and the directories its purge empties, a replace's new revision and the
// manifest naming it, a stopped relabel's removals) land on the place
// current when it ran, with a write admitted: each step enters the gate
// before it opens the storage, never writes through a store opened before.
func TestEveryCorrectionWriteLandsOnThePlaceTheGateAdmitted(t *testing.T) {
	r := newRig(t, false)
	r.archive(studyFiles...)
	e, _, seen := gatedEngine(r)

	// An instance delete: its records, then its purge's rewrite of the
	// scan's manifest and metadata.
	s := r.scanWith(3)
	in := r.held(pathOf(s))[0]
	d, err := r.deleteNow(e, tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label, Instance: in.SOPInstanceUID})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.PurgeNow(r.ctx, d.ID, r.audit(), nil); err != nil || got.State != record.StatePurged {
		t.Fatalf("the instance delete's purge: %+v %v", got, err)
	}
	wantGated(t, seen, "an instance delete and its purge")

	// A relabel: its copies and its publish, then its purge, which removes
	// the old keys and the directories it empties.
	seen.forget()
	s = r.scanWith(2)
	c, err := e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label},
		ToLabel: "renamed", Reason: "wrong series name", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := e.ContinueRelabel(r.ctx, c.ID); err != nil || got.State != record.StateCommitted {
		t.Fatalf("the relabel: %+v %v", got, err)
	}
	if got, err := e.PurgeNow(r.ctx, c.ID, r.audit(), nil); err != nil || got.State != record.StatePurged {
		t.Fatalf("the relabel's purge: %+v %v", got, err)
	}
	wantGated(t, seen, "a relabel and its purge")

	// A replace: its new revision, the manifest and metadata naming it.
	seen.forget()
	s = r.scanWith(2)
	path := pathOf(s)
	in = r.held(path)[0]
	old, err := readAll(r.ctx, r.storage, in.Key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	corrected := append(append([]byte(nil), old...), 0)
	q := r.holdDifferent(s, in, corrected)
	rp, err := e.Replace(r.ctx, ReplaceRequest{Quarantine: q, Storage: "main", Path: path, UID: in.SOPInstanceUID,
		Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(corrected)), nil }, Line: r.lineFor(path, in.SOPInstanceUID),
		Reason: "corrected re-send", Audit: r.audit()})
	if err != nil || rp.State != record.StateCommitted {
		t.Fatalf("the replace: %+v %v", rp, err)
	}
	wantGated(t, seen, "a replace")

	// A relabel stopped after its copy: the copies it removes.
	seen.forget()
	s = r.scans()[0]
	c, err = e.Relabel(r.ctx, RelabelRequest{Target: tenant.CorrectionTarget{Subject: s.SubjectLabel}, ToLabel: "sub-moved",
		Reason: "wrong subject id", Audit: r.audit()})
	if err != nil {
		t.Fatal(err)
	}
	e.SetCrashAt(crashAt("copied"))
	if _, err := e.ContinueRelabel(r.ctx, c.ID); err == nil {
		t.Fatal("the relabel was not stopped before its publish")
	}
	e.SetCrashAt(func(string) error { return nil })
	seen.forget()
	if got, err := e.Undo(r.ctx, c.ID, "asked by mistake", r.audit(), nil); err != nil || got.State != record.StateAbandoned {
		t.Fatalf("the stop of a relabel copied: %+v %v", got, err)
	}
	if got := r.state(c.ID); !got.Swept {
		t.Fatalf("the stopped relabel's copies were not removed: %+v", got)
	}
	wantGated(t, seen, "a stopped relabel's removal of its copies")
}
