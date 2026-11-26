package scrub

import (
	"bytes"
	"context"
	"errors"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// walk is the listing unit's look for orphans outside every scan the
// catalog holds, storage by storage: it lists each storage's data/ and
// meta/ trees one level at a time, down to the scans' level, and each
// object above that level, or under a scan's keys that no scan of this
// storage in the catalog has, is an orphan, unless a reception in flight
// explains it, or a correction not yet purged leaves it (orphansOutside).
// The correction records under meta/.corrections/ and the owner record
// are not a scan's and are skipped. An orphan is believed after a second look, SettleDelay later.
// An open orphan finding outside every scan whose object is gone, or is
// now a scan's, is cleared. It only lists and reads: nothing is written to
// a storage.
//
// A storage that does not answer its listing is passed over and tallied
// (tenant.TallyStorageUnreadable), and the walk goes on with the next: one
// storage out of reach does not hold back the others, nor the listing of
// the job's scans, whose units each read their own storage. Within one
// storage the walk is not resumable: a unit given back walks it again.
func (u *unitRun) walk() outcome {
	if !u.job.Target.Whole() {
		// A scrub of one scan reads that scan alone: the look for objects
		// outside every scan is a whole storage's.
		return outcome{}
	}
	storages := []string{u.job.Storage}
	if u.job.Storage == "" {
		var err error
		if storages, err = u.storageNames(); err != nil {
			return u.failure(err, reasonDatabase)
		}
	}
	unreadable := int64(0)
	for _, name := range storages {
		if out, stop := u.stopping(); stop {
			return out
		}
		out := u.walkOne(name)
		if out == (outcome{}) {
			continue
		}
		if !out.temporary || out.reason != reasonStorage {
			return out
		}
		unreadable++
		u.log.Warn("scrubs: a storage could not be listed for orphans; the look is passed over in this job", "storage", name)
	}
	if unreadable == 0 {
		return outcome{}
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	if err := u.p.AddScrubTallies(ctx, u.fence, map[string]int64{tenant.TallyStorageUnreadable: unreadable}); err != nil {
		return u.failure(err, reasonDatabase)
	}
	return outcome{}
}

// walkOne opens the storage named name and looks for orphans outside every
// scan on it.
func (u *unitRun) walkOne(name string) outcome {
	store, err := u.stores.Get(u.ctx, name)
	if err != nil {
		return u.failure(err, reasonStorage)
	}
	return u.walkStorage(store, name)
}

// storageNames returns the names of every storage of the tenant.
func (u *unitRun) storageNames() ([]string, error) {
	var out []string
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxListPages {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		rows, next, err := u.p.ListStorages(ctx, page)
		cancel()
		if err != nil {
			return nil, err
		}
		for _, s := range rows {
			out = append(out, s.Name)
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
	return nil, fault.New(fault.Internal, "scrub: the list of storages did not end")
}

// walkStorage looks for orphans outside every scan on one storage.
func (u *unitRun) walkStorage(store Storage, storage string) outcome {
	first, recs, err := u.orphansOutside(store, storage)
	if err != nil {
		return u.failure(err, walkReason(err))
	}
	if recs.noRow > 0 {
		u.log.Warn("scrubs: correction records on a storage have no row in the tenant's database; they explain only objects nothing names",
			"storage", storage, "records", recs.noRow)
		tallies := map[string]int64{tenant.TallyCorrectionRecordOnly: recs.noRow}
		if recs.unauthenticated > 0 {
			u.log.Warn("scrubs: correction records on a storage do not verify under the installation key; they explain nothing",
				"storage", storage, "records", recs.unauthenticated)
			tallies[tenant.TallyCorrectionUnauthenticated] = recs.unauthenticated
		}
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		err := u.p.AddScrubTallies(ctx, u.fence, tallies)
		cancel()
		if err != nil {
			return u.failure(err, reasonDatabase)
		}
	}
	believed := map[string]bool{}
	if len(first) > 0 {
		if err := u.sup.cfg.seams.sleep(u.ctx, u.sup.cfg.seams.settle); err != nil {
			return outcome{stopped: true}
		}
		second, _, err := u.orphansOutside(store, storage)
		if err != nil {
			return u.failure(err, walkReason(err))
		}
		for k := range second {
			if first[k] {
				believed[k] = true
			}
		}
	}
	for k := range believed {
		f := tenant.ScrubFinding{Kind: tenant.FindingOrphan, Storage: storage, Key: k}
		f.ID = tenant.ScrubFindingID(u.job.ID, storage, k, f.Kind)
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		_, _, err := u.p.RecordScrubFinding(ctx, tenant.ScrubFindingRecord{Fence: u.fence, Finding: f, Now: u.sup.cfg.Now()})
		cancel()
		if err != nil {
			return u.failure(err, reasonDatabase)
		}
		u.sup.cfg.Wake(u.t.name)
	}
	return u.clearOutside(storage, believed)
}

// clearOutside clears each open orphan finding of the storage that is not
// among those believed now and names no scan, or names a scan the catalog
// no longer holds on the storage (for a replica, on its primary): the walk
// just looked at every object outside the storage's scans, so an orphan
// of a scan gone is either believed again now or gone. An orphan of a
// scan still held is the scan's unit's to clear.
func (u *unitRun) clearOutside(storage string, believed map[string]bool) outcome {
	filter := tenant.ScrubFindingFilter{Kinds: []tenant.ScrubFindingKind{tenant.FindingOrphan}, Storage: storage}
	page := master.Page{Limit: master.MaxPageLimit}
	var stale []string
	held := map[label.Path]bool{}
	for range maxListPages {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		rows, next, err := u.p.ListScrubFindings(ctx, filter, page)
		cancel()
		if err != nil {
			return u.failure(err, reasonDatabase)
		}
		for _, f := range rows {
			if believed[f.Key] {
				continue
			}
			if f.Path != (label.Path{}) {
				in, ok := held[f.Path]
				if !ok {
					if in, err = u.heldOn(storage, f.Path); err != nil {
						return u.failure(err, reasonDatabase)
					}
					held[f.Path] = in
				}
				if in {
					continue
				}
			}
			stale = append(stale, f.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	for _, id := range stale {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		_, _, err := u.p.ClearScrubFinding(ctx, u.fence, id, u.sup.cfg.Now())
		cancel()
		if err != nil {
			return u.failure(err, reasonDatabase)
		}
	}
	return outcome{}
}

// heldOn reports whether the catalog holds the scan at path on the storage
// named storage, or, for a replica, on its primary.
func (u *unitRun) heldOn(storage string, path label.Path) (bool, error) {
	catalog, err := u.catalogStorage(storage)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	sc, err := u.p.CatalogScan(ctx, path)
	if fault.KindOf(err) == fault.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return sc.Storage == catalog, nil
}

// databaseError is a failure of the tenant's database during a walk, as
// opposed to the storage's: the walk is then given back, never passed
// over as a storage out of reach.
type databaseError struct{ error }

// Unwrap returns the database's error.
func (e databaseError) Unwrap() error { return e.error }

// walkReason returns the reason a walk that failed with err is given back
// for.
func walkReason(err error) string {
	var d databaseError
	if errors.As(err, &d) {
		return reasonDatabase
	}
	return reasonStorage
}

// walkResult is what one listing of a storage's trees found outside every
// scan of the catalog on it.
type walkResult struct {
	// out are the keys that are orphans whatever any reception claims.
	out map[string]bool
	// inFlight are instances' objects above the scans' level, or under a
	// label no scan can have, which a reception's claim explains.
	inFlight []string
	// paths are the objects under a scan's keys that no scan of this
	// storage in the catalog has, by the scan's labels: a reception in
	// flight writes a new scan's objects, manifest and sidecar before it
	// commits the scan.
	paths map[label.Path]*pathKeys
	// catalog names the storage whose scans the catalog holds for the
	// storage walked: itself, or, for a replica, its primary, whose keys it
	// holds (catalogStorage).
	catalog string
}

// catalogStorage returns the name of the storage whose scans the catalog
// holds for the storage named name: a replica's primary, since a replica
// holds its primary's objects under the same keys and the catalog names
// only the primary; name itself otherwise.
func (u *unitRun) catalogStorage(name string) (string, error) {
	row, err := u.storageRow(name)
	if err != nil {
		return "", err
	}
	if row.ReplicaOf != "" {
		return row.ReplicaOf, nil
	}
	return name, nil
}

// storageRow returns the storage named name as the tenant's database holds
// it, read once a unit.
func (u *unitRun) storageRow(name string) (tenant.Storage, error) {
	if s, ok := u.rows[name]; ok {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	s, err := u.p.Storage(ctx, name)
	if err != nil {
		return tenant.Storage{}, err
	}
	if u.rows == nil {
		u.rows = map[string]tenant.Storage{}
	}
	u.rows[name] = s
	return s, nil
}

// pathKeys are the objects under one scan's keys, in its data/ and meta/
// trees.
type pathKeys struct {
	data, meta []string
}

// orphansOutside lists the storage's data/ and meta/ trees down to the
// scans' level once and returns the keys of the objects outside every scan
// of this storage the catalog holds, but those a correction not yet purged
// leaves (a published delete's scans, a relabel's old and new trees, as
// their manifests name the objects: coverageOf) and those a reception in
// flight explains: an instance's object it claims, and the meta/ objects
// of a scan path it writes (one of whose instances it claims, by the
// path's data/ keys or by the manifest put under the path). It also
// returns how many correction records on the storage have no row, and how
// many of those do not verify.
func (u *unitRun) orphansOutside(store Storage, storage string) (map[string]bool, recordCount, error) {
	catalog, err := u.catalogStorage(storage)
	if err != nil {
		return nil, recordCount{}, databaseError{err}
	}
	w := &walkResult{out: map[string]bool{}, paths: map[label.Path]*pathKeys{}, catalog: catalog}
	for _, root := range []string{layout.DataRoot, layout.MetaRoot} {
		if err := u.walkLevel(store, storage, root, nil, w); err != nil {
			return nil, recordCount{}, err
		}
	}
	cov, recordOnly, err := u.coverageOf(store, storage, catalog)
	if err != nil {
		return nil, recordCount{}, err
	}
	for p, pk := range w.paths {
		from, ok := cov[p]
		if !ok {
			continue
		}
		// A scan the catalog no longer, or not yet, holds here because a
		// correction not yet purged moved it: what that correction leaves
		// under its keys is expected, anything else is still looked at.
		expected, err := u.expectedUnder(store, storage, p, from)
		if err != nil {
			return nil, recordCount{}, err
		}
		pk.data, pk.meta = dropExpected(pk.data, expected), dropExpected(pk.meta, expected)
	}
	out, err := u.orphansOf(store, storage, w)
	return out, recordOnly, err
}

// orphansOf returns the keys of w that are orphans, but those a reception
// in flight explains (orphansOutside).
func (u *unitRun) orphansOf(store Storage, storage string, w *walkResult) (map[string]bool, error) {
	candidates := append([]string(nil), w.inFlight...)
	for _, pk := range w.paths {
		candidates = append(candidates, pk.data...)
	}
	claimed, err := u.claimedAnywhere(candidates)
	if err != nil {
		return nil, databaseError{err}
	}
	isClaimed := func(k string) bool {
		lk, err := layout.Parse(k)
		return err == nil && lk.Kind == layout.Instance && claimed[lk.SOPInstanceUID]
	}
	for _, k := range w.inFlight {
		if !isClaimed(k) {
			w.out[k] = true
		}
	}
	for p, pk := range w.paths {
		writing := false
		for _, k := range pk.data {
			if isClaimed(k) {
				writing = true
				continue
			}
			w.out[k] = true
		}
		if !writing && len(pk.meta) > 0 {
			if writing, err = u.manifestClaimed(store, storage, p, pk.meta); err != nil {
				return nil, err
			}
		}
		if writing {
			continue // a reception in flight writes this scan
		}
		for _, k := range pk.meta {
			w.out[k] = true
		}
	}
	return w.out, nil
}

// manifestClaimed reports whether the manifest among the meta/ keys of the
// scan path p decodes and lists an instance a reception claims: the scan's
// own files, put before the reception commits it.
func (u *unitRun) manifestClaimed(store Storage, storage string, p label.Path, meta []string) (bool, error) {
	key, err := layout.ManifestKey(p)
	if err != nil {
		return false, nil
	}
	has := false
	for _, k := range meta {
		has = has || k == key
	}
	if !has {
		return false, nil
	}
	data, pr, err := u.readWhole(store, storage, key, int64(manifest.MaxBytes))
	if err != nil || pr != present {
		return false, err
	}
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		return false, nil
	}
	keys := make([]string, 0, len(m.Files))
	for _, f := range m.Files {
		keys = append(keys, f.Key)
	}
	claimed, err := u.claimedAnywhere(keys)
	if err != nil {
		return false, databaseError{err}
	}
	return len(claimed) > 0, nil
}

// walkLevel lists one level under prefix, whose labels so far are labels:
// an object at this level is an orphan; a prefix is listed further, down
// to the scan's level, where a scan the catalog holds on this storage (on
// its primary, for a replica) is left to its own unit and any other's objects are kept aside by their
// path, for orphansOutside to ask whether a reception writes them.
func (u *unitRun) walkLevel(store Storage, storage, prefix string, labels []string, w *walkResult) error {
	if o, stop := u.stopping(); stop && o.stopped {
		return u.ctx.Err()
	}
	if len(labels) == 3 {
		p := label.Path{Subject: labels[0], Session: labels[1], Scan: labels[2]}
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		sc, err := u.p.CatalogScan(ctx, p)
		cancel()
		switch {
		case err == nil && sc.Storage == w.catalog:
			return nil // the scan's own unit looks under its keys, on every copy it reads
		case err != nil && fault.KindOf(err) != fault.NotFound:
			return databaseError{err}
		}
		keys, err := listAll(u.ctx, store, prefix, "")
		if err != nil {
			return err
		}
		pk := w.paths[p]
		if pk == nil {
			pk = &pathKeys{}
			w.paths[p] = pk
		}
		if strings.HasPrefix(prefix, layout.MetaRoot) {
			pk.meta = append(pk.meta, keys...)
		} else {
			pk.data = append(pk.data, keys...)
		}
		return nil
	}
	entries, err := listAll(u.ctx, store, prefix, "/")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e, "/") {
			if e == layout.OwnerKey {
				continue
			}
			w.add(e)
			continue
		}
		if e == layout.CorrectionsPrefix {
			continue // the correction records, not a scan's
		}
		segment := strings.TrimSuffix(strings.TrimPrefix(e, prefix), "/")
		if !label.Valid(segment) {
			// No scan has a label like this: everything under it is an
			// orphan.
			keys, err := listAll(u.ctx, store, e, "")
			if err != nil {
				return err
			}
			for _, k := range keys {
				w.add(k)
			}
			continue
		}
		if err := u.walkLevel(store, storage, e, append(append([]string(nil), labels...), segment), w); err != nil {
			return err
		}
	}
	return nil
}

// add puts an orphan's key in out, or in inFlight when it is an instance's
// object a reception may claim.
func (w *walkResult) add(key string) {
	if lk, err := layout.Parse(key); err == nil && lk.Kind == layout.Instance {
		w.inFlight = append(w.inFlight, key)
		return
	}
	w.out[key] = true
}

// claimedAnywhere returns which instances of keys a reception claims now:
// asked with any study and series, since a claim is answered Claimed
// whatever they are.
func (u *unitRun) claimedAnywhere(keys []string) (map[string]bool, error) {
	var uids []string
	for _, k := range keys {
		if lk, err := layout.Parse(k); err == nil && lk.Kind == layout.Instance {
			uids = append(uids, lk.SOPInstanceUID)
		}
	}
	s := &scanCheck{u: u}
	return s.claimed(catalogView{studyUID: anyUID, seriesUID: anyUID}, uids)
}

// anyUID is a UID a duplicate check is asked with when only whether a UID
// is claimed matters.
const anyUID = "2.25.1"
