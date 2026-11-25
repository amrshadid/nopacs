package storageswitch

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// writeTimeout bounds each write of a run to the tenant's database that
// must not end with the Switcher's stop: a refusal, the swap, the finish.
const writeTimeout = 30 * time.Second

// errStopped is a run cut short by the Switcher's stop.
var errStopped = errors.New("storageswitch: the server is stopping")

// run carries an accepted switch through its pause, drain, owner record and
// swap, or refuses it; the gate of its primary is paused from its first
// step and resumed at its end either way.
func (s *Switcher) run(sw tenant.StorageSwitch, drain time.Duration) {
	start := s.cfg.Clock.Now()
	deadline := start.Add(drain)
	s.gate.pause(sw.Primary)
	defer s.gate.resume(sw.Primary)
	refused0 := s.gate.refusedCount(sw.Primary)
	log := s.log.With("primary", sw.Primary, "replica", sw.Replica, "switch", sw.ID)
	log.Info("a switch began: writes to the primary pause")
	refuse := func(phrase string) {
		s.refuse(sw, phrase)
		log.Warn("a switch was refused; writes resume on the old place", "refusal", phrase)
	}
	if err := s.step(sw.ID, tenant.SwitchPausing); err != nil {
		refuse(s.phraseOf(err, tenant.RefusalStorageNoAnswer))
		return
	}
	if err := s.wait(s.gate.idleOf(sw.Primary), deadline); err != nil {
		refuse(s.phraseOf(err, tenant.RefusalNotInStep))
		return
	}
	if err := s.step(sw.ID, tenant.SwitchDraining); err != nil {
		refuse(s.phraseOf(err, tenant.RefusalStorageNoAnswer))
		return
	}
	var drained int64 = -1
	owned := false
	for {
		f, phrase := s.drain(sw, deadline, &drained)
		if phrase != "" {
			refuse(phrase)
			return
		}
		if !owned {
			if err := s.step(sw.ID, tenant.SwitchOwning); err != nil {
				refuse(s.phraseOf(err, tenant.RefusalStorageNoAnswer))
				return
			}
			if err := s.takeOwner(s.root, f.Replica); err != nil {
				log.Warn("the owner record of the replica's place could not be taken", "reason", fault.Message(err))
				refuse(s.phraseOf(err, tenant.RefusalOwnerNotTaken))
				return
			}
			owned = true
		}
		done, err := s.swap(sw, f, max(drained, 0), refused0, start)
		if err == nil {
			s.finish(done, log)
			return
		}
		log.Warn("the swap was not made; the drain goes on", "reason", fault.Message(err))
		if !s.cfg.Clock.Now().Before(deadline) {
			refuse(s.phraseOf(err, tenant.RefusalStorageNoAnswer))
			return
		}
		if err := s.wait(nil, s.cfg.Clock.Now().Add(s.cfg.Poll)); err != nil && errors.Is(err, errStopped) {
			refuse(tenant.RefusalRestarted)
			return
		}
	}
}

// drain waits until every scan of sw's primary is in step on its replica
// and no finding holds on it, waking the replicator, and answers the facts
// then; or answers the refusal's phrase once deadline passes. drained is
// set, once, to the scans not in step when the drain began.
func (s *Switcher) drain(sw tenant.StorageSwitch, deadline time.Time, drained *int64) (tenant.SwitchFacts, string) {
	for {
		if s.cfg.WriteRecordsOwed != nil {
			if err := s.cfg.WriteRecordsOwed(s.root, sw.Replica); err != nil {
				s.log.Warn("the records owed to the replica could not all be written; the drain goes on", "replica", sw.Replica,
					"reason", fault.Message(err))
			}
		}
		f, err := s.cfg.DB.SwitchFacts(s.root, sw.Primary, sw.Replica)
		switch {
		case errors.Is(err, context.Canceled) && s.root.Err() != nil:
			return f, tenant.RefusalRestarted
		case err == nil:
			if *drained < 0 {
				*drained = f.ScansNotInStep
			}
			if f.ScansNotInStep == 0 && f.ReplicaFindings == 0 {
				return f, ""
			}
		}
		if !s.cfg.Clock.Now().Before(deadline) {
			switch {
			case err != nil:
				return f, tenant.RefusalStorageNoAnswer
			case f.ScansNotInStep > 0:
				return f, tenant.RefusalNotInStep
			}
			return f, tenant.RefusalFindingHolds
		}
		s.cfg.Wake()
		next := s.cfg.Clock.Now().Add(s.cfg.Poll)
		if next.After(deadline) {
			next = deadline
		}
		if err := s.wait(nil, next); errors.Is(err, errStopped) {
			return f, tenant.RefusalRestarted
		}
	}
}

