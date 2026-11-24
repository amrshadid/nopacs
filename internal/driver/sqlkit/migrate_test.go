package sqlkit

import (
	"crypto/sha256"
	"errors"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestCheckWithNoMigrations guards check against a binary with no files: a
// database that records migrations is refused as one a newer binary
// upgraded, rather than the check indexing an empty list and panicking.
func TestCheckWithNoMigrations(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	rows := []applied{{version: 1, name: "0001_baseline.sql", sum: sum[:]}}
	err := check(rows, []Migration{})
	if !errors.Is(err, ErrNewerSchema) || fault.KindOf(err) != fault.Conflict {
		t.Fatalf("check with no migrations = %v, want ErrNewerSchema as Conflict", err)
	}
	if err := check(nil, []Migration{}); err != nil {
		t.Errorf("check of a database that records nothing, with no migrations = %v, want nil", err)
	}
}

// TestCheckRefusesAnotherBaseline guards that the first recorded row is
// compared before any other: a database whose baseline differs by name or
// SHA-256 is refused as made by an earlier build even when it records more
// files than the binary has, which would otherwise read as a newer schema.
func TestCheckRefusesAnotherBaseline(t *testing.T) {
	ours := Migration{Version: 1, Name: "0001_baseline.sql", Sum: sha256.Sum256([]byte("ours"))}
	other := sha256.Sum256([]byte("theirs"))
	for name, rows := range map[string][]applied{
		"another SHA-256": {{version: 1, name: "0001_baseline.sql", sum: other[:]}, {version: 2, name: "0002_more.sql", sum: other[:]}},
		"another name":    {{version: 1, name: "0001_init.sql", sum: ours.Sum[:]}, {version: 2, name: "0002_more.sql", sum: other[:]}},
	} {
		if err := check(rows, []Migration{ours}); !errors.Is(err, ErrEarlierBuild) || fault.KindOf(err) != fault.Conflict {
			t.Errorf("%s: check = %v, want ErrEarlierBuild as Conflict", name, err)
		}
	}
	if err := check([]applied{{version: 1, name: ours.Name, sum: ours.Sum[:]}}, []Migration{ours}); err != nil {
		t.Errorf("the binary's own baseline: check = %v, want nil", err)
	}
}
