package tenant

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// Replication is what the tenant's database keeps of its replicas: which
// storage is a replica of which primary, how far each replica is from its
// primary scan by scan, and the queue of scans to copy, which is that same
// state.
//
// # A replica
//
// A replica is an ordinary storage whose ReplicaOf names its primary, a
// storage of the same tenant that holds the same keys. A primary has at most
// MaxReplicas; a replica serves one primary, is never itself replicated,
// and is never named by a receiver, a route or a release, so nothing is
// archived to it first (CheckArchivedTo). Only a replica may be of the
// archive tier. The rules are CheckAttach's, CheckStorageChange's and
// CheckStorageRemoval's, which every store applies in the transaction of
// the change.
//
// # State per scan, keyed by UIDs
//
// Each replica has a row per scan of its primary that it has been asked to
// hold, keyed by the replica's name and the scan's Series Instance UID,
// never by an integer id, so the row survives a rebuild that makes the
// catalog's ids again. A row is pending (to copy), copying (leased to the
// replicator), in step (the replica holds, verified, every object the
// scan's manifest names and that manifest, at the digest ManifestSHA256),
// or damaged (a scrub or a read found an object of the replica missing or
// wrong, ReplicaDamage, and the replicator copies it again from the
// primary, unasked). A scan is in step for a read only when its row is in
// step at the catalog's manifest digest now (InStepReplicas).
//
// # The copy is a job, behind the reception
//
// The transaction that publishes a change of a scan (a reception's commit,
// a correction's publish or undo) makes the scan pending for each replica
// of its storage, or, when it is pending already, counts the change, so a
// copy claimed before it never ends in step (EndReplicaScan). A delete's
// publish drops the rows of the scans it hides whole. A crash between the
// commit and the copy forgets nothing: the queue is these rows. The
// replicator claims rows (ClaimReplicaScans), each under a lease fenced by
// its attempt, as a unit of work is; a replica that does not answer keeps
// its rows pending (FailReplicaScan), and never refuses a reception.
//
// # Counts
//
// A replica's counts of scans by state, of damaged objects, and of the
// scans its backfill still owes are kept with every change of a row, so a
// status reads them without counting (ListReplicas).
//
// # Rebuilds and restores
//
// A replica's state is not part of the catalog: a rebuild neither builds
// nor compares it. When a rebuild of replace mode makes its shadow the
// catalog in use (and a restore ends with one), the rows are made to agree
// with the new catalog in the same transaction: a row whose scan is gone is
// dropped, a row in step whose digest is not the new manifest's becomes
// pending, and a scan of a primary with replicas that has no row gets a
// pending one. A row in step at the new manifest's digest stays in step:
// the replica's bytes did not change, and every read from a replica is
// checked against the catalog's checksums all the same.
type Replication interface {
	// AttachReplica makes the storage replica a replica of primary under
	// CheckAttach's rules, and makes every scan of the primary pending for
	// it, marked as its backfill, in one transaction with its audit entry.
	// It answers the replica as stored, the scans queued, and whether this
	// call attached it: a storage already a replica of primary is answered
	// as it stands, queuing nothing, its audit entry written all the same.
	// A replica or a primary the tenant does not have is NotFound and a
	// precondition failure respectively, as the contract's AttachReplica
	// says.
	AttachReplica(ctx context.Context, replica, primary string, now time.Time, audit master.AuditEntry, guard Guard) (Storage, int64, bool, error)
	// DetachReplica makes a replica an ordinary storage again under
	// CheckDetach's rules: its rows, its damage, its counts and the
	// correction records owed to it are dropped and ReplicaOf cleared, in
	// one transaction with its audit entry, and nothing on any storage is
	// touched. With an abandon, each committed or purging correction whose
	// scans are on its primary (those that count the replica among their
	// copies) names the replica in its CopiesAbandoned, and each object it
	// owns whose removal from the replica is not recorded is recorded
	// unknown there, in the same transaction. It answers the storage as
	// stored and those corrections' ids, in order. A storage that is not a
	// replica is a precondition failure.
	DetachReplica(ctx context.Context, d ReplicaDetach, audit master.AuditEntry, guard Guard) (Storage, []string, error)
	// BackfillReplica makes every scan of the replica's primary pending for
	// it, marked as its backfill: those with no row, and those in step,
	// whose objects the replicator verifies again (a replica restored from
	// elsewhere). It answers the scans it marked. A storage that is not a
	// replica is a precondition failure.
	BackfillReplica(ctx context.Context, replica string, now time.Time, audit master.AuditEntry, guard Guard) (int64, error)
	// ListReplicas answers a page of the tenant's replicas with their
	// counts, by primary then replica name; primary, when not empty, keeps
	// those of that primary.
	ListReplicas(ctx context.Context, primary string, page master.Page) ([]ReplicaStatus, string, error)
	// ReplicaScan answers the row of the scan of the series on the replica,
	// or NotFound when it has none.
	ReplicaScan(ctx context.Context, replica, seriesUID string) (ReplicaScan, error)
	// InStepReplicas answers, for each series of uids (at most
	// MaxReplicaLookup), the replicas in step for its scan at the catalog's
	// manifest digest now, by name; a series none is in step for is absent.
	// What a read that turns from its primary reads.
	InStepReplicas(ctx context.Context, seriesUIDs []string) (map[string][]string, error)
	// ClaimReplicaScans leases up to limit rows (1 to MaxReplicaClaim) to
	// owner until now + lease: those pending or damaged and due, earliest
	// due first, then those copying whose lease ended. Each moves to
	// copying with its attempt raised, and is answered with what the copy
	// needs (ReplicaClaim). A row whose scan the catalog no longer holds is
	// dropped on the way, not answered. A row of a scan an instance delete
	// in state purging touches is not claimed, and stays as it is: until
	// FinishPurge the catalog names the manifest the purge is replacing,
	// and a copy of it landing after the purge wrote the replica's manifest
	// and sidecar again would leave the replica naming the deleted instance;
	// FinishPurge queues the scan again at its new manifest. Two claims at
	// once take different rows. Nothing due answers none.
	ClaimReplicaScans(ctx context.Context, owner string, now time.Time, lease time.Duration, limit int) ([]ReplicaClaim, error)
	// RenewReplicaScan extends a claim's lease to now + lease. A row whose
	// attempt is not the fence's, or that is not copying, changes nothing
	// and is ErrLeaseLost.
	RenewReplicaScan(ctx context.Context, fence ReplicaFence, now time.Time, lease time.Duration) error
	// EndReplicaScan records the claim's copy done, the scan's objects and
	// manifest at digest manifest put on the replica and verified: the row
	// becomes in step at that digest, its damage cleared and its backfill
	// mark dropped, when no change of the scan was published since the
	// claim (nor damage recorded) and manifest is the catalog's digest now;
	// otherwise it is pending again (damaged, when damage is left), due at
	// now. It answers the row's state after. Fenced as RenewReplicaScan.
	EndReplicaScan(ctx context.Context, fence ReplicaFence, manifest SHA256, now time.Time) (ReplicaScanState, error)
	// FailReplicaScan records a copy that could not be done: the row is
	// pending (damaged, when damage is left) again, due at f.Retry, with the
	// reason, and the replica's last fault is f.Fault at f.Now. No count of
	// attempts gives up: a replica that does not answer keeps its scans
	// pending. Fenced as RenewReplicaScan.
	FailReplicaScan(ctx context.Context, f ReplicaFailure) error
	// RecordReplicaDamage records an object of the replica a scrub or a read
	// found missing or wrong, under d.Fence when given (a scrub's unit):
	// its damage row, and its scan's row damaged and due at once, counted
	// as a change (EndReplicaScan). It answers whether the damage is new; a
	// damage recorded already takes the newer values. A scan the replica
	// has no row for is a precondition failure: a scan never copied holds
	// nothing to be damaged.
	RecordReplicaDamage(ctx context.Context, d ReplicaDamageRecord) (bool, error)
	// ClearReplicaDamage removes the damage of the object (sopUID empty for
	// the scan's manifest or metadata) that a scrub read good again, under
	// fence when given, and answers whether there was one. A scan left with
	// no damage is in step again when it holds the catalog's manifest
	// digest, pending otherwise. The scrub finding recorded with the damage
	// is cleared with it, by the job of the fence's unit when it is a scrub
	// job's.
	ClearReplicaDamage(ctx context.Context, fence *WorkFence, replica, seriesUID, sopUID string, now time.Time) (bool, error)
	// ReplicaDamaged answers the replica's damaged objects of the scan of
	// the series: by SOP Instance UID ("" for the scan's manifest or
	// metadata), the id of the scrub finding recorded with each ("" for
	// none). What a scrub's unit reads once a scan, so an object read good
	// asks to clear its damage only when it has some.
	ReplicaDamaged(ctx context.Context, replica, seriesUID string) (map[string]string, error)
}

