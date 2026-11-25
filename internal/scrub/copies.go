package scrub

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A replica (tenant.Replication) holds byte-identical copies of its
// primary's objects under the same keys, so a scrub reads it as it reads
// the primary: each instance's object against the catalog's size and
// SHA-256, the scan's manifest against the catalog's digest, and the
// objects under the scan's keys against what the manifest names. What it
// finds wrong on a replica is the replica's damage
// (tenant.Replication.RecordReplicaDamage, under the unit's lease), never
// a mark on the instance: the instance's reads keep going to its primary,
// and the replicator copies the object again from it. An object read good
// again clears that damage, and any finding a switch moved to the
// replica's name with its place: a switch moves each place's open findings
// with it, so the findings once on a primary's place are the replica's
// after it, and they stand until a read of the replica shows them gone.
// Those findings are read once a scan (load), of every kind, and only what
// they and the row's damage name is cleared: a scan read good writes
// nothing.
//
// A replica that opens but fails a read or a listing is dropped for the
// rest of the unit: tallied unreadable, nothing recorded of it (no damage,
// no verification), and the scan read on its primary and its other
// copies, so one copy out of reach never holds back the scrub of the
// others.
//
// A scan is read on a replica only while the replica holds it in step, or
// damaged, at the catalog's manifest digest: a scan still pending or being
// copied holds nothing yet that could be called missing, and is skipped
// and counted (TallyReplicaSkipped). Before a difference is recorded the
// replica's row is read again, so a copy that began meanwhile is never
// called damaged.
//
// An object no manifest names on a replica (a copy that landed after a
// purge's last check) is recorded as an orphan on the replica's name, for
// a person or a later job to remove; a scrub never removes it. An
// instance a delete in state purging hides is not looked at on a replica:
// the replicator skips its object once the purge began, and the purge
// removes it from every copy.
//
// On a storage of the archive tier an object held cold is checked by
// presence (blob.Store's Stat: its size, and the store's own SHA-256 where
// it keeps one), never read and never recalled; the bytes of an object
// online there are read only for the share Config.ArchiveSample names.

// The tallies a scrub of replicas adds, beside the contract's.
const (
	// TallyReplicaSkipped counts the scans a replica was not read for:
	// pending or being copied, or not at the catalog's manifest digest.
	TallyReplicaSkipped = "replica|skipped"
	// TallyReplicaUnreadable counts the replicas of a scan that could not
	// be opened, or failed a read or a listing: the scan is read on its
	// other copies.
	TallyReplicaUnreadable = "replica|unreadable"
	// TallyReplicaDamaged and TallyReplicaMissing count the objects of a
	// replica found not as archived, and not there.
	TallyReplicaDamaged = "replica|damaged"
	TallyReplicaMissing = "replica|missing"
	// TallyReplicaPurging counts the instances a purging delete hides that
	// a replica's look passed over.
	TallyReplicaPurging = "replica|purging"
	// TallyPresenceChecked counts the objects of the archive tier checked
	// by presence; TallyPresenceSampled those whose bytes were read.
	TallyPresenceChecked = "presence|checked"
	TallyPresenceSampled = "presence|sampled"
)

// detailReplica is the detail of an instance refused in a job that reads a
// replica alone.
const detailReplica = "its copy on a replica is not what was archived"

// copyPlan is which copies of a scan a scan's unit reads: the scan's own
// storage (primary), and the replicas in copies.
type copyPlan struct {
	primary bool
	copies  []*copyCheck
	tallies map[string]int64
}