// swap seals the moving secrets again and swaps the places in one
// transaction. An answer lost after the commit is told by reading the
// switch again.
func (s *Switcher) swap(sw tenant.StorageSwitch, f tenant.SwitchFacts, drained, refused0 int64, start time.Time) (tenant.StorageSwitch, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.root), writeTimeout)
	defer cancel()
	var forPrimary, forReplica []byte
	if s.cfg.Reseal != nil {
		var err error
		if forPrimary, forReplica, err = s.cfg.Reseal(ctx, f.Primary, f.Replica); err != nil {
			return tenant.StorageSwitch{}, err
		}
	}
	now := s.now()
	done, err := s.cfg.DB.SwapStorages(ctx, tenant.SwitchSwap{ID: sw.ID, ScansDrained: drained,
		AssociationsRefused: s.gate.refusedCount(sw.Primary) - refused0, Pause: max(s.cfg.Clock.Now().Sub(start), 0),
		PrimarySecret: forPrimary, ReplicaSecret: forReplica, Now: now}, s.audit(sw, "swapped"))
	if err == nil {
		return done, nil
	}
	if got, gerr := s.cfg.DB.Switch(ctx, sw.ID); gerr == nil && (got.State == tenant.SwitchSwapped || got.State == tenant.SwitchDone) {
		return got, nil
	}
	return tenant.StorageSwitch{}, err
}

// finish ends a swapped switch. A finish that fails is done by the next
// start (Recover): the swap stands either way.
func (s *Switcher) finish(sw tenant.StorageSwitch, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.root), writeTimeout)
	defer cancel()
	if _, err := s.cfg.DB.FinishSwitch(ctx, sw.ID, s.now()); err != nil {
		log.Warn("a swapped switch could not be finished; the next start finishes it", "reason", fault.Message(err))
	}
	log.Info("a switch was made: the primary's name now names the replica's place", "scans_drained", sw.ScansDrained,
		"associations_refused", sw.AssociationsRefused, "pause", sw.Pause)
}

// refuse ends sw refused with phrase, trying again while the database does
// not answer: a switch left accepted keeps its primary's writes paused
// through the gate's reading of it, until a refusal, or the next start,
// ends it.
func (s *Switcher) refuse(sw tenant.StorageSwitch, phrase string) {
	delay := time.Second
	for {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(s.root), writeTimeout)
		_, err := s.cfg.DB.RefuseSwitch(ctx, sw.ID, phrase, s.now(), s.audit(sw, "refused"))
		cancel()
		if err == nil || fault.KindOf(err) == fault.Precondition || s.root.Err() != nil {
			if err != nil {
				s.log.Warn("a switch could not be refused", "switch", sw.ID, "reason", fault.Message(err))
			}
			return
		}
		select {
		case <-s.root.Done():
			return
		case <-s.cfg.Clock.After(delay):
		}
		delay = min(2*delay, time.Minute)
	}
}

// step records the step sw reached.
func (s *Switcher) step(id string, step tenant.SwitchStepName) error {
	ctx, cancel := context.WithTimeout(s.root, writeTimeout)
	defer cancel()
	_, err := s.cfg.DB.SwitchStep(ctx, id, step, s.now())
	return err
}

// wait waits until ch closes (nil: never) or deadline passes; it answers
// an error naming the drain's end when deadline passes first, errStopped
// when the Switcher stops.
func (s *Switcher) wait(ch <-chan struct{}, deadline time.Time) error {
	d := deadline.Sub(s.cfg.Clock.Now())
	if d <= 0 {
		select {
		case <-ch:
			return nil
		default:
		}
		if ch == nil {
			return nil
		}
		return fault.New(fault.Unavailable, "the writes in flight did not end within the drain timeout")
	}
	select {
	case <-ch:
		return nil
	case <-s.cfg.Clock.After(d):
		if ch == nil {
			return nil
		}
		return fault.New(fault.Unavailable, "the writes in flight did not end within the drain timeout")
	case <-s.root.Done():
		return errStopped
	}
}

// phraseOf returns the refusal phrase of a failed step: "server restarted"
// for a run the stop cut short, otherwise fallback.
func (s *Switcher) phraseOf(err error, fallback string) string {
	if errors.Is(err, errStopped) || (s.root.Err() != nil && errors.Is(err, context.Canceled)) {
		return tenant.RefusalRestarted
	}
	// A swap the database refused says why in one of the refusal phrases
	// when one fits (records still owed to the replica, a finding on it).
	if m := fault.Message(err); fault.KindOf(err) == fault.Precondition && tenant.ValidateRefusal(m) == nil {
		return m
	}
	return fallback
}
