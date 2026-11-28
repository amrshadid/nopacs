package receive

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// identify gives a complete reception its subject and session labels and its
// storage, and holds the files whose labels cannot be made.
//
// Order of effects, each safe to repeat or to stop after:
//
//  1. Read the reception. One past complete took this step before a crash:
//     its unit is completed with the unit its state needs.
//  2. Hold as lost every received file whose incoming copy is gone
//     (dropLost); the rest go on.
//  3. Read what the catalog holds for the study and its series
//     (CatalogLabels): a study or series it knows keeps its labels and
//     storage, so a later arrival lands beside the first. The rest are named
//     by the receiver's rule over the header of a file of each, the same
//     file every run (the first of its series by UID whose bytes are here),
//     so a run again makes the same labels. A study with no session whose
//     receiver was removed is held as unrouted (receiver_removed): its
//     subject would be named under the receiver's namespace, gone with it.
//  4. Hold, with the label reason, every file of a series whose scan label
//     cannot be made, or every file when the subject or session label cannot
//     be. A record's id is made from the reception and the file, so a run
//     again writes nothing new.
//     Then hold, as was_deleted, every file that names data a delete
//     removed (holdDeleted), the same way.
//  5. Route the study: a session the catalog holds gives its storage and
//     what was chosen for it; else another reception of the study in
//     flight, already identified, gives what was chosen for it (one not
//     yet identified that arrived first is waited for: the step is tried
//     again later); else the receiver's routing is evaluated (route), and
//     every file is held as unrouted when it does not route the study. A
//     run that finds files held as unrouted before finishes that hold.
//  6. Take the identify step with the labels, the storage and what was
//     chosen, or the quarantine step when no file can go on.
//  7. Complete the unit with the archive unit, in one transaction. A crash
//     between 6 and 7 is taken up by 1.
//
// A label that collides with one the catalog holds for another study or
// series is found by the archive step's commit, which holds those files
// then: the catalog decides that in its own transaction.
func (p *Pipeline) identify(ctx context.Context, w tenant.Work) error {
	r, arrivals, err := p.store.Reception(ctx, w.ReceptionID)
	if err != nil {
		return err
	}
	switch {
	case r.State.Ended():
		return p.complete(ctx, w, nil)
	case r.State != reception.Complete:
		return p.completeFor(ctx, w, r)
	}
	groups, err := p.receivedBySeries(ctx, r.ID)
	if err != nil {
		return err
	}
	if groups, err = p.dropLost(ctx, w, r, groups); err != nil {
		return err
	}
	if len(groups) == 0 {
		return p.quarantineAll(ctx, w, r)
	}
	// A run before held files as unrouted: that decision was made, and is
	// finished as it was made, whatever the routes say now, so a study is
	// never part held and part archived.
	if details, err := p.unroutedBefore(ctx, r.ID); err != nil || details != nil {
		if err != nil {
			return err
		}
		return p.holdUnrouted(ctx, w, r, groups, details)
	}
	catalog, err := p.catalogLabels(ctx, r.StudyUID, groups)
	if err != nil {
		return err
	}
	rule, err := p.ruleFor(ctx, r.Receiver)
	if err != nil {
		return err
	}
	rcv, err := p.receivers.get(ctx, r.Receiver)
	removed := fault.KindOf(err) == fault.NotFound
	if err != nil && !removed {
		return err
	}
	if removed && catalog.Session == nil {
		// A new subject is named under the receiver's namespace, which
		// is gone with the receiver; named under the default one, the
		// same patient would become a second subject, and a delete of the
		// first would not hold its files. Fail closed: hold the study.
		return p.holdUnrouted(ctx, w, r, groups, map[string]string{"cause": tenant.UnroutedReceiverRemoved})
	}
	var subject, session, storage string
	if s := catalog.Session; s != nil {
		subject, session, storage = s.SubjectLabel, s.SessionLabel, s.Storage
	} else {
		// The subject and session labels come from the first series whose
		// file the rule names whole. A fault at the subject or session
		// level is the study's: no session can be made, and every file is
		// held. A fault at the scan level is that series' alone, and the
		// next series is tried.
		for _, g := range groups {
			path, lf, err := p.labelsOf(ctx, rule, g)
			if err != nil {
				return err
			}
			if lf != nil && lf.level != "scan" {
				return p.holdEverything(ctx, w, r, groups, lf)
			}
			if lf == nil {
				subject, session = path.Subject, path.Session
				break
			}
		}
		if subject == "" {
			// Every series failed its scan label: hold each with its
			// fault, and nothing is left.
			if _, err := p.holdBadScans(ctx, w, r, groups, catalog, rule); err != nil {
				return err
			}
			return p.quarantineAll(ctx, w, r)
		}
	}
	// A file of data a delete removed is held as was_deleted, before the
	// study is routed, so a study sent again never lands again.
	if groups, err = p.holdDeleted(ctx, w, r, groups, subject); err != nil {
		return err
	}
	if len(groups) == 0 {
		return p.quarantineAll(ctx, w, r)
	}
	// A study is routed once, for its session: a later arrival goes by
	// what was chosen for the session, a reception of a study another
	// reception in flight was routed for goes by that, and only the first
	// reception of a new session evaluates the routes. What is chosen is
	// kept with the step, so a step run again after the step finds it
	// taken, and the steps after go by it.
	var way routed
	if s := catalog.Session; s != nil {
		way = routed{choice: s.Route.Archived(), storage: s.Storage}
	} else {
		sibling, found, err := p.routedInFlight(ctx, r, arrivals)
		switch {
		case err != nil:
			return err
		case found:
			way = routed{choice: sibling.Route.Archived(), storage: sibling.Storage}
		default:
			if way, err = p.route(ctx, r, rcv, groups); err != nil {
				return err
			}
		}
		storage = way.storage
	}
	if way.unrouted != nil {
		return p.holdUnrouted(ctx, w, r, groups, way.unrouted)
	}
	// The forward rules are each reception's own: every arrival of a
	// session is forwarded by the rules its receiver runs now, over its
	// own series.
	if way.choice.ForwardRuleSet, way.choice.Forwards, err = p.forwards(ctx, rcv, groups); err != nil {
		return err
	}
	left, err := p.holdBadScans(ctx, w, r, groups, catalog, rule)
	if err != nil {
		return err
	}
	if left == 0 {
		return p.quarantineAll(ctx, w, r)
	}
	p.at("identify.before-step")
	if _, err := p.step(ctx, w, tenant.ReceptionStep{ID: r.ID, From: reception.Complete, Event: reception.Identify,
		SubjectLabel: subject, SessionLabel: session, Storage: storage, Route: way.choice}); err != nil {
		return err
	}
	p.at("identify.after-step")
	next := unitFor(kindArchive, r.ID, p.now())
	return p.complete(ctx, w, &next)
}

