package tenant

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/master"
)

// JobImport is the files of a directory under an import receiver's root
// handed to that receiver, one at a time, as files that arrived
// (import_port.go).
const JobImport JobKind = "import"

// ImportWorkKind is the work kind of every unit of an import job: one kind,
// so the server's bound on import units at once is one claim loop's.
const ImportWorkKind = "import"

// ImportSkipCode is why an import did not hand a file in, the contract's
// ImportSkipCode in lowercase without its prefix. Its text is what a store
// keeps.
type ImportSkipCode string

// The skip codes.
const (
	SkipNotPart10        ImportSkipCode = "not_part10"
	SkipOutsideLayout    ImportSkipCode = "outside_layout"
	SkipUnmappedSession  ImportSkipCode = "unmapped_session"
	SkipDirectoryTooWide ImportSkipCode = "directory_too_wide"
	SkipUnreadable       ImportSkipCode = "unreadable"
	SkipNotRegular       ImportSkipCode = "not_regular"
	SkipTooLarge         ImportSkipCode = "too_large"
	// SkipPathTooLong is an entry, file or directory, whose path relative
	// to the root is longer than importpath.MaxPathLength: not handed in,
	// not entered, and recorded under its deepest ancestor whose path fits
	// (importpath.Fitting), so its row's key fits a store's index.
	SkipPathTooLong ImportSkipCode = "path_too_long"
	// SkipBadName is an entry, file or directory, whose own name breaks
	// the rule of a segment (importpath.Check: "\", ":", not UTF-8, longer
	// than importpath.MaxSegmentLength): not handed in, not entered, and
	// recorded under the folder that holds it, the name never kept in a row.
	SkipBadName ImportSkipCode = "bad_name"
)

// ImportSkipCodes returns every skip code, in the contract's order.
func ImportSkipCodes() []ImportSkipCode {
	return []ImportSkipCode{SkipNotPart10, SkipOutsideLayout, SkipUnmappedSession, SkipDirectoryTooWide, SkipUnreadable,
		SkipNotRegular, SkipTooLarge, SkipPathTooLong, SkipBadName}
}

// Valid reports whether c is one of ImportSkipCodes.
func (c ImportSkipCode) Valid() bool { return slices.Contains(ImportSkipCodes(), c) }

// MayHideDICOM reports whether a file skipped with c may be DICOM the
// archive lacks (unreadable, too large, in a directory too wide, under a
// path too long), so its job ends partial; the others are skipped by
// design.
func (c ImportSkipCode) MayHideDICOM() bool {
	return c == SkipUnreadable || c == SkipTooLarge || c == SkipDirectoryTooWide || c == SkipPathTooLong || c == SkipBadName
}

// The details a skipped file's record may carry, a closed set: a phrase of
// the server's that names the bound a file passed, never the operating
// system's text, which can repeat a path.
const (
	// ImportDetailMaxDirEntries names the setting a directory too wide
	// passed.
	ImportDetailMaxDirEntries = "import.max-dir-entries"
	// ImportDetailMaxFileBytes names the receiver's bound a file too large
	// passed.
	ImportDetailMaxFileBytes = "max_file_bytes"
)

// importDetails is the closed set of details, the empty one among them.
var importDetails = []string{"", ImportDetailMaxDirEntries, ImportDetailMaxFileBytes}

// ImportFileKind is what an import job's listed file is listed for, the
// contract's ImportFileKind in lowercase without its prefix.
type ImportFileKind string

// The kinds.
const (
	ImportFileSkipped     ImportFileKind = "skipped"
	ImportFileQuarantined ImportFileKind = "quarantined"
	ImportFileLost        ImportFileKind = "lost"
	ImportFileFailed      ImportFileKind = "failed"
	ImportFileDiscarded   ImportFileKind = "discarded"
)

// ImportFileKinds returns every kind, in the contract's order.
func ImportFileKinds() []ImportFileKind {
	return []ImportFileKind{ImportFileSkipped, ImportFileQuarantined, ImportFileLost, ImportFileFailed, ImportFileDiscarded}
}

// ImportUnitState is where one unit of an import job is, the contract's
// ImportUnitState in lowercase without its prefix. A unit goes forward
// only.
type ImportUnitState string

