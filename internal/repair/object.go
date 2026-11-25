package repair

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// target is the object a finding names, with the bytes the catalog says it
// holds now.
type target struct {
	key string
	// size is the catalog's size; -1 for a manifest, whose size the
	// catalog does not keep: its SHA-256 alone names its bytes, read to at
	// most manifest.MaxBytes.
	size int64
	sum  [sha256.Size]byte
	// series is the scan's Series Instance UID; uid the instance's SOP
	// Instance UID, empty for a manifest; revision the object's.
	series, uid string
	revision    int
}

// measured is an object read whole: its size and SHA-256, or missing.
type measured struct {
	missing bool
	size    int64
	sum     [sha256.Size]byte
}

// matches reports whether m is the target's bytes.
func (m measured) matches(t target) bool {
	return !m.missing && m.sum == t.sum && (t.size < 0 || m.size == t.size)
}

// mend repairs the object finding f names, as the package's doc says, and
// returns what became of it. Its error is errStop alone: the run's context
// ended or its lease was lost.
func (u *run) mend(f tenant.ScrubFinding) (Outcome, error) {
	skip := func(reason string) (Outcome, error) {
		return Outcome{Finding: f.ID, Kind: Skipped, Reason: reason}, nil
	}
	retry := func(reason string, err error) (Outcome, error) { return u.retry(f, reason, err) }
	refuse := func(reason string) (Outcome, error) {
		return Outcome{Finding: f.ID, Kind: NoGoodCopy, Reason: reason}, nil
	}

	t, why, err := u.target(f)
	if err != nil {
		return retry("the catalog could not be read", err)
	}
	if why != "" {
		return skip(why)
	}
	primaryRow, err := u.primaryRow()
	if err != nil {
		return retry("the storage's row could not be read", err)
	}
	names, err := u.inStep(t.series)
	if err != nil {
		return retry("the replicas in step could not be read", err)
	}
	if len(names) == 0 {
		return refuse("no good copy: no replica is in step for the scan at the catalog's manifest")
	}
	first, err := u.replicaRow(names[0])
	if err != nil {
		return retry("a replica's row could not be read", err)
	}
	if err := tenant.CheckRepair(f, primaryRow, first); err != nil {
		if fault.KindOf(err) == fault.Precondition {
			return skip(fault.Message(err))
		}
		return retry("the repair could not be checked", err)
	}
	primary, err := u.openPrimary()
	if err != nil {
		return retry("the storage could not be opened", err)
	}
	found, err := u.measure(primary, t)
	if err != nil {
		return retry("the storage's object could not be read", err)
	}
	if found.matches(t) {
		// The object reads whole now: nothing to put. Its finding is ended
		// as a repair from the first replica in step, the bytes found the
		// bytes expected, so the clear has its record and audit entry.
		return u.finish(f, t, names[0], found, false)
	}
	from, size, why, err := u.choose(f, t, names)
	if err != nil {
		return retry("a replica could not be read", err)
	}
	if why != "" {
		return refuse(why)
	}
	if t.size < 0 {
		t.size = size
	}
	return u.finish(f, t, from, found, true)
}

