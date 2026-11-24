package tenant

import (
	"context"
	"errors"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Rebuilding is what the tenant's database does for a rebuild: the catalog
// made again from what the tenant's storages hold, into a shadow catalog
// beside the catalog in use, never into it. The shadow has the catalog's
// tables in the same shape (on SQLite a database file beside the tenant's,
// on Postgres the scratch schema the job names) and also holds how far
// its build got, written in the transaction that writes each scan's rows
// there, so a build stopped anywhere goes on from the last scan written and
// never touches the catalog in use.
//
// A rebuild job is a job (Job, kind JobRebuild), made with CreateJob, with
// one unit of work (kind RebuildWorkKind) that does the whole of it in
// order: StartRebuildUnit, PrepareShadow, RebuildScan for each scan of each
// storage, FinishRebuildStorage after each storage, then CompareCatalog
// and EndRebuild (compare), or ActivateCatalog (replace); last
// DropShadow. Every write is fenced by the unit's lease: a worker whose
// lease was taken over changes nothing.
//
// # Identical
//
// Two catalogs are identical when every catalog table holds the same rows,
// each row matched by what names it (a subject by its label, a session by
// its Study Instance UID, a scan by its Series Instance UID, an instance by
// its SOP Instance UID; a reference by integer id compared as the label or
// UID it points at) and every column equal but the integer ids, which
// differ between two databases. CompareCatalog counts the differences so.
//
// # Activation
//
// ActivateCatalog makes the shadow the catalog in use in one transaction:
// the rows of the catalog in use that the shadow does not hold are removed,
// those it holds take the shadow's values (keeping their integer ids, so a
// table that names a subject, session or scan by id still names the same
// one), and those only the shadow holds are added; the instances and the
// derived tables are copied whole. A reader sees the whole old catalog or
// the whole new one, never part of either. The feed's history id is made
// again in that transaction, and its first event is EventCatalogRebuilt.
type Rebuilding interface {
	// StartRebuildUnit says what the leased unit's rebuild job is and moves
	// it from queued to running (its event) at its first start. A unit that
	// is not a rebuild job's is a precondition failure. A job that has
	// ended is answered as it stands: the worker drops its shadow and
	// completes the unit.
	StartRebuildUnit(ctx context.Context, fence WorkFence, now time.Time) (Job, error)
	// PrepareShadow makes the shadow of the unit's job, empty, or finds the
	// one the job made before, and answers how far its build got, its
	// tables made in a transaction that also checks the unit's lease. A
	// shadow left by another job is removed first on SQLite. On Postgres the
	// scratch schema holds the mark CreateJob left in it (rebuild_state
	// naming the job), and must hold nothing else and grant nothing to
	// another role: otherwise the refusal holds ErrRebuildRefused, changes
	// nothing, and the worker ends the job (FailRebuild).
	PrepareShadow(ctx context.Context, fence WorkFence) (RebuildProgress, error)
	// RebuildScan writes into the shadow what w says of one scan (its rows,
	// or what was left out of it, or both) and moves the build's cursor, in
	// one transaction, and answers the progress after. A write whose From is
	// not the build's cursor is a precondition failure that changes nothing
	// (ErrRebuildMoved). A label held by another row of the shadow is a
	// LabelConflict, a series held by a scan of another session holds
	// ErrSeriesElsewhere, an instance held by another scan holds
	// ErrInstanceElsewhere (each with ErrOtherStorage too when the scan
	// that holds it is on another storage), and a profile version the
	// tenant does not have holds ErrProfileUnknown: each a refusal that
	// writes nothing, and the worker leaves the scan out.
	RebuildScan(ctx context.Context, w RebuildScanWrite) (RebuildProgress, error)
	// ShadowHolder answers the storage of a scan of the unit's shadow that
	// holds one of the instances uids names (at most MaxRebuildBatch), or ""
	// when none does: the worker asks it of a scan's every instance before
	// the scan's first write, so a scan is never written in part and then
	// refused. It reads; the unit is the shadow's one writer, so what it
	// answers stays true while the lease holds.
	ShadowHolder(ctx context.Context, fence WorkFence, uids []string) (string, error)
	// FinishRebuildStorage ends the build of the storage the cursor is on,
	// adding tallies (what was found on that storage outside its scans,
	// such as its correction records) to the build's, and moves the cursor
	// to the storage next, or, when next is empty, marks
	// the build complete after giving every row its derived values (the
	// scans', sessions' and subjects' counts, times and totals, the
	// sessions' modalities, the receptions' scans, and the totals of the
	// catalog and of each storage), computed from the rows the shadow
	// holds. A storage other than the cursor's is a precondition failure.
	FinishRebuildStorage(ctx context.Context, fence WorkFence, storage, next string, tallies map[string]int64) (RebuildProgress, error)
	// CompareCatalog counts, table by table, the rows the shadow of the
	// unit's job holds that the catalog in use does not (added), those the
	// catalog holds that it does not (removed), and those both hold that
	// differ (changed), as Identical defines them, and answers up to
	// MaxCatalogDifferences of them by name. A build not complete is a
	// precondition failure. It reads; it is not fenced.
	CompareCatalog(ctx context.Context, fence WorkFence) (CatalogComparison, error)
	// EndRebuild ends the unit's compare job (EndState of its counts:
	// instances cataloged as sent, those left out as refused) with the
	// build's tallies and the comparison's (CatalogComparison.Tallies), keeps
	// the differences it names (RebuildDifferences), and writes its event.
	// It does not complete the unit: the worker drops the shadow first. A job
	// ended already is answered as it stands; a build not complete, and a job
	// of replace mode, which ends by its activation, are precondition
	// failures.
	EndRebuild(ctx context.Context, fence WorkFence, cmp CatalogComparison, now time.Time) (Job, error)
	// ActivateCatalog makes the shadow of the unit's replace job the
	// catalog in use, as Activation says, ends the job with the tally
	// TallyActivated, makes the feed's history id again, and writes
	// EventCatalogRebuilt and the job's event, in one transaction. A build
	// not complete, and a job of compare mode, are precondition failures. A
	// job activated already is answered as it stands. While a job not ended
	// names a scan (to send, resend or retry) that the shadow lacks or
	// changes, the activation is refused with ErrRebuildRefused, changing
	// nothing: that job would send what the catalog no longer says.
	ActivateCatalog(ctx context.Context, fence WorkFence, now time.Time) (Job, error)
	// FailRebuild ends the unit's rebuild job failed, with reason as its
	// reason, and its event: what the worker does with a refusal that holds
	// ErrRebuildRefused, which no attempt can get past. A job ended already
	// is answered as it stands. It does not complete the unit: the worker
	// drops the shadow first.
	FailRebuild(ctx context.Context, fence WorkFence, reason string, now time.Time) (Job, error)
	// RebuildDifferences answers the differences a compare job kept when it
	// ended (EndRebuild), at most MaxCatalogDifferences, in the order the
	// comparison found them. Their keys may hold UIDs: they are answered to
	// an admin, never logged.
	RebuildDifferences(ctx context.Context, jobID string) ([]CatalogDifference, error)
	// DropShadow removes the shadow of the rebuild job named, which has
	// ended: its tables on Postgres (never the schema, which is the
	// operator's), its file on SQLite. A shadow not there is nothing to
	// do. A job not ended is a precondition failure.
	DropShadow(ctx context.Context, jobID string) error
	// ShadowsLeft returns the rebuild jobs that have ended and whose shadow
	// DropShadow has not removed, at most MaxShadowsLeft, in order of id:
	// those a job left when it ended without its worker (its unit's
	// attempts ran out, and GiveUpJobUnit ended it failed; or it was
	// canceled while its unit was queued), which the worker then removes.
	ShadowsLeft(ctx context.Context) ([]string, error)
	// EventHistory answers the id of the event feed's history.
	EventHistory(ctx context.Context) (string, error)
	// RebuildReplacing reports whether a rebuild of replace mode is queued
	// or running: while it is, nothing should land in the catalog in use
	// that its shadow would miss.
	RebuildReplacing(ctx context.Context) (bool, error)
	// RebuildRunning reports whether a rebuild of either mode is queued or
	// running: while it is, the corrections write nothing to a storage or
	// to the catalog (the rule is the Corrections port's), since its build
	// reads both and would see a change made meanwhile torn or not at all.
	RebuildRunning(ctx context.Context) (bool, error)
}

// JobRebuild is the kind of a rebuild job (rebuild.go).
const JobRebuild JobKind = "rebuild"

// RebuildWorkKind is the kind of the unit of work of the tenant's rebuild
// jobs.
const RebuildWorkKind = "rebuild-jobs"

// RebuildMode is what a rebuild does with the shadow it builds, the
// contract's RebuildJobMode in lowercase without its prefix.
type RebuildMode string

// The modes.
const (
	// RebuildCompare compares the shadow with the catalog in use, then
	// drops it.
	RebuildCompare RebuildMode = "compare"
	// RebuildReplace makes the shadow the catalog in use.
	RebuildReplace RebuildMode = "replace"
)

// Valid reports whether m is one of the modes.
func (m RebuildMode) Valid() bool { return m == RebuildCompare || m == RebuildReplace }

// RebuildJob is what a rebuild job does.
type RebuildJob struct {
	// Mode is what it does with its shadow.
	Mode RebuildMode
	// Scratch is the Postgres schema its shadow is built in; empty on
	// SQLite.
	Scratch string
	// Activated says a replace made its shadow the catalog in use.
	// Answered by a store.
	Activated bool
}

// The tallies of a rebuild job, the contract's.
const (
	// TallyLeftOutProfileUnknown counts the instances of scans whose
	// metadata names a profile version the tenant does not have.
	TallyLeftOutProfileUnknown = "left_out|profile_unknown"
	// TallyLeftOutDeleted counts the instances a correction record hides.
	TallyLeftOutDeleted = "left_out|deleted"
	// TallyLeftOutUnreadable counts the instances of scans whose manifest
	// or metadata cannot be read, is of a form older than the one a
	// catalog is made from, or says another thing than the other.
	TallyLeftOutUnreadable = "left_out|unreadable"
	// TallyLeftOutLabelConflict counts the instances of scans whose labels
	// another row of the shadow holds.
	TallyLeftOutLabelConflict = "left_out|label_conflict"
	// TallyUnresolvedCorrection counts the correction records applied
	// without their row in the tenant's database: a delete as committed.
	TallyUnresolvedCorrection = "unresolved_correction"
	// TallyUnauthenticatedCorrection counts the correction records in the
	// form whose MAC is missing or does not verify under the installation
	// key for the tenant: each hides nothing.
	TallyUnauthenticatedCorrection = "unauthenticated_correction"
	// TallySeriesOnTwoStorages counts the scans left out because a scan of
	// another storage holds their series or one of their instances.
	TallySeriesOnTwoStorages = "series_on_two_storages"
	// TallyLeftOutHeldElsewhere counts the scans left out because another
	// scan of the same storage holds their series or one of their
	// instances: not two storages, but one storage that names an instance
	// twice.
	TallyLeftOutHeldElsewhere = "left_out|held_elsewhere"
	// TallyActivated is 1 once a replace made its shadow the catalog in use.
	TallyActivated = "activated"
)

// DifferTally returns the tally key of a compare's differences in table,
// of kind "added", "removed" or "changed".
func DifferTally(table, kind string) string { return "differs|" + table + "|" + kind }

// The bounds of rebuilds.
const (
	// MaxRebuildBatch is the most instances one RebuildScan writes.
	MaxRebuildBatch = 1000
	// MaxCatalogDifferences is the most differences CompareCatalog answers
	// by name, and a compare job keeps.
	MaxCatalogDifferences = 1000
	// MaxDifferenceKeyLength is the longest key of a difference: the UIDs
	// and label that name a row, joined.
	MaxDifferenceKeyLength = 1024
	// MaxScratchLength is the longest scratch schema's name, a Postgres
	// identifier's.
	MaxScratchLength = 63
	// MaxShadowsLeft is the most jobs one ShadowsLeft answers.
	MaxShadowsLeft = 100
)

// CatalogTables are the catalog's tables, those a rebuild makes again and
// compares, in the order a comparison answers them.
func CatalogTables() []string {
	return []string{"subjects", "sessions", "scans", "instances", "session_modalities", "reception_scans", "storage_totals",
		"catalog_totals"}
}

// RebuildCursor is where a build is: the storage it reads, the scans of it
// written whole, and the scan written in part.
type RebuildCursor struct {
	// Storage is the storage being read; empty before the first.
	Storage string
	// After is the key that orders the last scan of Storage written whole
	// or left out (the first key of the scan a listing of meta/ answers);
	// empty before the first.
	After string
	// Partial is the key of the scan written in part, PartialUID the SOP
	// Instance UID of the last instance of it written; both empty when no
	// scan is written in part.
	Partial, PartialUID string
}

// RebuildProgress is how far a build got.
type RebuildProgress struct {
	// Job is the rebuild job the shadow is of.
	Job string
	// Cursor is where it is.
	Cursor RebuildCursor
	// Complete says every storage was read and the derived values made.
	Complete bool
	// Scans counts the scans met; LeftOut and LeftOutBytes the instances
	// left out and their bytes. Instances and Bytes count those the shadow
	// holds, known once Complete.
	Scans, LeftOut, LeftOutBytes, Instances, Bytes int64
	// Tallies are the build's tallies so far.
	Tallies map[string]int64
}

// CatalogComparison is what CompareCatalog found.
type CatalogComparison struct {
	// Tables are the counts of each table that differs, by table.
	Tables map[string]TableDifference
	// Differences are some of the rows that differ, by name, at most
	// MaxCatalogDifferences.
	Differences []CatalogDifference
}

// Equal reports whether the comparison found no difference.
func (c CatalogComparison) Equal() bool { return len(c.Tables) == 0 }

// Tallies returns the comparison as a job's tallies (DifferTally).
func (c CatalogComparison) Tallies() map[string]int64 {
	out := map[string]int64{}
	for table, d := range c.Tables {
		for kind, n := range map[string]int64{"added": d.Added, "removed": d.Removed, "changed": d.Changed} {
			if n > 0 {
				out[DifferTally(table, kind)] = n
			}
		}
	}
	return out
}

// TableDifference counts how one table differs.
type TableDifference struct {
	// Added counts the rows only the shadow holds, Removed those only the
	// catalog in use holds, Changed those both hold that differ.
	Added, Removed, Changed int64
}

// CatalogDifference is one row that differs.
type CatalogDifference struct {
	// Table is its table; Kind "added", "removed" or "changed".
	Table, Kind string
	// Key is what names the row: a label, a UID, or several joined by "/".
	// It may hold UIDs, so it is never logged.
	Key string
}

// ValidateScratch returns an Invalid error unless s is a scratch schema's
// name the contract allows: 1 to MaxScratchLength bytes of a-z, 0-9 and
// "_", starting with a letter.
func ValidateScratch(s string) error {
	if s == "" || len(s) > MaxScratchLength || s[0] < 'a' || s[0] > 'z' {
		return invalid("a scratch schema is 1 to 63 bytes of a-z, 0-9 and _, starting with a letter")
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return invalid("a scratch schema is 1 to 63 bytes of a-z, 0-9 and _, starting with a letter")
		}
	}
	return nil
}

