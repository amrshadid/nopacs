package tenant

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// Scrubbing is what the tenant's database keeps for scrubs: the findings a
// scrub believed, and the marks on the instances whose bytes are missing or
// damaged. A scrub job itself is a job (Job, kind JobScrub), made with
// CreateJob and worked through JobWork like a deid job: a listing unit that
// queues one unit per scan of its storage, then each scan's unit, fenced by
// its attempt, its counts and tallies written by RecordJobProgress and
// EndJobScan.
//
// # Findings
//
// A finding is one object of one storage found wrong in one way
// (ScrubFindingKind). At most one finding holds per object and kind: one
// found again by a later scrub is the same finding, seen again (LastJob,
// LastSeen), so the findings that hold are counted once whatever number of
// scrubs found them. A finding is recorded by the unit that found it, fenced
// by its lease, in one transaction with its job's tally "finding|<kind>"
// (counted once per job and finding, so a write sent again counts nothing
// twice), the tally "orphan_objects" for an orphan, the mark of its
// instance for a missing or damaged one, and an EventScrubFinding. A
// finding read good later is cleared by a unit of a later scrub, in one
// transaction with the job's tally "cleared", the removal of its instance's
// mark (when this finding made it), and its event; its row stays, with when
// and by which job.
//
// # Marks
//
// An instance is marked while a finding of kind missing or damaged holds of
// it. The catalog's reads of instances answer the mark (ScanInstance.Damaged),
// so every reader of archived bytes fails it as data loss before its first
// byte, without reading the storage: a download, a read of files, a
// DICOMweb retrieve of an instance or a frame, a send. Nothing is deleted,
// moved or rewritten on a storage by any of this.
//
// # Keys
//
// A finding's key is the object's key on its storage, which holds a SOP
// Instance UID as received: the store keeps it in its own rows only, and no
// error, log or event carries it. A list's cursor holds a finding's id,
// never its key.
type Scrubbing interface {
	// RecordScrubFinding records what f says the unit of the fence found,
	// unless a finding of the same storage, key and kind holds already, in
	// which case it is seen again by the unit's job; it returns the finding
	// as it is after, and whether this call made it. The unit must be one of
	// a scrub job's (a precondition failure otherwise); a fence that no
	// longer holds is ErrLeaseLost. A finding of a kind that marks
	// (ScrubFindingKind.Marks) with an instance marks that instance, unless
	// another finding marks it already. f.ID is the id a new finding takes
	// (ScrubFindingID), unless a finding cleared already has that id (the
	// same job found the object wrong again after a later attempt read it
	// good): then it takes ScrubFindingIDAgain(f.ID, the fence), so a unit
	// never fails on an id its own job used.
	RecordScrubFinding(ctx context.Context, f ScrubFindingRecord) (ScrubFinding, bool, error)
	// ClearScrubFinding clears the finding of that id, which the unit of the
	// fence read good again, and removes the mark it made; it returns the
	// finding as it is after, and whether this call cleared it. A finding
	// cleared already is answered as it stands; one the tenant does not have
	// is NotFound; a unit not of a scrub job is a precondition failure.
	ClearScrubFinding(ctx context.Context, fence WorkFence, id string, now time.Time) (ScrubFinding, bool, error)
	// ClearObjectFindings clears, in one transaction, every finding of the
	// kinds given that holds of the object at key on storage, which the unit
	// of the fence read good (or, for a kind it no longer is, read as
	// something else), and removes the marks they made; it returns how many
	// it cleared. What a scan's unit calls on an instance it reads whole
	// while the catalog answers it marked, so the mark goes with the read
	// that disproves it, whatever the unit's cursor and however many
	// findings the scan has. A unit not of a scrub job is a precondition
	// failure.
	ClearObjectFindings(ctx context.Context, fence WorkFence, storage, key string, kinds []ScrubFindingKind, now time.Time) (int, error)
	// ScanScrubFindings returns the findings that hold of the scan at path
	// on storage and are about its files rather than its instances' bytes
	// (kinds that do not mark), at most MaxScanFindings, by key then kind,
	// after the key and kind given ("" for the first page): what a scan's
	// unit reads, page by page, to clear those it reads good again at the
	// scan's end. A finding of a kind that marks is cleared by the read of
	// its instance (ClearObjectFindings).
	ScanScrubFindings(ctx context.Context, storage string, path label.Path, afterKey string, afterKind ScrubFindingKind) ([]ScrubFinding, error)
	// AddScrubTallies adds to the tallies of the scrub job whose unit holds
	// the fence (a job not taking progress any more adds nothing): for what
	// a listing unit finds, which rides on no instance's write. A unit not
	// of a scrub job is a precondition failure.
	AddScrubTallies(ctx context.Context, fence WorkFence, tallies map[string]int64) error
	// LastScrub returns the scrub job of the whole storage named, or of
	// every storage, made last, and whether there is one: the scrub a
	// scheduled one follows. A scrub of one scan is never it, so a storage
	// whose reads ask for a scan's scrub each interval is still scrubbed
	// whole on time.
	LastScrub(ctx context.Context, storage string) (Job, bool, error)
	// ListScrubFindings returns a page of the findings the filter keeps, by
	// storage, then key, then kind, then id.
	ListScrubFindings(ctx context.Context, filter ScrubFindingFilter, page master.Page) ([]ScrubFinding, string, error)
	// OpenScrubFindings counts the findings that hold, by kind; a kind
	// with none is absent.
	OpenScrubFindings(ctx context.Context) (map[ScrubFindingKind]int64, error)
	// DamagedInstances returns, of the SOP Instance UIDs given (at most
	// MaxDamageLookup), those marked, each with the id of the finding that
	// marked it.
	DamagedInstances(ctx context.Context, uids []string) (map[string]string, error)
	// VerificationRecorder records when each scan was last read whole on
	// each storage, and VerificationSource answers it: the ground of a
	// storage's coverage.
	VerificationRecorder
	VerificationSource
}

