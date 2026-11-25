package tenant

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
)

// Holds is what the tenant's database keeps of holds: marks, each with a
// person's reason, on a subject, a session, a scan or the whole tenant, that
// stop every delete, purge and disposal of what they cover until a person
// releases them. A hold has no expiry.
//
// # What a hold stops
//
// While an active hold covers any of a delete's target, the delete's
// prepare is refused (a precondition failure with HoldRefusal in the
// chain); a delete committed before the hold stays committed and may be
// undone; its purge is refused before its next page of objects
// (Corrections.PurgeTargets answers the same refusal), so no object of
// held data is removed; and a disposal skips the session. A replace or a
// relabel is allowed: nothing is lost. A relabel's publishing transaction
// rewrites the labels of the holds whose target it moves, so a hold follows
// what it named, not a label.
//
// # Hold records
//
// Each hold has a record, meta/.holds/<id>.json, on every storage the
// target touches and on each of their replicas, so a hold outlives a lost
// database. The records are written through the storage port by the
// caller; the store keeps which storages a hold must be on and which have
// its record as it stands now (Hold.Storages, Hold.StoragesPending): a hold
// counts its changes (Seq), and a record is owed until it is written at the
// hold's count. A hold is placed only once its record is on every primary
// it touches (HoldPlace.Written); a replica that did not answer is owed it.
// A target that spreads after the hold is placed owes its record to where
// it spread, in the transaction that spread it: a replica attached to a
// primary the target has data on (Replication.AttachReplica), and a commit
// that lands a new scan of a held target on a storage the hold is not on,
// with that storage's replicas (Pipeline.CommitScan).
// A record found active on a storage with no row (AdoptHold) becomes an
// active hold, since keeping data longer is the lesser error.
type Holds interface {
	// HoldStorages answers the storages, by name, a hold of target must be
	// on: those of the scans it covers (in the catalog, or hidden by a
	// delete not yet purged), and their replicas; every storage for the
	// whole tenant. A target the tenant holds nothing of is NotFound.
	HoldStorages(ctx context.Context, target HoldTarget) ([]HoldStorage, error)
	// PlaceHold commits an active hold (p.Validate) with its audit entry,
	// and answers it and true; or answers the active hold with the same
	// target and reason, and false, placing nothing. A primary the target
	// touches that is not in p.Written is a precondition failure: the
	// record goes on every primary before the row commits.
	PlaceHold(ctx context.Context, p HoldPlace, audit master.AuditEntry, guard Guard) (Hold, bool, error)
	// MatchingHold answers the active hold with the same target and
	// reason, and false when there is none: a caller asks it before it
	// writes the records of a new one.
	MatchingHold(ctx context.Context, target HoldTarget, reason string) (Hold, bool, error)
	// ReleaseHold releases the hold (its records owed again, as released),
	// with its audit entry; one released already is answered as it stands.
	ReleaseHold(ctx context.Context, r HoldRelease, audit master.AuditEntry, guard Guard) (Hold, error)
	// Hold answers the hold of that id, or NotFound.
	Hold(ctx context.Context, id string) (Hold, error)
	// ListHolds answers a page of the holds the filter keeps, newest
	// first.
	ListHolds(ctx context.Context, f HoldFilter, page master.Page) ([]Hold, string, error)
	// HoldRecordsOwed answers, oldest hold first, at most limit (1 to
	// MaxHoldList) records owed: a storage that has no record of a hold
	// as it stands.
	HoldRecordsOwed(ctx context.Context, limit int) ([]HoldRecordOwed, error)
	// MarkHoldRecord records the hold's record written on the storage at
	// the hold's count seq. A count older than the one recorded changes
	// nothing; a storage the hold is not on is NotFound.
	MarkHoldRecord(ctx context.Context, id, storage string, seq int64) error
	// AdoptHold makes an active hold of a record found active on a
	// storage with no row (found_on_storage), with its audit entry, and
	// answers it and true; a hold that has a row is answered as it stands,
	// and false.
	AdoptHold(ctx context.Context, f HoldFound, audit master.AuditEntry) (Hold, bool, error)
	// CorrectionHeld answers the id of an active hold that covers any scan
	// of the correction, by its labels before the correction or those a
	// relabel gives it, or "" when none does.
	CorrectionHeld(ctx context.Context, id string) (string, error)
}