// validateRebuildJob returns an Invalid error unless a rebuild job's fields
// can be made: a mode, and no sender, profile, target or storage. Whether
// the scratch schema is required is the driver's: a Postgres store
// requires one and a SQLite store refuses one.
func validateRebuildJob(j NewJob) error {
	if j.Sender != "" || j.Mode != "" || !j.Profile.IsZero() || !j.Target.Whole() || j.Storage != "" || j.Scheduled {
		return invalid("a rebuild job names no sender, deid mode, profile, target or storage")
	}
	if j.Rebuild == nil || !j.Rebuild.Mode.Valid() {
		return invalid("a rebuild job's mode is compare or replace")
	}
	if j.Rebuild.Activated {
		return invalid("a rebuild job is made not activated")
	}
	if j.Rebuild.Scratch != "" {
		return ValidateScratch(j.Rebuild.Scratch)
	}
	return nil
}

// The errors of a rebuild, each in the chain of the refusal of a scan the
// worker leaves out, or of a write sent from a cursor the build has left.
var (
	// ErrRebuildMoved is a write whose From is not the build's cursor.
	ErrRebuildMoved = errors.New("the build's cursor has moved")
	// ErrProfileUnknown is a scan whose metadata names a profile version
	// the tenant does not have.
	ErrProfileUnknown = errors.New("the tenant has no profile version the scan's metadata names")
	// ErrInstanceElsewhere is an instance another scan of the shadow holds.
	ErrInstanceElsewhere = errors.New("another scan holds the instance")
	// ErrOtherStorage is beside ErrSeriesElsewhere or ErrInstanceElsewhere
	// when the scan that holds the series or the instance is on another
	// storage.
	ErrOtherStorage = errors.New("the scan that holds it is on another storage")
	// ErrRebuildRefused is a refusal no later attempt gets past: the job is
	// ended failed with its message (FailRebuild) rather than tried again.
	ErrRebuildRefused = errors.New("the rebuild cannot go on")
)

