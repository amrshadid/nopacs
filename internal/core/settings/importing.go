package settings

import (
	"errors"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/pathname"
)

// The settings of importing a directory (receiver.proto, ImportReceiver;
// job.proto, ImportJob): where an import may read, and how much of the
// server it may take.
const (
	// ImportRoots is the directories an import may read, one "name=path"
	// per line (ParseImportRoots). Installation-only: a tenant's admin must
	// not make the server read arbitrary files of the machine it runs on.
	ImportRoots = "import.roots"
	// ImportAllowedRoots is the names of the roots a tenant's import
	// receivers may read, separated by commas (ParseAllowedRoots). Set on
	// a tenant by the installation's admin only, with no installation
	// value.
	ImportAllowedRoots = "import.allowed-roots"
	// ImportMaxUnits is the most import units one tenant works at once
	// (ParseImportMaxUnits). Set on a tenant by the installation's admin
	// only; the installation's value is every tenant's default.
	ImportMaxUnits = "import.max-units"
	// ImportServerMaxUnits is the most import units of every tenant the
	// server works at once (ParseImportServerMaxUnits). Installation-only.
	ImportServerMaxUnits = "import.server-max-units"
	// ImportMaxDirEntries is the most names one directory read by an import
	// may hold (ParseMaxDirEntries). Installation-only, since it bounds the
	// server's memory.
	ImportMaxDirEntries = "import.max-dir-entries"
)

// The bounds of the import settings.
const (
	// MaxImportUnits bounds import.max-units; MaxImportServerUnits bounds
	// import.server-max-units. Both are at least 1.
	MaxImportUnits       = 64
	MaxImportServerUnits = 256
	// MinDirEntries and MaxDirEntries bound import.max-dir-entries. A
	// name read from a directory is held as its bytes and about 25 more
	// (measured with Go 1.26 on 64-bit: 43 bytes for a 20-byte name, 227
	// for a 200-byte one), per import unit, so 10,000,000 names are about
	// 0.8 GiB of 60-byte names and up to 2.6 GiB of 255-byte ones, for
	// each unit reading so wide a directory at once; 1,000 is the least
	// any real tree needs.
	MinDirEntries = 1000
	MaxDirEntries = 10000000
)

// ParseImportRoots reads import.roots: one root per line, name=path, by
// the rule of storage.directory-roots (ParseDirectoryRoots). Which
// directories a root may not be, or lie in, needs the server's file system
// and is checked where the value is set.
func ParseImportRoots(v string) (DirectoryRoots, error) { return ParseDirectoryRoots(v) }

// ParseAllowedRoots reads import.allowed-roots: root names separated by
// commas, each by the rule of a root's name, each once, at most
// MaxDirectoryRoots; empty for none.
func ParseAllowedRoots(v string) ([]string, error) {
	if v == "" {
		return nil, nil
	}
	names := strings.Split(v, ",")
	if len(names) > MaxDirectoryRoots {
		return nil, errors.New("lists at most " + strconv.Itoa(MaxDirectoryRoots) + " roots")
	}
	for i, n := range names {
		if !pathname.ValidName(n) {
			return nil, errors.New("has a root name that does not follow the rule of a tenant's name: 1 to 63 of a-z, 0-9 and -, starting with a letter, separated by commas without spaces")
		}
		if slices.Contains(names[:i], n) {
			return nil, errors.New("names the root " + n + " twice")
		}
	}
	return names, nil
}

// ParseImportMaxUnits reads import.max-units: a whole number from 1 to
// MaxImportUnits.
func ParseImportMaxUnits(v string) (int, error) {
	n, err := wholeNumber(v, MaxImportUnits)
	if err != nil || n < 1 {
		return 0, errors.New("is a whole number from 1 to " + strconv.Itoa(MaxImportUnits))
	}
	return int(n), nil
}

// ParseImportServerMaxUnits reads import.server-max-units: a whole number
// from 1 to MaxImportServerUnits.
func ParseImportServerMaxUnits(v string) (int, error) {
	n, err := wholeNumber(v, MaxImportServerUnits)
	if err != nil || n < 1 {
		return 0, errors.New("is a whole number from 1 to " + strconv.Itoa(MaxImportServerUnits))
	}
	return int(n), nil
}

// ParseMaxDirEntries reads import.max-dir-entries: a whole number from
// MinDirEntries to MaxDirEntries.
func ParseMaxDirEntries(v string) (int, error) {
	n, err := wholeNumber(v, MaxDirEntries)
	if err != nil || n < MinDirEntries {
		return 0, errors.New("is a whole number from " + strconv.Itoa(MinDirEntries) + " to " + strconv.Itoa(MaxDirEntries))
	}
	return int(n), nil
}

// importDefinitions returns the definitions of the import settings, for
// Builtin.
func importDefinitions() []Definition {
	return []Definition{{
		// Empty by default, so no import can read anything until the
		// installation's admin names a root.
		Name:     ImportRoots,
		Default:  "",
		Validate: func(v string) error { _, err := ParseImportRoots(v); return err },
	}, {
		// Handed out per tenant, as receiver.ports: empty by default, and no
		// installation value, since a root every tenant may read by default
		// would be one tree shared by all.
		Name:                    ImportAllowedRoots,
		Default:                 "",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		TenantOnly:              true,
		Validate:                func(v string) error { _, err := ParseAllowedRoots(v); return err },
	}, {
		// Four units of one tenant at once keep a large import moving while
		// leaving the reception pipeline to live receivers.
		Name:                    ImportMaxUnits,
		Default:                 "4",
		TenantOverride:          true,
		TenantSetByInstallation: true,
		Validate:                func(v string) error { _, err := ParseImportMaxUnits(v); return err },
	}, {
		Name:     ImportServerMaxUnits,
		Default:  "8",
		Validate: func(v string) error { _, err := ParseImportServerMaxUnits(v); return err },
	}, {
		// 200,000 names are about 16 MiB of 60-byte names held while one
		// directory is read, and up to 54 MiB of 255-byte ones.
		Name:     ImportMaxDirEntries,
		Default:  "200000",
		Validate: func(v string) error { _, err := ParseMaxDirEntries(v); return err },
	}}
}