// MaxReplicas is the most replicas a primary has, the contract's.
const MaxReplicas = 2

// The bounds of replication.
const (
	// MaxReplicaClaim is the most rows one ClaimReplicaScans leases.
	MaxReplicaClaim = 100
	// MaxReplicaLookup is the most series one InStepReplicas looks up.
	MaxReplicaLookup = 1000
	// MaxReplicaDamage is the most damaged objects a claim names; a scan
	// with more is copied again whole (ReplicaClaim.RecopyAll).
	MaxReplicaDamage = 1000
	// MaxReplicaReason bounds the reason a failed copy keeps.
	MaxReplicaReason = 256
)

// ReplicaScanState is where a replica's row of a scan is.
type ReplicaScanState string

// The states.
const (
	// ReplicaPending is a scan to copy: never copied, or changed on the
	// primary since.
	ReplicaPending ReplicaScanState = "pending"
	// ReplicaCopying is a scan leased to the replicator.
	ReplicaCopying ReplicaScanState = "copying"
	// ReplicaInStep is a scan the replica holds, verified, at its
	// manifest's digest.
	ReplicaInStep ReplicaScanState = "in_step"
	// ReplicaDamaged is a scan with an object of the replica found missing
	// or wrong, to copy again.
	ReplicaDamaged ReplicaScanState = "damaged"
)

