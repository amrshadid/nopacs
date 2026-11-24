package scrub

import (
	"context"
	"errors"

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
}

// scrubScan reads every instance of the scan's stream after the unit's
// cursor and compares each with the catalog; then the scan's manifest, its
// sidecar and the objects under its keys (files); then clears the findings
// of its files it read good again. It returns how it ended; the zero
// outcome is the scan's end.
func (u *unitRun) scrubScan(scan tenant.JobScan, rec *recorder) outcome {
	store, err := u.stores.Get(u.ctx, scan.Storage)
	if err != nil {
		return u.failure(err, reasonStorage)
	}
	s := &scanCheck{u: u, scan: scan, store: store, open: map[findingKey]tenant.ScrubFinding{}, found: map[findingKey]bool{},
		good: map[findingKey]bool{}, tallies: map[string]int64{}}
	if err := s.loadOpen(); err != nil {
		return u.failure(err, reasonDatabase)
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
			if out, stop := s.checkpoint(rec); stop {
				return out
			}
			if out := s.instance(in, rec); out != (outcome{}) {
				return out
			}
		}
		if len(page) < instancePage {
			break
		}
		after = page[len(page)-1].SOPInstanceUID
	}
	if out := s.files(); out != (outcome{}) {
		return out
	}
	if out := s.clear(); out != (outcome{}) {
		return out
	}
	rec.tally(s.tallies)
	return outcome{}
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
func (s *scanCheck) instance(in tenant.ScanInstance, rec *recorder) outcome {
	u := s.u
	offset, length, size := int64(0), int64(-1), in.Size
	if in.Offset != 0 || in.Size != in.Length {
		// One instance of an object that holds several: its own bytes.
		offset, length, size = in.Offset, in.Length, in.Length
	}
	m, err := u.measureTwice(s.store, s.scan.Storage, in.Key, offset, length, size, in.ArchivedSHA256)
	if err != nil {
		return u.failure(err, reasonStorage)
	}
	switch {
	case m.presence == notOnline:
		rec.add(in, tenant.FailureNotOnline, detailNotOnline, map[string]int64{tenant.TallyNotOnline: 1})
		return outcome{}
	case m.matches(size, in.ArchivedSHA256):
		if in.Damaged {
			if out := s.clearObject(in.Key, tenant.FindingMissing, tenant.FindingDamaged); out != (outcome{}) {
				return out
			}
		}
		rec.add(in, "", "", nil)
		return outcome{}
	}
	f := tenant.ScrubFinding{Kind: tenant.FindingDamaged, Storage: s.scan.Storage, Key: in.Key, Path: s.scan.Path,
		SOPInstanceUID: in.SOPInstanceUID, Revision: revisionOf(in.Key), ExpectedSize: &size, ExpectedSHA256: in.ArchivedSHA256}
	detail := detailDamaged
	if m.presence == missing {
		f.Kind, detail = tenant.FindingMissing, detailMissing
	} else {
		found := m.size
		f.FoundSize, f.FoundSHA256 = &found, m.sum
	}
	if out := s.record(f); out != (outcome{}) {
		return out
	}
	if f.Kind == tenant.FindingDamaged && in.Damaged {
		// The object is there again, damaged: a finding that it was
		// missing no longer holds. Cleared after the damage is recorded,
		// so the instance is never unmarked between the two.
		if out := s.clearObject(in.Key, tenant.FindingMissing); out != (outcome{}) {
			return out
		}
	}
	u.log.Error("scrubs: an instance's object is not what was archived; it is marked, and every read of it answers data loss",
		"kind", string(f.Kind))
	rec.add(in, tenant.FailureDamagedInStorage, detail, nil)
	return outcome{}
}

// clearObject clears the findings of the kinds given that hold of the
// object at key, under the unit's lease.
func (s *scanCheck) clearObject(key string, kinds ...tenant.ScrubFindingKind) outcome {
	u := s.u
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	n, err := u.p.ClearObjectFindings(ctx, u.fence, s.scan.Storage, key, kinds, u.sup.cfg.Now())
	if err != nil {
		return u.failure(err, reasonDatabase)
	}
	if n > 0 {
		u.log.Info("scrubs: an instance's object was read good again; its findings are cleared and its mark removed", "findings", n)
		u.sup.cfg.Wake(u.t.name)
	}
	return outcome{}
}

// record records a finding under the unit's lease.
func (s *scanCheck) record(f tenant.ScrubFinding) outcome {
	u := s.u
	f.ID = tenant.ScrubFindingID(u.job.ID, f.Storage, f.Key, f.Kind)
	ctx, cancel := context.WithTimeout(u.ctx, opTimeout)
	defer cancel()
	if _, _, err := u.p.RecordScrubFinding(ctx, tenant.ScrubFindingRecord{Fence: u.fence, Finding: f, Now: u.sup.cfg.Now()}); err != nil {
		return u.failure(err, reasonDatabase)
	}
	s.found[findingKey{f.Key, f.Kind}] = true
	u.sup.cfg.Wake(u.t.name)
	return outcome{}
}

// checkpoint writes the batch when it is due, and reports whether the unit
// must stop: its job ended (a cancel, seen at this write), or the write
// failed.
func (s *scanCheck) checkpoint(rec *recorder) (outcome, bool) {
	u := s.u
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