// The unit states.
const (
	ImportUnitQueued   ImportUnitState = "queued"
	ImportUnitWalking  ImportUnitState = "walking"
	ImportUnitWalked   ImportUnitState = "walked"
	ImportUnitDone     ImportUnitState = "done"
	ImportUnitFailed   ImportUnitState = "failed"
	ImportUnitCanceled ImportUnitState = "canceled"
)

// ImportUnitStates returns every unit state, in the contract's order.
func ImportUnitStates() []ImportUnitState {
	return []ImportUnitState{ImportUnitQueued, ImportUnitWalking, ImportUnitWalked, ImportUnitDone, ImportUnitFailed,
		ImportUnitCanceled}
}

// WalkEnded reports whether a unit in s has ended its walk, whatever it
// found: walked, done, failed or canceled.
func (s ImportUnitState) WalkEnded() bool {
	return s != ImportUnitQueued && s != ImportUnitWalking
}

// The reasons a store writes on an import job or unit, from the contract's
// fixed set of phrases.
const (
	// ImportReasonSettling is a running job whose walks have all ended and
	// some of whose files have no outcome yet.
	ImportReasonSettling = "settling"
	// ImportReasonSettlingTimedOut is a job ended partial with files still
	// processing.
	ImportReasonSettlingTimedOut = "settling timed out"
	// ImportReasonRootUnavailable is a unit whose root no longer resolves
	// where it did, or cannot be opened.
	ImportReasonRootUnavailable = "root unavailable"
)

// The bounds of imports.
const (
	// MaxImportMapCSV is the most bytes of a map given inline with a job.
	MaxImportMapCSV = 3 << 20
	// MaxImportMapKeys is the most key columns, and key parts, a map
	// configuration names.
	MaxImportMapKeys = 8
	// MaxImportMapName is the most bytes of a map column's name.
	MaxImportMapName = 256
	// MaxImportUnitBatch is the most folders one AddImportUnits queues.
	MaxImportUnitBatch = 1000
	// MaxImportFilters is the most kinds, or states, a list's filter names.
	MaxImportFilters = 16
)

// ImportCounts are what became of the files an import job, or one of its
// units, found so far, the contract's ImportCounts. They reconcile at every
// answer (Reconcile):
//
//	Found + HandedAgain = skipped + HandedIn
//	HandedIn = Archived + AlreadyHeld + quarantined + Lost + Failed + Discarded + Processing
//
// skipped and quarantined being the sums of their counts by reason. A store
// writes Found, the skips and HandedIn with each file's record and its
// unit's cursor in one transaction, and each outcome in the transaction
// that decides it, so the identities hold after every commit.
type ImportCounts struct {
	// Found counts the entries a walk judged: each file, and each
	// directory skipped whole.
	Found int64
	// Skipped counts the files skipped, by code; a code with none is
	// absent.
	Skipped map[ImportSkipCode]int64
	// HandedIn counts the files handed to the receiver; HandedAgain those
	// handed in a second time, always 0 while each hand-off is written with
	// its unit's cursor.
	HandedIn, HandedAgain int64
	// Archived counts the files that became a new instance; AlreadyHeld
	// those the tenant held, the same SOP Instance UID with the same
	// received checksum.
	Archived, AlreadyHeld int64
	// Quarantined counts the files quarantined, by reason, at their first
	// decision; a reason with none is absent. A file whose incoming copy
	// was lost counts Lost instead.
	Quarantined map[QuarantineReason]int64
	// Lost, Failed and Discarded count the files whose copy was gone,
	// whose reception failed, and whose reception was discarded.
	Lost, Failed, Discarded int64
	// Processing counts the files handed in that have no outcome yet.
	Processing int64
	// BytesHandedIn is the bytes of the files handed in, as read from the
	// root.
	BytesHandedIn int64
	// LabelDiffers stands beside the others, in none of the sums.
	LabelDiffers int64
	// Units counts the job's units (1 for a unit's own counts);
	// UnitsWalked those whose walk has ended.
	Units, UnitsWalked int64
}

// SkippedTotal returns the files skipped, every code together.
func (c ImportCounts) SkippedTotal() int64 {
	var n int64
	for _, v := range c.Skipped {
		n += v
	}
	return n
}

// QuarantinedTotal returns the files quarantined, every reason together.
func (c ImportCounts) QuarantinedTotal() int64 {
	var n int64
	for _, v := range c.Quarantined {
		n += v
	}
	return n
}