// holdBadScans holds the files of each series whose scan label cannot be
// made, and returns how many series can go on.
func (p *Pipeline) holdBadScans(ctx context.Context, w tenant.Work, r tenant.Reception, groups []series, catalog tenant.CatalogLabels, rule *naming.Rule) (int, error) {
	left := 0
	var records []tenant.QuarantinedFile
	for _, g := range groups {
		if _, ok := catalog.Scans[g.uid]; ok {
			left++
			continue
		}
		_, lf, err := p.labelsOf(ctx, rule, g)
		if err != nil {
			return 0, err
		}
		if lf != nil {
			for _, f := range g.files {
				records = append(records, p.holdRecord(r, f, tenant.QuarantineBadLabel, lf.details()))
			}
			continue
		}
		left++
	}
	return left, p.hold(ctx, w, records)
}

// holdEverything holds every file of the reception with the label fault,
// then ends it as quarantined.
func (p *Pipeline) holdEverything(ctx context.Context, w tenant.Work, r tenant.Reception, groups []series, lf *labelFault) error {
	var records []tenant.QuarantinedFile
	for _, g := range groups {
		for _, f := range g.files {
			records = append(records, p.holdRecord(r, f, tenant.QuarantineBadLabel, lf.details()))
		}
	}
	if err := p.hold(ctx, w, records); err != nil {
		return err
	}
	return p.quarantineAll(ctx, w, r)
}

// quarantineAll ends a reception none of whose files can go on, and
// completes its unit with nothing after it.
func (p *Pipeline) quarantineAll(ctx context.Context, w tenant.Work, r tenant.Reception) error {
	p.at("step.before-quarantine")
	if _, err := p.step(ctx, w, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Quarantine}); err != nil {
		return err
	}
	return p.complete(ctx, w, nil)
}