// ReplicaScanStates returns every state, in the order a status counts them.
func ReplicaScanStates() []ReplicaScanState {
	return []ReplicaScanState{ReplicaPending, ReplicaCopying, ReplicaInStep, ReplicaDamaged}
}

// Valid reports whether s is one of ReplicaScanStates.
func (s ReplicaScanState) Valid() bool { return slices.Contains(ReplicaScanStates(), s) }

// ReplicaScan is a replica's row of one scan.
type ReplicaScan struct {
	// Replica names the replica; SeriesUID the scan.
	Replica, SeriesUID string
	// State is where it is.
	State ReplicaScanState
	// ManifestSHA256 is the manifest digest the replica holds verified;
	// zero until the scan was first in step.
	ManifestSHA256 SHA256
	// Queued is when it became pending, the start of its lag; zero while it
	// is in step.
	Queued time.Time
	// Due is when a claim may take it.
	Due time.Time
	// Verified is when it was last in step; zero until then.
	Verified time.Time
	// Backfill says it is owed to the replica's backfill.
	Backfill bool
	// Attempt counts its claims and fences the replicator's writes.
	Attempt int
	// LastError is why its last copy failed, empty after one that did not.
	LastError string
	// Damaged counts its damaged objects.
	Damaged int64
}

// ReplicaFence is a claim's lease: the row and the attempt that holds it.
type ReplicaFence struct {
	Replica, SeriesUID string
	Attempt            int
}

// Validate returns an Invalid error unless f is well formed.
func (f ReplicaFence) Validate() error {
	if err := ValidateName("storage", f.Replica); err != nil {
		return err
	}
	if err := ValidateSeriesUID(f.SeriesUID); err != nil {
		return err
	}
	if f.Attempt < 1 {
		return invalid("a replica's lease holds an attempt of 1 or more")
	}
	return nil
}

