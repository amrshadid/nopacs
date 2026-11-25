package receive

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// errClaimedElsewhere is a step that must wait: another reception in flight
// claims some of its instances. It is tried again through the unit's retry
// delay, by which time that reception has confirmed them (a held answer) or
// let them go (they can be claimed).
var errClaimedElsewhere = fault.New(fault.Unavailable, "another reception in flight claims some of these instances; the step is tried again later")

// archive writes an identified reception's files to its storage and commits
// them to the catalog, one scan at a time.
//
// Order of effects, each safe to repeat or to stop after (archiveScan gives
// the order inside one scan):
//
//  1. Read the reception. One past identified took this step before a
//     crash: its unit is completed with the unit its state needs.
//  2. Hold as lost every received file whose incoming copy is gone
//     (dropLost), before any claim. Then for each series, in order of UID: its scan label from the catalog, or
//     the receiver's rule over the same file identify read (the series'
//     files are held with the label reason when it cannot be made: identify
//     held them already, and the record's id makes the second hold
//     nothing); then the scan is archived (archiveScan), which holds the
//     series' files as a label conflict, before any put, when the label
//     is another series' scan in the catalog.
//     A session the catalog holds on another storage than the
//     reception's (another reception of the study made it meanwhile)
//     holds every file as unrouted, session_elsewhere, before any put.
//  3. When an instance is claimed by another reception, the step fails
//     here, after every other scan is archived, and is tried again after
//     the unit's retry delay; the scans done stay done (their claims answer
//     ArchivedBefore on the next run).
//  4. Take the archive step with the duplicate counts, counted afresh on
//     every run from the claims' answers, or the quarantine step when
//     nothing was archived and nothing was a duplicate. Archived counts only
//     files read back equal and committed, and a file that read back other
//     bytes is quarantined and never archived, so a reception's received
//     files are its archived, duplicate and quarantined ones, by
//     construction.
//  5. Complete the unit with the verify unit, in one transaction. A crash
//     between 4 and 5 is taken up by 1.
func (p *Pipeline) archive(ctx context.Context, w tenant.Work) error {
	r, _, err := p.store.Reception(ctx, w.ReceptionID)
	if err != nil {
		return err
	}
	switch {
	case r.State.Ended():
		return p.complete(ctx, w, nil)
	case r.State == reception.Archived || r.State == reception.Verified:
		return p.completeFor(ctx, w, r)
	case r.State != reception.Identified:
		return fault.New(fault.Internal, "an archive unit names a reception that is not identified")
	}
	if p.cfg.Pause != nil {
		// Entered before the storage is opened and held to the step's end:
		// a switch of the storage waits for this step's puts and commit, or
		// this step waits for the switch and opens the place it leaves.
		release, err := p.cfg.Pause.Enter(ctx, r.Storage)
		if err != nil {
			return err
		}
		defer release()
		p.at("archive.entered")
	}
	store, err := p.cfg.OpenStorage(ctx, r.Storage)
	if err != nil {
		return err
	}
	groups, err := p.receivedBySeries(ctx, r.ID)
	if err != nil {
		return err
	}
	if groups, err = p.dropLost(ctx, w, r, groups); err != nil {
		return err
	}
	catalog, err := p.catalogLabels(ctx, r.StudyUID, groups)
	if err != nil {
		return err
	}
	if c := catalog.Session; c != nil && c.Storage != r.Storage {
		// Another reception of the study made its session on another
		// storage while this one was in flight: its files are held before
		// any is put, rather than put where the session is not.
		return p.holdUnrouted(ctx, w, r, groups, map[string]string{"cause": "session_elsewhere"})
	}
	rule, err := p.ruleFor(ctx, r.Receiver)
	if err != nil {
		return err
	}
	kept, err := p.heldPaths(ctx, r.ID)
	if err != nil {
		return err
	}
	var total scanResult
	for _, g := range groups {
		scan, ok := catalog.Scans[g.uid]
		if !ok {
			path, lf, err := p.labelsOf(ctx, rule, g)
			if err != nil {
				return err
			}
			if lf != nil {
				records := make([]tenant.QuarantinedFile, 0, len(g.files))
				for _, f := range g.files {
					records = append(records, p.holdRecord(r, f, tenant.QuarantineBadLabel, lf.details()))
				}
				if err := p.hold(ctx, w, records); err != nil {
					return err
				}
				continue
			}
			scan = path.Scan
		}
		res, err := p.archiveScan(ctx, w, r, store, label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: scan}, g, kept, catalog.Session)
		if err != nil {
			return err
		}
		total.kept += res.kept
		total.dupFiles += res.dupFiles
		total.dupBytes += res.dupBytes
		total.claimed = total.claimed || res.claimed
	}
	if total.claimed {
		return errClaimedElsewhere
	}
	if total.kept == 0 && total.dupFiles == 0 {
		return p.quarantineAll(ctx, w, r)
	}
	p.at("archive.before-step")
	if _, err := p.step(ctx, w, tenant.ReceptionStep{ID: r.ID, From: reception.Identified, Event: reception.Archive,
		Counts: &tenant.StepCounts{FilesDuplicate: total.dupFiles, BytesDuplicate: total.dupBytes}}); err != nil {
		return err
	}
	p.at("archive.after-step")
	next := unitFor(kindVerify, r.ID, p.now())
	return p.complete(ctx, w, &next)
}