// The bounds of holds.
const (
	// MaxHoldReason bounds a hold's reason, the contract's.
	MaxHoldReason = 1024
	// MaxHoldList is the most records one HoldRecordsOwed answers.
	MaxHoldList = 1000
	// MaxHoldStates bounds a filter's states.
	MaxHoldStates = 16
)

// HoldState is whether a hold holds: active to released only.
type HoldState string

// The states.
const (
	HoldActive   HoldState = "active"
	HoldReleased HoldState = "released"
)

// HoldTarget names what a hold covers, by the labels the catalog has now.
type HoldTarget struct {
	// Subject, Session and Scan are labels; empty for the levels below the
	// one named.
	Subject, Session, Scan string
	// WholeTenant covers every subject, now and later; the labels are then
	// empty.
	WholeTenant bool
}

// Level returns the level the target names: "tenant", "subject",
// "session" or "scan".
func (t HoldTarget) Level() string {
	switch {
	case t.WholeTenant:
		return "tenant"
	case t.Scan != "":
		return "scan"
	case t.Session != "":
		return "session"
	}
	return "subject"
}

// Validate returns an Invalid error unless t names the whole tenant, or a
// subject, a session of it or a scan of that.
func (t HoldTarget) Validate() error {
	if t.WholeTenant {
		if t.Subject != "" || t.Session != "" || t.Scan != "" {
			return invalid("a hold of the whole tenant names no label")
		}
		return nil
	}
	if t.Subject == "" {
		return invalid("a hold names a subject, or the whole tenant")
	}
	if t.Scan != "" && t.Session == "" {
		return invalid("a hold names each level above the one it names")
	}
	for _, l := range []string{t.Subject, t.Session, t.Scan} {
		if l != "" && !label.Valid(l) {
			return invalid("a hold's labels pass the label rule")
		}
	}
	return nil
}

// Same reports whether t and o name the same target, the labels compared
// case folded as the catalog keeps them unique.
func (t HoldTarget) Same(o HoldTarget) bool {
	return t.WholeTenant == o.WholeTenant && label.Fold(t.Subject) == label.Fold(o.Subject) &&
		label.Fold(t.Session) == label.Fold(o.Session) && label.Fold(t.Scan) == label.Fold(o.Scan)
}

// Covers reports whether a hold of t covers any of what p names (a
// correction's target, a session): the whole tenant, or the two paths
// agree at every level both name.
func (t HoldTarget) Covers(p label.Path) bool {
	if t.WholeTenant {
		return true
	}
	eq := func(a, b string) bool { return a == "" || b == "" || label.Fold(a) == label.Fold(b) }
	return label.Fold(t.Subject) == label.Fold(p.Subject) && eq(t.Session, p.Session) && (t.Session == "" || p.Session == "" || eq(t.Scan, p.Scan))
}

// Hold is one hold and where its records stand.
type Hold struct {
	// ID is its id, made by the caller in the form of a correction's.
	ID string
	// Target is what it covers, by the labels the catalog has now.
	Target HoldTarget
	// Reason is the person's words; answered only to an operator or more.
	Reason string
	// State is whether it holds.
	State HoldState
	// PlacedBy and ReleasedBy are the keys, empty for the local operator;
	// ReleaseReason is the person's.
	PlacedBy, ReleasedBy, ReleaseReason string
	// Placed and Released are its times; Released zero while active.
	Placed, Released time.Time
	// FoundOnStorage says it was made from a record found with no row.
	FoundOnStorage bool
	// Seq counts its changes: placed, relabeled, released.
	Seq int64
	// Storages are the storages its record is on, by name; StoragesPending
	// those still owed its record as it stands.
	Storages, StoragesPending []string
}

// HoldStorage is one storage a hold's record goes on.
type HoldStorage struct {
	// Name is the storage's; Replica says it is a replica, which may be
	// owed the record.
	Name    string
	Replica bool
}

