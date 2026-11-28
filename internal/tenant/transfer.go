package tenant

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// JobTransfer is the kind of a transfer job: the archived instances of a
// subject or a session copied into another tenant through that tenant's
// receiver of type job, and for a move deleted from this tenant once every
// copy is confirmed (Transfers). The job lives in the source tenant.
const JobTransfer JobKind = "transfer"

// ReceiverJob is a receiver that never listens: the archive's own jobs hand
// it files, one by one, as a file that arrived (a transfer job's copy; a
// later retrieve will use the same type). Its routing, profile and checks
// apply as to every receiver, so what it archives is the target tenant's
// own: its labels, its de-identification.
const ReceiverJob ReceiverType = "job"

// TransferStep is where a transfer job is, the order a job goes through and
// never back: listing the source's instances, copying them, confirming each
// in the target, and for a move deleting the source.
type TransferStep string

// The steps.
const (
	// TransferListing reads the source's catalog into the job's items.
	TransferListing TransferStep = "listing"
	// TransferCopying reads each item's archived bytes and hands them to the
	// target's job receiver.
	TransferCopying TransferStep = "copying"
	// TransferConfirming finds each copy in the target and compares its
	// received checksum with the source's.
	TransferConfirming TransferStep = "confirming"
	// TransferDeleting, for a move, deletes the source through the ordinary
	// delete.
	TransferDeleting TransferStep = "deleting"
	// TransferEnded is a job that ended; its state says how.
	TransferEnded TransferStep = "ended"
)

// Valid reports whether s is a step.
func (s TransferStep) Valid() bool {
	switch s {
	case TransferListing, TransferCopying, TransferConfirming, TransferDeleting, TransferEnded:
		return true
	}
	return false
}

// TransferItemState is where one instance of a transfer job is.
type TransferItemState string

// The states of an item; an item goes forward only.
const (
	// TransferItemListed is an instance listed and not yet handed to the
	// target.
	TransferItemListed TransferItemState = "listed"
	// TransferItemCopied is an instance the target's receiver took whole
	// (durable in its incoming area); the target has not been seen to hold
	// it yet.
	TransferItemCopied TransferItemState = "copied"
	// TransferItemConfirmed is an instance found in the target with the
	// received checksum the source's bytes give.
	TransferItemConfirmed TransferItemState = "confirmed"
	// TransferItemFailed is an instance that will not be copied or whose copy
	// was not confirmed; the job's failures name why.
	TransferItemFailed TransferItemState = "failed"
)

// Valid reports whether s is a state.
func (s TransferItemState) Valid() bool {
	switch s {
	case TransferItemListed, TransferItemCopied, TransferItemConfirmed, TransferItemFailed:
		return true
	}
	return false
}

// The bounds of transfers.
const (
	// MaxTransferItemBatch is the most items one AddTransferItems call adds.
	MaxTransferItemBatch = 1000
	// MaxTransferItemPage is the most items one TransferItems call answers.
	MaxTransferItemPage = 1000
	// MaxTransferFound is the most UIDs one FindTransferred call looks up.
	MaxTransferFound = MaxSeenUIDs
)