// RebuildMoved returns the refusal of a write sent from a cursor the build
// has left.
func RebuildMoved() error {
	return fault.Wrap(fault.Precondition, ErrRebuildMoved, "the build has moved on from the place this write follows")
}

// RebuildRefused returns a precondition failure with ErrRebuildRefused in
// its chain and message as what the caller, and the job's reason, read.
func RebuildRefused(message string) error {
	return fault.Wrap(fault.Precondition, ErrRebuildRefused, message)
}

// CatalogRebuilt is a catalog made again from the storages and put in use,
// the contract's message of that name.
type CatalogRebuilt struct {
	// Job is the rebuild job's id; empty for one a restore or an import ran.
	Job string `json:"job"`
	// Instances counts the instances the new catalog holds.
	Instances int64 `json:"instances"`
	// NewHistory says it begins a new history of the feed.
	NewHistory bool `json:"new_history"`
}

// EventType implements EventPayload.
func (*CatalogRebuilt) EventType() EventType { return EventCatalogRebuilt }

// validate implements EventPayload.
func (p *CatalogRebuilt) validate() error {
	if p.Job != "" {
		if err := master.ValidateID("job id", p.Job); err != nil {
			return err
		}
	}
	if p.Instances < 0 {
		return invalid("a rebuilt catalog's instances are not negative")
	}
	return nil
}

// ValidateEventHistory returns an Invalid error unless id is a history id
// as a store makes it: 32 lowercase hexadecimal digits.
func ValidateEventHistory(id string) error {
	if len(id) != 32 {
		return invalid("an event history id is 32 lowercase hexadecimal digits")
	}
	for i := 0; i < len(id); i++ {
		if c := id[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return invalid("an event history id is 32 lowercase hexadecimal digits")
		}
	}
	return nil
}
