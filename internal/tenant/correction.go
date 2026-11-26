package tenant

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
)

// Corrections is what the tenant's database keeps of corrections: recorded
// changes to archived data, a delete, a relabel or a replace. The storage
// side (the correction records, the copies, the new revisions, the purge
// of objects) is the caller's; the store holds the one authority on how
// far each correction got while the database lives.
//
// # The state machine
//
// A correction moves forward only (record.State.CanFollow): prepared to
// committed or abandoned, committed to purging or (a delete) undone,
// purging to purged. PrepareCorrection commits its row, prepared, with the
// operation id the caller made (ValidateCorrectionID), its target, its
// reason and its audit entry, before the caller writes anything to a
// storage. CommitCorrection is the publishing transaction: what the
// correction changes becomes visible to every read at once, with an
// EventCatalogChanged. A transition a correction's state does not allow is
// a precondition failure that changes nothing; asked of a correction
// already in the state it would reach, it is answered as it stands, so a
// step run again after a crash changes nothing more.
//
// # What a delete hides
//
// A delete's publishing transaction moves the catalog's rows of its target
// out of every read: the subject, session and scan rows it removes whole
// (a scan whose last instance it removes, a session whose last scan, a
// subject whose last session, go with it), the instances, their scans'
// receptions' counts, the sessions' modalities; the running totals of the
// rows that stay, of the catalog and of each storage move with them. An
// undo puts every row back as it was, its integer id kept when no row has
// taken it since. Its purge, once its objects are removed, drops the rows
// for good, and keeps for each instance its revision and checksum
// (ResolveInstances answers it deleted).
//
// What names the deleted data is kept from the prepare on
// (DeletedMatches): the subject's label for a subject's delete, and each
// label the subject had before a relabel; each session's Study Instance UID
// for a session's or a subject's, each scan's Series Instance UID, the
// instance's SOP Instance UID. It stays after the purge: a withdrawal
// holds. An abandoned or undone delete keeps nothing; AllowArrivals stops
// one value of a purged delete from matching.
//
// # What a correction owns
//
// The publishing transaction fixes the objects the correction may remove
// at its purge (PurgeTargets): the objects of the instances it hides,
// relabels or supersedes, by their labels then, and the manifest and
// metadata of each scan it hides or relabels, with the SHA-256 the caller
// read of them before the publish (CorrectionCommit.Snapshots). A purge
// never decides what to remove from what a storage holds now. While a
// delete or relabel has not ended, the labels and UIDs it holds (a delete's
// hidden rows; a relabel's old labels, and its new ones while prepared) are
// taken for arrivals (HeldByCorrection): the archive step holds a file that
// would land on them as a label conflict naming the correction, and the
// commit refuses it.
//
// # The clock
//
// A purge is refused while the server's clock reads earlier than the
// latest commit, purge or trusted time the corrections recorded (a clock
// set back would purge early); an automatic purge is also paused while the
// clock reads more than retention.max-clock-step (as the last check of
// retention's clock stored it) past the last time it was trusted (a clock
// that jumped forward would end every grace at once). An admin's
// purge trusts the clock it runs at.
//
// # Relabel and replace
//
// A relabel's publishing transaction changes the labels of the whole
// target at once: the subject, session and scan rows and the copies of
// their labels in the rows beneath, and the keys of the instances whose
// revision is past the first (whose key the catalog holds whole). A
// replace's gives its instance the next revision, at its revision key, with
// the new checksums, size and reception; keeps the revision it superseded
// (instance_revisions); clears the instance's damage mark; and marks the
// quarantined file released. Both give each scan they touch the SHA-256 of
// the manifest written for it.
//
// # Every copy
//
// A correction reaches the replicas of its scans' storages too
// (correction_copies.go): its record is owed to each in its publishing
// transaction, in its purge's start and end and in a later attach, until
// the replica holds it, and an owed record keeps the purge from every
// copy; the purge removes each object from the primary and from each
// replica at the same key, records what each did, and checks every
// removed key absent on every copy before it ends. A purge and a switch of
// its storages exclude each other, and no replica is attached to a
// primary while a correction on it is purging.
//
// # Overlap
//
// A correction is refused (precondition) while another not yet ended
// covers any of the scans its target covers, under the old labels or a
// relabel's new ones, compared case folded (label.Fold); and a delete or
// relabel while a reception of any of its studies, or of its subject, has
// not ended.
//
// While a rebuild of the tenant is queued or running (Rebuilding's
// RebuildRunning), a new correction (PrepareCorrection), the publish of one
// prepared before (CommitCorrection), an undo, the start of a purge whoever
// asks it (StartPurge of a committed correction, the server's as an
// admin's), each page of a purge's objects settled (SettlePurgeTargets) and
// each older version of a manifest or metadata recorded
// (RecordPurgedObject while purging), and a purge's end (FinishPurge) are
// refused (precondition),
// changing nothing: the shadow the rebuild builds from the storages would
// miss a change made while it reads them, or read a scan's manifest and
// metadata torn, and a replace would make that the catalog at its
// activation. A purge refused part way goes on from where it stopped once
// the rebuild has ended. Each check is made under the tenant's
// configuration lock, as the making of a rebuild is.
//
// A correction's prepare, publish, purge's start, a purged object's record
// and the purge's end are refused with ScanRepairingError, writing
// nothing, while a repair holds a scan the correction touches at the
// step's time (Repairs): the repair is putting an object of it back, and
// the step is retried once it ended.
type Corrections interface {
	// PreviewDelete answers what a delete of target would remove, as the
	// catalog holds it now, and the token a delete must carry (Confirm):
	// a digest of the answer and of now.
	PreviewDelete(ctx context.Context, target CorrectionTarget, now time.Time) (DeletePreview, error)
	// PrepareCorrection commits a new correction, prepared, with its audit
	// entry; it returns the correction, and false when an existing one is
	// answered instead: a delete whose confirm token made one already, a
	// replace of a quarantined file a replace not abandoned used, a
	// relabel of the same target to the same label prepared or committed
	// (one purged is done, and the same relabel asked later is new). A
	// delete's token that does not match the preview made in this
	// transaction, or is more than MaxConfirmAge old, is a precondition
	// failure, as is a correction while a switch of the storage of a scan
	// it touches runs (Switching).
	PrepareCorrection(ctx context.Context, p CorrectionPrepare, audit master.AuditEntry, guard Guard) (Correction, bool, error)
	// CorrectionScans returns the scans the correction touches, by the
	// labels they had when it was made, in order of storage then Series
	// Instance UID.
	CorrectionScans(ctx context.Context, id string) ([]CorrectionScan, error)
	// CommitCorrection publishes a prepared correction (the publishing
	// transaction above), fixing its CommitTime and PurgeAfter from
	// c.Now and c.Grace.
	CommitCorrection(ctx context.Context, c CorrectionCommit) (Correction, error)
	// AbandonCorrection ends a prepared correction as abandoned, with why
	// (a phrase of at most MaxStateReasonLength bytes); what names a
	// delete's data stops matching.
	AbandonCorrection(ctx context.Context, id, why string, now time.Time) (Correction, error)
	// UndoCorrection undoes a committed delete, showing its rows again as
	// they were, with the reason given and an EventCatalogChanged, or
	// abandons a prepared relabel. A row of the catalog that has taken one
	// of the delete's labels since is a precondition failure.
	UndoCorrection(ctx context.Context, id, reason string, now time.Time, audit master.AuditEntry, guard Guard) (Correction, error)
	// StartPurge moves a committed correction to purging. With audit nil it
	// is the server's, and only a correction whose PurgeAfter has come and
	// that is not unresolved moves; with an audit entry it is an admin's,
	// and moves now (confirming an unresolved one). Both are refused while
	// the clock reads earlier than a time recorded; the server's is also
	// refused while the clock jumped (CheckPurgeClock). Moving to purging
	// ends the lease of every replica's row of its scans being copied, and
	// keeps the latest due among them, plus ReplicaPutGrace, as the
	// correction's CopiesInFlightUntil when it is later than the one kept.
	StartPurge(ctx context.Context, id string, now time.Time, audit *master.AuditEntry, guard Guard) (Correction, error)
	// CheckPurgeClock answers a precondition failure when an automatic
	// purge may not run at now: the clock reads earlier than the latest
	// commit, purge or trusted time recorded (changing nothing), or more than
	// retention.max-clock-step after the trusted time, which also pauses
	// retention, committed before the refusal; else it records now as
	// trusted.
	CheckPurgeClock(ctx context.Context, now time.Time) error
	// PurgeTargets returns, in order (every instance, then each scan's
	// metadata, then its manifest), at most limit objects of a purging
	// correction not yet settled, each saying whether a row of the catalog
	// resolves to it now. A correction an active hold covers answers the
	// hold's refusal (HoldRefusalError) and no page; the holds are read
	// once for the page, so a hold placed while its objects are being
	// removed does not stop that page. A caller therefore asks
	// CorrectionHeld again before removing each object (a read of the few
	// active holds, no write) and stops, leaving the rest unsettled, when
	// it answers a hold. While a switch of the storage of one of the
	// correction's scans runs (accepted or swapped) it is a precondition
	// failure, retried later: a switch and a purge exclude each other.
	// A target's Done names the copies whose removal is recorded at the
	// switch generation each copy is at now; when one copy's generation
	// moved since, Done is empty, and every copy is asked again.
	PurgeTargets(ctx context.Context, id string, limit int) ([]PurgeTarget, error)
	// PurgedTargets returns, in PurgeTargets' order and after the target
	// given (the zero PurgeTarget for the first page), at most limit
	// objects of a purging correction settled as purged: what the purge's
	// last pass checks absent on every copy before the correction is
	// purged. While a switch of the storage of one of the correction's
	// scans runs it is a precondition failure, as PurgeTargets is.
	PurgedTargets(ctx context.Context, id string, after PurgeTarget, limit int) ([]PurgeTarget, error)
	// AddPurgeTargets records objects a purging correction owns that its
	// publish did not fix (a correction known only from its record, after a
	// restore); one recorded already is left as it is.
	AddPurgeTargets(ctx context.Context, id string, targets []PurgeTarget) error
	// SettlePurgeTargets records, in one transaction, what became of each
	// of at most MaxPurgeTargets objects of a purging correction, and what
	// its storage did with each version of those purged; one settled
	// already, or a version recorded already, is left as it is. Each
	// removal is recorded at its storage's switch generation now, the one
	// it was made at: a switch is not begun while a purge of its storages
	// has an object not settled, and this is refused, as PurgeTargets is,
	// while one runs.
	SettlePurgeTargets(ctx context.Context, id string, settled []PurgeSettled) error
	// RecordPurgedObject records one object of a purging correction removed
	// from a storage, and what the storage did; recording the same object
	// and version again changes nothing. For a purged correction it records
	// what a later pass did with a version the storage retained. For a
	// purging one it is refused, as PurgeTargets is, while a switch runs.
	RecordPurgedObject(ctx context.Context, o PurgedObject) error
	// RetainedDue returns at most limit versions purged corrections
	// recorded retained whose retention has ended by now.
	RetainedDue(ctx context.Context, now time.Time, limit int) ([]PurgedObject, error)
	// ClearWithdrawn removes, for a purging delete, what else names its
	// data: the quarantined files of its target (records, any reason and
	// state) and, for a session or subject, the received files of its
	// ended receptions, whose subject, session and study it gives the
	// correction's id. It returns the incoming paths whose bytes the caller
	// removes; a run again finds nothing.
	ClearWithdrawn(ctx context.Context, id string) ([]string, error)
	// FinishPurge ends a purging correction as purged: a delete's hidden
	// rows dropped (each instance's revision kept), a superseded revision
	// marked purged; for an instance delete, the scan given the SHA-256 of
	// the manifest written without the instance, when its manifest is still
	// the one the purge read (ManifestBefore).
	FinishPurge(ctx context.Context, f PurgeFinish) (Correction, error)
	// RecordCopied sets how many objects a prepared relabel has copied.
	RecordCopied(ctx context.Context, id string, copied int64) error
	// MarkRecords records that every correction record of the correction
	// was written with state on the storages of its scans.
	MarkRecords(ctx context.Context, id string, state record.State) error
	// CorrectionCopies answers the copies a correction reaches besides the
	// storages of its scans: each replica of those storages now, by
	// primary then name, with the correction's scans it is in step for at
	// the catalog's manifest digest. A replica attached after the
	// correction was made is among them; one detached is not.
	CorrectionCopies(ctx context.Context, id string) ([]CorrectionCopy, error)
	// OweRecord records that the replica could not take the correction's
	// record as its row says now, at now; one owed already is left as it
	// is. The transaction that publishes a correction owes its record to
	// every replica of its scans' storages itself.
	OweRecord(ctx context.Context, id, replica string, now time.Time) error
	// MarkRecordWritten records that the replica holds the correction's
	// record written with state: what the replica was owed is cleared when
	// state is the correction's state now, and left otherwise (the
	// correction moved on since, and its record is owed at its new state).
	MarkRecordWritten(ctx context.Context, id, replica string, state record.State) error
	// RecordsOwed answers at most limit records owed, in order of
	// correction then replica, after the pair given (the zero RecordOwed
	// for the first page).
	RecordsOwed(ctx context.Context, after RecordOwed, limit int) ([]RecordOwed, error)
	// DropRecordOwed removes the record of the correction owed to the
	// storage named when that storage is not a replica of the storage of
	// any of the correction's scans now, and answers whether it did; one
	// still a copy keeps what it is owed. Nothing is written to a storage
	// that is not a copy, so its row would stay owed for ever.
	DropRecordOwed(ctx context.Context, id, storage string) (bool, error)
	// NotePurge sets the state reason of a purging correction to why it
	// waits (StateReasonStorage, StateReasonHeld,
	// StateReasonCopiesInFlight), or clears it with "";
	// a correction that is not purging is left as it is.
	NotePurge(ctx context.Context, id, why string) error
	// Correction returns the correction of that id, or NotFound.
	Correction(ctx context.Context, id string) (Correction, error)
	// ListCorrections returns a page of the corrections the filter keeps,
	// newest first.
	ListCorrections(ctx context.Context, filter CorrectionFilter, page master.Page) ([]Correction, string, error)
	// PendingCorrections returns, in order of id after the id given, at
	// most limit corrections a start has to take up: those not ended, those
	// whose records were not written with their state, and abandoned
	// relabels whose copies were not swept.
	PendingCorrections(ctx context.Context, after string, limit int) ([]Correction, error)
	// MarkSwept records that an abandoned relabel's copies were removed.
	MarkSwept(ctx context.Context, id string) error
	// SweepableScans answers, by Series Instance UID, which scans of a
	// relabel no longer prepared may have their copies under the new labels
	// removed: those whose new path no live scan and no other correction
	// not ended holds.
	SweepableScans(ctx context.Context, id string) (map[string]bool, error)
	// DueCorrections returns, in order of PurgeAfter, at most limit
	// committed corrections whose PurgeAfter is not after now, unresolved
	// ones left out, then the purging ones a stop or a failure left.
	DueCorrections(ctx context.Context, now time.Time, limit int) ([]Correction, error)
	// AllowArrivals lets one value a purged delete holds (Correction.Holds)
	// arrive again. A delete not yet purged is a precondition failure, as is
	// a correction that is not a delete or holds nothing.
	AllowArrivals(ctx context.Context, id string, value HeldValue, reason string, now time.Time, audit master.AuditEntry,
		guard Guard) (Correction, error)
	// DeletedMatches answers which of what p names a delete not undone,
	// abandoned or allowed names.
	DeletedMatches(ctx context.Context, p DeletedProbe) (DeletedMatch, error)
	// HeldByCorrection answers the correction not ended, and the level,
	// that holds the scan path p or its study or series for arrivals, or
	// the zero value.
	HeldByCorrection(ctx context.Context, p label.Path, studyUID, seriesUID string) (CorrectionHold, error)
	// ResolveInstances answers each pair, in the order given.
	ResolveInstances(ctx context.Context, refs []InstanceRef) ([]ResolvedInstance, error)
	RelabelPointers
}

