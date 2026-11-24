package driver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/amrshadid/nopacs/internal/driver/postgres"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// SQLiteDriver is master.SQLiteDriver, the name of the SQLite database type,
// kept here for the callers that already import this package to open a
// tenant's database.
const SQLiteDriver = master.SQLiteDriver

// PostgresDriver is master.PostgresDriver, the name of the Postgres database
// type, kept here for the callers that already import this package to open a
// tenant's database.
const PostgresDriver = master.PostgresDriver

// TenantsDir is the directory, inside the data directory, that holds the
// database file of every tenant on SQLite.
const TenantsDir = "tenants"

// SQLiteTenants opens the databases of tenants on SQLite. Each is the file
// <DataDir>/tenants/<name>.db, so a tenant on SQLite is given no address:
// its sealed address is empty, and its file follows the data directory if
// that is moved. It is what package registry calls a Driver.
type SQLiteTenants struct {
	// DataDir is the installation's data directory.
	DataDir string
}

// Path returns the file of the tenant named name. The name is refused unless
// it is a valid tenant name (master.ValidateTenantName: a-z, 0-9 and "-",
// starting with a letter), and the path built from it is checked to be a
// file directly inside the tenants directory: the name is validated by the
// master before it gets here, and this check means that a mistake there
// still cannot make a database outside the data directory.
func (d SQLiteTenants) Path(name string) (string, error) {
	if d.DataDir == "" {
		return "", fault.New(fault.Internal, "no data directory is set for tenants on SQLite")
	}
	if err := master.ValidateTenantName(name); err != nil {
		return "", err
	}
	file := name + ".db"
	if !filepath.IsLocal(file) || strings.ContainsAny(file, `/\:`) || windowsDevice(name) {
		return "", fault.New(fault.Invalid, "a tenant's name cannot be used as a file name")
	}
	dir, err := filepath.Abs(filepath.Join(d.DataDir, TenantsDir))
	if err != nil {
		return "", fault.New(fault.Internal, "the data directory cannot be made absolute")
	}
	path := filepath.Join(dir, file)
	if filepath.Dir(path) != dir {
		return "", fault.New(fault.Invalid, "a tenant's name cannot be used as a file name")
	}
	return path, nil
}

// Location implements registry.Driver: the path of the tenant's file, which
// holds no secret. A tenant on SQLite has no address; one given is Invalid,
// as the API contract says.
func (d SQLiteTenants) Location(name, address string) (string, error) {
	if address != "" {
		return "", fault.New(fault.Invalid, "a tenant on SQLite takes no database address; its file is made in the data directory")
	}
	return d.Path(name)
}

// SQLiteFilesPerTenant is the most files one SQLite tenant's store holds open:
// its database file and its write-ahead log for each of its five connections
// (one writer and four readers), and its shared-memory file, which SQLite
// opens once per database in a process (unix VFS, os_unix.c). An idle store,
// as measured with 1,000 tenants, holds five; this is the most, so the
// registry's warning comes before the limit is met under load, not after.
const SQLiteFilesPerTenant = 11

// FilesPerTenant says how many files one tenant's store can hold open at
// most. The registry reads it, as an optional method of its Driver, for its
// warning about the process's limit on open files.
func (SQLiteTenants) FilesPerTenant() int { return SQLiteFilesPerTenant }

// Open implements registry.Driver: it opens, creating them when they do not
// exist, the tenants directory (readable by its owner only) and the tenant's
// file. The store is bound to the tenant id (sqlite.OpenTenant). It does not
// migrate.
func (d SQLiteTenants) Open(ctx context.Context, id, name, address string) (tenant.Store, error) {
	path, err := d.Location(name, address)
	if err != nil {
		return nil, err
	}
	// 0700, as the master's file is 0600: the directory holds every
	// tenant's catalog. MkdirAll leaves an existing directory as it is.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		var pathErr *os.PathError
		if errors.As(err, &pathErr) {
			return nil, fault.New(fault.Unavailable, "the tenants directory cannot be made: "+pathErr.Err.Error())
		}
		return nil, fault.New(fault.Unavailable, "the tenants directory cannot be made")
	}
	s, err := sqlite.OpenTenant(ctx, path, id)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// windowsDevice reports, on Windows, whether name is one of the device names
// Windows reserves (CON, PRN, AUX, NUL, COM0 to COM9, LPT0 to LPT9). Before
// Windows 11 such a name opened the device even with an extension, so
// "con.db" was the console, not a file; filepath.IsLocal no longer refuses
// it since Go 1.23, which follows Windows 11 (seen in CI). The tenant name
// rule (master.ValidateTenantName) refuses these names on every system; this
// is the second wall, so a change to that rule cannot make a device here. A
// tenant's name is lowercase, so the comparison is too.
func windowsDevice(name string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	switch name {
	case "con", "prn", "aux", "nul":
		return true
	}
	return len(name) == 4 && (strings.HasPrefix(name, "com") || strings.HasPrefix(name, "lpt")) &&
		name[3] >= '0' && name[3] <= '9'
}

// PostgresTenants opens the databases of tenants on Postgres. Each tenant has
// the address of its own database (a postgres:// URL, sealed in the master),
// in the schema the URL's search_path names; one tenant to a database and
// schema, which the store checks (postgres.OpenTenant). It is what package
// registry calls a Driver.
type PostgresTenants struct{}

// Location implements registry.Driver: host:port/database, or for a Unix
// socket unix:/directory/database, with no user and no password. An address
// that is not a postgres:// URL, that does not parse, or that is to another
// machine with no sslmode written down is Invalid, with a message that does
// not repeat it.
func (PostgresTenants) Location(_, address string) (string, error) {
	return postgres.Location(address)
}

// Open implements registry.Driver: it opens a pool of connections to the
// tenant's database and checks that it answers and that it was not made for
// another tenant. It does not migrate.
func (PostgresTenants) Open(ctx context.Context, id, _, address string) (tenant.Store, error) {
	s, err := postgres.OpenTenant(ctx, address, id)
	if err != nil {
		return nil, err
	}
	return s, nil
}

// FilesPerTenant says how many connections, each a socket, one tenant's store
// holds at most with the default pool (postgres.DefaultTenantConnections). An
// address may set pool_max_conns to more; the registry's warning is an
// estimate from the default.
func (PostgresTenants) FilesPerTenant() int { return postgres.DefaultTenantConnections }
