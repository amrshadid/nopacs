package scrub

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// findingKey names a finding by what it is about: one object and one kind.
type findingKey struct {
	key  string
	kind tenant.ScrubFindingKind
}

// scanCheck is one scan's unit at work: the scan, its storage, the
// findings of its files that held when the unit started, and what this
// unit found again and found good.
type scanCheck struct {
	u     *unitRun
	scan  tenant.JobScan
	store Storage
	// open are the findings of the scan's files (kinds that do not mark)
	// that held when the unit started, every one of them. A finding of an
	// instance's bytes is cleared by the read of the instance instead
	// (instance), so neither the unit's cursor nor the number of findings
	// can leave one behind.
	open map[findingKey]tenant.ScrubFinding
	// found are the differences this unit found and recorded; good those it
	// checked and found gone. A finding that held and is good, and was not
	// found again, is cleared at the scan's end.
	found, good map[findingKey]bool
	// tallies are counted with the scan's last progress write.
	tallies map[string]int64
	// exp is what the scan's corrections not yet purged make expected, and
	// recs the objects the records of corrections with no row keep, each
	// read at most once a look and only when the look needs it (expect).
	exp  *scanExpect
	recs map[string]bool
	// short says an instance or the manifest was not online when read:
	// the scan was not read whole, and is not recorded verified.
	short bool
}

// scrubScan reads every instance of the scan's stream after the unit's
// cursor and compares each with the catalog, on the scan's storage and on
// each of its replicas the job reads (planCopies); then the scan's
// manifest, its sidecar and the objects under its keys (files) on each;
// then clears the findings of its files it read good again. It returns how
// it ended; the zero outcome is the scan's end.
func (u *unitRun) scrubScan(scan tenant.JobScan, rec *recorder) outcome {
	plan, out := u.planCopies(scan)
	if out != (outcome{}) {
		return out
	}
	var s *scanCheck
	if plan.primary {
		store, err := u.stores.Get(u.ctx, scan.Storage)
		if err != nil {
			return u.failure(err, reasonStorage)
		}
		s = &scanCheck{u: u, scan: scan, store: store, open: map[findingKey]tenant.ScrubFinding{}, found: map[findingKey]bool{},
			good: map[findingKey]bool{}, tallies: map[string]int64{}}
		if err := s.loadOpen(); err != nil {
			return u.failure(err, reasonDatabase)
		}
	}
	for _, c := range plan.copies {
		if out := c.load(); out != (outcome{}) {
			return out
		}
	}
	after := scan.Cursor
	for {
		if out, stop := u.stopping(); stop {
			return out
		}
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		page, err := u.p.JobScanInstances(ctx, u.fence, after, instancePage)
		cancel()
		if err != nil {
			return u.failure(err, reasonDatabase)
		}
		for _, in := range page {
			if out, stop := u.stopping(); stop {
				return out
			}
			// The batch is written before the next instance is read, never
			// after the last: the scan's last write then counts at least
			// one instance, and can carry the scan's tallies.
			if out, stop := u.checkpoint(rec); stop {
				return out
			}
			var v verdict
			if s != nil {
				if v, out = s.instance(in); out != (outcome{}) {
					return out
				}
				if v.code == tenant.FailureNotOnline {
					s.short = true
				}
			}
			for _, c := range plan.copies {
				cv, out := c.instance(in)
				if out != (outcome{}) {
					return out
				}
				v = v.with(cv, !plan.primary)
			}
			rec.add(in, v.code, v.detail, v.tallies)
		}
		if len(page) < instancePage {
			break
		}
		after = page[len(page)-1].SOPInstanceUID
	}
	if s != nil {
		if out := s.files(); out != (outcome{}) {
			return out
		}
		if out := s.clear(); out != (outcome{}) {
			return out
		}
		rec.tally(s.tallies)
		if !s.short {
			// Coverage counts a scan read whole: one with an object not
			// online now was not, and is read again by a later scrub.
			if out := u.verified(scan.Storage, scan.Path, false); out != (outcome{}) {
				return out
			}
		}
	}
	for _, c := range plan.copies {
		if out := c.files(); out != (outcome{}) {
			return out
		}
		rec.tally(c.tallies)
		if c.dropped || c.tallies[tenant.TallyNotOnline] > 0 {
			continue // changed while read, failed a read, or an object not online: not read whole at one digest
		}
		if out := u.verified(c.name, scan.Path, c.byPresence); out != (outcome{}) {
			return out
		}
	}
	rec.tally(plan.tallies)
	return outcome{}
}

