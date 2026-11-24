package tenant

import (
	"bytes"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// ReceivedFile is a file that has landed in the incoming area, synced, before
// its sender was answered.
type ReceivedFile struct {
	// ReceptionID names the reception it belongs to.
	ReceptionID string
	// SOPInstanceUID, SeriesUID and StudyUID are its UIDs as received.
	SOPInstanceUID, SeriesUID, StudyUID string
	// SOPClassUID and TransferSyntaxUID are its class and transfer syntax.
	SOPClassUID, TransferSyntaxUID string
	// Size is its size in bytes as written to the incoming area.
	Size int64
	// SHA256 is the checksum of the file as written: the received checksum.
	SHA256 SHA256
	// Incoming is where it is in the incoming area, relative to it.
	Incoming string
	// Caller is who sent it.
	Caller Caller
	// Association is the receiver's id of the association (or request)
	// that brought it, empty for a file no association brought (a release).
	Association string
	// Released says it came from a quarantine release.
	Released bool
	// ReleasedStorage, set only on a released file, is the storage its
	// release named (Release.Storage): the reception it joins keeps it
	// (ReceptionRoute.ReleasedStorage) unless that reception keeps one
	// already. It is not kept with the file.
	ReleasedStorage string
	// ReleasedProfile, set only on a released file, is the profile version
	// its release named (Release.Profile), kept by the reception it joins
	// as ReleasedStorage is (ReceptionRoute.ReleasedProfile). It is not
	// kept with the file.
	ReleasedProfile ProfileRef
	// Deidentified says it was recorded at a receiver that de-identifies:
	// its placing UIDs are the keyed remapping of the file's and its
	// checksum is keyed, and the archive step de-identifies it. False, it
	// was recorded as received and is archived as received; at a receiver
	// that de-identifies now it is held instead.
	Deidentified bool
	// Import, when not nil, is the import unit that handed the file in and
	// its path (Importing): the store writes the arrival, the unit's
	// cursor and its counts with the file's row. Written only; a read
	// answers nil.
	Import *ImportArrival
	// Received is when it was recorded. Answered by a store.
	Received time.Time
}

// Validate returns an Invalid error unless f can be recorded. Its UIDs are
// checked by the core's rule, and the study UID must be its reception's,
// which the store checks.
func (f ReceivedFile) Validate() error {
	if err := master.ValidateID("reception id", f.ReceptionID); err != nil {
		return err
	}
	for _, u := range []struct{ what, v string }{
		{"SOP Instance UID", f.SOPInstanceUID}, {"Series Instance UID", f.SeriesUID},
		{"Study Instance UID", f.StudyUID}, {"SOP Class UID", f.SOPClassUID},
		{"Transfer Syntax UID", f.TransferSyntaxUID},
	} {
		if err := validateUID(u.what, u.v); err != nil {
			return err
		}
	}
	if f.Size < 0 {
		return invalid("a file's size is not negative")
	}
	if f.SHA256.IsZero() {
		return invalid("a received file has its checksum")
	}
	if err := validateIncoming(f.Incoming); err != nil {
		return err
	}
	if err := f.Caller.Validate(); err != nil {
		return err
	}
	if f.ReleasedStorage != "" {
		if !f.Released {
			return invalid("only a released file names the storage its release named")
		}
		if err := ValidateName("storage", f.ReleasedStorage); err != nil {
			return err
		}
	}
	if !f.ReleasedProfile.IsZero() {
		if !f.Released {
			return invalid("only a released file names the profile version its release named")
		}
		if err := f.ReleasedProfile.Validate(); err != nil {
			return err
		}
	}
	if f.Import != nil {
		if f.Released || f.Association != "" {
			return invalid("a file an import hands in came by no association and no release")
		}
		if err := f.Import.Validate(); err != nil {
			return err
		}
	}
	if f.Association != "" {
		return master.ValidateID("association id", f.Association)
	}
	return nil
}

// ValidateReceivedBatch returns an Invalid error unless the batch is within
// MaxReceivedBatch and every file is valid.
func ValidateReceivedBatch(files []ReceivedFile, at time.Time) error {
	if len(files) > MaxReceivedBatch {
		return fault.New(fault.Invalid, "at most 1000 received files are recorded at once")
	}
	for _, f := range files {
		if err := f.Validate(); err != nil {
			return err
		}
	}
	return master.ValidateTime("receive time", at)
}

// DuplicateQuery is one file whose SOP Instance UID is looked up among what
// the tenant holds.
type DuplicateQuery struct {
	// SOPInstanceUID, StudyUID and SeriesUID are the file's, as received.
	SOPInstanceUID, StudyUID, SeriesUID string
	// SHA256 is its received checksum.
	SHA256 SHA256
}

// Validate returns an Invalid error unless the query is well formed.
func (q DuplicateQuery) Validate() error {
	for _, u := range []struct{ what, v string }{
		{"SOP Instance UID", q.SOPInstanceUID}, {"Study Instance UID", q.StudyUID}, {"Series Instance UID", q.SeriesUID},
	} {
		if err := validateUID(u.what, u.v); err != nil {
			return err
		}
	}
	if q.SHA256.IsZero() {
		return invalid("a duplicate check is given the file's checksum")
	}
	return nil
}

// ValidateDuplicateBatch returns an Invalid error unless the batch is within
// MaxDuplicateBatch and every query is valid.
func ValidateDuplicateBatch(files []DuplicateQuery) error {
	if len(files) > MaxDuplicateBatch {
		return invalid("at most 1000 files are checked at once")
	}
	for _, q := range files {
		if err := q.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// DuplicateKind is how a file compares with the instance the tenant holds,
// or a reception claims, under its SOP Instance UID: what CheckDuplicates and
// ClaimInstances answer for each file.
type DuplicateKind int

// The kinds, each with what the archive step does with the file.
const (
	// NotHeld is a SOP Instance UID no instance row has: nothing holds or
	// claims it (CheckDuplicates only). The file goes on to its claim.
	NotHeld DuplicateKind = iota
	// SameFile is the UID held by another reception, in the same study and
	// series, with the same checksum. The file is ignored and counted
	// duplicate.
	SameFile
	// DifferentFile is the UID held, in the same study and series, with
	// another checksum; or claimed by this same reception with another
	// checksum, when one reception received two files under one UID (the
	// one claimed first is kept). The file is quarantined as a different
	// file, with HeldSHA256 as the kept checksum. In this version it can
	// only be rejected: replacing an archived instance deletes archived
	// data, and is not part of reception.
	DifferentFile
	// UIDElsewhere is the UID held under another study or series, or
	// claimed by this reception under another one: an instance UID is one
	// instance in a tenant. The file is quarantined with that reason.
	UIDElsewhere
	// Claimed is the UID claimed by another reception that has not
	// confirmed it yet, whatever its study and series: the answer is
	// decided against what is held, never against a claim that may be
	// released. The caller's step is tried again later through its leased
	// work, when the claim has been confirmed (a held answer) or released
	// (it can then be claimed).
	Claimed
	// ClaimedNow is the UID claimed by this call for this reception
	// (ClaimInstances only). The file goes on to the storage.
	ClaimedNow
	// ClaimedBefore is the UID claimed by this reception before, with the
	// same checksum, and not confirmed yet, as when a step runs again after
	// a crash (ClaimInstances only). The file goes on to the storage.
	ClaimedBefore
	// ArchivedBefore is the UID held, with the same checksum, by this same
	// reception: a commit of its scan happened before a crash, and the step
	// is running again (ClaimInstances only). The file is archived already
	// and counted so (CommitScan counted it): it is neither written again
	// nor counted duplicate.
	ArchivedBefore
)

// String returns the kind's name, for messages and logs.
func (k DuplicateKind) String() string {
	switch k {
	case NotHeld:
		return "not held"
	case SameFile:
		return "same file"
	case DifferentFile:
		return "different file"
	case UIDElsewhere:
		return "UID elsewhere"
	case Claimed:
		return "claimed by another reception"
	case ClaimedNow:
		return "claimed now"
	case ClaimedBefore:
		return "claimed before"
	case ArchivedBefore:
		return "archived before by this reception"
	}
	return "DuplicateKind(" + strconv.Itoa(int(k)) + ")"
}

// Duplicate is the answer for one file of CheckDuplicates or
// ClaimInstances.
type Duplicate struct {
	// Kind is how it compares.
	Kind DuplicateKind
	// HeldSHA256, HeldStudyUID and HeldSeriesUID are those of the row the
	// answer is about: the instance held, or the claim (this reception's
	// first, or another reception's). Zero for NotHeld and ClaimedNow.
	HeldSHA256                  SHA256
	HeldStudyUID, HeldSeriesUID string
	// HeldBy is the reception that confirmed the held instance, or that
	// claims the UID.
	HeldBy string
}

// CompareHeld returns how a file compares with an instance of the tenant
// under its UID, by study and series first, since an instance UID is unique
// in a tenant whatever the bytes, then by checksum. It is the one place the
// comparison is defined: CheckDuplicates and ClaimInstances both decide by
// it, in every driver.
func CompareHeld(q DuplicateQuery, held Duplicate) DuplicateKind {
	switch {
	case held.HeldStudyUID != q.StudyUID || held.HeldSeriesUID != q.SeriesUID:
		return UIDElsewhere
	case held.HeldSHA256 == q.SHA256:
		return SameFile
	}
	return DifferentFile
}

// InstanceClaim is one file a reception claims before it writes anything to
// a storage. It must be one of the reception's received files: its UIDs and
// checksum are those the file was received with.
type InstanceClaim struct {
	// SOPInstanceUID, StudyUID and SeriesUID are the file's, as received.
	SOPInstanceUID, StudyUID, SeriesUID string
	// SHA256 is its received checksum.
	SHA256 SHA256
}

// Validate returns an Invalid error unless the claim is well formed.
func (c InstanceClaim) Validate() error {
	return c.Query().Validate()
}

// Query returns the claim as the query CompareHeld takes.
func (c InstanceClaim) Query() DuplicateQuery {
	return DuplicateQuery(c)
}

// InstanceRow is what a driver read of the instance row under a UID: an
// instance claimed by a reception, or held, never both.
type InstanceRow struct {
	// StudyUID and SeriesUID are the instance's, as received.
	StudyUID, SeriesUID string
	// SHA256 is its received checksum.
	SHA256 SHA256
	// By is the reception that claims it, or that confirmed it.
	By string
	// Held says the instance is held; otherwise it is only claimed.
	Held bool
}

// ClaimAction is what a driver writes for one file of a claim.
type ClaimAction int

// The actions.
const (
	// ClaimNothing writes nothing: the answer is about the row as it is.
	ClaimNothing ClaimAction = iota
	// ClaimInsert inserts the row, claimed by the reception.
	ClaimInsert
)

// DecideClaim returns the answer of a claim by reception receptionID for c,
// given the row under its UID (nil when there is none), and what to write.
// Every driver decides by it, so the claim's rules are stated once:
//
//   - no row: claimed now (insert);
//   - claimed by another reception: claimed, whatever its study and series;
//   - claimed by this reception: another study or series is the UID
//     elsewhere, the same checksum claimed before, another checksum a
//     different file (the first claimed is kept);
//   - held: another study or series is the UID elsewhere; the same
//     checksum is archived before when this reception confirmed it and the
//     same file otherwise; another checksum is a different file.
func DecideClaim(receptionID string, c InstanceClaim, row *InstanceRow) (Duplicate, ClaimAction) {
	if row == nil {
		return Duplicate{Kind: ClaimedNow}, ClaimInsert
	}
	d := Duplicate{HeldSHA256: row.SHA256, HeldStudyUID: row.StudyUID, HeldSeriesUID: row.SeriesUID, HeldBy: row.By}
	if !row.Held && row.By != receptionID {
		d.Kind = Claimed
		return d, ClaimNothing
	}
	d.Kind = CompareHeld(c.Query(), d)
	switch {
	case d.Kind == SameFile && !row.Held:
		d.Kind = ClaimedBefore
	case d.Kind == SameFile && row.By == receptionID:
		d.Kind = ArchivedBefore
	}
	return d, ClaimNothing
}

// JudgeRow returns CheckDuplicates's answer for q given the row under its
// UID (nil when none): a claim not confirmed is Claimed, whoever holds it;
// a held instance is compared (CompareHeld).
func JudgeRow(q DuplicateQuery, row *InstanceRow) Duplicate {
	if row == nil {
		return Duplicate{}
	}
	d := Duplicate{HeldSHA256: row.SHA256, HeldStudyUID: row.StudyUID, HeldSeriesUID: row.SeriesUID, HeldBy: row.By, Kind: Claimed}
	if row.Held {
		d.Kind = CompareHeld(q, d)
	}
	return d
}

// SortClaims returns the indexes of files in the order every driver claims
// them in: by SOP Instance UID, then checksum, byte by byte, whatever order
// the caller gave. One order for every claim means two claims that share
// UIDs never wait for each other in a cycle, and two files of one batch
// under one UID always keep the lower checksum (DifferentFile for the
// other).
func SortClaims(files []InstanceClaim) []int {
	order := make([]int, len(files))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		if c := strings.Compare(files[a].SOPInstanceUID, files[b].SOPInstanceUID); c != 0 {
			return c
		}
		return bytes.Compare(files[a].SHA256[:], files[b].SHA256[:])
	})
	return order
}

// ValidateClaimBatch returns an Invalid error unless the batch is within
// MaxClaimBatch, every claim is valid, and no UID appears twice with the
// same checksum (a UID twice with two checksums is two received files, and
// the claim answers the second as different).
func ValidateClaimBatch(receptionID string, files []InstanceClaim, now time.Time) error {
	if err := master.ValidateID("reception id", receptionID); err != nil {
		return err
	}
	if len(files) > MaxClaimBatch {
		return invalid("at most " + strconv.Itoa(MaxClaimBatch) + " instances are claimed at once")
	}
	type key struct {
		uid string
		sum SHA256
	}
	seen := make(map[key]bool, len(files))
	for _, c := range files {
		if err := c.Validate(); err != nil {
			return err
		}
		k := key{c.SOPInstanceUID, c.SHA256}
		if seen[k] {
			return invalid("a claim names one file twice")
		}
		seen[k] = true
	}
	return master.ValidateTime("claim time", now)
}