// JobScrub is the kind of a scrub job (scrub.go).
const JobScrub JobKind = "scrub"

// ScrubWorkKind is the kind of every unit of work of the tenant's scrub
// jobs: one kind, since a scrub waits for no sender.
const ScrubWorkKind = "scrub-jobs"

// ScrubFindingKind is what a scrub found wrong with a stored object, the
// contract's ScrubFindingKind in lowercase without its prefix.
type ScrubFindingKind string

// The kinds.
const (
	// FindingMissing is an object the catalog and the manifest name that
	// the storage does not hold.
	FindingMissing ScrubFindingKind = "missing"
	// FindingOrphan is an object under the tenant's keys that no scan, no
	// manifest and no claim names.
	FindingOrphan ScrubFindingKind = "orphan"
	// FindingDamaged is an object whose length or SHA-256 is not what was
	// archived.
	FindingDamaged ScrubFindingKind = "damaged"
	// FindingDisagreeing is a scan whose manifest or sidecar is whole but
	// says of an instance what the catalog does not.
	FindingDisagreeing ScrubFindingKind = "disagreeing"
	// FindingManifestDamaged is a scan's manifest missing, unreadable, or
	// not of the SHA-256 the catalog holds.
	FindingManifestDamaged ScrubFindingKind = "manifest_damaged"
)

// ScrubFindingKinds returns every kind, in the contract's order.
func ScrubFindingKinds() []ScrubFindingKind {
	return []ScrubFindingKind{FindingMissing, FindingOrphan, FindingDamaged, FindingDisagreeing, FindingManifestDamaged}
}

// Valid reports whether k is one of ScrubFindingKinds.
func (k ScrubFindingKind) Valid() bool { return slices.Contains(ScrubFindingKinds(), k) }

// Marks reports whether a finding of kind k marks its instance: its bytes
// cannot be answered as archived.
func (k ScrubFindingKind) Marks() bool { return k == FindingMissing || k == FindingDamaged }

// ScrubTally returns the tally key a job counts a finding of kind k under.
func ScrubTally(k ScrubFindingKind) string { return "finding|" + string(k) }