// Transfers is what the source tenant's database keeps of transfer jobs: the
// job and its items, which are the instance list and stay in the source, so
// the target never stores a UID of the source. The job's steps are saved, so
// a restart goes on from the last item recorded and copies nothing twice.
type Transfers interface {
	// QueueTransfer makes a transfer job of q.ID (kind JobTransfer) at the
	// step listing, with its audit entry and event, in one transaction, and
	// answers it. The target is a subject or a session the tenant holds
	// (NotFound otherwise); a transfer of the tenant to itself, a rebuild
	// running, and a transfer job not ended over the same subject, are
	// precondition failures.
	QueueTransfer(ctx context.Context, q TransferQueue, audit master.AuditEntry, guard Guard) (Job, error)
	// TransferJob answers the transfer job of that id with its own part and
	// the count of its items in each state; NotFound for none.
	TransferJob(ctx context.Context, id string) (TransferJob, error)
	// ListTransfers returns a page of the tenant's transfer jobs, newest
	// first.
	ListTransfers(ctx context.Context, page master.Page) ([]TransferJob, string, error)
	// AddTransferItems adds the instances of items to the job at the step
	// listing, each as listed; one the job holds already (same scan labels
	// and UID) is kept as it is, so a listing run again changes nothing. It
	// answers how many were added. Each item's revision is the instance's
	// revision now (1 and the revisions replaced).
	AddTransferItems(ctx context.Context, id string, items []TransferNewItem) (int, error)
	// StartTransferStep moves the job from the step before to to, which must
	// be the next of its job (a move has the step deleting, a copy has not),
	// writing its event; the job queued becomes running. Leaving listing sets
	// the job's instances and bytes to what its items hold; leaving the step
	// before the end is refused while an item is in a state that step must
	// settle (a listed item before copying ends, a copied item before
	// confirming ends). A job already at to is answered as it is.
	StartTransferStep(ctx context.Context, id string, to TransferStep, now time.Time) (TransferJob, error)
	// TransferItems returns at most limit (1 to MaxTransferItemPage) items of
	// the job in state, in order of their scan labels and UID, after the
	// item named (the zero key for the first).
	TransferItems(ctx context.Context, id string, state TransferItemState, after TransferItemKey, limit int) ([]TransferItem, error)
	// RecordTransferItem moves one item forward (listed to copied, listed or
	// copied to failed, copied to confirmed) and counts it: a confirmed item
	// counts its instance and bytes sent, a failed one refused with its
	// failure listed. An item already in the state asked is left as it is; a
	// job ended refuses it (ErrTransferEnded) and its counts stay final.
	RecordTransferItem(ctx context.Context, r TransferRecord) error
	// RecordTransferDelete records the correction a move's delete made, at
	// the step deleting, so a restart does not make a second.
	RecordTransferDelete(ctx context.Context, id, correction string) error
	// EndTransfer ends the job as done, partial or failed, with a reason of
	// 0 to MaxFailureLength bytes, at the step ended, with its event, and
	// answers it; one ended already is answered as it is.
	EndTransfer(ctx context.Context, id string, state JobState, reason string, now time.Time) (Job, error)
}

// ErrTransferEnded is in the chain of a transfer's step refused because its
// job has ended: a cancel stops what the runner records.
var ErrTransferEnded = errors.New("the transfer job has ended")

// TransferTargets is what the target tenant's database keeps for transfers
// that came in: the provenance of each instance, and the lookups a job's
// confirmation makes. It holds no UID of the source and no original
// identifier.
type TransferTargets interface {
	// FindTransferred answers, for each of at most MaxTransferFound UIDs the
	// tenant holds as an instance, the checksum it was received with and its
	// scan's labels; a UID it does not hold is absent from the answer.
	FindTransferred(ctx context.Context, uids []string) (map[string]TransferFound, error)
	// RecordTransferProvenance records where each instance came from, keyed
	// by the target's SOP Instance UID; a row held already stays as it is,
	// so recording again changes nothing and the first source an instance
	// came from is never overwritten by a later transfer that found it held.
	RecordTransferProvenance(ctx context.Context, rows []TransferProvenance) error
	// TransferProvenanceOf answers the provenance of the instance with that
	// UID, and false when it has none.
	TransferProvenanceOf(ctx context.Context, uid string) (TransferProvenance, bool, error)
	// ListTransferProvenance returns a page of the provenance rows of one
	// source job, in UID order.
	ListTransferProvenance(ctx context.Context, sourceJob string, page master.Page) ([]TransferProvenance, string, error)
}