// Decided returns the files handed in that have an outcome.
func (c ImportCounts) Decided() int64 {
	return c.Archived + c.AlreadyHeld + c.QuarantinedTotal() + c.Lost + c.Failed + c.Discarded
}

// Reconcile returns, in words, the first identity c breaks, or "" when
// both hold and no count is negative. A store's counts always reconcile;
// the contract suite checks it at every answer.
func (c ImportCounts) Reconcile() string {
	for _, n := range []int64{c.Found, c.HandedIn, c.HandedAgain, c.Archived, c.AlreadyHeld, c.Lost, c.Failed, c.Discarded,
		c.Processing, c.BytesHandedIn, c.LabelDiffers, c.Units, c.UnitsWalked} {
		if n < 0 {
			return "a count is negative"
		}
	}
	for _, n := range c.Skipped {
		if n < 0 {
			return "a skip count is negative"
		}
	}
	for _, n := range c.Quarantined {
		if n < 0 {
			return "a quarantine count is negative"
		}
	}
	switch {
	case c.Found+c.HandedAgain != c.SkippedTotal()+c.HandedIn:
		return "found plus handed again is not skipped plus handed in"
	case c.HandedIn != c.Decided()+c.Processing:
		return "handed in is not the outcomes plus processing"
	case c.UnitsWalked > c.Units:
		return "more units walked than units"
	}
	return ""
}

// Partial reports whether a job with counts c ends partial rather than
// done: a file quarantined, lost, failed or discarded, or skipped for a
// cause that may hide DICOM.
func (c ImportCounts) Partial() bool {
	if c.QuarantinedTotal()+c.Lost+c.Failed+c.Discarded > 0 {
		return true
	}
	for code, n := range c.Skipped {
		if n > 0 && code.MayHideDICOM() {
			return true
		}
	}
	return false
}

// JobCounts returns c as a job's counts, the contract's mapping: instances
// are the files met (found plus handed again), sent those archived or
// already held, refused those skipped, quarantined, lost, failed or
// discarded, remaining those processing; scans are the units, listed says
// every unit is queued. So JobCounts' identity holds whenever c reconciles.
func (c ImportCounts) JobCounts(listed bool) JobCounts {
	refused := c.SkippedTotal() + c.QuarantinedTotal() + c.Lost + c.Failed + c.Discarded
	return JobCounts{Instances: c.Found + c.HandedAgain, InstancesSent: c.Archived + c.AlreadyHeld, InstancesRefused: refused,
		Scans: c.Units, ScansDone: c.UnitsWalked, Listed: listed}
}

// ImportEndState is the state an import job ends in once every unit's
// walk has ended and nothing is processing (or a unit failed, which ends
// it without waiting): failed with failReason when a unit failed, partial
// when Partial, done otherwise.
func ImportEndState(c ImportCounts, failReason string) (JobState, string) {
	switch {
	case failReason != "":
		return JobFailed, failReason
	case c.Partial():
		return JobPartial, ""
	}
	return JobDone, ""
}

// ImportMap is the map configuration of an import job, which replaces its
// receiver's whole: the four are given together or not at all. Which parts
// a layout gives and which column names a map holds are the receiver's
// rules (checked where a job is made); the store keeps them as given.
type ImportMap struct {
	// KeyColumns are the map's header names whose values form a row's key,
	// 1 to MaxImportMapKeys.
	KeyColumns []string
	// ValueColumn is the map's column whose value fills Gives.
	ValueColumn string
	// KeyParts are the layout's parts whose values form a file's key, in
	// the order of KeyColumns.
	KeyParts []string
	// Gives is the part the map fills.
	Gives string
}

// IsZero reports whether m names nothing: the receiver's configuration
// applies.
func (m ImportMap) IsZero() bool {
	return len(m.KeyColumns) == 0 && m.ValueColumn == "" && len(m.KeyParts) == 0 && m.Gives == ""
}

// Validate returns an Invalid error unless m is zero or whole.
func (m ImportMap) Validate() error {
	if m.IsZero() {
		return nil
	}
	if len(m.KeyColumns) == 0 || len(m.KeyColumns) > MaxImportMapKeys || len(m.KeyParts) != len(m.KeyColumns) ||
		m.ValueColumn == "" || m.Gives == "" {
		return invalid("a map configuration names 1 to " + strconv.Itoa(MaxImportMapKeys) +
			" key columns, as many key parts, a value column and the part it fills, together or not at all")
	}
	for _, c := range append(slices.Clone(m.KeyColumns), m.ValueColumn) {
		if err := validateText("map column", c, 1, MaxImportMapName); err != nil {
			return err
		}
	}
	for _, p := range append(slices.Clone(m.KeyParts), m.Gives) {
		if !validPartName(p) {
			return invalid("a map's part is a lowercase ASCII letter followed by lowercase letters, digits and _, at most 64 bytes")
		}
	}
	return nil
}