// finish records the repair of f from the replica from, the object found as
// found, puts the replica's bytes when put says so, and ends the repair.
func (u *run) finish(f tenant.ScrubFinding, t target, from string, found measured, put bool) (Outcome, error) {
	retry := func(reason string, err error) (Outcome, error) { return u.retry(f, reason, err) }
	size := t.size
	if size < 0 {
		size = found.size
	}
	rec := tenant.Repair{ID: tenant.RepairID(u.job.ID, f.ID), Finding: f.ID, Storage: u.job.Storage, From: from, Job: u.job.ID,
		ExpectedSize: size, ExpectedSHA256: tenant.SHA256(t.sum)}
	if !found.missing {
		n := found.size
		rec.FoundSize, rec.FoundSHA256 = &n, tenant.SHA256(found.sum)
	}
	// The record comes before any byte is written: whatever happens to the
	// put, what the damaged object was stays known.
	// The record also holds the scan still until the repair ends: no
	// correction of it and no reception's commit to it lands meanwhile, and
	// it is refused while either is under way or the catalog no longer
	// names the bytes expected.
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	stored, made, err := u.r.cfg.DB.RecordRepair(ctx, tenant.RepairRecord{Fence: &u.fence, Repair: rec, Now: u.r.cfg.Now()})
	cancel()
	switch {
	case errors.Is(err, tenant.ErrLeaseLost):
		return u.retry(f, "", err)
	case errors.Is(err, tenant.ErrRepairJobEnded):
		// Canceled while it ran: nothing more is put, and the walk ends.
		return Outcome{}, errJobEnded
	case errors.Is(err, tenant.ErrScanRepairing):
		// An earlier attempt's hold of this object has not run out.
		return Outcome{Finding: f.ID, Kind: Retry, Reason: tenant.ScanRepairing}, nil
	case fault.KindOf(err) == fault.Precondition, fault.KindOf(err) == fault.NotFound:
		return Outcome{Finding: f.ID, Kind: Skipped, Reason: fault.Message(err)}, nil
	case err != nil:
		return retry("the repair could not be recorded", err)
	}
	// Every way out of the object but its end lets the scan go at once: a
	// hold left to run out would refuse every change of the scan, and every
	// reception to it, for the rest of a job that may take hours.
	ended := false
	defer func() {
		if !ended {
			u.release(stored.ID)
		}
	}()
	kind := AlreadyGood
	if put {
		kind = Repaired
		// The replica checked now, not the one an earlier attempt's record
		// names: that one may no longer be in step.
		if out, err := u.put(from, t, stored.HeldUntil); err != nil || out != "" {
			if err != nil {
				return retry(out, err)
			}
			return Outcome{Finding: f.ID, Kind: Retry, Reason: out}, nil
		}
	} else if !made {
		// Recorded by an earlier attempt of this job, whose put landed
		// before its end was written.
		kind = Repaired
	}
	audit := master.AuditEntry{ID: AuditID(stored.ID), Time: u.r.cfg.Now(), Method: CompleteMethod, TenantName: u.r.cfg.Tenant,
		Object: u.job.Storage, Outcome: master.OutcomeDone,
		Details: map[string]string{"job": u.job.ID, "finding": f.ID, "from": from, "kind": string(f.Kind), "outcome": string(kind)}}
	ctx, cancel = context.WithTimeout(u.ctx, opTimeout)
	done, err := u.r.cfg.DB.CompleteRepair(ctx, &u.fence, stored.ID, u.r.cfg.Now(), audit)
	cancel()
	ended = err == nil
	switch {
	case errors.Is(err, tenant.ErrLeaseLost):
		return u.retry(f, "", err)
	case fault.KindOf(err) == fault.Precondition:
		// A switch moved the finding to the other place since the record:
		// the bytes put say nothing of the place it names now.
		return Outcome{Finding: f.ID, Kind: Skipped, Reason: fault.Message(err)}, nil
	case err != nil:
		return retry("the repair could not be ended", err)
	case done.Changed:
		// The hold ran out and the scan changed meanwhile: the finding stays
		// for a scrub to read again, and the key put is recorded as an
		// orphan with the end.
		return Outcome{Finding: f.ID, Kind: Skipped, Reason: tenant.RepairCatalogChanged}, nil
	}
	out := Outcome{Finding: f.ID, Kind: kind}
	if put {
		out.Bytes = t.size
	}
	return out, nil
}

// retry returns the outcome of f left for the unit's next attempt, for the
// reason given; errStop instead when the run's context ended or its lease
// was lost, which err may say.
func (u *run) retry(f tenant.ScrubFinding, reason string, err error) (Outcome, error) {
	if u.ctx.Err() != nil || errors.Is(err, tenant.ErrLeaseLost) {
		return Outcome{}, errStop
	}
	return Outcome{Finding: f.ID, Kind: Retry, Reason: retryReason(reason, err)}, nil
}

// put streams the replica's object to the same key on the primary through
// blob.PutConfirmed against the catalog's size and SHA-256, and reads it
// back unless the primary confirmed the checksum itself. It returns a
// reason when the object is not put back whole, with the error that made
// it, if any.
//
// The put is cut, by its context, PutMargin before the earlier of the due
// the lease's last renewal returned and the due the hold was recorded
// under (heldUntil less tenant.RepairHoldGrace): a worker that loses the
// database mid-put cannot let its hold go, and the hold runs out a grace
// after that due, so the put has ended before any change of the scan can
// be taken.
func (u *run) put(from string, t target, heldUntil time.Time) (string, error) {
	src, err := u.source(from)
	if err != nil {
		return "the replica could not be opened", err
	}
	primary, err := u.openPrimary()
	if err != nil {
		return "the storage could not be opened", err
	}
	due := u.lease.due()
	if !heldUntil.IsZero() {
		due = minTime(due, heldUntil.Add(-tenant.RepairHoldGrace))
	}
	left := due.Add(-u.r.cfg.PutMargin).Sub(u.r.cfg.Now())
	if left <= 0 {
		return "the job's lease leaves no time for the put before the repair's hold runs out", nil
	}
	ctx, cancel := context.WithTimeout(u.ctx, min(u.objectTimeout(), left))
	defer cancel()
	if u.r.cfg.Pause != nil {
		// Admitted before the first byte and released after the put's last
		// effect, so a switch of the storage never swaps its place under it.
		release, err := u.r.cfg.Pause.Enter(ctx, u.job.Storage)
		if err != nil {
			return "the storage's writes are paused", err
		}
		defer release()
	}
	body, err := src.Read(ctx, t.key, 0, -1)
	if err != nil {
		return "the replica's object could not be read", err
	}
	wr, err := blob.PutConfirmed(ctx, primary, t.key, body, t.size, t.sum)
	_ = body.Close() // only read from; nothing to lose
	switch {
	case err != nil && errors.Is(err, blob.ErrNotTheBytesExpected) && fault.KindOf(err) == fault.DataLoss:
		// PutConfirmed kept nothing: the key holds what it held before.
		return "the replica's object changed after it was checked; nothing was put", err
	case err != nil:
		return "the storage did not take the object", err
	case wr.Confirmed:
		return "", nil
	}
	back, err := u.measure(primary, t)
	switch {
	case err != nil:
		return "the object put could not be read back", err
	case !back.matches(t):
		return "the object read back from the storage is not the bytes put", nil
	}
	return "", nil
}

