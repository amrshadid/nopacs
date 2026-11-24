package tenant

import (
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Reception is the files of one study arriving at one receiver, and what
// became of them, as a store keeps it.
type Reception struct {
	// ID is the reception's id, given by the caller (master.ValidateID).
	ID string
	// Receiver names the receiver it arrived at.
	Receiver string
	// StudyUID is the Study Instance UID of its files, as received.
	StudyUID string
	// Arrival is its place among the receptions of the study in the
	// tenant, from 1, in the order they opened.
	Arrival int
	// State is where it is, as the core names it; the store keeps its
	// text, so a state a later version adds is kept as it is.
	State reception.State
	// Counts are its files and bytes so far.
	Counts ReceptionCounts
	// SubjectLabel, SessionLabel and Storage are set when it is
	// identified.
	SubjectLabel, SessionLabel, Storage string
	// Route is what its receiver's routing chose when it was identified,
	// or, before, the storage a release into it named.
	Route ReceptionRoute
	// Failure is why it failed, a phrase for a person; empty unless failed.
	Failure string
	// Started is when its first file arrived; LastFile when its latest
	// did; StepTime when it reached its state; Ended when it reached an
	// end, zero while in progress.
	Started, LastFile, StepTime, Ended time.Time
	// Associations counts the associations that brought its files.
	Associations int64
	// CallingAETitles are their calling AE titles, each once, in the order
	// they first appeared, at most MaxCallingAETitles; Truncated says when
	// there were more.
	CallingAETitles []string
	// CallingAETitlesTruncated says CallingAETitles holds only the first.
	CallingAETitlesTruncated bool
	// RetryOf is the failed reception whose files this one runs again.
	RetryOf string
	// RetryReception is the reception made from this failed one.
	RetryReception string
	// DiscardReason and DiscardTime are set when its files were discarded.
	DiscardReason string
	// DiscardTime is when its files were discarded, zero unless they were.
	DiscardTime time.Time
}

// ReceptionCounts are a reception's files and bytes. Each count has one
// writer, so no step overwrites what another change added:
//
//   - the received pair, by AddReceivedFiles, by ReleaseQuarantined into
//     another reception, and by HoldFiles for a file that arrived and could
//     not be recorded as a received file (too large, not DICOM): a file
//     that arrived is counted received once, whatever became of it;
//   - Released, by ReleaseQuarantined;
//   - the archived pair, by CommitScan, for each instance confirmed;
//   - the duplicate pair, by the step that decides them (StepCounts);
//   - the quarantined pair, by HoldFiles, less a file released back into
//     the same reception, which is no longer quarantined there.
//
// So at the end of a reception that did not fail, received equals archived
// plus duplicate plus quarantined, files and bytes, as the API contract
// states, when the pipeline holds each file it does not archive or ignore.
type ReceptionCounts struct {
	FilesReceived, BytesReceived       int64
	FilesArchived, BytesArchived       int64
	FilesDuplicate, BytesDuplicate     int64
	FilesQuarantined, BytesQuarantined int64
	FilesReleased                      int64
}

// StepCounts are the counts a step decides: the files ignored as the same
// file the tenant holds.
type StepCounts struct {
	FilesDuplicate, BytesDuplicate int64
}

// Arrival is one reception of a study, as Reception lists them.
type Arrival struct {
	// ID is the reception's id.
	ID string
	// Arrival is its arrival number.
	Arrival int
	// State is its state.
	State reception.State
	// Started is when its first file arrived.
	Started time.Time
}

// ReceptionStep is one step of a reception (Pipeline.StepReception).
type ReceptionStep struct {
	// ID names the reception.
	ID string
	// From is the state the caller found it in: the compare of the
	// compare-and-set.
	From reception.State
	// Event is what happened; the core's table gives the state it leads to.
	// File is refused: files are recorded by AddReceivedFiles.
	Event reception.Event
	// At is when the step is taken.
	At time.Time
	// LastFile, when not zero, must be the reception's last file time as
	// stored: a step decided from a quiet period is refused with
	// ErrStateChanged when a file came after the decision.
	LastFile time.Time
	// Counts, when not nil, are the duplicate totals.
	Counts *StepCounts
	// SubjectLabel, SessionLabel and Storage are required with Identify
	// and ignored otherwise; the storage must be the tenant's.
	SubjectLabel, SessionLabel, Storage string
	// Route is what the routing chose, kept with Identify and ignored
	// otherwise; the zero value chose nothing of its own. A storage a
	// release named (ReceptionRoute.ReleasedStorage) is kept whatever the
	// step gives.
	Route ReceptionRoute
	// Failure is required with Fail, a phrase for a person of at most
	// MaxFailureLength bytes, and ignored otherwise.
	Failure string
	// Fence, when not nil, is the lease of the worker taking the step: the
	// step is made only while that lease holds (ErrLeaseLost otherwise).
	Fence *WorkFence
}

// Validate returns an Invalid error unless the step is well formed. Whether
// it is allowed is the core's to say (reception.Step), in the store.
func (s ReceptionStep) Validate() error {
	if err := master.ValidateID("reception id", s.ID); err != nil {
		return err
	}
	if err := master.ValidateTime("step time", s.At); err != nil {
		return err
	}
	if s.Event == reception.File {
		return invalid("files are recorded with AddReceivedFiles, not as a step")
	}
	if !s.LastFile.IsZero() {
		if err := master.ValidateTime("last file time", s.LastFile); err != nil {
			return err
		}
	}
	if c := s.Counts; c != nil && (c.FilesDuplicate < 0 || c.BytesDuplicate < 0) {
		return invalid("a reception's counts are not negative")
	}
	switch s.Event {
	case reception.Identify:
		if err := (label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: "x"}).Check(); err != nil {
			return fault.Wrap(fault.Invalid, err, "an identified reception has valid subject and session labels: "+err.Error())
		}
		if err := ValidateName("storage", s.Storage); err != nil {
			return err
		}
		if err := s.Route.Validate(); err != nil {
			return err
		}
	case reception.Fail:
		if err := validateText("failure", s.Failure, 1, MaxFailureLength); err != nil {
			return err
		}
	}
	if s.Fence != nil {
		return s.Fence.Validate()
	}
	return nil
}