// validPartName reports whether p names a layout's part: a lowercase ASCII
// letter followed by lowercase letters, digits and "_", at most 64 bytes.
func validPartName(p string) bool {
	if p == "" || len(p) > 64 || p[0] < 'a' || p[0] > 'z' {
		return false
	}
	for i := 1; i < len(p); i++ {
		b := p[i]
		if (b < 'a' || b > 'z') && (b < '0' || b > '9') && b != '_' {
			return false
		}
	}
	return true
}

// ImportJob is what an import job reads and what became of it so far
// (Job.Import).
type ImportJob struct {
	// Receiver names the import receiver the files are handed to.
	Receiver string
	// Path is the directory read, relative to the receiver's root; empty
	// for the whole root. It can identify a person: kept in the tenant's
	// database only, never in an error, a log, an event or an audit entry.
	Path string
	// MapFile is the map's path relative to the root, or empty.
	MapFile string
	// MapCSV is the map given inline, as given. Answered by
	// Imports.ImportJob alone; nil in every other answer.
	MapCSV []byte
	// Map is the job's map configuration, zero for the receiver's.
	Map ImportMap
	// Counts are what became of its files so far.
	Counts ImportCounts
	// FailuresKept is the most files the job lists (ListImportFiles), the
	// setting job.failures-kept when it was made; FilesNotListed counts
	// those past it.
	FailuresKept   int
	FilesNotListed int64
	// Listed says every unit is queued: the listing unit's walk ended.
	Listed bool
	// SettlingSince is when the job's last walk ended with files still
	// processing; zero until then, and when none were.
	SettlingSince time.Time
	// RetentionStart is the start time retention counts from for the
	// sessions it archives first; zero for their archive time.
	RetentionStart time.Time
}

// NewImportJob is an import job a person asks for (Imports.CreateImportJob).
type NewImportJob struct {
	// ID is its id, given by the caller.
	ID string
	// Receiver names the tenant's import receiver.
	Receiver string
	// Path is the directory read, empty for the whole root
	// (importpath.Check otherwise).
	Path string
	// MapFile is the map's path relative to the root, or empty; at most one
	// of MapFile and MapCSV is given.
	MapFile string
	// MapCSV is the map inline, at most MaxImportMapCSV bytes, or nil.
	MapCSV []byte
	// Map is the map configuration, zero for the receiver's.
	Map ImportMap
	// FailuresKept is the setting job.failures-kept now.
	FailuresKept int
	// RetentionStart, when not zero, is the start time retention counts
	// from for each session the job archives first (an admin's, never a
	// file's): not before 1970 and not after Created
	// (ValidateRetentionStart).
	RetentionStart time.Time
	// Created is when it is made.
	Created time.Time
}

// Validate returns an Invalid error unless j can be made. No message names
// the path: importpath names a segment by its number.
func (j NewImportJob) Validate() error {
	if err := master.ValidateID("job id", j.ID); err != nil {
		return err
	}
	if err := ValidateName("receiver", j.Receiver); err != nil {
		return err
	}
	if err := validateImportPath("job's path", j.Path, true); err != nil {
		return err
	}
	if err := validateImportPath("map file", j.MapFile, true); err != nil {
		return err
	}
	if j.MapFile != "" && len(j.MapCSV) > 0 {
		return invalid("an import job names a map file or gives a map, not both")
	}
	if len(j.MapCSV) > MaxImportMapCSV {
		return invalid("a map given with a job is at most 3 MiB")
	}
	if err := j.Map.Validate(); err != nil {
		return err
	}
	if j.FailuresKept < MinFailuresKept || j.FailuresKept > MaxFailuresKept {
		return invalid("a job keeps 100 to 1000000 failures")
	}
	if err := ValidateRetentionStart(j.RetentionStart, j.Created); err != nil {
		return err
	}
	return master.ValidateTime("job time", j.Created)
}

