package tenant

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
)

// ReceptionStepReached is a reception reaching a step, the contract's
// message of that name.
type ReceptionStepReached struct {
	// Reception is the reception's id.
	Reception string `json:"reception"`
	// Receiver names the receiver it arrived at.
	Receiver string `json:"receiver"`
	// Arrival is its arrival number.
	Arrival int `json:"arrival"`
	// Step is the step it reached.
	Step reception.State `json:"step"`
	// PreviousStep is the step it left; empty for a reception's first
	// step (receiving, or complete for one made by RetryReception).
	PreviousStep reception.State `json:"previous_step"`
	// Counts are its totals when it reached the step.
	Counts EventCounts `json:"counts"`
	// SubjectLabel and SessionLabel are its labels, empty before it is
	// identified.
	SubjectLabel string `json:"subject_label"`
	// SessionLabel is its session's label, empty before it is identified.
	SessionLabel string `json:"session_label"`
	// RetryOf is the failed reception it runs again, empty unless it was
	// made by RetryReception.
	RetryOf string `json:"retry_of"`
}

// EventCounts are a reception's totals as an event carries them, the
// contract's ReceptionCounts. Its fields are ReceptionCounts' own, in the
// same order, so one converts to the other and a count added to one and
// not the other does not compile.
type EventCounts struct {
	FilesReceived    int64 `json:"files_received"`
	BytesReceived    int64 `json:"bytes_received"`
	FilesArchived    int64 `json:"files_archived"`
	BytesArchived    int64 `json:"bytes_archived"`
	FilesDuplicate   int64 `json:"files_duplicate"`
	BytesDuplicate   int64 `json:"bytes_duplicate"`
	FilesQuarantined int64 `json:"files_quarantined"`
	BytesQuarantined int64 `json:"bytes_quarantined"`
	FilesReleased    int64 `json:"files_released"`
}

// EventType implements EventPayload.
func (*ReceptionStepReached) EventType() EventType { return EventReceptionStep }

// validate implements EventPayload.
func (p *ReceptionStepReached) validate() error {
	if err := master.ValidateID("reception id", p.Reception); err != nil {
		return err
	}
	if err := ValidateName("receiver", p.Receiver); err != nil {
		return err
	}
	if p.Arrival < 1 {
		return invalid("an arrival number is at least 1")
	}
	if !knownState(p.Step) || (p.PreviousStep != "" && !knownState(p.PreviousStep)) {
		return invalid("a step is one of " + statesText())
	}
	c := p.Counts
	for _, n := range []int64{c.FilesReceived, c.BytesReceived, c.FilesArchived, c.BytesArchived, c.FilesDuplicate,
		c.BytesDuplicate, c.FilesQuarantined, c.BytesQuarantined, c.FilesReleased} {
		if n < 0 {
			return invalid("a reception's counts are not negative")
		}
	}
	for _, l := range []string{p.SubjectLabel, p.SessionLabel} {
		if l != "" && !label.Valid(l) {
			return invalid("an event's labels are valid labels")
		}
	}
	if p.RetryOf != "" {
		return master.ValidateID("retried reception id", p.RetryOf)
	}
	return nil
}

// ReceptionStepEvent returns the event of a reception as it is after a
// step from the state previous (empty for its first), at the step's time.
// Its study's UID is not in it.
func ReceptionStepEvent(previous reception.State, r Reception) (EventRecord, error) {
	return NewEvent(&ReceptionStepReached{
		Reception: r.ID, Receiver: r.Receiver, Arrival: r.Arrival, Step: r.State, PreviousStep: previous, Counts: EventCounts(r.Counts),
		SubjectLabel: r.SubjectLabel, SessionLabel: r.SessionLabel, RetryOf: r.RetryOf,
	}, r.StepTime)
}