// The bounds of corrections.
const (
	// MaxStateReasonLength is the longest state reason, the contract's.
	MaxStateReasonLength = 256
	// MaxConfirmAge is how long a delete's confirm token holds.
	MaxConfirmAge = time.Hour
	// MaxPreviewJobs is the most jobs a preview names.
	MaxPreviewJobs = 100
	// MaxResolve is the most pairs one ResolveInstances answers.
	MaxResolve = 1000
	// MaxDeletedProbe is the most series or instances one DeletedMatches
	// looks up.
	MaxDeletedProbe = 1000
	// MaxGrace is the longest grace period, the contract's 90 days.
	MaxGrace = 90 * 24 * time.Hour
	// DefaultGrace is delete.grace when the tenant sets none.
	DefaultGrace = 7 * 24 * time.Hour
	// MaxClockStep is retention.max-clock-step's built-in default: how far
	// past the last trusted time the clock may read for an automatic purge
	// to run before any check of retention's clock stored the setting. Two
	// days lets a server be stopped over a weekend without an admin.
	MaxClockStep = 48 * time.Hour
	// MaxPurgeTargets is the most objects one PurgeTargets answers.
	MaxPurgeTargets = 1000
)

// OriginPerson is the origin of a correction a person asked for;
// OriginRetention of a delete a disposal of a retention rule made.
const (
	OriginPerson    = "person"
	OriginRetention = "retention"
)

