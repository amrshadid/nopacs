package tenant

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// ProfileTrial is the record of a new profile version tried over a
// receiver's last receptions before the receiver is pointed at it (change
// control): when, by which key, and how many sessions and files it read,
// how many of those files the version would change and how many it would
// hold. It holds counts only, never a value.
type ProfileTrial struct {
	// Receiver is the receiver tried for; Profile the version tried.
	Receiver string
	Profile  ProfileRef
	// Tried is when; KeyID the key that asked, "" for the local operator.
	Tried time.Time
	KeyID string
	// Sessions, Files, Changed and Held are what the trial counted.
	Sessions, Files, Changed, Held int64
}

// ProfileTrials is what the tenant's database keeps of change control: the
// latest trial of each receiver and profile version. A receiver is pointed
// at a version only when it was tried and none of its files was held, or
// when a person forces it with a reason, which is audited.
type ProfileTrials interface {
	// RecordProfileTrial stores the trial, replacing an earlier one of the
	// same receiver and version, with the audit entry written in the same
	// transaction. The version must exist (NotFound otherwise).
	RecordProfileTrial(ctx context.Context, t ProfileTrial, audit master.AuditEntry, guard Guard) error
	// ProfileTrial returns the latest trial of the receiver and version,
	// or NotFound when it was never tried.
	ProfileTrial(ctx context.Context, receiver string, ref ProfileRef) (ProfileTrial, error)
}

// Validate returns an Invalid error unless t can be recorded.
func (t ProfileTrial) Validate() error {
	if err := ValidateName("receiver", t.Receiver); err != nil {
		return err
	}
	if t.Profile.IsZero() {
		return invalid("a trial names the profile version tried")
	}
	if err := t.Profile.Validate(); err != nil {
		return err
	}
	if t.Sessions < 0 || t.Files < 0 || t.Changed < 0 || t.Held < 0 || t.Changed+t.Held > t.Files {
		return invalid("a trial's counts are not negative and its changed and held files are among its files")
	}
	return master.ValidateTime("trial time", t.Tried)
}