// validateImportPath returns an Invalid error unless p follows
// importpath.Check, or is empty when empty allows it. The message names a
// segment by its number, never the path.
func validateImportPath(what, p string, empty bool) error {
	if p == "" && empty {
		return nil
	}
	if err := importpath.Check(p); err != nil {
		return invalid("an import " + what + " breaks the rule of a path under a root: " +
			strings.TrimPrefix(err.Error(), "importpath: "))
	}
	return nil
}

// ImportUnit is one unit of an import job: one folder, walked and resumed
// on its own (the contract's ImportUnit).
type ImportUnit struct {
	// ID is its id, unique in the job, naming no path (ImportUnitID).
	ID string
	// Folder is its folder relative to the root; the listing unit's is the
	// job's path.
	Folder string
	// Listing says it is the job's listing unit, which queues the others
	// and judges the files above their depth.
	Listing bool
	// State is where it is.
	State ImportUnitState
	// Cursor is the last path it judged, in importpath.Compare's order;
	// empty before the first. Its walk resumes after it. It is the entry's
	// own path, up to importpath.MaxEntryPathLength bytes for an entry
	// skipped as too long, so each such entry is judged once.
	Cursor string
	// Counts are what became of its files; Units is 1.
	Counts ImportCounts
	// Reason is why it failed, from the fixed phrases; empty otherwise.
	Reason string
	// Attempts is the attempts its unit of work has used.
	Attempts int
	// WalkEnded is when its walk ended; zero until then.
	WalkEnded time.Time
}

// ImportFile is one file of an import job that did not reach the archive
// (the contract's ImportFile).
type ImportFile struct {
	// Path is its path relative to the root.
	Path string
	// Kind is what it is listed for.
	Kind ImportFileKind
	// SkipCode is why it was skipped, for ImportFileSkipped.
	SkipCode ImportSkipCode
	// QuarantinedFile and QuarantineReason are its quarantine record and
	// why, for ImportFileQuarantined.
	QuarantinedFile  string
	QuarantineReason QuarantineReason
	// Unit is the unit that found it.
	Unit string
	// Size is its size as the walk saw it; 0 when not read.
	Size int64
	// Detail is a phrase of the server's (ImportDetailMaxDirEntries...).
	Detail string
	// Time is when it was listed.
	Time time.Time
	// Reception is the reception it was handed into, for a file
	// quarantined, failed or discarded.
	Reception string
}

// ImportFileFilter keeps the files a list answers; every filter given must
// hold.
type ImportFileFilter struct {
	// Kinds keeps the files of these kinds, at most MaxImportFilters.
	Kinds []ImportFileKind
	// Unit keeps the files of the unit with this id.
	Unit string
}

// Validate returns an Invalid error when f cannot be applied.
func (f ImportFileFilter) Validate() error {
	if len(f.Kinds) > MaxImportFilters {
		return invalid("a filter names at most 16 kinds")
	}
	for _, k := range f.Kinds {
		if !slices.Contains(ImportFileKinds(), k) {
			return invalid("an import file's kind is one of skipped, quarantined, lost, failed, discarded")
		}
	}
	if f.Unit != "" {
		return master.ValidateID("unit id", f.Unit)
	}
	return nil
}

// ImportUnitFilter keeps the units a list answers.
type ImportUnitFilter struct {
	// States keeps the units in one of these states, at most
	// MaxImportFilters.
	States []ImportUnitState
}

// Validate returns an Invalid error when f cannot be applied.
func (f ImportUnitFilter) Validate() error {
	if len(f.States) > MaxImportFilters {
		return invalid("a filter names at most 16 states")
	}
	for _, s := range f.States {
		if !slices.Contains(ImportUnitStates(), s) {
			return invalid("an import unit's state is one of queued, walking, walked, done, failed, canceled")
		}
	}
	return nil
}

// ImportUnitID returns the id of an import job's unit number n: the
// listing unit is 0, the others numbered in the order they were queued. It
// names no path.
func ImportUnitID(n int64) string { return "u" + strconv.FormatInt(n, 10) }

// ImportUnitKey is the unique key of an import unit's work, of ids only,
// never a path, so a unit is queued once.
func ImportUnitKey(jobID, unitID string) string { return "import:" + jobID + ":" + unitID }

// ImportUnitWorkID is the id of an import unit's work, made from the job's
// and the unit's ids.
func ImportUnitWorkID(jobID, unitID string) string { return "imp-" + digest(jobID+"\x00"+unitID) }
