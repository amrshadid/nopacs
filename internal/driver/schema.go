package driver

import (
	"github.com/amrshadid/nopacs/internal/driver/postgres"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
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

// SchemaFingerprints returns the fingerprint of the master's and of a
// tenant's baseline schema across every database type this binary knows
// (sqlkit.Fingerprint of the SQLite baseline, then the Postgres one). A
// backup and a tenant's export record the one of each section's database,
// and a restore or an import refuses a section whose fingerprint differs:
// its rows were read from a schema this binary's chain does not begin with,
// whichever database type either side runs.
func SchemaFingerprints() (masterFingerprint, tenantFingerprint string, err error) {
	sm, st, err := sqlite.Baselines()
	if err != nil {
		return "", "", err
	}
	pm, pt, err := postgres.Baselines()
	if err != nil {
		return "", "", err
	}
	return sqlkit.Fingerprint(sm, pm), sqlkit.Fingerprint(st, pt), nil
}