// The other tallies of a scrub job, the contract's.
const (
	// TallyCleared counts the findings a job cleared.
	TallyCleared = "cleared"
	// TallyOrphanObjects counts the objects no scan names.
	TallyOrphanObjects = "orphan_objects"
	// TallyNotOnline counts the objects the storage answered as not
	// readable at once (held in a tier it cannot read now).
	TallyNotOnline = "unreadable|not_online"
	// TallySidecarMissing counts the scans whose metadata file is not on
	// their storage: a scan archived before metadata files were written has
	// none, which is not a finding.
	TallySidecarMissing = "sidecar|missing"
	// TallyStorageUnreadable counts the storages whose keys could not be
	// listed for objects outside every scan: that look was not made on
	// them in this job.
	TallyStorageUnreadable = "unreadable|storage"
	// TallyCorrectionRecordOnly counts the correction records on a storage
	// whose correction the tenant's database holds no row of: each explains
	// only objects nothing names, never a difference.
	TallyCorrectionRecordOnly = "correction|record_only"
	// TallyCorrectionUnauthenticated counts those of them in the form whose
	// MAC is missing or does not verify under the installation key for the
	// tenant: each explains nothing.
	TallyCorrectionUnauthenticated = "correction|unauthenticated"
)

// The bounds of scrubs.
const (
	// MaxScrubKeyLength is the longest key a finding names, the contract's.
	MaxScrubKeyLength = 1024
	// MaxScanFindings is the most findings ScanScrubFindings answers.
	MaxScanFindings = 1000
	// MaxDamageLookup is the most UIDs one DamagedInstances looks up.
	MaxDamageLookup = 1000
	// MaxFindingKinds is the most kinds a list's filter names, the
	// contract's.
	MaxFindingKinds = 16
)

// ScrubFinding is one difference a scrub found and believed.
type ScrubFinding struct {
	// ID names it, unique in the tenant.
	ID string
	// Kind is what was found.
	Kind ScrubFindingKind
	// Storage is the storage the object is on; Key its key there, at most
	// MaxScrubKeyLength bytes.
	Storage, Key string
	// Path is the scan it belongs to; zero for an orphan whose key names no
	// scan the catalog holds.
	Path label.Path
	// SOPInstanceUID is the instance of an instance's object, as the
	// catalog holds it; empty for a manifest, a sidecar or an orphan.
	SOPInstanceUID string
	// Revision is the revision the catalog or the manifest says the
	// instance's object holds; 0 for any other object.
	Revision int
	// ExpectedSize is the size the catalog or the manifest holds, nil for
	// an orphan; FoundSize the size found, nil for an object missing.
	ExpectedSize, FoundSize *int64
	// ExpectedSHA256 is the checksum the catalog or the manifest holds,
	// zero for an orphan; FoundSHA256 the checksum of the bytes found, zero
	// for an object missing.
	ExpectedSHA256, FoundSHA256 SHA256
	// Job is the scrub job that found it, Found when. Answered by a store.
	Job   string
	Found time.Time
	// LastJob is the latest job that found it, LastSeen when. Answered by a
	// store.
	LastJob  string
	LastSeen time.Time
	// Cleared is when a later scrub read the object good, ClearJob the job
	// that did; zero and empty while it holds. A finding of a replica's
	// damage is cleared with that damage, and its ClearJob is empty when
	// the replicator's verified copy cleared it rather than a scrub.
	// Answered by a store.
	Cleared  time.Time
	ClearJob string
	// ReplicaOf names, when Storage was a replica when the finding was
	// made, its primary; empty for a primary. A finding on a replica never
	// marks its instance: reads keep going to the primary, and the
	// replicator copies the object again from it. Answered by a store.
	ReplicaOf string
}

// Holds reports whether the finding holds: no scrub cleared it.
func (f ScrubFinding) Holds() bool { return f.Cleared.IsZero() }

// ScrubFindingRecord is a finding a unit records (RecordScrubFinding).
type ScrubFindingRecord struct {
	// Fence is the unit's lease; its job is the finding's.
	Fence WorkFence
	// Finding is what was found; its Job, Found, LastJob, LastSeen,
	// Cleared and ClearJob are the store's and ignored.
	Finding ScrubFinding
	// Now is when it was found.
	Now time.Time
}

// ScrubFindingID returns the id a finding of job takes when it is new: made
// from the job, the storage, the key and the kind, so a write sent again
// names the same finding.
func ScrubFindingID(job, storage, key string, kind ScrubFindingKind) string {
	return "sf-" + digest(job+"\x00"+storage+"\x00"+key+"\x00"+string(kind))
}