// StateReasonUnresolved is the state reason of a correction found on a
// storage with no row.
const StateReasonUnresolved = "found on storage without its row"

// StateReasonFromRecord is the state reason of a correction a restore found
// prepared in its backup and moved to the state its record says
// (Restoring.AdoptCorrection).
const StateReasonFromRecord = "moved by a restore to the state its record says"

// StateReasonRestoreHeld is the state reason of a delete a restore kept
// committed as its backup holds it, unresolved until an admin confirms it
// (Restoring.HoldCommittedDeletes): a record lagging an undo made after the
// backup cannot be told apart, and a purge cannot be undone.
const StateReasonRestoreHeld = "kept by a restore: undo it, or purge it to confirm it"

// CorrectionTarget names what a correction acts on, by labels and, for one
// instance, its SOP Instance UID.
type CorrectionTarget struct {
	// Subject is the subject's label; required.
	Subject string
	// Session and Scan are the session's and scan's labels, empty for the
	// level above.
	Session, Scan string
	// Instance is one instance's SOP Instance UID; empty for the whole
	// scan.
	Instance string
}

// Path returns the target's labels.
func (t CorrectionTarget) Path() label.Path {
	return label.Path{Subject: t.Subject, Session: t.Session, Scan: t.Scan}
}

// Level returns the level the target names (record.LevelSubject and the
// others).
func (t CorrectionTarget) Level() string {
	switch {
	case t.Instance != "":
		return record.LevelInstance
	case t.Scan != "":
		return record.LevelScan
	case t.Session != "":
		return record.LevelSession
	}
	return record.LevelSubject
}

