package receive

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// verify checks that a reception's work is whole, then lets its incoming
// copies go and finishes it. Every file was read back before its row was
// committed (archiveScan), so this step reads no file again: it checks what
// is cheap and would show a step that lost something, before the copies
// that could repair it are removed.
//
// Order of effects, each safe to repeat or to stop after:
//
//  1. Read the reception. Done or ended: the unit is completed. Verified:
//     the check passed before a crash; go to 4.
//  2. Check (checkWhole): every instance this reception confirmed is on its
//     storage at its key with the size it was committed with (Stat); each
//     scan's manifest on the storage lists every instance this reception
//     confirmed with its checksum, every instance the catalog holds in the
//     scan, whichever reception confirmed it, with its place and checksum
//     (scanListed), and every entry it lists is held by the catalog under
//     that UID, study, series and checksum; and no claim of
//     this reception is left but those a held record of it keeps (a file
//     whose read-back failed, whose claim the finish step releases). A
//     failure is an error: the step is tried again after its delay and,
//     when its unit gives up, the reception fails with every incoming copy
//     kept. Nothing is held here, since an instance confirmed is counted
//     archived already.
//  3. Take the verify step.
//  4. Remove the incoming copy of each file the catalog holds under its UID
//     with the same checksum (confirmed by this reception or, for a
//     duplicate, by another), unless a held quarantine record of the
//     reception keeps it. Every other copy stays. A removal that a crash
//     undoes leaves a recorded name with no row once the reception is done,
//     which a restart removes.
//  5. Take the finish step, which releases the claims the reception did not
//     confirm, and complete the unit with nothing after it, in that order; a
//     crash between them is taken up by 1 and 4.
func (p *Pipeline) verify(ctx context.Context, w tenant.Work) error {
	r, _, err := p.store.Reception(ctx, w.ReceptionID)
	if err != nil {
		return err
	}
	switch r.State {
	case reception.Archived:
		p.at("verify.before-check")
		if err := p.checkWhole(ctx, r); err != nil {
			return err
		}
		p.at("verify.before-step")
		if r, err = p.step(ctx, w, tenant.ReceptionStep{ID: r.ID, From: reception.Archived, Event: reception.Verify}); err != nil {
			return err
		}
		p.at("verify.after-step")
	case reception.Verified:
	default:
		if r.State.Ended() {
			return p.complete(ctx, w, nil)
		}
		return fault.New(fault.Internal, "a verify unit names a reception that is not archived")
	}
	if err := p.removeCopies(ctx, r); err != nil {
		return err
	}
	if r.State == reception.Verified {
		p.at("verify.before-finish")
		if _, err := p.step(ctx, w, tenant.ReceptionStep{ID: r.ID, From: reception.Verified, Event: reception.Finish}); err != nil {
			return err
		}
		if p.cfg.Forwarded != nil {
			p.cfg.Forwarded()
		}
	}
	p.at("verify.after-finish")
	return p.complete(ctx, w, nil)
}

// judged is a page of a reception's received files with the catalog's
// answer for each.
type judged struct {
	f tenant.ReceivedFile
	d tenant.Duplicate
}