// FileQuarantined is one file quarantined, the contract's message of that
// name.
type FileQuarantined struct {
	// File is the record's id.
	File string `json:"file"`
	// Reception is its reception's id, empty when it had none.
	Reception string `json:"reception"`
	// Receiver names the receiver it arrived at.
	Receiver string `json:"receiver"`
	// Reason is why it was quarantined.
	Reason QuarantineReason `json:"reason"`
	// SizeBytes is its size.
	SizeBytes int64 `json:"size_bytes"`
	// HeldFiles and HeldBytes are the quarantine's totals with it.
	HeldFiles int64 `json:"held_files"`
	// HeldBytes are the bytes kept for the files held, as
	// QuarantineTotals counts them.
	HeldBytes int64 `json:"held_bytes"`
}

// EventType implements EventPayload.
func (*FileQuarantined) EventType() EventType { return EventFileQuarantined }

// validate implements EventPayload.
func (p *FileQuarantined) validate() error {
	if err := master.ValidateID("quarantine id", p.File); err != nil {
		return err
	}
	if p.Reception != "" {
		if err := master.ValidateID("reception id", p.Reception); err != nil {
			return err
		}
	}
	if err := ValidateName("receiver", p.Receiver); err != nil {
		return err
	}
	if !p.Reason.Valid() {
		return invalid("a quarantine reason is one of the contract's codes")
	}
	if p.SizeBytes < 0 || p.HeldFiles < 1 || p.HeldBytes < 0 {
		return invalid("a quarantined file's size and totals are not negative, and the files held count it")
	}
	return nil
}

// FileQuarantinedEvent returns the event of a record just held, with the
// quarantine's totals after it, at its quarantine time.
func FileQuarantinedEvent(q QuarantinedFile, heldFiles, heldBytes int64) (EventRecord, error) {
	return NewEvent(&FileQuarantined{File: q.ID, Reception: q.ReceptionID, Receiver: q.Receiver, Reason: q.Reason,
		SizeBytes: q.Size, HeldFiles: heldFiles, HeldBytes: heldBytes}, q.Quarantined)
}

// QuarantineDecided is a quarantined file released or rejected, the
// contract's message of that name.
type QuarantineDecided struct {
	// File is the record's id.
	File string `json:"file"`
	// State is what was decided: released or rejected.
	State QuarantineState `json:"state"`
	// ReleaseMode is how it was released; empty for a rejection.
	ReleaseMode ReleaseMode `json:"release_mode"`
	// ReleaseReception is the reception its release joined; empty for a
	// rejection, and for a release of a file that joined none.
	ReleaseReception string `json:"release_reception"`
	// HeldFiles and HeldBytes are the quarantine's totals after it.
	HeldFiles int64 `json:"held_files"`
	// HeldBytes are the bytes kept for the files held after it.
	HeldBytes int64 `json:"held_bytes"`
}

// EventType implements EventPayload.
func (*QuarantineDecided) EventType() EventType { return EventQuarantineDecided }

// validate implements EventPayload.
func (p *QuarantineDecided) validate() error {
	if err := master.ValidateID("quarantine id", p.File); err != nil {
		return err
	}
	switch p.State {
	case QuarantineReleased:
		if p.ReleaseMode != ReleaseAsIs {
			return invalid("a release's mode is as_is")
		}
		if p.ReleaseReception != "" {
			if err := master.ValidateID("reception id", p.ReleaseReception); err != nil {
				return err
			}
		}
	case QuarantineRejected:
		if p.ReleaseMode != "" || p.ReleaseReception != "" {
			return invalid("a rejection has no release mode or reception")
		}
	default:
		return invalid("a decision is released or rejected")
	}
	if p.HeldFiles < 0 || p.HeldBytes < 0 {
		return invalid("the quarantine's totals are not negative")
	}
	return nil
}

// QuarantineDecidedEvent returns the event of a record just released or
// rejected, with the quarantine's totals after it, at its decision time.
func QuarantineDecidedEvent(q QuarantinedFile, heldFiles, heldBytes int64) (EventRecord, error) {
	return NewEvent(&QuarantineDecided{File: q.ID, State: q.State, ReleaseMode: q.ReleaseMode,
		ReleaseReception: q.ReleaseReception, HeldFiles: heldFiles, HeldBytes: heldBytes}, q.Decided)
}