// ReceptionFilter narrows ListReceptions. A zero field keeps every
// reception; the fields set must all hold.
type ReceptionFilter struct {
	// Receiver keeps the receptions of the receiver of this name.
	Receiver string
	// StudyUID keeps the receptions of this study, as received.
	StudyUID string
	// States keeps the receptions in one of these states, at most
	// MaxStepFilter, each one the core knows.
	States []reception.State
	// InProgress keeps the receptions that have not ended, whatever their
	// state: the store keeps whether one ended, as the core says
	// (reception.State.Ended), so a state added later is counted right.
	InProgress bool
	// Since keeps the receptions that started at or after it.
	Since time.Time
	// Until keeps the receptions that started before it.
	Until time.Time
}

// Validate returns an Invalid error when f cannot be applied.
func (f ReceptionFilter) Validate() error {
	if f.Receiver != "" {
		if err := ValidateName("receiver", f.Receiver); err != nil {
			return err
		}
	}
	if f.StudyUID != "" {
		if err := validateUID("Study Instance UID", f.StudyUID); err != nil {
			return err
		}
	}
	if len(f.States) > MaxStepFilter {
		return invalid("a filter names at most " + strconv.Itoa(MaxStepFilter) + " steps")
	}
	for _, s := range f.States {
		if !knownState(s) {
			return invalid("a filter's step is one of " + statesText())
		}
	}
	return validateRange(f.Since, f.Until)
}

// NarrowStates returns the states a filter keeps, each once, in the order
// it names them, with the rule of InProgress applied: with InProgress set,
// a state that is an end keeps nothing and is left out. none is true when
// the filter can keep no reception at all (InProgress with only ended
// states named), so a store answers an empty page without reading. An empty
// list with none false keeps every state (InProgress then keeps those that
// have not ended).
func (f ReceptionFilter) NarrowStates() (states []reception.State, none bool) {
	for _, st := range f.States {
		if !slices.Contains(states, st) && (!f.InProgress || !st.Ended()) {
			states = append(states, st)
		}
	}
	return states, f.InProgress && len(f.States) > 0 && len(states) == 0
}

// AddCallingAETitle adds a calling AE title to a reception's list when it
// is not in it, keeping at most MaxCallingAETitles in the order they first
// appeared and saying when there were more. An empty title adds nothing.
// It returns a new slice, never changing the one given, so a change that
// runs again starts from what it read.
func AddCallingAETitle(titles []string, truncated bool, title string) ([]string, bool) {
	if title == "" || slices.Contains(titles, title) {
		return titles, truncated
	}
	if len(titles) >= MaxCallingAETitles {
		return titles, true
	}
	return append(slices.Clip(titles), title), truncated
}

// validateRange checks a filter's optional time range.
func validateRange(since, until time.Time) error {
	for _, t := range []struct {
		name string
		t    time.Time
	}{{"start time", since}, {"end time", until}} {
		if !t.t.IsZero() {
			if err := master.ValidateTime(t.name, t.t); err != nil {
				return err
			}
		}
	}
	if !since.IsZero() && !until.IsZero() && !until.After(since) {
		return invalid("an end time is after the start time")
	}
	return nil
}

// knownState reports whether s is a state the core knows.
func knownState(s reception.State) bool {
	for _, k := range reception.States() {
		if k == s {
			return true
		}
	}
	return false
}

// statesText lists the core's states for a message.
func statesText() string {
	out := ""
	for i, s := range reception.States() {
		if i > 0 {
			out += ", "
		}
		out += string(s)
	}
	return out
}

// ValidateOpen returns an Invalid error unless the arguments of
// OpenReception are well formed.
func ValidateOpen(receiver, studyUID, id string, at time.Time) error {
	if err := ValidateName("receiver", receiver); err != nil {
		return err
	}
	if err := validateUID("Study Instance UID", studyUID); err != nil {
		return err
	}
	if err := master.ValidateID("reception id", id); err != nil {
		return err
	}
	return master.ValidateTime("start time", at)
}

// ValidateDiscardReason returns an Invalid error unless reason is a reason a
// person may give to discard a reception or reject a file: 1 to 1024 bytes of
// UTF-8, not only spaces.
func ValidateDiscardReason(reason string) error { return validateReason("reason", reason) }