// ReplicaClaim is one row ClaimReplicaScans leased, with what its copy
// needs.
type ReplicaClaim struct {
	// Fence is the lease.
	Fence ReplicaFence
	// Primary names the replica's primary, the storage the copy reads.
	Primary string
	// StudyUID is the scan's study, Path its labels, as the catalog holds
	// them at the claim.
	StudyUID string
	Path     label.Path
	// ManifestSHA256 is the catalog's manifest digest at the claim: the
	// digest EndReplicaScan must name for the scan to be in step.
	ManifestSHA256 SHA256
	// SidecarSHA256 is the catalog's digest of the scan's metadata file at
	// the claim, zero when it keeps none (CatalogScan.SidecarSHA256): the
	// copy puts the file against it.
	SidecarSHA256 SHA256
	// Backfill says it is owed to the replica's backfill.
	Backfill bool
	// Damaged are the replica's objects of the scan found missing or wrong,
	// which the copy puts again even where an object is there, at most
	// MaxReplicaDamage; RecopyAll says there are more, and every object is
	// put again.
	Damaged   []ReplicaDamage
	RecopyAll bool
	// Purging are the SOP Instance UIDs of the scan a correction in state
	// purging hides, in order: the copy skips their objects. The purge
	// removes them from every copy and took its copies when it began, so
	// an object put on the replica now would outlive the purge.
	Purging []string
}

// ReplicaDamage is one object of a replica found missing or wrong.
type ReplicaDamage struct {
	// SOPInstanceUID is the instance whose object it is; empty for the
	// scan's manifest or metadata.
	SOPInstanceUID string
	// Revision is the revision the object holds; 0 for a manifest.
	Revision int
	// FoundSize and FoundSHA256 are what was found: nil and zero for an
	// object missing.
	FoundSize   *int64
	FoundSHA256 SHA256
	// Found is when it was found.
	Found time.Time
}

// ReplicaDamageRecord is what RecordReplicaDamage records.
type ReplicaDamageRecord struct {
	// Fence is the scrub unit's lease it is recorded under; nil for none.
	Fence *WorkFence
	// Replica names the replica; SeriesUID the scan.
	Replica, SeriesUID string
	// Damage is what was found; its Found is the record's time.
	Damage ReplicaDamage
	// Finding, when not nil, is what a scrub's unit knows of the object
	// beyond the damage: the damage is then also recorded as a scrub
	// finding on the replica's name (ScrubFinding.ReplicaOf set, never
	// marking the instance), seen again when one holds of the same key and
	// kind, so a list of findings shows a replica's damage as the contract
	// says; it is cleared with the damage. It needs Fence, of a scrub
	// job's unit, whose job is the finding's.
	Finding *ReplicaFinding
}

// ReplicaFinding is what the scrub finding recorded with a replica's damage
// says beyond the damage.
type ReplicaFinding struct {
	// Kind is what was found: missing or damaged for an object the manifest
	// names, manifest_damaged for the manifest, disagreeing for a metadata
	// file not of the digest the catalog holds.
	Kind ScrubFindingKind
	// Key is the object's key; Path the scan's labels.
	Key  string
	Path label.Path
	// ExpectedSize and ExpectedSHA256 are what the catalog or the manifest
	// holds of it: a size for an object missing or damaged, nil and zero
	// when not known.
	ExpectedSize   *int64
	ExpectedSHA256 SHA256
}

// Validate returns an Invalid error unless r can be recorded.
func (r ReplicaDamageRecord) Validate() error {
	if r.Fence != nil {
		if err := r.Fence.Validate(); err != nil {
			return err
		}
	}
	if f := r.Finding; f != nil {
		if r.Fence == nil {
			return invalid("a replica's damage recorded as a finding is recorded under a scrub unit's lease")
		}
		if !f.Kind.Valid() || f.Kind == FindingOrphan {
			return invalid("a replica's damage is found missing, damaged, manifest_damaged or disagreeing")
		}
		if err := ValidateFindingKey(f.Key); err != nil {
			return err
		}
		if err := ValidateScanPath(f.Path); err != nil {
			return err
		}
		if f.Kind.Marks() && f.ExpectedSize == nil {
			return invalid("a missing or damaged object's finding states the size that was archived")
		}
		if f.ExpectedSize != nil && *f.ExpectedSize < 0 {
			return invalid("a finding's sizes are not negative")
		}
		if f.Kind == FindingMissing && r.Damage.FoundSize != nil {
			return invalid("a missing object's finding states nothing found")
		}
	}
	if err := ValidateName("storage", r.Replica); err != nil {
		return err
	}
	if err := ValidateSeriesUID(r.SeriesUID); err != nil {
		return err
	}
	d := r.Damage
	if d.SOPInstanceUID != "" {
		if err := ValidateInstanceUID(d.SOPInstanceUID); err != nil {
			return err
		}
	}
	if d.Revision < 0 || d.Revision > maxFindingRevision {
		return invalid("a damaged object's revision is 0 to " + strconv.Itoa(maxFindingRevision))
	}
	if d.FoundSize != nil && *d.FoundSize < 0 {
		return invalid("a damaged object's size is not negative")
	}
	if d.FoundSize == nil && !d.FoundSHA256.IsZero() {
		return invalid("a missing object has no checksum found")
	}
	return master.ValidateTime("damage time", d.Found)
}