// TransferQueue is a transfer job to make.
type TransferQueue struct {
	// ID is the job's id, given by the caller.
	ID string
	// Target is what it transfers: a subject or a session of it, by labels.
	Target JobTarget
	// TargetTenant and TargetTenantID name and identify the tenant it goes
	// to; Receiver names that tenant's receiver of type job.
	TargetTenant, TargetTenantID, Receiver string
	// SourceTenant names this tenant, which a transfer to itself is refused
	// by.
	SourceTenant string
	// Move says the source is deleted once every copy is confirmed.
	Move bool
	// Reason is why, kept with the job and used for a move's delete.
	Reason string
	// KeyID is the key that asked, empty for the local operator.
	KeyID string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless q is well formed.
func (q TransferQueue) Validate() error {
	if err := master.ValidateID("job id", q.ID); err != nil {
		return err
	}
	if err := q.Target.Validate(); err != nil {
		return err
	}
	if q.Target.ScanLabel != "" {
		return invalid("a transfer takes a subject or a session, not a scan")
	}
	if err := master.ValidateTenantName(q.TargetTenant); err != nil {
		return err
	}
	if err := master.ValidateID("tenant id", q.TargetTenantID); err != nil {
		return err
	}
	if q.TargetTenant == q.SourceTenant {
		return invalid("a transfer goes to another tenant")
	}
	if err := ValidateName("receiver", q.Receiver); err != nil {
		return err
	}
	if err := validateCorrectionReason("transfer's reason", q.Reason); err != nil {
		return err
	}
	if q.KeyID != "" {
		if err := master.ValidateID("key id", q.KeyID); err != nil {
			return err
		}
	}
	return master.ValidateTime("time", q.Now)
}

// TransferJob is a transfer job's own part.
type TransferJob struct {
	// Job is the job. Its Counts read: Instances and Bytes, what the job
	// lists; sent, the instances confirmed in the target; refused, those that
	// failed.
	Job Job
	// TargetTenant, TargetTenantID and Receiver say where it goes.
	TargetTenant, TargetTenantID, Receiver string
	// Move says the source is deleted once every copy is confirmed.
	Move bool
	// Reason is the reason it was made with.
	Reason string
	// Step is where it is.
	Step TransferStep
	// DeleteCorrection is the correction a move's delete made, empty for
	// none.
	DeleteCorrection string
	// Items counts its items in each state.
	Items TransferItemCounts
}

// TransferItemCounts is how many items of a transfer job are in each state.
type TransferItemCounts struct {
	Listed, Copied, Confirmed, Failed int64
}

// Total returns every item counted.
func (c TransferItemCounts) Total() int64 { return c.Listed + c.Copied + c.Confirmed + c.Failed }

// TransferNewItem is an instance to list in a transfer job.
type TransferNewItem struct {
	// Path is its scan's labels; SOPInstanceUID names it.
	Path           label.Path
	SOPInstanceUID string
	// Size is its file's size in bytes; SHA256 the checksum of the bytes the
	// source archived (ScanInstance.ArchivedSHA256), which every read of it is
	// checked against.
	Size   int64
	SHA256 SHA256
	// AsReceived says it was archived as received, not de-identified.
	AsReceived bool
}

// Validate returns an Invalid error unless i can be listed.
func (i TransferNewItem) Validate() error {
	if err := ValidateScanPath(i.Path); err != nil {
		return err
	}
	if err := ValidateInstanceUID(i.SOPInstanceUID); err != nil {
		return err
	}
	if i.Size < 0 {
		return invalid("an item's size is not negative")
	}
	return nil
}

// TransferItemKey is where a page of items starts after: the scan labels
// and UID of the last one read, zero for the start.
type TransferItemKey struct {
	Path           label.Path
	SOPInstanceUID string
}

// TransferItem is one instance of a transfer job.
type TransferItem struct {
	// Path and SOPInstanceUID name it in the source.
	Path           label.Path
	SOPInstanceUID string
	// Size and SHA256 are its file's size and archived checksum when listed.
	Size   int64
	SHA256 SHA256
	// Revision is its revision in the source when listed.
	Revision int
	// AsReceived says the source archived it as received.
	AsReceived bool
	// State is where it is; Code and Detail say why a failed one failed.
	State  TransferItemState
	Code   JobFailureCode
	Detail string
	// TargetUID is the UID it has in the target, set when it was copied (the
	// target's remapping of the source's UID when its receiver
	// de-identifies); empty before.
	TargetUID string
	// Copied is when the target took it, zero before.
	Copied time.Time
}

// Key returns the item's place in the job's order.
func (i TransferItem) Key() TransferItemKey {
	return TransferItemKey{Path: i.Path, SOPInstanceUID: i.SOPInstanceUID}
}

// TransferRecord is one item's move forward.
type TransferRecord struct {
	// JobID and Key name the item.
	JobID string
	Key   TransferItemKey
	// State is where it goes: copied, confirmed or failed.
	State TransferItemState
	// TargetUID is the UID it has in the target, for copied and confirmed.
	TargetUID string
	// Code, PeerStatus and Detail say why a failed item failed (a code of
	// the job failures, a phrase of at most MaxFailureLength bytes naming a
	// fact, never a value of the file).
	Code       JobFailureCode
	PeerStatus int32
	Detail     string
	// Now is when.
	Now time.Time
}

// Validate returns an Invalid error unless r can be recorded.
func (r TransferRecord) Validate() error {
	if err := master.ValidateID("job id", r.JobID); err != nil {
		return err
	}
	if err := ValidateScanPath(r.Key.Path); err != nil {
		return err
	}
	if err := ValidateInstanceUID(r.Key.SOPInstanceUID); err != nil {
		return err
	}
	switch r.State {
	case TransferItemCopied, TransferItemConfirmed:
		if r.TargetUID == "" {
			return invalid("an item copied or confirmed names its UID in the target")
		}
		if err := ValidateInstanceUID(r.TargetUID); err != nil {
			return err
		}
		if r.Code != "" || r.Detail != "" {
			return invalid("only a failed item has a failure")
		}
	case TransferItemFailed:
		f := JobFailure{SOPInstanceUID: r.Key.SOPInstanceUID, Code: r.Code, Detail: r.Detail, Time: r.Now}
		if err := f.Validate(); err != nil {
			return err
		}
		if r.TargetUID != "" {
			if err := ValidateInstanceUID(r.TargetUID); err != nil {
				return err
			}
		}
	default:
		return invalid("an item moves to copied, confirmed or failed")
	}
	return master.ValidateTime("time", r.Now)
}

// TransferFound is an instance the target holds, as a confirmation reads it.
type TransferFound struct {
	// SHA256 is the checksum it was received with: the SHA-256 of the bytes
	// as they came when the receiver archives as received, the keyed form of
	// it when the receiver de-identifies (a transfer's runner computes the
	// form the target's receiver gives).
	SHA256 SHA256
	// Path is its scan's labels in the target.
	Path label.Path
}

// TransferProvenance is where one instance of the target came from: the
// source tenant, the instance's labels there, its revision there, and the
// source's job. The labels are empty for an instance the source archived as
// received, whose labels may be an original identifier. It is keyed by the
// target's SOP Instance UID and holds no UID of the source.
type TransferProvenance struct {
	// SOPInstanceUID is the instance's UID in the target.
	SOPInstanceUID string
	// SourceTenantID is the source tenant's id.
	SourceTenantID string
	// Path is its scan's labels in the source, empty for an instance the
	// source archived as received.
	Path label.Path
	// Revision is its revision in the source when it was copied.
	Revision int
	// JobID is the source's transfer job.
	JobID string
	// Recorded is when the row was written.
	Recorded time.Time
}

// Validate returns an Invalid error unless p can be recorded.
func (p TransferProvenance) Validate() error {
	if err := ValidateInstanceUID(p.SOPInstanceUID); err != nil {
		return err
	}
	if err := master.ValidateID("tenant id", p.SourceTenantID); err != nil {
		return err
	}
	if p.Path != (label.Path{}) {
		if err := ValidateScanPath(p.Path); err != nil {
			return err
		}
	}
	if p.Revision < 1 {
		return invalid("a revision is 1 or more")
	}
	if err := master.ValidateID("job id", p.JobID); err != nil {
		return err
	}
	return master.ValidateTime("time", p.Recorded)
}

// ValidateTransferUIDs returns an Invalid error unless uids are at most
// MaxTransferFound valid SOP Instance UIDs.
func ValidateTransferUIDs(uids []string) error {
	if len(uids) > MaxTransferFound {
		return invalid("at most " + strconv.Itoa(MaxTransferFound) + " UIDs are looked up at once")
	}
	for _, u := range uids {
		if err := ValidateInstanceUID(u); err != nil {
			return err
		}
	}
	return nil
}

// TransferFailureDetail phrases, fixed words that name a fact of the
// catalog or of the target, never a value of a file.
const (
	TransferDetailChanged   = "the instance changed in the source after the job listed it"
	TransferDetailGone      = "the instance is no longer in the source"
	TransferDetailRefused   = "the target's receiver did not take the file"
	TransferDetailNotFound  = "the target does not hold the instance; it may be in the target's quarantine"
	TransferDetailMismatch  = "the target holds the instance with another checksum than the source's bytes give"
	TransferDetailAsRecv    = "the source archived it as received and the target's receiver does not de-identify"
	TransferDetailNoProfile = "the source records its profile version only in part"
	TransferDetailPixels    = "a pixel rule of the source accepted its image at risk of burned-in text and the target does not de-identify"
	TransferDetailDamaged   = "the source's bytes are not what the catalog records"
	TransferDetailOffline   = "the source's storage does not hold the object online"
)