// planCopies returns the copies of the scan the unit reads. A scrub of the
// whole tenant reads the scan's storage and each of its replicas; a scrub
// of one storage reads that storage: the scan's own, or one of its
// replicas.
func (u *unitRun) planCopies(scan tenant.JobScan) (copyPlan, outcome) {
	plan := copyPlan{tallies: map[string]int64{}}
	var replicas []tenant.ReplicaStatus
	switch job := u.job.Storage; job {
	case scan.Storage:
		plan.primary = true
		return plan, outcome{}
	case "":
		plan.primary = true
		all, err := u.replicasOf(scan.Storage)
		if err != nil {
			return plan, u.failure(err, reasonDatabase)
		}
		replicas = all
	default:
		all, err := u.replicasOf(scan.Storage)
		if err != nil {
			return plan, u.failure(err, reasonDatabase)
		}
		for _, r := range all {
			if r.Replica == job {
				replicas = append(replicas, r)
			}
		}
		if len(replicas) == 0 {
			// The listing answered a scan of another storage than the
			// job's: it is read where the catalog holds it.
			plan.primary = true
			return plan, outcome{}
		}
	}
	if len(replicas) == 0 {
		return plan, outcome{}
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	sc, err := u.p.CatalogScan(ctx, scan.Path)
	cancel()
	if fault.KindOf(err) == fault.NotFound {
		return plan, outcome{} // gone from the catalog: no copy is owed it
	}
	if err != nil {
		return plan, u.failure(err, reasonDatabase)
	}
	for _, r := range replicas {
		c := &copyCheck{u: u, name: r.Replica, archive: r.Tier == tenant.TierArchive, seriesUID: sc.SeriesUID, path: scan.Path,
			manifest: sc.ManifestSHA256, tallies: map[string]int64{}}
		row, ok, err := c.row()
		if err != nil {
			return plan, u.failure(err, reasonDatabase)
		}
		if !ok {
			plan.tallies[TallyReplicaSkipped]++
			continue
		}
		c.damageLeft = row.Damaged
		store, err := u.stores.Get(u.ctx, r.Replica)
		if err != nil {
			u.log.Warn("scrubs: a replica could not be opened; the scan is read on its other copies", "storage", r.Replica,
				"reason", fault.Message(err))
			plan.tallies[TallyReplicaUnreadable]++
			continue
		}
		c.store = store
		c.sc = &scanCheck{u: u, scan: tenant.JobScan{Path: scan.Path, Storage: r.Replica}, store: store}
		plan.copies = append(plan.copies, c)
	}
	return plan, outcome{}
}

// replicasOf returns the replicas of the storage named primary, read once
// a unit.
func (u *unitRun) replicasOf(primary string) ([]tenant.ReplicaStatus, error) {
	if r, ok := u.replicas[primary]; ok {
		return r, nil
	}
	var out []tenant.ReplicaStatus
	page := master.Page{Limit: master.MaxPageLimit}
	for range maxListPages {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		rows, next, err := u.p.ListReplicas(ctx, primary, page)
		cancel()
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
		if next == "" {
			if u.replicas == nil {
				u.replicas = map[string][]tenant.ReplicaStatus{}
			}
			u.replicas[primary] = out
			return out, nil
		}
		page.Cursor = next
	}
	return nil, fault.New(fault.Internal, "scrub: the list of replicas did not end")
}

// copyCheck is one replica of a scan at work in a scan's unit.
type copyCheck struct {
	u *unitRun
	// name is the replica's; store the replica, to read and list only.
	name  string
	store Storage
	// archive says the replica is of the archive tier.
	archive bool
	// seriesUID and path name the scan; manifest is the catalog's digest
	// of its manifest when the unit planned it.
	seriesUID string
	path      label.Path
	manifest  tenant.SHA256
	// damageLeft is how many damaged objects the replica's row counted when
	// planned, less those this unit cleared: an object read good has its
	// damage cleared only while some is left, so a scan read good with no
	// damage writes nothing.
	damageLeft int64
	// open are the findings of the scan on the replica's name that held
	// when the unit started, of every kind (orphans found before, and
	// findings a switch moved there with its place); a finding is taken out
	// once cleared.
	open map[findingKey]tenant.ScrubFinding
	// sc answers what the corrections not yet purged keep under the scan's
	// keys (expectedObject), its records read from the replica.
	sc *scanCheck
	// dropped says the replica no longer held the scan at the planned
	// digest when a difference was to be recorded, or failed a read: nothing
	// more is read or recorded of it in this unit.
	dropped bool
	// byPresence says an object was checked by presence only.
	byPresence bool
	tallies    map[string]int64
}

// row reads the replica's row of the scan and reports whether the scan is
// read there: held in step, or damaged, at the catalog's manifest digest.
func (c *copyCheck) row() (tenant.ReplicaScan, bool, error) {
	ctx, cancel := context.WithTimeout(c.u.ctx, opTimeout)
	defer cancel()
	row, err := c.u.p.ReplicaScan(ctx, c.name, c.seriesUID)
	if fault.KindOf(err) == fault.NotFound {
		return tenant.ReplicaScan{}, false, nil
	}
	if err != nil {
		return tenant.ReplicaScan{}, false, err
	}
	held := (row.State == tenant.ReplicaInStep || row.State == tenant.ReplicaDamaged) && !c.manifest.IsZero() &&
		row.ManifestSHA256 == c.manifest
	return row, held, nil
}

// stillHeld reports whether the replica still holds the scan at the
// planned digest, the check before a difference is recorded; a replica
// that no longer does is dropped from the unit.
func (c *copyCheck) stillHeld() (bool, outcome) {
	if c.dropped {
		return false, outcome{}
	}
	_, ok, err := c.row()
	if err != nil {
		return false, c.u.failure(err, reasonDatabase)
	}
	if !ok {
		c.dropped = true
		c.u.log.Info("scrubs: a replica's copy of a scan changed while it was read; it is read again by a later scrub", "storage", c.name)
	}
	return ok, outcome{}
}

// load reads the findings of the scan on the replica's name that hold, of
// every kind: those of its files (ScanScrubFindings), and those of its
// instances' objects, which only a switch puts on a replica's name (a
// scrub records a replica's damage as the replica's, never as a finding),
// read from the replica's findings of those kinds and kept when they name
// the scan.
func (c *copyCheck) load() outcome {
	c.open = map[findingKey]tenant.ScrubFinding{}
	filter := tenant.ScrubFindingFilter{Kinds: []tenant.ScrubFindingKind{tenant.FindingMissing, tenant.FindingDamaged}, Storage: c.name}
	page := master.Page{Limit: master.MaxPageLimit}
	for i := 0; ; i++ {
		if i == maxListPages {
			return c.u.failure(fault.New(fault.Internal, "scrub: a replica's findings did not end"), reasonInternal)
		}
		ctx, cancel := context.WithTimeout(c.u.ctx, opTimeout)
		rows, next, err := c.u.p.ListScrubFindings(ctx, filter, page)
		cancel()
		if err != nil {
			return c.u.failure(err, reasonDatabase)
		}
		for _, f := range rows {
			if f.Path == c.path {
				c.open[findingKey{f.Key, f.Kind}] = f
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	afterKey, afterKind := "", tenant.ScrubFindingKind("")
	for range maxListPages {
		ctx, cancel := context.WithTimeout(c.u.ctx, opTimeout)
		held, err := c.u.p.ScanScrubFindings(ctx, c.name, c.path, afterKey, afterKind)
		cancel()
		if err != nil {
			return c.u.failure(err, reasonDatabase)
		}
		for _, f := range held {
			c.open[findingKey{f.Key, f.Kind}] = f
		}
		if len(held) < tenant.MaxScanFindings {
			return outcome{}
		}
		last := held[len(held)-1]
		afterKey, afterKind = last.Key, last.Kind
	}
	return c.u.failure(fault.New(fault.Internal, "scrub: a scan's findings did not end"), reasonInternal)
}

// instance reads the instance's object on the replica against the
// catalog's size and SHA-256 and returns how it counts there: good, or
// refused with its damage recorded on the replica. An object not online is
// tallied: not readable now is not missing.
func (c *copyCheck) instance(in tenant.ScanInstance) (verdict, outcome) {
	if c.dropped {
		return verdict{}, outcome{}
	}
	offset, length, size := ownBytes(in)
	m, err := c.measure(in.Key, offset, length, size, in.ArchivedSHA256, in.Size)
	if err != nil {
		return verdict{}, c.lost(err)
	}
	return c.judge(in.SOPInstanceUID, in.Key, size, in.ArchivedSHA256, m)
}

// lost drops the replica from the unit after its storage failed a read or
// a listing with err: it is tallied unreadable and nothing more is read or
// recorded of it, while the unit goes on with the primary and the other
// copies. A failure that is the unit's own (its context ended, its lease
// lost, no read slot free) ends or gives back the unit as any read's
// does.
func (c *copyCheck) lost(err error) outcome {
	u := c.u
	if u.ctx.Err() != nil || errors.Is(err, tenant.ErrLeaseLost) || fault.KindOf(err) == fault.Exhausted {
		return u.failure(err, reasonStorage)
	}
	if !c.dropped {
		c.dropped = true
		c.tallies[TallyReplicaUnreadable]++
		u.log.Warn("scrubs: a replica failed a read; the scan is read on its other copies, and nothing is recorded of this one",
			"storage", c.name, "kind", fault.KindOf(err).String())
	}
	return outcome{}
}

// judge records what a measure of the object at key, of the instance uid
// (empty for the manifest), found against the size and checksum expected.
func (c *copyCheck) judge(uid, key string, size int64, sum tenant.SHA256, m measured) (verdict, outcome) {
	switch {
	case m.presence == notOnline:
		c.tallies[tenant.TallyNotOnline]++
		return verdict{}, outcome{}
	case m.matches(size, sum):
		return verdict{}, c.good(uid, key)
	}
	held, out := c.stillHeld()
	if !held {
		return verdict{}, out
	}
	d := tenant.ReplicaDamage{SOPInstanceUID: uid, Revision: revisionOf(key), Found: c.u.sup.cfg.Now()}
	tally := TallyReplicaMissing
	if m.presence == present {
		found := m.size
		d.FoundSize, d.FoundSHA256, tally = &found, m.sum, TallyReplicaDamaged
	}
	ctx, cancel := context.WithTimeout(c.u.ctx, opTimeout)
	_, err := c.u.p.RecordReplicaDamage(ctx, tenant.ReplicaDamageRecord{Fence: &c.u.fence, Replica: c.name, SeriesUID: c.seriesUID, Damage: d})
	cancel()
	if err != nil {
		return verdict{}, c.u.failure(err, reasonDatabase)
	}
	c.tallies[tally]++
	c.u.sup.cfg.Wake(c.u.t.name)
	c.u.log.Error("scrubs: a replica's copy of an object is not what was archived; the replica is marked damaged and copies it again",
		"storage", c.name, "found", tally)
	return verdict{code: tenant.FailureDamagedInStorage, detail: detailReplica}, outcome{}
}

// good clears, of an object read good on the replica, the damage the
// replica's row holds of it, while the row has damage left, and the
// findings on the replica's name that it was missing or damaged (a
// switch moves them there with the place), when load found one. The row's
// damage is kept with no list of its objects, so while some is left each
// object read good asks; once the row's count is cleared nothing more is.
func (c *copyCheck) good(uid, key string) outcome {
	if c.damageLeft > 0 {
		ctx, cancel := context.WithTimeout(c.u.ctx, opTimeout)
		cleared, err := c.u.p.ClearReplicaDamage(ctx, &c.u.fence, c.name, c.seriesUID, uid, c.u.sup.cfg.Now())
		cancel()
		if err != nil {
			return c.u.failure(err, reasonDatabase)
		}
		if cleared {
			c.damageLeft--
			c.u.log.Info("scrubs: a replica's copy of an object was read good again; its damage is cleared", "storage", c.name)
			c.u.sup.cfg.Wake(c.u.t.name)
		}
	}
	var kinds []tenant.ScrubFindingKind
	for _, kind := range []tenant.ScrubFindingKind{tenant.FindingMissing, tenant.FindingDamaged} {
		if _, ok := c.open[findingKey{key, kind}]; ok {
			kinds = append(kinds, kind)
			delete(c.open, findingKey{key, kind})
		}
	}
	if len(kinds) == 0 {
		return outcome{}
	}
	return c.u.clearObject(c.name, key, kinds...)
}

// measure reads length bytes of key from offset on the replica, as the
// scan's storage is read (measureTwice); on the archive tier it checks an
// object by presence instead, unless the object is online and sampled.
// objectSize is the size of the whole object, which presence compares.
func (c *copyCheck) measure(key string, offset, length, size int64, sum tenant.SHA256, objectSize int64) (measured, error) {
	if !c.archive {
		return c.u.measureTwice(c.store, c.name, key, offset, length, size, sum)
	}
	m, online, err := c.presence(key, size, sum, objectSize, length < 0)
	if err != nil {
		return m, err
	}
	if !m.matches(size, sum) {
		// A difference is believed when a second look finds it too.
		if m, online, err = c.presence(key, size, sum, objectSize, length < 0); err != nil {
			return m, err
		}
		if !m.matches(size, sum) {
			c.tallies[TallyPresenceChecked]++
			c.byPresence = true
			return m, nil
		}
	}
	if online && c.u.sampled(key) {
		c.tallies[TallyPresenceSampled]++
		return c.u.measureTwice(c.store, c.name, key, offset, length, size, sum)
	}
	c.tallies[TallyPresenceChecked]++
	c.byPresence = true
	return m, nil
}

// presence checks the object at key by what the store says of it, without
// reading it: there, of objectSize bytes, and of the SHA-256 sum where the
// store keeps one of the whole object and the instance's bytes are the
// whole object (whole). A match answers the size and checksum expected; a
// difference answers what the store said. It reports whether the object is
// online.
func (c *copyCheck) presence(key string, size int64, sum tenant.SHA256, objectSize int64, whole bool) (measured, bool, error) {
	ctx, cancel := context.WithTimeout(c.u.ctx, opTimeout)
	defer cancel()
	info, err := c.store.Stat(ctx, key)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return measured{presence: missing}, false, nil
	case err != nil:
		return measured{}, false, err
	}
	m := measured{presence: present, size: size, sum: sum}
	if info.Size != objectSize {
		m.size, m.sum = info.Size, tenant.SHA256{}
		if info.HasSHA256 {
			m.sum = tenant.SHA256(info.SHA256)
		}
	} else if whole && info.HasSHA256 && tenant.SHA256(info.SHA256) != sum {
		m.sum = tenant.SHA256(info.SHA256)
	}
	return m, info.State == blob.Online, nil
}

// sampled reports whether the bytes of the archive-tier object at key are
// read: for the share Config.ArchiveSample of keys, chosen by the key's
// SHA-256 so the same objects are chosen each time and a sample can be
// told again.
func (u *unitRun) sampled(key string) bool {
	share := u.sup.cfg.ArchiveSample
	if share <= 0 {
		return false
	}
	if share >= 1 {
		return true
	}
	h := sha256.Sum256([]byte(key))
	return float64(binary.BigEndian.Uint64(h[:8]))/float64(^uint64(0)) < share
}