// HoldPlace is a hold to place.
type HoldPlace struct {
	// ID is its id (ValidateHoldID).
	ID string
	// Target is what it covers.
	Target HoldTarget
	// Reason is the person's (1 to 1024 bytes, no control character).
	Reason string
	// KeyID is the key placing it, empty for the local operator.
	KeyID string
	// Written are the storages its record was put on before the call:
	// every primary HoldStorages answered, and the replicas that
	// answered.
	Written []string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless p can be placed.
func (p HoldPlace) Validate() error {
	if err := ValidateHoldID(p.ID); err != nil {
		return err
	}
	if err := p.Target.Validate(); err != nil {
		return err
	}
	if err := validateCorrectionReason("hold's reason", p.Reason); err != nil {
		return err
	}
	if p.KeyID != "" {
		if err := master.ValidateID("key id", p.KeyID); err != nil {
			return err
		}
	}
	for _, s := range p.Written {
		if err := ValidateName("storage", s); err != nil {
			return err
		}
	}
	return master.ValidateTime("hold time", p.Now)
}

// HoldRelease is a hold to release.
type HoldRelease struct {
	// ID names it; Reason is the person's; KeyID the key releasing it.
	ID, Reason, KeyID string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless r is well formed.
func (r HoldRelease) Validate() error {
	if err := ValidateHoldID(r.ID); err != nil {
		return err
	}
	if err := validateCorrectionReason("release's reason", r.Reason); err != nil {
		return err
	}
	if r.KeyID != "" {
		if err := master.ValidateID("key id", r.KeyID); err != nil {
			return err
		}
	}
	return master.ValidateTime("release time", r.Now)
}

// HoldFilter keeps the holds a list answers: every filter given must hold.
type HoldFilter struct {
	// States keeps those in these states; empty for all.
	States []HoldState
	// Subject keeps the holds of this subject and those of the whole
	// tenant, which cover it too; empty for all.
	Subject string
}

// Validate returns an Invalid error unless f is well formed.
func (f HoldFilter) Validate() error {
	if len(f.States) > MaxHoldStates {
		return invalid("a filter names at most " + strconv.Itoa(MaxHoldStates) + " states")
	}
	for _, s := range f.States {
		if s != HoldActive && s != HoldReleased {
			return invalid("a hold's state is active or released")
		}
	}
	if f.Subject != "" && !label.Valid(f.Subject) {
		return invalid("a hold filter's subject label passes the label rule")
	}
	return nil
}

// HoldRecordOwed is one record a storage is owed.
type HoldRecordOwed struct {
	// Hold is the hold as it stands; Storage the storage owed its record.
	Hold    Hold
	Storage string
}

// HoldFound is a hold record found active on a storage.
type HoldFound struct {
	// ID, Target, Reason, PlacedBy and Placed are what the record kept.
	ID       string
	Target   HoldTarget
	Reason   string
	PlacedBy string
	Placed   time.Time
	// Storage is the storage it was found on.
	Storage string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless f can be adopted.
func (f HoldFound) Validate() error {
	if err := ValidateHoldID(f.ID); err != nil {
		return err
	}
	if err := f.Target.Validate(); err != nil {
		return err
	}
	if err := validateText("hold's reason", f.Reason, 1, MaxHoldReason); err != nil {
		return err
	}
	if f.PlacedBy != "" {
		if err := master.ValidateID("key id", f.PlacedBy); err != nil {
			return err
		}
	}
	if err := ValidateName("storage", f.Storage); err != nil {
		return err
	}
	if err := master.ValidateTime("placed time", f.Placed); err != nil {
		return err
	}
	return master.ValidateTime("time", f.Now)
}

// ValidateHoldID returns an Invalid error unless id is a hold's id: an
// operation id, as a correction's.
func ValidateHoldID(id string) error {
	if err := layout.ValidOperationID(id); err != nil {
		return invalid("a hold's id is a ULID in lowercase: 26 of 0-9 and a-z but i, l, o and u")
	}
	return nil
}

// CheckHoldWritten returns a precondition failure unless every primary of
// storages is in written: a hold commits only once its record is on every
// primary it touches.
func CheckHoldWritten(storages []HoldStorage, written []string) error {
	for _, s := range storages {
		if !s.Replica && !slices.Contains(written, s.Name) {
			return Precondition("the hold's record is not on the storage " + s.Name + "; it is put on every primary before the hold is placed")
		}
	}
	for _, w := range written {
		if !slices.ContainsFunc(storages, func(s HoldStorage) bool { return s.Name == w }) {
			return Precondition("the hold's target does not touch the storage " + w)
		}
	}
	return nil
}

// JobReplicate, JobRepair and JobDispose are the kinds of a replica's
// copy, a repair from a replica, and a disposal of due sessions.
const (
	JobReplicate JobKind = "replicate"
	JobRepair    JobKind = "repair"
	JobDispose   JobKind = "dispose"
)
