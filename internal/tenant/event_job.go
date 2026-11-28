package tenant

import (
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// EventJobCounts are a job's counts as an event carries them, the
// contract's JobCounts with the remaining ones written out, so a reader
// needs no rule to read them.
type EventJobCounts struct {
	Instances          int64 `json:"instances"`
	Bytes              int64 `json:"bytes"`
	InstancesSent      int64 `json:"instances_sent"`
	BytesSent          int64 `json:"bytes_sent"`
	InstancesWarned    int64 `json:"instances_warned"`
	BytesWarned        int64 `json:"bytes_warned"`
	InstancesRefused   int64 `json:"instances_refused"`
	BytesRefused       int64 `json:"bytes_refused"`
	InstancesRemaining int64 `json:"instances_remaining"`
	BytesRemaining     int64 `json:"bytes_remaining"`
	Scans              int64 `json:"scans"`
	ScansDone          int64 `json:"scans_done"`
	Listed             bool  `json:"listed"`
}

// eventJobCounts returns c as an event carries it.
func eventJobCounts(c JobCounts) EventJobCounts {
	return EventJobCounts{Instances: c.Instances, Bytes: c.Bytes, InstancesSent: c.InstancesSent, BytesSent: c.BytesSent,
		InstancesWarned: c.InstancesWarned, BytesWarned: c.BytesWarned, InstancesRefused: c.InstancesRefused,
		BytesRefused: c.BytesRefused, InstancesRemaining: c.InstancesRemaining(), BytesRemaining: c.BytesRemaining(),
		Scans: c.Scans, ScansDone: c.ScansDone, Listed: c.Listed}
}

// validate returns an Invalid error unless every count is one a job has.
func (c EventJobCounts) validate() error {
	for _, n := range []int64{c.Instances, c.Bytes, c.InstancesSent, c.BytesSent, c.InstancesWarned, c.BytesWarned,
		c.InstancesRefused, c.BytesRefused, c.InstancesRemaining, c.BytesRemaining, c.Scans, c.ScansDone} {
		if n < 0 {
			return invalid("a job's counts are not negative")
		}
	}
	if c.Instances != c.InstancesSent+c.InstancesWarned+c.InstancesRefused+c.InstancesRemaining ||
		c.Bytes != c.BytesSent+c.BytesWarned+c.BytesRefused+c.BytesRemaining || c.ScansDone > c.Scans {
		return invalid("a job's instances are each sent, warned, refused or remaining")
	}
	return nil
}

// JobStepReached is a job made or reaching a state, the contract's message
// of that name.
type JobStepReached struct {
	// Job is the job's id.
	Job string `json:"job"`
	// State is the state it reached: queued for a job just made.
	State JobState `json:"state"`
	// PreviousState is the state it left; empty for a job just made.
	PreviousState JobState `json:"previous_state"`
	// Counts are its counts when it reached the state.
	Counts EventJobCounts `json:"counts"`
	// Sender names a send job's sender; empty for a deid job.
	Sender string `json:"sender"`
	// Receiver and Reception name the receiver and reception of a
	// forward; empty otherwise.
	Receiver  string `json:"receiver"`
	Reception string `json:"reception"`
	// RetryOf is the ended job it sends again; empty unless RetryJob made
	// it.
	RetryOf string `json:"retry_of"`
	// Reason is why it ended as it did, as Job.Reason.
	Reason string `json:"reason"`
}

// EventType implements EventPayload.
func (*JobStepReached) EventType() EventType { return EventJobStep }

func (p *JobStepReached) validate() error {
	if err := master.ValidateID("job id", p.Job); err != nil {
		return err
	}
	if !p.State.Valid() || (p.PreviousState != "" && !p.PreviousState.Valid()) {
		return invalid("a job's state is one of queued, running, done, partial, failed, canceled")
	}
	if p.Sender != "" {
		if err := ValidateSenderName(p.Sender); err != nil {
			return err
		}
	}
	if p.Receiver != "" {
		if err := ValidateName("receiver", p.Receiver); err != nil {
			return err
		}
	}
	for _, id := range []string{p.Reception, p.RetryOf} {
		if id != "" {
			if err := master.ValidateID("id", id); err != nil {
				return err
			}
		}
	}
	if err := validateText("job reason", p.Reason, 0, MaxFailureLength); err != nil {
		return err
	}
	return p.Counts.validate()
}

// JobStepEvent returns the event of job j as it is after reaching its state
// from previous (empty for a job just made), at the time given.
func JobStepEvent(previous JobState, j Job, at time.Time) (EventRecord, error) {
	if err := eventSender(j); err != nil {
		return EventRecord{}, err
	}
	return NewEvent(&JobStepReached{Job: j.ID, State: j.State, PreviousState: previous, Counts: eventJobCounts(j.Counts),
		Sender: j.Sender, Receiver: j.Receiver, Reception: j.ReceptionID, RetryOf: j.RetryOf, Reason: j.Reason}, at)
}

// JobProgressed is a running job's counts, the contract's message of that
// name.
type JobProgressed struct {
	// Job is the job's id.
	Job string `json:"job"`
	// Counts are its counts now.
	Counts EventJobCounts `json:"counts"`
	// Sender names a send job's sender; empty for a deid job.
	Sender string `json:"sender"`
}

// EventType implements EventPayload.
func (*JobProgressed) EventType() EventType { return EventJobProgress }

func (p *JobProgressed) validate() error {
	if err := master.ValidateID("job id", p.Job); err != nil {
		return err
	}
	if p.Sender != "" {
		if err := ValidateSenderName(p.Sender); err != nil {
			return err
		}
	}
	return p.Counts.validate()
}

// JobProgressEvent returns the event of job j's counts now, at the time
// given.
func JobProgressEvent(j Job, at time.Time) (EventRecord, error) {
	if err := eventSender(j); err != nil {
		return EventRecord{}, err
	}
	return NewEvent(&JobProgressed{Job: j.ID, Counts: eventJobCounts(j.Counts), Sender: j.Sender}, at)
}

// eventSender returns an Invalid error unless j names a sender as its kind
// says: a send job its sender, a deid or an import job none. A payload
// alone cannot tell, since the contract's events carry no kind.
func eventSender(j Job) error {
	noSender := j.Kind == JobDeid || j.Kind == JobImport || j.Kind == JobScrub || j.Kind == JobRebuild || j.Kind == JobReplicate ||
		j.Kind == JobRepair || j.Kind == JobDispose || j.Kind == JobDeleteBatch || j.Kind == JobTransfer || j.Kind == JobSearchBackfill || j.Kind == JobDeidCoverage
	switch {
	case noSender && j.Sender != "":
		return invalid("only a send job's event names a sender")
	case !noSender && j.Sender == "":
		return invalid("a send job's event names its sender")
	}
	return nil
}

// storedTime returns t as a store keeps it: in UTC, to the microsecond; the
// zero time stays zero.
func storedTime(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.UTC().Truncate(time.Microsecond)
}

// validateTimes returns an Invalid error unless since is a time a store
// keeps and each other time is zero or one.
func validateTimes(since time.Time, others ...time.Time) error {
	if err := master.ValidateTime("health time", since); err != nil {
		return err
	}
	for _, t := range others {
		if !t.IsZero() {
			if err := master.ValidateTime("health time", t); err != nil {
				return err
			}
		}
	}
	return nil
}