// CorrectionCounts are what a correction covers and how far it got.
type CorrectionCounts struct {
	// Subjects, Sessions, Scans and Instances are what it covers, Bytes
	// those instances' bytes.
	Subjects, Sessions, Scans, Instances, Bytes int64
	// ObjectsRemoved and BytesRemoved count what its purge recorded.
	ObjectsRemoved, BytesRemoved int64
	// ObjectsCopied counts what a relabel copied.
	ObjectsCopied int64
	// ObjectsSuperseded counts what a relabel or replace keeps until its
	// purge.
	ObjectsSuperseded int64
	// ObjectsKept counts the objects its purge left because a live row
	// resolves to them or their bytes changed since the publish;
	// ObjectsOwned counts every object it owns (PurgeTargets), settled or
	// not.
	ObjectsKept, ObjectsOwned int64
}

// PeerCopy is one sender a correction's data was sent to.
type PeerCopy struct {
	// Sender is the sender's name.
	Sender string
	// Instances counts the instances it answered sent.
	Instances int64
	// LastSend is when the last of its jobs that sent any changed state.
	LastSend time.Time
}

// DeletePreview is what a delete would remove.
type DeletePreview struct {
	// Target is what was asked, and Level what the delete removes: the
	// target's, or a scan's for an instance that is its scan's last.
	Target CorrectionTarget
	// Counts is what it covers.
	Counts CorrectionCounts
	// Storages are the storages that hold any of it, by name.
	Storages []string
	// Replicas are the replicas of those storages, by primary then name:
	// the purge removes the data from each of them too.
	Replicas []ReplicaCopy
	// SentTo are the senders any of it was sent to, by name: for one
	// instance, those its scan was sent to.
	SentTo []PeerCopy
	// Jobs are the ids of at most MaxPreviewJobs jobs not ended that name
	// its subject (and its session and scan, when they name one), newest
	// first; JobsNotListed counts the rest. TenantJobs counts the jobs not
	// ended over the whole tenant, which may reach it too; they start and
	// end without changing the token.
	Jobs          []string
	JobsNotListed int64
	TenantJobs    int64
	// Quarantined and Receptions count what its purge clears besides its
	// objects: the quarantined files of its target, and the receptions of
	// its sessions whose labels and study it blanks.
	Quarantined, Receptions int64
	// HeldAfterPurge are the subject labels arrivals stay held under after
	// the purge: a subject's label and its labels before a relabel.
	HeldAfterPurge []string
	// Holds are the ids of the active holds that cover any of it, in
	// order; a delete of it is refused while any is.
	Holds []string
	// SessionsUnderMinimum counts its sessions a rule keeps at least until
	// a time to come, KeepUntil the latest such time (zero for none): a
	// person's delete of them needs OverrideRetention.
	SessionsUnderMinimum int64
	KeepUntil            time.Time
	// Issued is when the preview was made; Confirm its token.
	Issued  time.Time
	Confirm string
}

