package scrub

import (
	"bytes"
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A correction (tenant.Corrections) changes a scan's files on its storage
// before and after the catalog shows the change: a relabel copies a tree
// to its new labels before its publish and keeps the old one until its
// purge, a replace puts its new revision and rewrites the scan's manifest
// and metadata before its publish and keeps the superseded revision until
// its purge, and a delete hides the catalog's rows at its publish and
// removes the objects only at its purge. What a correction not yet purged
// is doing is expected, never an orphan, a missing object or a manifest
// that disagrees; anything else beside it is still found.
//
// The tenant's database is the authority on corrections while it lives.
// A correction record under meta/.corrections/ whose correction has no
// row (the database restored from before it, or lost) is counted
// (tenant.TallyCorrectionRecordOnly) and explains only objects nothing
// names, by what the scan's own manifest lists; it never explains a
// manifest, a metadata line or an instance that differs, since a record
// on a storage can be damaged or written by hand like any other object. A
// record whose MAC does not verify under the installation key for the
// tenant explains nothing at all and is counted
// (tenant.TallyCorrectionUnauthenticated).
//
// An object of a target a committed delete hides is not read for damage:
// the catalog no longer serves it, and its purge removes it.

// pendingStates are the states of a correction whose work is on a
// storage and not yet purged.
var pendingStates = []record.State{record.StatePrepared, record.StateCommitted, record.StatePurging}

// scanExpect is what the corrections not yet purged of one scan, by the
// database's rows, make expected of its files although the catalog does
// not name it.
type scanExpect struct {
	// hidden are the instances a committed instance delete hides: their
	// objects and lines stay until the purge rewrites the manifest and
	// metadata without them.
	hidden map[string]bool
	// next are, by SOP Instance UID, the line a prepared replace wrote for
	// its new revision: the manifest and the metadata name it before the
	// publish gives the catalog its row.
	next map[string]manifest.File
	// keys are the objects a replace keeps beside the catalog's: its new
	// revision before the publish, the revision it superseded until the
	// purge.
	keys map[string]bool
}

// any reports whether a correction of the scan is pending.
func (e *scanExpect) any() bool { return len(e.hidden)+len(e.next)+len(e.keys) > 0 }

// listCorrections calls fn with every correction of the kinds given not
// yet purged, by the database's rows.
func (u *unitRun) listCorrections(kinds []record.Kind, fn func(tenant.Correction) error) error {
	filter := tenant.CorrectionFilter{Kinds: kinds, States: pendingStates}
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxListPages {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		rows, next, err := u.p.ListCorrections(ctx, filter, page)
		cancel()
		if err != nil {
			return err
		}
		for _, c := range rows {
			if err := fn(c); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
	return fault.New(fault.Internal, "scrub: the list of corrections did not end")
}

// expect reads, once a look, what the corrections not yet purged of the
// scan make expected; read only when a look found something they may
// explain, so a scan with no difference costs no read of them.
func (s *scanCheck) expect() (*scanExpect, error) {
	if s.exp != nil {
		return s.exp, nil
	}
	e := &scanExpect{hidden: map[string]bool{}, next: map[string]manifest.File{}, keys: map[string]bool{}}
	err := s.u.listCorrections([]record.Kind{record.KindDelete, record.KindReplace}, func(c tenant.Correction) error {
		if c.Target.Path() != s.scan.Path || c.Target.Instance == "" {
			return nil
		}
		uid := c.Target.Instance
		switch c.Kind {
		case record.KindDelete:
			if c.State != record.StatePrepared && c.Level == record.LevelInstance {
				e.hidden[uid] = true
			}
		case record.KindReplace:
			key, err := layout.RevisionKey(s.scan.Path, uid, c.NewRevision)
			if err != nil {
				return nil // a revision no key names: nothing it can explain
			}
			e.keys[key] = true
			if c.InstanceKey != "" {
				e.keys[c.InstanceKey] = true
			}
			if c.State == record.StatePrepared {
				e.next[uid] = manifest.File{Key: key, Length: c.NewSize, Size: c.NewSize, SHA256: c.NewSHA256.String(), SOPInstanceUID: uid}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	s.exp = e
	return e, nil
}

// hiddenOf returns the instances a committed delete hides, read only when
// uids, the instances the catalog lacks, is not empty.
func (s *scanCheck) hiddenOf(uids []string) (map[string]bool, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	e, err := s.expect()
	if err != nil {
		return nil, err
	}
	return e.hidden, nil
}

// sidecarLineExpected reports whether a metadata line that is not as the
// catalog holds its instance is the one a prepared replace wrote for its
// new revision.
func (s *scanCheck) sidecarLineExpected(uid, key, sum string, size int64) (bool, error) {
	e, err := s.expect()
	if err != nil {
		return false, err
	}
	n, ok := e.next[uid]
	return ok && n.Key == key && n.Size == size && n.SHA256 == sum, nil
}

// expectedObject reports whether an object under the scan's keys that
// nothing names is one a correction not yet purged keeps there: a
// replace's new or superseded revision, by the database's rows or, for a
// replace with no row, by its record on the storage.
func (s *scanCheck) expectedObject(key string) (bool, error) {
	e, err := s.expect()
	if err != nil || e.keys[key] {
		return err == nil, err
	}
	if s.recs == nil {
		recs, _, err := s.u.recordsOnly(s.store, s.scan.Storage)
		if err != nil {
			return false, err
		}
		s.recs = map[string]bool{}
		for _, r := range recs {
			if r.Kind != record.KindReplace || r.Replace == nil || !pending(r.State) || len(r.Scans) != 1 || pathOfScan(r.Scans[0]) != s.scan.Path {
				continue
			}
			for _, n := range []int{r.Replace.OldRevision, r.Replace.NewRevision} {
				if k, err := layout.RevisionKey(s.scan.Path, r.Replace.SOPInstanceUID, n); err == nil {
					s.recs[k] = true
				}
			}
		}
	}
	return s.recs[key], nil
}

// manifestCorrected reports whether a manifest that is not of the checksum
// the catalog holds is the one a correction not yet purged of the scan
// wrote: a prepared replace's, naming its new revision before the publish
// gives the catalog the new checksum, or an instance delete's purge's,
// written without the hidden instance before the purge ends. It is when
// the manifest decodes, lists each instance once, lists every instance the
// catalog holds as the catalog holds it or as a prepared replace wrote it,
// and lists nothing else but instances a delete hides. It returns the keys
// the manifest lists then.
func (s *scanCheck) manifestCorrected(v catalogView, data []byte) (map[string]bool, bool, error) {
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false, nil
	}
	e, err := s.expect()
	if err != nil || !e.any() {
		return nil, false, err
	}
	listed := make(map[string]bool, len(m.Files))
	seen := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		if seen[f.SOPInstanceUID] {
			return nil, false, nil
		}
		listed[f.Key], seen[f.SOPInstanceUID] = true, true
		if e.hidden[f.SOPInstanceUID] {
			continue
		}
		if n, ok := e.next[f.SOPInstanceUID]; ok && f.Key == n.Key && f.Offset == 0 && f.Length == n.Length && f.Size == n.Size &&
			f.SHA256 == n.SHA256 {
			continue
		}
		in, held := v.byUID[f.SOPInstanceUID]
		if !held || !lineAgrees(f, in) {
			return nil, false, nil
		}
	}
	for uid := range v.byUID {
		if !seen[uid] {
			return nil, false, nil
		}
	}
	return listed, true, nil
}

// lineAgrees reports whether a manifest's line says of its instance what
// the catalog's row says.
func lineAgrees(f manifest.File, in tenant.ScanInstance) bool {
	return f.Key == in.Key && f.Offset == in.Offset && f.Length == in.Length && f.Size == in.Size && f.SHA256 == in.ArchivedSHA256.String()
}

// pending reports whether a correction in state st is not yet purged.
func pending(st record.State) bool {
	return st == record.StatePrepared || st == record.StateCommitted || st == record.StatePurging
}

// pathOfScan returns a record's scan's labels.
func pathOfScan(s record.Scan) label.Path {
	return label.Path{Subject: s.Subject, Session: s.Session, Scan: s.Scan}
}

// recordCount counts the correction records of a storage a scrub reads.
type recordCount struct {
	// noRow counts the records whose correction the tenant's database
	// holds no row of (tenant.TallyCorrectionRecordOnly).
	noRow int64
	// unauthenticated counts those of them in the form whose MAC is missing
	// or does not verify (tenant.TallyCorrectionUnauthenticated).
	unauthenticated int64
}

// recordTenant returns the tenant's id, which its records are
// authenticated for, read once a unit; a failure is a databaseError.
func (u *unitRun) recordTenant() (string, error) {
	if u.tenantID != "" {
		return u.tenantID, nil
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	id, err := u.sup.cfg.TenantID(ctx, u.t.name)
	if err != nil {
		return "", databaseError{err}
	}
	u.tenantID = id
	return id, nil
}

// recordsOnly reads the correction records on store, the storage named
// storage, whose correction the tenant's database holds no row of, and
// returns those that decode and verify under the installation key for the
// tenant, with how many such records it found and how many of them did not
// verify. A record that does not decode explains nothing; one that does
// not verify explains nothing either, since anyone who can write to a
// storage can put one there to pass an orphan off as a correction's. A
// failure of the database is a databaseError.
func (u *unitRun) recordsOnly(store Storage, storage string) ([]record.Correction, recordCount, error) {
	var n recordCount
	keys, err := listAll(u.ctx, store, layout.CorrectionsPrefix, "")
	if err != nil {
		return nil, n, err
	}
	var out []record.Correction
	for _, k := range keys {
		id, err := layout.ParseCorrectionKey(k)
		if err != nil {
			continue // not a record's key: the walk's look for orphans is not made under this prefix
		}
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		_, err = u.p.Correction(ctx, id)
		cancel()
		if err == nil {
			continue
		}
		if fault.KindOf(err) != fault.NotFound {
			return nil, n, databaseError{err}
		}
		n.noRow++
		data, p, err := u.readWhole(store, storage, k, record.MaxBytes)
		if err != nil {
			return nil, n, err
		}
		if p != present {
			continue
		}
		tenantID, err := u.recordTenant()
		if err != nil {
			return nil, n, err
		}
		r, err := record.DecodeCorrection(bytes.NewReader(data), tenantID, u.sup.cfg.Keys)
		if errors.Is(err, record.ErrUnauthenticated) {
			n.unauthenticated++
			continue
		}
		if err != nil || r.ID != id {
			continue
		}
		out = append(out, r)
	}
	return out, n, nil
}

// coverage is, by a scan's labels, what corrections not yet purged leave
// under them on one storage while the catalog holds no scan there: each
// path whose own manifest names what is expected, and the paths whose
// manifests' objects a relabel copies to it.
type coverage map[label.Path][]label.Path

// add records that the objects under to are expected as the manifest
// under from names them (from is to itself but for a relabel's new tree).
func (c coverage) add(to, from label.Path) {
	for _, f := range c[to] {
		if f == from {
			return
		}
	}
	c[to] = append(c[to], from)
}

// coverageOf reads what the corrections not yet purged leave on the
// storage: by the database's rows, a published delete's scans and a
// relabel's old and new trees; by the records with no row that verify, the
// same. A correction's scans are those of catalog, the storage whose scans
// the catalog holds for this one (its primary, for a replica, which holds
// the same keys). It returns how many records have no row, and how many of
// those do not verify.
func (u *unitRun) coverageOf(store Storage, storage, catalog string) (coverage, recordCount, error) {
	cov := coverage{}
	err := u.listCorrections(nil, func(c tenant.Correction) error {
		if c.Kind == record.KindReplace || c.Kind == record.KindDelete && (c.State == record.StatePrepared || c.Level == record.LevelInstance) {
			return nil // a replace's scan, and an instance delete's, stay in the catalog
		}
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		scans, err := u.p.CorrectionScans(ctx, c.ID)
		cancel()
		if err != nil {
			return err
		}
		for _, s := range scans {
			if s.Storage != catalog {
				continue
			}
			cov.add(s.Path, s.Path)
			if c.Kind == record.KindRelabel && s.NewPath != (label.Path{}) {
				cov.add(s.NewPath, s.NewPath)
				cov.add(s.NewPath, s.Path)
			}
		}
		return nil
	})
	if err != nil {
		return nil, recordCount{}, databaseError{err}
	}
	recs, n, err := u.recordsOnly(store, storage)
	if err != nil {
		return nil, recordCount{}, err
	}
	for _, r := range recs {
		if !pending(r.State) || r.Kind == record.KindReplace ||
			r.Kind == record.KindDelete && (r.State == record.StatePrepared || r.Level == record.LevelInstance) {
			continue
		}
		for _, s := range r.Scans {
			p := pathOfScan(s)
			cov.add(p, p)
			if r.Relabel != nil {
				if to, ok := relabeled(p, *r.Relabel); ok {
					cov.add(to, to)
					cov.add(to, p)
				}
			}
		}
	}
	return cov, n, nil
}

// relabeled returns the labels a relabel gives the scan at p.
func relabeled(p label.Path, r record.Relabel) (label.Path, bool) {
	switch r.Level {
	case record.LevelSubject:
		p.Subject = r.To
	case record.LevelSession:
		p.Session = r.To
	case record.LevelScan:
		p.Scan = r.To
	default:
		return label.Path{}, false
	}
	return p, true
}

// expectedUnder returns the keys under the scan path p that the
// corrections covering it (from) leave there: p's manifest and metadata,
// each object p's own manifest lists, and each object a manifest under a
// path a relabel copies from lists, at its key under p.
func (u *unitRun) expectedUnder(store Storage, storage string, p label.Path, from []label.Path) (map[string]bool, error) {
	out := map[string]bool{}
	mk, err := layout.ManifestKey(p)
	if err != nil {
		return out, nil
	}
	sk, _ := layout.MetadataKey(p)
	out[mk], out[sk] = true, true
	for _, f := range from {
		key, err := layout.ManifestKey(f)
		if err != nil {
			continue
		}
		data, pr, err := u.readWhole(store, storage, key, int64(manifest.MaxBytes))
		if err != nil {
			return nil, err
		}
		if pr != present {
			continue
		}
		m, err := manifest.Decode(bytes.NewReader(data))
		if err != nil {
			continue // a manifest that does not decode names nothing
		}
		for _, line := range m.Files {
			k, err := layout.Parse(line.Key)
			if err != nil || k.Kind != layout.Instance || k.Path != f {
				continue
			}
			k.Path = p
			if s, err := k.String(); err == nil {
				out[s] = true
			}
		}
	}
	return out, nil
}

// dropExpected removes from keys those in expected.
func dropExpected(keys []string, expected map[string]bool) []string {
	out := keys[:0]
	for _, k := range keys {
		if !expected[k] {
			out = append(out, k)
		}
	}
	return out
}