// verdict is how one instance's reads end for the job's counts: read good
// (no code), or refused with a code and a detail, with the tallies its
// write adds to.
type verdict struct {
	code    tenant.JobFailureCode
	detail  string
	tallies map[string]int64
}

// with joins what a replica's read found to v. The replica's refusal
// counts the instance refused only when refuse is set (a job that reads
// the replica alone); otherwise the instance, whose reads go to its
// primary, is counted as the primary read it, and the replica's damage is
// in its tallies and its replica's damage rows.
func (v verdict) with(c verdict, refuse bool) verdict {
	if refuse && v.code == "" && c.code != "" {
		v.code, v.detail = c.code, c.detail
	}
	for k, n := range c.tallies {
		if v.tallies == nil {
			v.tallies = map[string]int64{}
		}
		v.tallies[k] += n
	}
	return v
}

// loadOpen reads every finding of the scan's files that holds, page by
// page.
func (s *scanCheck) loadOpen() error {
	u := s.u
	afterKey, afterKind := "", tenant.ScrubFindingKind("")
	for range maxListPages {
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		held, err := u.p.ScanScrubFindings(ctx, s.scan.Storage, s.scan.Path, afterKey, afterKind)
		cancel()
		if err != nil {
			return err
		}
		for _, f := range held {
			s.open[findingKey{f.Key, f.Kind}] = f
		}
		if len(held) < tenant.MaxScanFindings {
			return nil
		}
		last := held[len(held)-1]
		afterKey, afterKind = last.Key, last.Kind
	}
	return fault.New(fault.Internal, "scrub: a scan's findings did not end")
}

// instance reads one instance's object, twice when the first read differs
// from the catalog, and counts it: read good, or refused with a finding
// recorded first. An object not online is tallied and refused without a
// finding: not readable now is not missing. An instance the catalog
// answers marked whose object reads whole has its findings cleared at
// once, with its mark; one found damaged has a finding that it was missing
// cleared.
func (s *scanCheck) instance(in tenant.ScanInstance) (verdict, outcome) {
	return s.u.checkInstance(s.store, s.scan.Storage, s.scan.Path, in)
}

// checkInstance reads the instance in of the scan at path on store, the
// storage named storage that holds it in the catalog, as instance says, and
// returns how it counts.
func (u *unitRun) checkInstance(store Storage, storage string, path label.Path, in tenant.ScanInstance) (verdict, outcome) {
	offset, length, size := ownBytes(in)
	m, err := u.measureTwice(store, storage, in.Key, offset, length, size, in.ArchivedSHA256)
	if err != nil {
		return verdict{}, u.failure(err, reasonStorage)
	}
	return u.judgeInstance(storage, path, in, size, m)
}

// judgeInstance records what a read of the instance in, of size own bytes,
// found (m) on the storage named storage, as checkInstance says, and
// returns how it counts.
func (u *unitRun) judgeInstance(storage string, path label.Path, in tenant.ScanInstance, size int64, m measured) (verdict, outcome) {
	switch {
	case m.presence == notOnline:
		return verdict{code: tenant.FailureNotOnline, detail: detailNotOnline, tallies: map[string]int64{tenant.TallyNotOnline: 1}}, outcome{}
	case m.matches(size, in.ArchivedSHA256):
		if in.Damaged {
			if out := u.clearObject(storage, in.Key, tenant.FindingMissing, tenant.FindingDamaged); out != (outcome{}) {
				return verdict{}, out
			}
		}
		return verdict{}, outcome{}
	}
	f := tenant.ScrubFinding{Kind: tenant.FindingDamaged, Storage: storage, Key: in.Key, Path: path,
		SOPInstanceUID: in.SOPInstanceUID, Revision: revisionOf(in.Key), ExpectedSize: &size, ExpectedSHA256: in.ArchivedSHA256}
	detail := detailDamaged
	if m.presence == missing {
		f.Kind, detail = tenant.FindingMissing, detailMissing
	} else {
		found := m.size
		f.FoundSize, f.FoundSHA256 = &found, m.sum
	}
	if out := u.recordFinding(f); out != (outcome{}) {
		return verdict{}, out
	}
	if f.Kind == tenant.FindingDamaged && in.Damaged {
		// The object is there again, damaged: a finding that it was
		// missing no longer holds. Cleared after the damage is recorded,
		// so the instance is never unmarked between the two.
		if out := u.clearObject(storage, in.Key, tenant.FindingMissing); out != (outcome{}) {
			return verdict{}, out
		}
	}
	u.log.Error("scrubs: an instance's object is not what was archived; it is marked, and every read of it answers data loss",
		"kind", string(f.Kind))
	return verdict{code: tenant.FailureDamagedInStorage, detail: detail}, outcome{}
}