// eachJudged calls fn for each received file of a reception with what the
// catalog holds under its UID (CheckDuplicates), a page at a time.
func (p *Pipeline) eachJudged(ctx context.Context, receptionID string, fn func(judged) error) error {
	page := master.Page{Limit: min(master.MaxPageLimit, tenant.MaxDuplicateBatch)}
	for {
		files, next, err := p.store.ReceivedFiles(ctx, receptionID, page)
		if err != nil {
			return err
		}
		qs := make([]tenant.DuplicateQuery, len(files))
		for i, f := range files {
			qs[i] = tenant.DuplicateQuery{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
		}
		if len(qs) > 0 {
			ds, err := p.store.CheckDuplicates(ctx, qs)
			if err != nil {
				return err
			}
			if len(ds) != len(files) {
				return fault.New(fault.Internal, "a duplicate check answered another number of files than it was given")
			}
			for i, f := range files {
				if err := fn(judged{f: f, d: ds[i]}); err != nil {
					return err
				}
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// errNotWhole is the cause of a verify step whose check found the
// reception's work not whole.
var errNotWhole = errors.New("the reception's archive is not whole")

// notWhole returns the failure of the check, with a fixed phrase.
func notWhole(message string) error {
	return fault.Wrap(fault.Internal, errNotWhole, "verification: "+message)
}

// checkWhole checks a reception's work (see verify, step 2). Its memory is
// one scan's manifest at a time and a page of files.
func (p *Pipeline) checkWhole(ctx context.Context, r tenant.Reception) error {
	scans, more, err := p.store.ReceptionScans(ctx, r.ID)
	if err != nil {
		return err
	}
	if more {
		return fault.New(fault.Internal, "a reception confirmed more scans than can be listed for its verification")
	}
	kept, err := p.heldPaths(ctx, r.ID)
	if err != nil {
		return err
	}
	store, err := p.cfg.OpenStorage(ctx, r.Storage)
	if err != nil {
		return err
	}
	type scanCheck struct {
		path      label.Path
		listed    map[string]manifest.File
		held      map[string]tenant.ScanInstance
		want      int64
		confirmed int64
	}
	bySeries := make(map[string]*scanCheck, len(scans))
	for _, sc := range scans {
		path := label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: sc.Label}
		key, err := layout.ManifestKey(path)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "a scan's labels make no key")
		}
		m, err := readManifest(ctx, store, key)
		if err != nil {
			return err
		}
		c := &scanCheck{path: path, listed: make(map[string]manifest.File, len(m.Files)), want: sc.Instances}
		for _, f := range m.Files {
			c.listed[f.SOPInstanceUID] = f
		}
		if c.held, err = p.scanListed(ctx, sc.SeriesUID, c.listed); err != nil {
			return err
		}
		if err := manifestHeld(m.Files, c.held); err != nil {
			return err
		}
		bySeries[sc.SeriesUID] = c
	}
	err = p.eachJudged(ctx, r.ID, func(j judged) error {
		switch {
		case j.d.Kind == tenant.Claimed && j.d.HeldBy == r.ID:
			if !kept[j.f.Incoming] {
				return notWhole("a claim of this reception is left that no record explains")
			}
			return nil
		case j.d.Kind != tenant.SameFile || j.d.HeldBy != r.ID:
			return nil
		}
		c, ok := bySeries[j.f.SeriesUID]
		if !ok {
			return notWhole("an instance this reception confirmed is in none of its scans")
		}
		c.confirmed++
		k, err := layout.InstanceKey(c.path, j.f.SOPInstanceUID)
		if err != nil {
			return fault.Wrap(fault.Internal, err, "a confirmed instance makes no key")
		}
		// The catalog's row of the instance, which the manifest lists as
		// it is (scanListed), holds the checksum it was received with and
		// that of what was archived, which differ for a file
		// de-identified: the received file is matched by the one, the
		// object on the storage by the other.
		in, held := c.held[j.f.SOPInstanceUID]
		e, ok := c.listed[j.f.SOPInstanceUID]
		if !ok || !held || e.Key != k || in.SHA256 != j.f.SHA256 || e.SHA256 != in.ArchivedSHA256.String() || e.Size != in.Size {
			return notWhole("the scan's manifest does not list an instance this reception confirmed")
		}
		p.at("verify.before-stat")
		info, err := store.Stat(ctx, k)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			return notWhole("an instance this reception confirmed is not on its storage")
		case err != nil:
			return storageError(err, "an archived file cannot be looked at")
		case info.Size != in.Size:
			return notWhole("an instance this reception confirmed has another size on its storage")
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, c := range bySeries {
		if c.confirmed != c.want {
			return notWhole("the catalog and the reception's files disagree on what it confirmed in a scan")
		}
	}
	return nil
}

// manifestHeld checks that every entry of a scan's manifest is an instance
// the catalog holds in the scan (held, as scanListed read it) with that
// place, size and checksum of what was archived: with scanListed, which
// checks the other way, the manifest and the catalog list the same
// instances.
func manifestHeld(files []manifest.File, held map[string]tenant.ScanInstance) error {
	for _, f := range files {
		in, ok := held[f.SOPInstanceUID]
		if !ok || f != manifestFile(in) {
			return notWhole("the scan's manifest lists an instance the catalog does not hold as listed")
		}
	}
	return nil
}

// readSum reads an object as a stream through SHA-256.
func readSum(ctx context.Context, store blob.Store, key string) (tenant.SHA256, error) {
	rc, err := store.Read(ctx, key, 0, -1)
	if err != nil {
		if fault.KindOf(err) == fault.NotFound {
			return tenant.SHA256{}, err
		}
		return tenant.SHA256{}, storageError(err, "an archived file cannot be read back")
	}
	defer func() { _ = rc.Close() }()
	sum, err := sumReader(rc)
	if err != nil {
		return tenant.SHA256{}, storageError(err, "an archived file cannot be read back")
	}
	return sum, nil
}

// removeCopies removes the incoming copies the catalog no longer needs (see
// verify, step 4).
func (p *Pipeline) removeCopies(ctx context.Context, r tenant.Reception) error {
	kept, err := p.heldPaths(ctx, r.ID)
	if err != nil {
		return err
	}
	return p.eachJudged(ctx, r.ID, func(j judged) error {
		if j.d.Kind != tenant.SameFile || kept[j.f.Incoming] {
			return nil
		}
		p.at("verify.before-remove")
		return p.area.Remove(j.f.Incoming)
	})
}

// heldPaths returns the incoming paths the reception's held quarantine
// records keep.
func (p *Pipeline) heldPaths(ctx context.Context, receptionID string) (map[string]bool, error) {
	out := map[string]bool{}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		qs, next, err := p.store.ListQuarantined(ctx, tenant.QuarantineFilter{ReceptionID: receptionID, State: tenant.QuarantineHeld}, page)
		if err != nil {
			return nil, err
		}
		for _, q := range qs {
			if q.ContentKept {
				out[q.Incoming] = true
			}
		}
		if next == "" {
			return out, nil
		}
		page.Cursor = next
	}
}