// ReplicaFailure is what FailReplicaScan records.
type ReplicaFailure struct {
	// Fence is the claim's lease.
	Fence ReplicaFence
	// Fault is why, from the contract's StorageFault in lowercase without
	// its prefix (FaultUnreachable and the others).
	Fault string
	// Reason is a phrase of the server's, at most MaxReplicaReason bytes.
	Reason string
	// Retry is when the row is due again; Now when the failure was seen.
	Retry, Now time.Time
}

// storageFaults are the contract's StorageFault values.
var storageFaults = []string{FaultUnreachable, FaultTLSFailed, FaultRefused, FaultNotFound, FaultWrongAnswer, FaultAddressNotAllowed}

// Validate returns an Invalid error unless f can be recorded.
func (f ReplicaFailure) Validate() error {
	if err := f.Fence.Validate(); err != nil {
		return err
	}
	if !slices.Contains(storageFaults, f.Fault) {
		return invalid("a storage fault is one of unreachable, tls_failed, refused, not_found, wrong_answer, address_not_allowed")
	}
	if err := validateText("copy failure reason", f.Reason, 0, MaxReplicaReason); err != nil {
		return err
	}
	if err := master.ValidateTime("retry time", f.Retry); err != nil {
		return err
	}
	return master.ValidateTime("time", f.Now)
}

// ReplicaStatus is how far one replica is from its primary, as kept with
// every change of its rows.
type ReplicaStatus struct {
	// Replica names the replica; Primary its primary.
	Replica, Primary string
	// Tier is the replica's tier.
	Tier StorageTier
	// SharesBucket says it lies on its primary's endpoint and bucket
	// (SharesBucket).
	SharesBucket bool
	// Pending, Copying, InStep and Damaged count its rows by state.
	Pending, Copying, InStep, Damaged int64
	// ObjectsDamaged counts its damaged objects not yet copied again.
	ObjectsDamaged int64
	// BackfillPending counts the scans its backfill still owes.
	BackfillPending int64
	// Attached is when it became a replica; BackfillDone when its last
	// backfill was owed nothing more, zero while one is.
	Attached, BackfillDone time.Time
	// OldestQueued is when the oldest scan not in step became pending, zero
	// when every scan is in step: the start of its lag.
	OldestQueued time.Time
	// LastFault is the fault of its last failed copy, empty after one that
	// succeeded; LastFaultTime when it was seen.
	LastFault     string
	LastFaultTime time.Time
	// RecallsInProgress counts its recalls requested or in progress.
	RecallsInProgress int64
}

// ValidateReplicaLookup returns an Invalid error unless uids is a lookup
// InStepReplicas takes.
func ValidateReplicaLookup(uids []string) error {
	if len(uids) > MaxReplicaLookup {
		return invalid("at most " + strconv.Itoa(MaxReplicaLookup) + " series are looked up at once")
	}
	for _, u := range uids {
		if err := ValidateSeriesUID(u); err != nil {
			return err
		}
	}
	return nil
}

// ValidateReplicaClaim returns an Invalid error unless a claim's arguments
// are well formed.
func ValidateReplicaClaim(owner string, now time.Time, lease time.Duration, limit int) error {
	if err := validateText("owner", owner, 1, maxOwnerLength); err != nil {
		return err
	}
	if lease <= 0 || lease > maxReplicaLease {
		return invalid("a replica's lease is longer than zero and at most a day")
	}
	if limit < 1 || limit > MaxReplicaClaim {
		return invalid("a claim leases 1 to " + strconv.Itoa(MaxReplicaClaim) + " scans")
	}
	return master.ValidateTime("time", now)
}

// maxOwnerLength bounds a claim's owner, a process's name for itself.
const maxOwnerLength = 128

// maxReplicaLease bounds a claim's lease.
const maxReplicaLease = 24 * time.Hour