// choose reads each replica of names whole, in turn, and returns the first
// whose object is the catalog's bytes, with its size. A replica whose
// object is missing or other bytes is recorded as that replica's damage,
// and the next tried. It returns a refusal's reason when none holds the
// bytes; an error when one could not be read and none was good.
func (u *run) choose(f tenant.ScrubFinding, t target, names []string) (string, int64, string, error) {
	var unread error
	for _, name := range names {
		row, err := u.replicaRow(name)
		if err != nil {
			return "", 0, "", err
		}
		primaryRow, err := u.primaryRow()
		if err != nil {
			return "", 0, "", err
		}
		if tenant.CheckRepair(f, primaryRow, row) != nil {
			continue
		}
		src, err := u.source(name)
		if err != nil {
			unread = err
			continue
		}
		m, err := u.measure(src, t)
		if err != nil {
			if u.ctx.Err() != nil {
				return "", 0, "", err
			}
			unread = err
			continue
		}
		if m.matches(t) {
			return name, m.size, "", nil
		}
		if err := u.replicaDamage(name, t, m); err != nil {
			return "", 0, "", err
		}
	}
	if unread != nil {
		return "", 0, "", unread
	}
	return "", 0, "no good copy: no replica in step holds the catalog's bytes", nil
}

// replicaDamage records the replica's object found missing or wrong, under
// the run's fence, so the replicator copies it again from the primary.
func (u *run) replicaDamage(replica string, t target, m measured) error {
	d := tenant.ReplicaDamage{SOPInstanceUID: t.uid, Revision: t.revision, Found: u.r.cfg.Now()}
	if !m.missing {
		n := m.size
		d.FoundSize, d.FoundSHA256 = &n, tenant.SHA256(m.sum)
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	_, err := u.r.cfg.DB.RecordReplicaDamage(ctx, tenant.ReplicaDamageRecord{Fence: &u.fence, Replica: replica, SeriesUID: t.series, Damage: d})
	if err != nil {
		return err
	}
	u.r.log.Warn("repairs: a replica's object is not the catalog's bytes; recorded as its damage", "job", u.job.ID, "replica", replica)
	return nil
}

// target returns the object f names and the bytes the catalog says it
// holds now, or the reason the job leaves it.
func (u *run) target(f tenant.ScrubFinding) (target, string, error) {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	scan, err := u.r.cfg.DB.CatalogScan(ctx, f.Path)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return target{}, "the catalog no longer holds the scan", nil
	case err != nil:
		return target{}, "", err
	case scan.Storage != u.job.Storage:
		return target{}, "the scan is on another storage now", nil
	}
	t := target{key: f.Key, series: scan.SeriesUID, revision: f.Revision}
	if f.Kind == tenant.FindingManifestDamaged {
		key, err := layout.ManifestKey(f.Path)
		if err != nil || key != f.Key {
			return target{}, "the finding's key is not its scan's manifest", nil
		}
		if scan.ManifestSHA256.IsZero() {
			return target{}, "the catalog holds no manifest digest of the scan", nil
		}
		t.size, t.sum, t.revision = -1, scan.ManifestSHA256, 0
		return t, "", nil
	}
	if f.SOPInstanceUID == "" {
		return target{}, "the finding names no instance", nil
	}
	in, err := u.r.cfg.DB.HeldInstance(ctx, f.Path, f.SOPInstanceUID)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return target{}, "the catalog no longer holds the instance", nil
	case err != nil:
		return target{}, "", err
	case in.Key != f.Key:
		// A replace archived the instance at a new key: the old key's bytes
		// are not the catalog's answer for it any more.
		return target{}, "the catalog names another key for the instance now", nil
	case in.Offset != 0 || in.Length != in.Size || !ownObject(in.Key, f.Path, in.SOPInstanceUID):
		return target{}, "the object may hold more than the instance's file, and the catalog names no checksum of it whole", nil
	}
	t.size, t.sum, t.uid = in.Size, in.ArchivedSHA256, in.SOPInstanceUID
	return t, "", nil
}

