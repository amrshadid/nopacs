package tenant

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestScrubJobTargets guards what a scrub job may name: the whole tenant or
// one scan, on a storage or on every one; a subject or a session alone is
// refused.
func TestScrubJobTargets(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	scan := JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1", ScanLabel: "scan-1"}
	for _, c := range []struct {
		name   string
		target JobTarget
		ok     bool
	}{
		{"the whole tenant", JobTarget{}, true},
		{"one scan", scan, true},
		{"a subject", JobTarget{SubjectLabel: "sub-01"}, false},
		{"a session", JobTarget{SubjectLabel: "sub-01", SessionLabel: "ses-1"}, false},
		{"a scan without its session", JobTarget{SubjectLabel: "sub-01", ScanLabel: "scan-1"}, false},
	} {
		for _, storage := range []string{"", "main"} {
			err := NewJob{ID: "j1", Kind: JobScrub, Storage: storage, Target: c.target, Created: now}.Validate()
			if c.ok && err != nil {
				t.Errorf("a scrub of %s on %q = %v; want it made", c.name, storage, err)
			}
			if !c.ok && fault.KindOf(err) != fault.Invalid {
				t.Errorf("a scrub of %s on %q = %v; want Invalid", c.name, storage, err)
			}
		}
	}
}

// TestRebuildFromStorageIsACompareOnly guards that a rebuild names a
// storage to read from only in compare mode, by a valid name.
func TestRebuildFromStorageIsACompareOnly(t *testing.T) {
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	mk := func(mode RebuildMode, from string) error {
		return NewJob{ID: "j1", Kind: JobRebuild, Rebuild: &RebuildJob{Mode: mode, FromStorage: from}, Created: now}.Validate()
	}
	if err := mk(RebuildCompare, "copy"); err != nil {
		t.Errorf("a compare from a storage = %v; want it made", err)
	}
	if err := mk(RebuildReplace, "copy"); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a replace from a storage = %v; want Invalid", err)
	}
	if err := mk(RebuildCompare, "Bad Name"); fault.KindOf(err) != fault.Invalid {
		t.Errorf("a compare from a malformed name = %v; want Invalid", err)
	}
}

// TestAssociationRefusedWhileSwitching guards that a receiver's event of a
// caller turned away because its storage is being switched is one the
// tenant's feed takes.
func TestAssociationRefusedWhileSwitching(t *testing.T) {
	_, err := NewEvent(&AssociationFailed{Receiver: "recv", Failure: FailureStorageSwitching, Count: 3, Hosts: 2},
		time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Errorf("an association refused while its storage switches = %v; want an event", err)
	}
}
