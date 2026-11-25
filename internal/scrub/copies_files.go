package scrub

import (
	"bytes"
	"context"
	"crypto/sha256"

	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// copyLook is what one look at a replica's copy of a scan's files found.
type copyLook struct {
	// ok says the catalog still holds the scan at the planned manifest
	// digest; a look that is not ok found nothing.
	ok bool
	// manifest is what was read of the manifest when it is not the
	// catalog's; manifestGood says it was read and is.
	manifest     *measured
	manifestGood bool
	// lines are the manifest's lines of instances the catalog does not
	// serve (a delete hides them until its purge) whose object was not as
	// the line says; linesGood those read good, by SOP Instance UID.
	lines     map[string]lineDiff
	linesGood map[string]string
	// strays are the objects under the scan's keys the manifest does not
	// name; straysLooked says the look listed them.
	strays       map[string]bool
	straysLooked bool
	// agrees are the keys a finding that the scan's files disagree with the
	// catalog names, read to agree now: an instance's key whose line in a
	// manifest at the catalog's digest says what the catalog's row says, and
	// the sidecar's key when its lines agree (read only when such a finding
	// holds of it).
	agrees map[string]bool
	// tallies are what the look counts.
	tallies map[string]int64
}

// lineDiff is a manifest's line whose object a look found otherwise.
type lineDiff struct {
	key  string
	size int64
	sum  tenant.SHA256
	m    measured
}

// differs reports whether the look found something to record.
func (r copyLook) differs() bool { return r.manifest != nil || len(r.lines) > 0 || len(r.strays) > 0 }

// both keeps what r and next both found, as next found it, and what next
// found good.
func (r copyLook) both(next copyLook) copyLook {
	out := next
	if r.manifest == nil {
		out.manifest = nil
	}
	out.lines = map[string]lineDiff{}
	for uid, d := range next.lines {
		if _, ok := r.lines[uid]; ok {
			out.lines[uid] = d
		}
	}
	out.strays = map[string]bool{}
	for k := range next.strays {
		if r.strays[k] {
			out.strays[k] = true
		}
	}
	return out
}

// files compares the replica's copy of the scan's manifest with the
// catalog's digest, reads the objects of the manifest's lines the catalog
// does not serve, and lists the objects under the scan's keys for those
// the manifest does not name. A difference is believed only when a second
// look, SettleDelay later, finds it too. It records what it believes, and
// clears what it read good again.
func (c *copyCheck) files() outcome {
	if c.dropped {
		return outcome{}
	}
	r, out := c.look()
	if out != (outcome{}) || !r.ok || c.dropped {
		return out
	}
	if r.differs() {
		if err := c.u.sup.cfg.seams.sleep(c.u.ctx, c.u.sup.cfg.seams.settle); err != nil {
			return outcome{stopped: true}
		}
		next, out := c.look()
		if out != (outcome{}) || !next.ok || c.dropped {
			return out
		}
		r = r.both(next)
	}
	for k, n := range r.tallies {
		c.tallies[k] += n
	}
	mk, _ := layout.ManifestKey(c.path)
	if r.manifest != nil {
		if _, out := c.judge("", mk, r.manifest.size, c.manifest, *r.manifest); out != (outcome{}) {
			return out
		}
	} else if r.manifestGood {
		if out := c.good("", mk); out != (outcome{}) {
			return out
		}
	}
	for uid, d := range r.lines {
		if _, out := c.judge(uid, d.key, d.size, d.sum, d.m); out != (outcome{}) {
			return out
		}
	}
	for uid, key := range r.linesGood {
		if out := c.good(uid, key); out != (outcome{}) {
			return out
		}
	}
	found := map[findingKey]bool{}
	for k := range r.strays {
		if held, out := c.stillHeld(); !held {
			return out
		}
		f := tenant.ScrubFinding{Kind: tenant.FindingOrphan, Storage: c.name, Key: k, Path: c.path}
		if out := c.u.recordFinding(f); out != (outcome{}) {
			return out
		}
		found[findingKey{k, f.Kind}] = true
		c.u.log.Warn("scrubs: a replica holds an object its scan's manifest does not name; it is recorded, never removed by a scrub",
			"storage", c.name)
	}
	return c.clear(r, found)
}

// clear clears each finding of the scan's files on the replica's name that
// held when the unit started and that this look read good: an orphan no
// longer there or now named, a manifest read at the catalog's digest, a
// disagreement whose line (or sidecar) now agrees. A finding of an
// instance's object was cleared by its read (good).
func (c *copyCheck) clear(r copyLook, found map[findingKey]bool) outcome {
	for k, f := range c.open {
		var good bool
		switch k.kind {
		case tenant.FindingOrphan:
			good = r.straysLooked
		case tenant.FindingManifestDamaged:
			good = r.manifestGood
		case tenant.FindingDisagreeing:
			good = r.manifestGood && r.agrees[k.key]
		}
		if !good || found[k] {
			continue
		}
		ctx, cancel := context.WithTimeout(c.u.ctx, opTimeout)
		_, _, err := c.u.p.ClearScrubFinding(ctx, c.u.fence, f.ID, c.u.sup.cfg.Now())
		cancel()
		if err != nil {
			return c.u.failure(err, reasonDatabase)
		}
		c.u.log.Info("scrubs: a finding on a replica was read good again and is cleared", "storage", c.name, "kind", string(f.Kind))
		c.u.sup.cfg.Wake(c.u.t.name)
	}
	return outcome{}
}

// look reads the replica's copy of the scan's files once.
func (c *copyCheck) look() (copyLook, outcome) {
	r := copyLook{lines: map[string]lineDiff{}, linesGood: map[string]string{}, strays: map[string]bool{}, agrees: map[string]bool{},
		tallies: map[string]int64{}}
	c.sc.exp, c.sc.recs = nil, nil // a correction may have moved since the last look
	v, ok, err := c.sc.view()
	if err != nil {
		return r, c.u.failure(err, reasonDatabase)
	}
	if !ok || v.manifestSHA256 != c.manifest {
		return r, outcome{} // changed in the catalog: the replica's row is pending again
	}
	r.ok = true
	mk, err := layout.ManifestKey(c.path)
	if err != nil {
		return r, c.u.failure(fault.Wrap(fault.Internal, err, "the scan's labels make no key"), reasonInternal)
	}
	data, p, err := c.u.readWhole(c.store, c.name, mk, int64(manifest.MaxBytes))
	switch {
	case err != nil:
		return r, c.lost(err)
	case p == notOnline:
		r.tallies[tenant.TallyNotOnline]++
		return r, outcome{}
	case p == missing:
		r.manifest = &measured{presence: missing}
		return r, outcome{}
	}
	sum := tenant.SHA256(sha256.Sum256(data))
	if sum != c.manifest {
		listed, corrected, err := c.sc.manifestCorrected(v, data)
		if err != nil {
			return r, c.u.failure(err, reasonDatabase)
		}
		if corrected {
			// A correction not yet ended wrote it on every copy (a purge
			// rewrites the manifest without the instances it removes
			// before the catalog takes the new digest): nothing to record
			// of it, and what it names is not a stray.
			return r, c.lookStrays(listed, &r)
		}
		r.manifest = &measured{presence: present, size: int64(len(data)), sum: sum}
		return r, outcome{}
	}
	m, err := manifest.Decode(bytes.NewReader(data))
	if err != nil {
		// The catalog's own manifest does not decode: the scrub of the
		// scan's storage finds it there; nothing here names an object.
		return r, outcome{}
	}
	r.manifestGood = true
	for _, f := range m.Files {
		if in, held := v.byUID[f.SOPInstanceUID]; held && lineAgrees(f, in) {
			r.agrees[f.Key] = true
		}
	}
	if out := c.lookSidecar(v, &r); out != (outcome{}) || c.dropped {
		return r, out
	}
	if out := c.lookLines(v, m, &r); out != (outcome{}) || c.dropped {
		return r, out
	}
	listed := make(map[string]bool, len(m.Files))
	for _, f := range m.Files {
		listed[f.Key] = true
	}
	return r, c.lookStrays(listed, &r)
}

// lookSidecar reads the replica's sidecar against the catalog, as the
// scan's storage's is read (scanCheck.lookSidecar), only when a finding
// that it disagrees holds on the replica's name (a switch moves one there
// with its place): a replica's sidecar is otherwise not read, since the
// replicator copies it with the scan. Its lines agreeing clears that
// finding.
func (c *copyCheck) lookSidecar(v catalogView, r *copyLook) outcome {
	sk, err := layout.MetadataKey(c.path)
	if err != nil {
		return c.u.failure(fault.Wrap(fault.Internal, err, "the scan's labels make no key"), reasonInternal)
	}
	if _, held := c.open[findingKey{sk, tenant.FindingDisagreeing}]; !held {
		return outcome{}
	}
	lr := lookResult{good: map[findingKey]bool{}, tallies: map[string]int64{}}
	if err := c.sc.lookSidecar(v, &lr); err != nil {
		return c.lost(err)
	}
	if lr.good[findingKey{sk, tenant.FindingDisagreeing}] {
		r.agrees[sk] = true
	}
	return outcome{}
}

// lookLines reads the object of each of the manifest's lines whose
// instance the catalog does not serve, against the line: a delete hides an
// instance from the catalog at its publish, and the replica holds its
// object, as the primary does, until the purge, so an undo finds it whole.
// An instance a purging delete hides is passed over: its object is being
// removed from every copy, and a replica copied after the purge began
// never held it.
func (c *copyCheck) lookLines(v catalogView, m manifest.Manifest, r *copyLook) outcome {
	var purging map[string]bool
	for _, f := range m.Files {
		if _, held := v.byUID[f.SOPInstanceUID]; held {
			continue // read with the scan's instances
		}
		if purging == nil {
			var err error
			if purging, err = c.purging(); err != nil {
				return c.u.failure(err, reasonDatabase)
			}
		}
		if purging[f.SOPInstanceUID] {
			r.tallies[TallyReplicaPurging]++
			continue
		}
		offset, length, size := ownBytes(tenant.ScanInstance{Offset: f.Offset, Length: f.Length, Size: f.Size})
		sum := hexSHA256(f.SHA256)
		got, err := c.measure(f.Key, offset, length, size, sum, f.Size)
		if err != nil {
			return c.lost(err)
		}
		switch {
		case got.presence == notOnline:
			r.tallies[tenant.TallyNotOnline]++
		case got.matches(size, sum):
			r.linesGood[f.SOPInstanceUID] = f.Key
		default:
			r.lines[f.SOPInstanceUID] = lineDiff{key: f.Key, size: size, sum: sum, m: got}
		}
	}
	return outcome{}
}

// lookStrays lists the objects under the scan's keys on the replica, in
// data/ and meta/, and keeps each that the manifest does not name (listed),
// that is not the manifest or the metadata file, and that no correction
// not yet purged keeps there (a replace's other revision).
func (c *copyCheck) lookStrays(listed map[string]bool, r *copyLook) outcome {
	data, err := layout.InstancePrefix(c.path)
	if err != nil {
		return c.u.failure(fault.Wrap(fault.Internal, err, "the scan's labels make no key"), reasonInternal)
	}
	mk, _ := layout.ManifestKey(c.path)
	sk, _ := layout.MetadataKey(c.path)
	for _, prefix := range []string{data, mk[:len(mk)-len(layout.ManifestName)]} {
		keys, err := listAll(c.u.ctx, c.store, prefix, "")
		if err != nil {
			return c.lost(err)
		}
		for _, k := range keys {
			if listed[k] || k == mk || k == sk {
				continue
			}
			kept, err := c.sc.expectedObject(k)
			if err != nil {
				return c.u.failure(err, walkReason(err))
			}
			if !kept {
				r.strays[k] = true
			}
		}
	}
	r.straysLooked = true
	return outcome{}
}

// purging returns the instances of the scan a delete in state purging
// hides, by the tenant's database.
func (c *copyCheck) purging() (map[string]bool, error) {
	out := map[string]bool{}
	err := c.u.listCorrections([]record.Kind{record.KindDelete}, func(x tenant.Correction) error {
		if x.State == record.StatePurging && x.Level == record.LevelInstance && x.Target.Instance != "" && x.Target.Path() == c.path {
			out[x.Target.Instance] = true
		}
		return nil
	})
	return out, err
}