// ScrubFindingIDAgain returns the id a new finding takes when the id it was
// given (ScrubFindingID) names a finding cleared already: made from that id
// and the unit's attempt, so a write sent again by the same attempt names
// the same finding, and a later attempt that finds the object wrong once
// more makes a finding of its own.
func ScrubFindingIDAgain(id string, fence WorkFence) string {
	return "sf-" + digest(id+"\x00"+fence.ID+"\x00"+strconv.Itoa(fence.Attempt))
}

// ValidateScrubTallies returns an Invalid error unless t is a batch
// AddScrubTallies takes: at least one tally, each as a progress write's.
func ValidateScrubTallies(t map[string]int64) error {
	if len(t) == 0 {
		return invalid("a write of tallies adds to at least one")
	}
	return validateTallies(t)
}

// ValidateFindingKey returns an Invalid error unless key is a key a finding
// can name.
func ValidateFindingKey(key string) error {
	return validateText("finding key", key, 1, MaxScrubKeyLength)
}

// ValidateObjectKinds returns an Invalid error unless kinds are at least one
// valid kind, each once.
func ValidateObjectKinds(kinds []ScrubFindingKind) error {
	if len(kinds) == 0 || len(kinds) > len(ScrubFindingKinds()) {
		return invalid("a clear of an object names one to five finding kinds")
	}
	for i, k := range kinds {
		if !k.Valid() || slices.Contains(kinds[:i], k) {
			return invalid("a clear of an object names each finding kind once")
		}
	}
	return nil
}

// Validate returns an Invalid error unless r can be recorded.
func (r ScrubFindingRecord) Validate() error {
	if err := r.Fence.Validate(); err != nil {
		return err
	}
	f := r.Finding
	if err := master.ValidateID("finding id", f.ID); err != nil {
		return err
	}
	if !f.Kind.Valid() {
		return invalid("a finding's kind is one of missing, orphan, damaged, disagreeing, manifest_damaged")
	}
	if err := ValidateName("storage", f.Storage); err != nil {
		return err
	}
	if err := validateText("finding key", f.Key, 1, MaxScrubKeyLength); err != nil {
		return err
	}
	if f.Path != (label.Path{}) {
		if err := ValidateScanPath(f.Path); err != nil {
			return err
		}
	} else if f.Kind != FindingOrphan {
		return invalid("only an orphan's finding names no scan")
	}
	if f.SOPInstanceUID != "" {
		if err := ValidateInstanceUID(f.SOPInstanceUID); err != nil {
			return err
		}
		if f.Path == (label.Path{}) {
			return invalid("a finding of an instance names its scan")
		}
	}
	if f.Kind.Marks() && f.ExpectedSize == nil {
		return invalid("a missing or damaged object's finding states the size that was archived")
	}
	if f.Revision < 0 || f.Revision > maxFindingRevision {
		return invalid("a finding's revision is 0 to " + strconv.Itoa(maxFindingRevision))
	}
	for _, n := range []*int64{f.ExpectedSize, f.FoundSize} {
		if n != nil && *n < 0 {
			return invalid("a finding's sizes are not negative")
		}
	}
	switch f.Kind {
	case FindingMissing:
		if f.FoundSize != nil || !f.FoundSHA256.IsZero() {
			return invalid("a missing object's finding states nothing found")
		}
	case FindingOrphan:
		if f.ExpectedSize != nil || !f.ExpectedSHA256.IsZero() || f.SOPInstanceUID != "" || f.Revision != 0 {
			return invalid("an orphan's finding states nothing expected and no instance")
		}
	}
	return master.ValidateTime("finding time", r.Now)
}

// maxFindingRevision is the highest revision a finding names: the layout's.
const maxFindingRevision = 1000000

// ScrubFindingFilter keeps the findings a list answers; every filter given
// must hold.
type ScrubFindingFilter struct {
	// Kinds keeps the findings of these kinds, at most MaxFindingKinds;
	// empty keeps every kind.
	Kinds []ScrubFindingKind
	// Storage keeps the findings on the storage of this name.
	Storage string
	// Path, when not zero, keeps the findings of the scan with these
	// labels.
	Path label.Path
	// IncludeCleared keeps the findings cleared too; false keeps only those
	// that hold.
	IncludeCleared bool
}

