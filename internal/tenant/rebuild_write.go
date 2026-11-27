package tenant

import (
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/master"
)

// What a rebuild writes into its shadow (Rebuilding.RebuildScan), and what
// a compare job keeps of its comparison (Rebuilding.EndRebuild), with the
// checks a store makes of both before it writes anything.

// RebuiltScan is a scan as its storage's manifest and metadata say: what a
// RebuildScan writes into the shadow's subject, session and scan rows.
type RebuiltScan struct {
	// Storage is the storage it is on.
	Storage string
	// Path is its labels.
	Path label.Path
	// StudyUID and SeriesUID are its session's and its own.
	StudyUID, SeriesUID string
	// SessionReceiver and Receiver name the receivers that first archived
	// into its session and into it; empty for none known. Of the scans of
	// one session, the shadow's session takes the SessionReceiver and Route
	// of the scan created first (the earliest Created; of two created at
	// once, the one written first), as the catalog keeps those of the
	// reception that made the session.
	SessionReceiver, Receiver string
	// Route is its session's routing; zero for none.
	Route ReceptionRoute
	// Values are what the catalog keeps from its files' headers.
	Values ScanValues
	// Created is when its first file was archived, Updated the last.
	Created, Updated time.Time
	// ManifestSHA256 is the checksum of its manifest as read.
	ManifestSHA256 SHA256
}

// RebuiltInstance is one instance as its scan's manifest and metadata say.
type RebuiltInstance struct {
	// SOPInstanceUID names it.
	SOPInstanceUID string
	// Key is its object's key on the storage.
	Key string
	// Offset and Length place it in the object; Size is its file's size.
	Offset, Length, Size int64
	// ReceivedSHA256 is the checksum it was received with, ArchivedSHA256
	// that of the object.
	ReceivedSHA256, ArchivedSHA256 SHA256
	// Profile is the profile version that de-identified it; zero for one
	// archived as received.
	Profile ProfileRef
	// PixelRiskAccepted says its metadata records an image at risk of
	// burned-in text that a pixel rule accepted as it is; only with
	// Profile.
	PixelRiskAccepted bool
	// Reception names the reception that confirmed it; HeldAt is when.
	Reception string
	HeldAt    time.Time
}

// RebuildScanWrite is one write of a build (RebuildScan).
type RebuildScanWrite struct {
	// Fence is the unit's lease.
	Fence WorkFence
	// From is the cursor the write follows; it must be the build's.
	From RebuildCursor
	// Key is the key that orders the scan (RebuildCursor.After).
	Key string
	// Scan is the scan's rows to write, nil when nothing of it is written
	// (it is left out whole).
	Scan *RebuiltScan
	// Instances are instances of the scan to write, in order of SOP
	// Instance UID, after any written before; at most MaxRebuildBatch.
	Instances []RebuiltInstance
	// Done says the scan is written whole with this write, or left out:
	// the cursor's After becomes Key. Otherwise the scan is written in part
	// and the cursor's Partial becomes Key.
	Done bool
	// LeftOut and LeftOutBytes count the instances of the scan left out.
	LeftOut, LeftOutBytes int64
	// Tallies are added to the build's.
	Tallies map[string]int64
}

// Validate returns an Invalid error unless w can be written.
func (w RebuildScanWrite) Validate() error {
	if err := w.Fence.Validate(); err != nil {
		return err
	}
	if err := validateRebuildKey(w.Key); err != nil {
		return err
	}
	if len(w.Instances) > MaxRebuildBatch {
		return invalid("at most " + strconv.Itoa(MaxRebuildBatch) + " instances are written in one write of a rebuild")
	}
	if w.LeftOut < 0 || w.LeftOutBytes < 0 {
		return invalid("a rebuild's counts are not negative")
	}
	if len(w.Tallies) > 0 {
		if err := validateTallies(w.Tallies); err != nil {
			return err
		}
	}
	if w.Scan == nil {
		if len(w.Instances) > 0 {
			return invalid("a rebuild's instances are written with their scan")
		}
		return nil
	}
	s := w.Scan
	if err := ValidateName("storage", s.Storage); err != nil {
		return err
	}
	if w.From.Storage != s.Storage {
		return invalid("a rebuild writes the scans of the storage its cursor is on")
	}
	if err := ValidateScanPath(s.Path); err != nil {
		return err
	}
	if err := validateUID("Study Instance UID", s.StudyUID); err != nil {
		return err
	}
	if err := validateUID("Series Instance UID", s.SeriesUID); err != nil {
		return err
	}
	for _, r := range []string{s.SessionReceiver, s.Receiver} {
		if r != "" {
			if err := ValidateName("receiver", r); err != nil {
				return err
			}
		}
	}
	if err := s.Route.Validate(); err != nil {
		return err
	}
	if err := s.Values.Validate(); err != nil {
		return err
	}
	if err := master.ValidateTime("scan time", s.Created); err != nil {
		return err
	}
	if err := master.ValidateTime("scan time", s.Updated); err != nil {
		return err
	}
	prev := w.From.PartialUID
	if w.From.Partial != w.Key {
		prev = ""
	}
	for _, in := range w.Instances {
		if err := in.validate(s.Path); err != nil {
			return err
		}
		if in.SOPInstanceUID <= prev {
			return invalid("a rebuild writes a scan's instances once each, in order of SOP Instance UID")
		}
		prev = in.SOPInstanceUID
	}
	return nil
}