// CorrectionPrepare is a correction to record, prepared.
type CorrectionPrepare struct {
	// ID is its operation id (ValidateCorrectionID).
	ID string
	// Kind is what it does.
	Kind record.Kind
	// Target is what it acts on: for a relabel a subject, session or scan;
	// for a replace, filled by the store from the quarantined file.
	Target CorrectionTarget
	// ToLabel is a relabel's new label.
	ToLabel string
	// Quarantine is a replace's quarantined file; NewSHA256 and NewSize the
	// bytes it puts (after de-identification); RestoreLine the instance's
	// line of the scan's metadata as it was, kept while it is prepared so
	// a stop can write it back.
	Quarantine  string
	NewSHA256   SHA256
	NewSize     int64
	RestoreLine []byte
	// Reason is the person's reason; Confirm a delete's token.
	Reason, Confirm string
	// KeyID is the key that asked, empty for the local operator.
	KeyID string
	// Retention, for a delete a disposal makes (origin retention), is the
	// rule and due time it was listed under; nil for a person's.
	Retention *RetentionOrigin
	// OverrideRetention says a person's delete goes over a rule's
	// keep-at-least, with its own OverrideReason. Who may ask it is
	// checked before the store; the store records it and KeyID.
	OverrideRetention bool
	OverrideReason    string
	// Now is when.
	Now time.Time
}

