package tenant

import (
	"errors"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
)

// What a correction owns, what its purge does with each object, and what a
// delete holds arrivals by.

// ScanSnapshot is what was read of a scan's manifest and metadata: the
// SHA-256 and size of each, zero for one not there.
type ScanSnapshot struct {
	Manifest, Metadata         SHA256
	ManifestSize, MetadataSize int64
}

// The order of a correction's objects, the order a purge removes them in:
// the instances, then each scan's metadata, then its manifest, which says
// what the scan held.
const (
	TargetInstance = 1
	TargetMetadata = 2
	TargetManifest = 3
)

// PurgeTarget is one object a correction owns.
type PurgeTarget struct {
	// Ord is TargetInstance, TargetMetadata or TargetManifest.
	Ord int
	// Storage and Path place it; UID is an instance's SOP Instance UID and
	// StorageKey the key the catalog held whole ("" for the layout's).
	Storage    string
	Path       label.Path
	UID        string
	StorageKey string
	// Size and SHA256 are what the catalog or the snapshot held of it.
	Size   int64
	SHA256 SHA256
	// Shared says a live row of the catalog resolves to it now: an
	// instance's object, or a live scan's manifest or metadata.
	Shared bool
}

// Key returns the target's key on its storage.
func (t PurgeTarget) Key() (string, error) {
	switch t.Ord {
	case TargetInstance:
		if t.StorageKey != "" {
			return t.StorageKey, nil
		}
		return layout.InstanceKey(t.Path, t.UID)
	case TargetMetadata:
		return layout.MetadataKey(t.Path)
	}
	return layout.ManifestKey(t.Path)
}

// Validate returns an Invalid error unless t can be recorded.
func (t PurgeTarget) Validate() error {
	if t.Ord < TargetInstance || t.Ord > TargetManifest {
		return invalid("a purge target is an instance, a metadata or a manifest")
	}
	if err := ValidateName("storage", t.Storage); err != nil {
		return err
	}
	if t.Size < 0 {
		return invalid("an object's size is not negative")
	}
	if (t.Ord == TargetInstance) != (t.UID != "") || t.Ord != TargetInstance && t.StorageKey != "" {
		return invalid("an instance's target names its UID, and only an instance's")
	}
	if t.Ord == TargetInstance {
		if err := ValidateInstanceUID(t.UID); err != nil {
			return err
		}
	}
	if _, err := t.Key(); err != nil {
		return invalid("a purge target names no key of the layout")
	}
	return nil
}

// PurgeOutcome is what became of a target.
type PurgeOutcome string

// The outcomes.
const (
	// OutcomePurged: the storage was asked to remove it, and said what it
	// did (PurgedObject).
	OutcomePurged PurgeOutcome = "purged"
	// OutcomeKeptShared: a live row resolves to it; it stays.
	OutcomeKeptShared PurgeOutcome = "kept_shared"
	// OutcomeKeptChanged: its bytes are no longer the ones the correction
	// read; it stays.
	OutcomeKeptChanged PurgeOutcome = "kept_changed"
)

// Valid reports whether o is one of the outcomes.
func (o PurgeOutcome) Valid() bool {
	return o == OutcomePurged || o == OutcomeKeptShared || o == OutcomeKeptChanged
}

// PurgeSettled is what became of one target: its outcome and, when it was
// purged, what its storage did with each version (each PurgedObject's ID
// is the correction's).
type PurgeSettled struct {
	Target   PurgeTarget
	Outcome  PurgeOutcome
	Removals []PurgedObject
}

// Validate returns an Invalid error unless s can be recorded for the
// correction id.
func (s PurgeSettled) Validate(id string) error {
	if !s.Outcome.Valid() {
		return invalid("a purge's outcome is purged, kept_shared or kept_changed")
	}
	if s.Outcome != OutcomePurged && len(s.Removals) > 0 {
		return invalid("only a purged object has removals")
	}
	for _, o := range s.Removals {
		if o.ID != id {
			return invalid("a removal names its correction")
		}
		if err := o.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// HeldValue is one value a delete holds arrivals by: Kind is subject (a
// label), study, series or instance (a UID).
type HeldValue struct {
	Kind, Value string
}

// The kinds of a HeldValue.
const (
	HeldSubject  = "subject"
	HeldStudy    = "study"
	HeldSeries   = "series"
	HeldInstance = "instance"
)

// Validate returns an Invalid error unless v names a value of its kind.
func (v HeldValue) Validate() error {
	switch v.Kind {
	case HeldSubject:
		if !label.Valid(v.Value) {
			return invalid("a held subject label passes the label rule")
		}
		return nil
	case HeldStudy, HeldSeries, HeldInstance:
		return validateUID("held UID", v.Value)
	}
	return invalid("a held value is a subject, a study, a series or an instance")
}

// CorrectionHold is a correction not ended that holds a path for
// arrivals, and at which level (subject, session, scan or instance).
type CorrectionHold struct {
	Correction, Level string
}

// FaultHeldByCorrection is the fault of a label conflict whose label or
// UID a correction not ended holds; the quarantine record names it.
const FaultHeldByCorrection = "held_by_correction"

// ErrClock is in the chain of the precondition failure of a purge the
// server's clock does not allow (Corrections.CheckPurgeClock).
var ErrClock = errors.New("the server's clock does not allow a purge now")

// ClockError returns the precondition failure of a purge the clock does not
// allow, saying why.
func ClockError(why string) error {
	return fault.Wrap(fault.Precondition, ErrClock, why)
}

// HeldError returns the Conflict of an arrival whose labels or UIDs the
// correction h holds, as a label conflict naming the correction.
func HeldError(h CorrectionHold) error {
	return fault.Wrap(fault.Conflict, &LabelConflict{Label: h.Level, Fault: FaultHeldByCorrection, Correction: h.Correction},
		"the "+h.Level+" is held by a correction not ended ("+FaultHeldByCorrection+")")
}