// validate returns an Invalid error unless in can be written in the scan at
// path.
func (in RebuiltInstance) validate(path label.Path) error {
	if err := ValidateInstanceUID(in.SOPInstanceUID); err != nil {
		return err
	}
	k, err := layout.Parse(in.Key)
	if err != nil || k.Kind != layout.Instance || k.Path != path || k.SOPInstanceUID != in.SOPInstanceUID {
		return invalid("a rebuilt instance's key is a key of the layout of its scan and its SOP Instance UID")
	}
	if in.Size < 0 || in.Offset < 0 || in.Length < 0 {
		return invalid("an instance's size, offset and length are not negative")
	}
	if in.ReceivedSHA256.IsZero() || in.ArchivedSHA256.IsZero() {
		return invalid("a rebuilt instance has its received and archived checksums")
	}
	if !in.Profile.IsZero() {
		if err := in.Profile.Validate(); err != nil {
			return err
		}
	} else if in.PixelRiskAccepted {
		return invalid("an instance records an accepted pixel risk only with the profile version that accepted it")
	}
	if err := master.ValidateID("reception id", in.Reception); err != nil {
		return err
	}
	return master.ValidateTime("held time", in.HeldAt)
}

// validateRebuildKey returns an Invalid error unless key can order a scan.
func validateRebuildKey(key string) error {
	return validateText("scan key", key, 1, layout.MaxKeyLength)
}

// ValidateShadowUIDs returns an Invalid error unless uids can be asked of a
// shadow at once (ShadowHolder): 1 to MaxRebuildBatch SOP Instance UIDs.
func ValidateShadowUIDs(uids []string) error {
	if len(uids) == 0 || len(uids) > MaxRebuildBatch {
		return invalid("1 to " + strconv.Itoa(MaxRebuildBatch) + " instances are asked of a shadow at once")
	}
	for _, u := range uids {
		if err := ValidateInstanceUID(u); err != nil {
			return err
		}
	}
	return nil
}

// Validate returns an Invalid error unless c can be kept as a compare job's
// end: tables of the catalog, counts not negative, and at most
// MaxCatalogDifferences differences, each of a table of the catalog, of
// kind added, removed or changed, its key at most MaxDifferenceKeyLength
// bytes.
func (c CatalogComparison) Validate() error {
	tables := CatalogTables()
	known := func(t string) bool {
		for _, n := range tables {
			if n == t {
				return true
			}
		}
		return false
	}
	for table, d := range c.Tables {
		if !known(table) {
			return invalid("a comparison names a table that is not the catalog's")
		}
		if d.Added < 0 || d.Removed < 0 || d.Changed < 0 {
			return invalid("a comparison's counts are not negative")
		}
	}
	if len(c.Differences) > MaxCatalogDifferences {
		return invalid("a comparison keeps at most " + strconv.Itoa(MaxCatalogDifferences) + " differences")
	}
	for _, d := range c.Differences {
		if !known(d.Table) {
			return invalid("a difference names a table that is not the catalog's")
		}
		if d.Kind != "added" && d.Kind != "removed" && d.Kind != "changed" {
			return invalid("a difference is added, removed or changed")
		}
		if len(d.Key) > MaxDifferenceKeyLength {
			return invalid("a difference's key is at most " + strconv.Itoa(MaxDifferenceKeyLength) + " bytes")
		}
	}
	return nil
}