// CorrectionCommit is a correction to publish.
type CorrectionCommit struct {
	// ID names it.
	ID string
	// Now is the publishing time, Grace the tenant's delete.grace then.
	Now   time.Time
	Grace time.Duration
	// Manifests are, by Series Instance UID, the SHA-256 of the manifest a
	// relabel or replace wrote for each scan it touches.
	Manifests map[string]SHA256
	// Sidecars are, by Series Instance UID, the SHA-256 of the metadata
	// file written with the manifest of Manifests, kept with it
	// (CatalogScan.SidecarSHA256). A scan whose manifest is given without
	// its sidecar's digest keeps none, and its metadata file is not checked
	// against one.
	Sidecars map[string]SHA256
	// Snapshots are, by Series Instance UID, what a delete of scans or a
	// relabel read of each scan's manifest and metadata under the old labels
	// before the publish: the objects its purge may remove, and only while
	// their bytes are these.
	Snapshots map[string]ScanSnapshot
	// ReplicaManifests are, by replica then Series Instance UID, the
	// SHA-256 of the manifest a relabel wrote under the new labels on a
	// replica it found in step for the scan and copied to before the
	// publish. A replica's row of such a scan stays in step, at this
	// digest, when it is the scan's new manifest digest and the row is in
	// step still; every other replica's row is queued (the replicator
	// copies the scan under its new labels).
	ReplicaManifests map[string]map[string]SHA256
}