// ownBytes returns the range of an instance's own bytes in its object and
// their size: the whole object, or one instance's part of an object that
// holds several.
func ownBytes(in tenant.ScanInstance) (offset, length, size int64) {
	if in.Offset != 0 || in.Size != in.Length {
		return in.Offset, in.Length, in.Length
	}
	return 0, -1, in.Size
}

// clearObject clears the findings of the kinds given that hold of the
// object at key on storage, under the unit's lease.
func (u *unitRun) clearObject(storage, key string, kinds ...tenant.ScrubFindingKind) outcome {
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	n, err := u.p.ClearObjectFindings(ctx, u.fence, storage, key, kinds, u.sup.cfg.Now())
	if err != nil {
		return u.failure(err, reasonDatabase)
	}
	if n > 0 {
		u.log.Info("scrubs: an object was read good again; its findings are cleared and the marks they made removed", "findings", n)
		u.sup.cfg.Wake(u.t.name)
	}
	return outcome{}
}

// record records a finding under the unit's lease.
func (s *scanCheck) record(f tenant.ScrubFinding) outcome {
	if out := s.u.recordFinding(f); out != (outcome{}) {
		return out
	}
	s.found[findingKey{f.Key, f.Kind}] = true
	return outcome{}
}

// recordFinding records a finding under the unit's lease.
func (u *unitRun) recordFinding(f tenant.ScrubFinding) outcome {
	f.ID = tenant.ScrubFindingID(u.job.ID, f.Storage, f.Key, f.Kind)
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	if _, _, err := u.p.RecordScrubFinding(ctx, tenant.ScrubFindingRecord{Fence: u.fence, Finding: f, Now: u.sup.cfg.Now()}); err != nil {
		return u.failure(err, reasonDatabase)
	}
	u.sup.cfg.Wake(u.t.name)
	return outcome{}
}

// checkpoint writes the batch when it is due, and reports whether the unit
// must stop: its job ended (a cancel, seen at this write), or the write
// failed.
func (u *unitRun) checkpoint(rec *recorder) (outcome, bool) {
	if !rec.due() {
		return outcome{}, false
	}
	job, wrote, err := rec.flush(u.ctx)
	if !u.write("RecordJobProgress", err) {
		return outcome{stopped: true}, true
	}
	if wrote && job.State.Ended() {
		u.log.Info("scrubs: the job ended while the unit read; the unit ends", "state", string(job.State))
		ctx, cancel := context.WithTimeout(context.WithoutCancel(u.ctx), opTimeout)
		defer cancel()
		if _, err := rec.end(ctx); err != nil && !errors.Is(err, tenant.ErrLeaseLost) {
			u.log.Info("scrubs: the scan of an ended job could not be ended; its lease runs out")
		}
		return outcome{stopped: true}, true
	}
	return outcome{}, false
}

// clear clears each finding of the scan's files that held when the unit
// started and that this unit checked and found good, not found again.
func (s *scanCheck) clear() outcome {
	u := s.u
	for k, f := range s.open {
		if !s.good[k] || s.found[k] {
			continue
		}
		ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
		_, _, err := u.p.ClearScrubFinding(ctx, u.fence, f.ID, u.sup.cfg.Now())
		cancel()
		if err != nil {
			return u.failure(err, reasonDatabase)
		}
		u.log.Info("scrubs: a finding was read good again and is cleared", "kind", string(f.Kind))
		u.sup.cfg.Wake(u.t.name)
	}
	return outcome{}
}

// revisionOf returns the revision an instance's key names, 0 for a key
// that is not one of the layout's.
func revisionOf(key string) int {
	k, err := layout.Parse(key)
	if err != nil || k.Kind != layout.Instance {
		return 0
	}
	return k.Rev()
}