// inStep returns the replicas in step for the series at the catalog's
// manifest digest, narrowed to the job's replica when it names one.
func (u *run) inStep(series string) ([]string, error) {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	m, err := u.r.cfg.DB.InStepReplicas(ctx, []string{series})
	if err != nil {
		return nil, err
	}
	names := m[series]
	if u.job.From == "" {
		return names, nil
	}
	for _, n := range names {
		if n == u.job.From {
			return []string{n}, nil
		}
	}
	return nil, nil
}

// measure reads the target's key on s whole, streaming, and returns its size
// and SHA-256, or missing. A manifest is read to at most one byte past
// manifest.MaxBytes: anything longer is not a manifest.
func (u *run) measure(s Source, t target) (measured, error) {
	ctx, cancel := context.WithTimeout(u.ctx, u.objectTimeout())
	defer cancel()
	body, err := s.Read(ctx, t.key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return measured{missing: true}, nil
	}
	if err != nil {
		return measured{}, err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	var r io.Reader = body
	if t.size < 0 {
		r = io.LimitReader(body, int64(manifest.MaxBytes)+1)
	}
	h := sha256.New()
	n, err := io.CopyBuffer(h, r, make([]byte, copyBuffer))
	if err != nil {
		return measured{}, err
	}
	return measured{size: n, sum: [sha256.Size]byte(h.Sum(nil))}, nil
}

// ownObject reports whether key, the catalog's key of the instance uid in
// the scan at path, is the layout's key of that very instance (any
// revision). The scan's manifest refuses a line whose key names another
// instance, so such an object holds that instance's file alone (one
// instance per object); a key the layout does not give the instance may be
// an object other files share, of which the catalog names no checksum
// whole, and a put of the instance's bytes there would lose the others.
func ownObject(key string, path label.Path, uid string) bool {
	k, err := layout.Parse(key)
	return err == nil && k.Kind == layout.Instance && k.Path == path && k.SOPInstanceUID == uid
}

// objectTimeout bounds one read or put of an object, so a storage that
// hangs cannot hold the unit for ever.
func (u *run) objectTimeout() time.Duration { return u.r.cfg.ObjectTimeout }

// release lets go the hold of the repair of id, under its own short bound
// and whatever became of the run's context: a stop or a failed object
// never leaves its scan held until the hold runs out. A release that fails
// (the database not answering, the lease lost) leaves the hold to run out
// by itself, which is safe, only slower.
func (u *run) release(id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), releaseTimeout)
	defer cancel()
	if err := u.r.cfg.DB.ReleaseRepair(ctx, u.fence, id); err != nil {
		u.r.log.Warn("repairs: a repair's hold of its scan could not be let go; it runs out by itself", "job", u.job.ID,
			"kind", fault.KindOf(err).String())
	}
}

// minTime returns the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// primaryRow returns the row of the storage the job repairs, read once.
func (u *run) primaryRow() (tenant.Storage, error) {
	if u.storage != nil {
		return *u.storage, nil
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	s, err := u.r.cfg.DB.Storage(ctx, u.job.Storage)
	if err != nil {
		return tenant.Storage{}, err
	}
	u.storage = &s
	return s, nil
}

// replicaRow returns the row of the storage named, read once.
func (u *run) replicaRow(name string) (tenant.Storage, error) {
	if s, ok := u.replicas[name]; ok {
		return s, nil
	}
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	s, err := u.r.cfg.DB.Storage(ctx, name)
	if err != nil {
		return tenant.Storage{}, err
	}
	u.replicas[name] = s
	return s, nil
}

// openPrimary returns the storage the job repairs, opened once: the one
// storage a job writes to, and only through blob.PutConfirmed.
func (u *run) openPrimary() (blob.Store, error) {
	if u.primary == nil {
		s, err := u.r.cfg.Open(u.ctx, u.job.Storage)
		if err != nil {
			return nil, err
		}
		u.primary = s
	}
	return u.primary, nil
}

// source returns the replica named, opened once, held as Source.
func (u *run) source(name string) (Source, error) {
	if s, ok := u.sources[name]; ok {
		return s, nil
	}
	s, err := u.r.cfg.Open(u.ctx, name)
	if err != nil {
		return nil, err
	}
	u.sources[name] = s
	return s, nil
}