// CorrectionScan is a scan a correction touches.
type CorrectionScan struct {
	// Storage is the storage it is on.
	Storage string
	// Path is its labels when the correction was made; NewPath a
	// relabel's, the same as Path otherwise.
	Path, NewPath label.Path
	// StudyUID and SeriesUID name it.
	StudyUID, SeriesUID string
	// Instances and Bytes are what it held.
	Instances, Bytes int64
}

// Disposition is what a storage did with an object a purge removed.
type Disposition string

// The dispositions, the contract's ObjectDisposition.
const (
	DispositionRemoved  Disposition = "removed"
	DispositionHidden   Disposition = "hidden"
	DispositionRetained Disposition = "retained"
	DispositionUnknown  Disposition = "unknown"
)

// PurgedObject is one object a purge removed.
type PurgedObject struct {
	// ID is the correction's.
	ID string
	// Storage and Key name the object; Version its version on a versioned
	// bucket, "" for none.
	Storage, Key, Version string
	// Size and SHA256 are what the manifest or the catalog held of it.
	Size   int64
	SHA256 SHA256
	// Disposition is what the storage did; RetainUntil when its retention
	// ends, when it said.
	Disposition Disposition
	RetainUntil time.Time
	// Now is when.
	Now time.Time
}

// PurgeFinish ends a purge.
type PurgeFinish struct {
	// ID names the correction; Now is when.
	ID  string
	Now time.Time
	// Manifest is, for an instance delete, the SHA-256 of the scan's
	// manifest written without the instance, and ManifestBefore that of the
	// manifest it was made from: the scan takes Manifest only while the
	// catalog still holds ManifestBefore, so a reception that wrote the
	// scan since keeps its own. ManifestBefore is zero when the rewrite
	// changed nothing (a purge run again after a stop found the manifest
	// already without the instance): the scan then takes Manifest.
	Manifest, ManifestBefore SHA256
	// Audit is the server's entry for the purge's end, or nil.
	Audit *master.AuditEntry
}

// StorageDisposition sums what one storage did with a correction's purged
// objects, at one place: a purge that spans a switch has one per storage
// and switch generation.
type StorageDisposition struct {
	// Storage is its name.
	Storage string
	// SwitchGeneration is the storage's switch generation when the purge
	// acted on it; ReplicaOf names the primary it was a replica of then,
	// empty for a primary.
	SwitchGeneration int64
	ReplicaOf        string
	// Removed, Hidden, Retained and Unknown count objects by disposition.
	Removed, Hidden, Retained, Unknown int64
	// VersionsRemoved counts versions deleted by version id.
	VersionsRemoved int64
	// RetainUntil is the latest retention end a storage stated.
	RetainUntil time.Time
}

