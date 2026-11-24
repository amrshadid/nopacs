package driver

import (
	"github.com/amrshadid/nopacs/internal/driver/postgres"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
)

// SchemaVersions returns the newest schema version of the master and of a
// tenant that every database type this binary knows migrates to: the lower
// of the drivers' when they differ, so a version it answers loads into
// either type.
func SchemaVersions() (masterVersion, tenantVersion int, err error) {
	sm, st, err := sqlite.SchemaVersions()
	if err != nil {
		return 0, 0, err
	}
	pm, pt, err := postgres.SchemaVersions()
	if err != nil {
		return 0, 0, err
	}
	return min(sm, pm), min(st, pt), nil
}
