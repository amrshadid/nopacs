package scrub

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// An edit gives every instance of its target a new revision as a replace
// gives one: before its publish it puts each new revision and rewrites
// each scan's manifest and metadata to name them, and it keeps every
// superseded revision until its purge. Undone, it keeps its new revisions
// until its sweep removes them. What it does to a scan is expected as a
// replace's is.

// covers reports whether a correction's target holds the scan at p.
func covers(t tenant.CorrectionTarget, p label.Path) bool {
	return t.Subject == p.Subject && (t.Session == "" || t.Session == p.Session) && (t.Scan == "" || t.Scan == p.Scan)
}

// expectEdit adds to e what an edit not yet purged, or undone and not yet
// swept, makes expected of the scan: each of its instances' new revision
// key and the superseded revision's, and, while it is prepared, each new
// revision's line, which the manifest and metadata name before the publish
// gives the catalog its row. Its new revisions' sizes and checksums are
// known only from its publish, so a prepared edit's line is expected by
// its key. An undone edit's superseded revisions are current again: only
// its new ones are expected.
func (s *scanCheck) expectEdit(e *scanExpect, c tenant.Correction) error {
	if !covers(c.Target, s.scan.Path) {
		return nil
	}
	ctx, cancel := context.WithTimeout(s.u.ctx, opTimeout)
	full, err := s.u.p.Correction(ctx, c.ID)
	cancel()
	if err != nil {
		return err
	}
	for _, in := range full.Edits {
		// By labels only, as a replace's: a replica holds the same keys.
		if in.Path != s.scan.Path {
			continue
		}
		key, err := layout.RevisionKey(s.scan.Path, in.UID, in.NewRevision)
		if err != nil {
			continue // a revision no key names: nothing it can explain
		}
		e.keys[key] = true
		if full.State == record.StateUndone {
			continue
		}
		e.keys[in.OldKey] = true
		if full.State == record.StatePrepared {
			e.next[in.UID] = manifest.File{Key: key, SOPInstanceUID: in.UID}
		}
	}
	return nil
}

// nextAgrees reports whether a manifest's or a metadata's line of an
// instance names the new revision n a prepared correction wrote: at its
// key, whole (its length its size), and of its size and checksum when the
// correction knows them (a replace's from its prepare; an edit's only from
// its publish, n's checksum empty until then).
func nextAgrees(n manifest.File, key string, length, size int64, sum string) bool {
	if n.Key != key || length != size {
		return false
	}
	return n.SHA256 == "" || n.Size == size && n.SHA256 == sum
}

// editRecordKeys adds to keys the old and new revision keys of the
// instances of the scan at p that a pending edit's record names, for a
// record whose correction has no row: they are objects nothing else names
// that the edit keeps.
func editRecordKeys(keys map[string]bool, r record.Correction, p label.Path) {
	if r.Kind != record.KindEdit || !pending(r.State) {
		return
	}
	in := map[string]bool{}
	for _, sc := range r.Scans {
		if pathOfScan(sc) == p {
			in[sc.SeriesUID] = true
		}
	}
	for _, x := range r.Edits {
		if !in[x.SeriesUID] {
			continue
		}
		for _, n := range []int{x.OldRevision, x.NewRevision} {
			if k, err := layout.RevisionKey(p, x.SOPInstanceUID, n); err == nil {
				keys[k] = true
			}
			if n == 1 {
				if k, err := layout.InstanceKey(p, x.SOPInstanceUID); err == nil {
					keys[k] = true
				}
			}
		}
	}
}