// Validate returns an Invalid error unless f can be applied.
func (f ScrubFindingFilter) Validate() error {
	if len(f.Kinds) > MaxFindingKinds {
		return invalid("a filter names at most " + strconv.Itoa(MaxFindingKinds) + " finding kinds")
	}
	for _, k := range f.Kinds {
		if !k.Valid() {
			return invalid("a finding's kind is one of missing, orphan, damaged, disagreeing, manifest_damaged")
		}
	}
	if f.Path != (label.Path{}) {
		if err := ValidateScanPath(f.Path); err != nil {
			return err
		}
	}
	if f.Storage != "" {
		return ValidateName("storage", f.Storage)
	}
	return nil
}

// ValidateDamageLookup returns an Invalid error unless uids is a lookup
// DamagedInstances takes.
func ValidateDamageLookup(uids []string) error {
	if len(uids) > MaxDamageLookup {
		return invalid("at most " + strconv.Itoa(MaxDamageLookup) + " instances are looked up at once")
	}
	for _, u := range uids {
		if err := ValidateInstanceUID(u); err != nil {
			return err
		}
	}
	return nil
}

// validateScrubJob returns an Invalid error unless a scrub job's fields can
// be made: no sender, mode or profile; a storage by its name, or none for
// every storage; the whole tenant, or one scan by its labels. A scrub of
// one scan reads only that scan, on the storage named or on every copy:
// what a request to scrub one instance makes, so a read that found its
// bytes wrong costs a scan's read, not a whole storage's.
func validateScrubJob(j NewJob) error {
	if j.Sender != "" || j.Mode != "" || !j.Profile.IsZero() {
		return invalid("a scrub job names no sender, mode or profile")
	}
	if t := j.Target; !t.Whole() {
		if t.ScanLabel == "" {
			return invalid("a scrub job names the whole tenant or one scan")
		}
		if err := t.Validate(); err != nil {
			return err
		}
	}
	if j.Storage != "" {
		return ValidateName("storage", j.Storage)
	}
	return nil
}

// ScrubFindingChanged is a scrub finding recorded or cleared, the
// contract's message of that name. It names the scan by labels, never the
// object's key or the instance's UID.
type ScrubFindingChanged struct {
	// Finding is the finding's id.
	Finding string `json:"finding"`
	// Kind is what was found.
	Kind ScrubFindingKind `json:"kind"`
	// Storage is the storage the object is on.
	Storage string `json:"storage"`
	// SubjectLabel, SessionLabel and ScanLabel place the scan; empty for an
	// orphan that names none.
	SubjectLabel string `json:"subject_label"`
	// SessionLabel is the scan's session's label.
	SessionLabel string `json:"session_label"`
	// ScanLabel is the scan's label.
	ScanLabel string `json:"scan_label"`
	// Cleared says a later scrub read it good and cleared it.
	Cleared bool `json:"cleared"`
	// Job is the scrub job that recorded or cleared it.
	Job string `json:"job"`
}

// EventType implements EventPayload.
func (*ScrubFindingChanged) EventType() EventType { return EventScrubFinding }

// validate implements EventPayload.
func (p *ScrubFindingChanged) validate() error {
	if err := master.ValidateID("finding id", p.Finding); err != nil {
		return err
	}
	if !p.Kind.Valid() {
		return invalid("a finding's kind is one of missing, orphan, damaged, disagreeing, manifest_damaged")
	}
	if err := ValidateName("storage", p.Storage); err != nil {
		return err
	}
	if p.SubjectLabel != "" || p.SessionLabel != "" || p.ScanLabel != "" {
		if err := ValidateScanPath(label.Path{Subject: p.SubjectLabel, Session: p.SessionLabel, Scan: p.ScanLabel}); err != nil {
			return err
		}
	}
	return master.ValidateID("job id", p.Job)
}

// ScrubFindingEvent returns the event of finding f recorded, or cleared, by
// job, at the time given.
func ScrubFindingEvent(f ScrubFinding, cleared bool, job string, at time.Time) (EventRecord, error) {
	return NewEvent(&ScrubFindingChanged{Finding: f.ID, Kind: f.Kind, Storage: f.Storage, SubjectLabel: f.Path.Subject,
		SessionLabel: f.Path.Session, ScanLabel: f.Path.Scan, Cleared: cleared, Job: job}, at)
}