// Correction is one correction and how far it got.
type Correction struct {
	// ID is its operation id; Kind what it does; State where it is.
	ID    string
	Kind  record.Kind
	State record.State
	// Level is what a delete removes (possibly above its target's level,
	// for a last instance), a relabel's level, or instance for a replace.
	Level string
	// Target is what it acts on, by the labels it had.
	Target CorrectionTarget
	// ToLabel is a relabel's new label; Quarantine a replace's file.
	ToLabel, Quarantine string
	// Reason, KeyID and Origin say who asked and why.
	Reason, KeyID, Origin string
	// Created, CommitTime, PurgeAfter and Ended are its times; zero when not
	// reached.
	Created, CommitTime, PurgeAfter, Ended time.Time
	// StateReason says why it is not moving or was abandoned.
	StateReason string
	// RecordsState is the state its records were last written with.
	RecordsState record.State
	// OldRevision, NewRevision, OldSHA256, NewSHA256 are a replace's;
	// ReceivedSHA256 and NewSize the new file's; InstanceKey and
	// InstanceSize the old revision's object, or an instance delete's.
	OldRevision, NewRevision    int
	OldSHA256, NewSHA256        SHA256
	ReceivedSHA256              SHA256
	NewSize                     int64
	InstanceKey                 string
	InstanceSize                int64
	ArrivalsAllowed, Unresolved bool
	UndoReason                  string
	Counts                      CorrectionCounts
	Storages                    []string
	Dispositions                []StorageDisposition
	// CopiesAbandoned are the replicas detached with abandon_purges while
	// the correction counted them among its copies, by name: their objects
	// not removed by then are recorded unknown, and the bytes may be there.
	CopiesAbandoned []string
	// RecordsOwed are the replicas still owed its record, by name.
	RecordsOwed []string
	RestoreLine []byte
	// Swept says an abandoned relabel's copies were removed.
	Swept bool
	// Holds are the values a delete holds arrivals by, read with one
	// correction (Correction), not with a list.
	Holds []HeldValue
	// RetentionRule and RetentionDue are, for origin retention, the rule
	// and the due time its session was listed under.
	RetentionRule string
	RetentionDue  time.Time
	// OverrideRetention says the delete went over a rule's keep-at-least,
	// for OverrideReason.
	OverrideRetention bool
	OverrideReason    string
	// CopiesInFlightUntil is, once its start or its delete's publish ended
	// copies to a replica of its scans that were under way, the last moment
	// a put of one of them can land; its purge removes nothing before it.
	// Zero when it ended none ("Every copy").
	CopiesInFlightUntil time.Time
}

// HasEnded reports whether the correction reached an end.
func (c Correction) HasEnded() bool {
	return c.State == record.StatePurged || c.State == record.StateUndone || c.State == record.StateAbandoned
}

// CorrectionFilter keeps the corrections a list answers.
type CorrectionFilter struct {
	// Kinds and States keep those of these; empty keeps all.
	Kinds  []record.Kind
	States []record.State
}

// DeletedProbe is what an arrival names, to match against deletes.
type DeletedProbe struct {
	// Subject is the subject label it would be archived under, "" for
	// none known.
	Subject string
	// StudyUID is its study.
	StudyUID string
	// Series and Instances are its series' and instances' UIDs, at most
	// MaxDeletedProbe each.
	Series, Instances []string
}

// DeletedMatch is which of a probe a delete names, each with the delete's
// id; empty and absent for none.
type DeletedMatch struct {
	// Subject and Study are the delete that names the probe's subject
	// label and study.
	Subject, Study string
	// Series and Instances are, by UID, the delete that names each.
	Series, Instances map[string]string
}

// Of returns the id of a delete that matches a file of that series and
// instance (or its study or subject), or "".
func (m DeletedMatch) Of(series, instance string) string {
	switch {
	case m.Instances[instance] != "":
		return m.Instances[instance]
	case m.Series[series] != "":
		return m.Series[series]
	case m.Study != "":
		return m.Study
	}
	return m.Subject
}

// InstanceRef is an instance and one of its revisions.
type InstanceRef struct {
	// UID is its SOP Instance UID; Revision from 1.
	UID      string
	Revision int
}

// The answers of ResolveInstances.
const (
	ResolvedCurrent    = "current"
	ResolvedSuperseded = "superseded"
	ResolvedDeleted    = "deleted"
	ResolvedUnknown    = "unknown"
)

// ResolvedInstance is what became of one pair.
type ResolvedInstance struct {
	// Ref is the pair asked.
	Ref InstanceRef
	// Disposition is one of the Resolved words.
	Disposition string
	// CurrentRevision is the instance's current revision, when held.
	CurrentRevision int
	// SHA256 is the asked revision's checksum, when known.
	SHA256 SHA256
	// Correction is the correction that superseded or deleted it.
	Correction string
	// BytesKept says the asked revision's bytes are still stored.
	BytesKept bool
	// Path is the instance's scan, by its labels now, for current and
	// superseded.
	Path label.Path
}
