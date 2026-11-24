package tenant

import (
	"errors"
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// The bounds of the catalog's operations.
const (
	// MaxClaimBatch is the most instances one ClaimInstances claims.
	MaxClaimBatch = 1000
	// MaxScanCommit is the most instances one CommitScan confirms. A scan
	// with more is committed in several calls, each its own transaction,
	// so no transaction holds the writer for long: on a database whose
	// event feed waits for the oldest open transaction, a long one stalls
	// every reader of the feed.
	MaxScanCommit = 1000
	// MaxSeriesLookup is the most series one CatalogLabels looks up.
	MaxSeriesLookup = 1000
	// MaxReceptionScans is the most scans ReceptionScans returns, the
	// contract's 10000.
	MaxReceptionScans = 10000
)

// ConfirmedInstance is one claimed instance, written to the storage and
// read back, that a reception confirms (CommitScan).
type ConfirmedInstance struct {
	// SOPInstanceUID names the instance; this reception must claim it.
	SOPInstanceUID string
	// Key is where the file is on the storage, a key of the layout
	// (package layout) for this scan's labels and this UID.
	Key string
	// Offset and Length are where the instance is inside the object at
	// Key: 0 and its size while each instance is one object.
	Offset, Length int64
	// Size is the file's size in bytes.
	Size int64
	// ArchivedSHA256 is the checksum of what was written and read back.
	ArchivedSHA256 SHA256
	// Profile is the profile version that de-identified the file, which
	// the tenant must have; the zero ref for a file archived as received.
	Profile ProfileRef
}

// ScanCommit is the catalog rows of one scan, or of a part of one, that a
// reception writes in one transaction, with the confirmation of the
// instances it archived into the scan.
type ScanCommit struct {
	// ReceptionID names the reception confirming.
	ReceptionID string
	// StudyUID and SeriesUID are the session's and the scan's, as
	// received.
	StudyUID, SeriesUID string
	// SubjectLabel, SessionLabel and ScanLabel are the labels. Those of a
	// session or scan that exists are its own, found with CatalogLabels
	// before any naming rule runs; others are refused.
	SubjectLabel, SessionLabel, ScanLabel string
	// Storage names the storage the files are on: the session's own once
	// it has one.
	Storage string
	// ManifestSHA256 is the checksum of the scan's manifest as written
	// with this commit; zero keeps the scan's.
	ManifestSHA256 SHA256
	// Values are what the scan's files say of it and of its session
	// (ScanValues), read from one of them: a scan or session made by this
	// commit takes them, and one already there fills only those it holds
	// empty.
	Values ScanValues
	// Instances are the instances confirmed, at most MaxScanCommit.
	Instances []ConfirmedInstance
	// At is when the commit is made.
	At time.Time
	// Fence, when not nil, is the lease of the worker committing: the
	// commit is made only while it holds.
	Fence *WorkFence
}

// Validate returns an Invalid error unless the commit is well formed: every
// key is the layout's key of this scan and its instance's UID.
func (c ScanCommit) Validate() error {
	if err := master.ValidateID("reception id", c.ReceptionID); err != nil {
		return err
	}
	if err := validateUID("Study Instance UID", c.StudyUID); err != nil {
		return err
	}
	if err := validateUID("Series Instance UID", c.SeriesUID); err != nil {
		return err
	}
	path := label.Path{Subject: c.SubjectLabel, Session: c.SessionLabel, Scan: c.ScanLabel}
	if err := path.Check(); err != nil {
		return fault.Wrap(fault.Invalid, err, "a scan's labels are valid labels: "+err.Error())
	}
	if err := ValidateName("storage", c.Storage); err != nil {
		return err
	}
	if err := c.Values.Validate(); err != nil {
		return err
	}
	if len(c.Instances) == 0 {
		return invalid("a commit confirms at least one instance: a scan is made with what it holds")
	}
	if len(c.Instances) > MaxScanCommit {
		return invalid("at most " + strconv.Itoa(MaxScanCommit) + " instances are confirmed in one commit")
	}
	seen := make(map[string]bool, len(c.Instances))
	for _, in := range c.Instances {
		if err := validateUID("SOP Instance UID", in.SOPInstanceUID); err != nil {
			return err
		}
		if seen[in.SOPInstanceUID] {
			return invalid("a commit confirms an instance once")
		}
		if !in.Profile.IsZero() {
			if err := in.Profile.Validate(); err != nil {
				return err
			}
		}
		seen[in.SOPInstanceUID] = true
		k, err := layout.Parse(in.Key)
		if err != nil || k.Kind != layout.Instance || k.Path != path || k.SOPInstanceUID != in.SOPInstanceUID {
			return invalid("an instance's key is the layout's key of its scan and its SOP Instance UID")
		}
		if in.Size < 0 || in.Offset < 0 || in.Length < 0 {
			return invalid("an instance's size, offset and length are not negative")
		}
		if in.ArchivedSHA256.IsZero() {
			return invalid("a confirmed instance has its archived checksum")
		}
	}
	if err := master.ValidateTime("commit time", c.At); err != nil {
		return err
	}
	if c.Fence != nil {
		return c.Fence.Validate()
	}
	return nil
}

// Scan is a scan of the catalog as a commit leaves it.
type Scan struct {
	// SubjectLabel, SessionLabel and Label place it.
	SubjectLabel, SessionLabel, Label string
	// StudyUID and SeriesUID are its session's and its own, as received.
	StudyUID, SeriesUID string
	// Storage is its session's storage.
	Storage string
	// Instances and Bytes count the instances held in it.
	Instances, Bytes int64
	// ManifestSHA256 is the checksum of its manifest as last written.
	ManifestSHA256 SHA256
}

// CatalogLabels are the labels the catalog already gives a study and its
// series, which a reception uses before any naming rule runs, so a later
// arrival lands beside the first.
type CatalogLabels struct {
	// Session is the study's session, nil when the catalog has none.
	Session *SessionLabels
	// Scans maps each series looked up that has a scan to the scan's
	// label; a series with none is absent.
	Scans map[string]string
}

// SessionLabels are a session's labels and storage.
type SessionLabels struct {
	SubjectLabel, SessionLabel, Storage string
	// Route is what the routing chose for the session, as the reception
	// whose commit made it kept it (Reception.Route): every later arrival
	// of the session goes by it. Zero for a session made before routing,
	// or by a reception that chose nothing of its own.
	Route ReceptionRoute
}

// CatalogCounts are the catalog's running totals, kept in the transaction
// of every commit, so they equal a count over the catalog after each.
type CatalogCounts struct {
	Subjects, Sessions, Scans, Instances int64
}

// ReceptionScan is one scan a reception archived files into, with the
// instances it confirmed there.
type ReceptionScan struct {
	// Label is the scan's label; SeriesUID its Series Instance UID.
	Label, SeriesUID string
	// Instances and Bytes count what this reception confirmed in it.
	Instances, Bytes int64
}

// The faults of a label conflict, the words a quarantine record's details
// carry under "fault" for QuarantineLabelConflict.
const (
	// FaultHeldByAnotherSession is a session label held by another study
	// in the same subject.
	FaultHeldByAnotherSession = "held_by_another_session"
	// FaultTwoSubjectsForOneStudy is a study whose files gave a subject
	// label other than its session's.
	FaultTwoSubjectsForOneStudy = "two_subjects_for_one_study"
	// FaultHeldByAnotherScan is a scan label held by another series in the
	// same session.
	FaultHeldByAnotherScan = "held_by_another_scan"
	// FaultCaseCollision is a label that differs from a label held at the
	// same level only in the case of its letters, which a file system that
	// ignores case would make one directory (label.Fold).
	FaultCaseCollision = "case_collision"
)

// LabelConflict is why a commit's labels are refused: the level ("subject",
// "session" or "scan") and the fault, from the constants above. It is in the
// chain of a Conflict, for the quarantine record the caller makes.
type LabelConflict struct {
	Label, Fault string
	// Correction is the correction that holds the label, for
	// FaultHeldByCorrection; empty otherwise.
	Correction string
}

// Error returns the level and the fault, never a label.
func (e *LabelConflict) Error() string {
	return "the " + e.Label + " label conflicts: " + e.Fault
}

// LabelConflictError returns the Conflict of a label conflict.
func LabelConflictError(level, faultWord string) error {
	return fault.Wrap(fault.Conflict, &LabelConflict{Label: level, Fault: faultWord},
		"the "+level+"'s label conflicts with one the catalog holds ("+faultWord+")")
}

// ErrSeriesElsewhere is in the chain of a commit whose series belongs to a
// scan of another session: a Series Instance UID is one scan in a tenant.
var ErrSeriesElsewhere = errors.New("the series belongs to a scan of another session")

// ValidateSeriesLookup returns an Invalid error unless the arguments of
// CatalogLabels are well formed.
func ValidateSeriesLookup(studyUID string, seriesUIDs []string) error {
	if err := validateUID("Study Instance UID", studyUID); err != nil {
		return err
	}
	if len(seriesUIDs) > MaxSeriesLookup {
		return invalid("at most " + strconv.Itoa(MaxSeriesLookup) + " series are looked up at once")
	}
	for _, s := range seriesUIDs {
		if err := validateUID("Series Instance UID", s); err != nil {
			return err
		}
	}
	return nil
}
