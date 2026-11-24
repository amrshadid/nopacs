package driver

import (
	"context"
	"strings"

	"github.com/amrshadid/nopacs/internal/driver/postgres"
	"github.com/amrshadid/nopacs/internal/driver/sqlite"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// SQLiteScheme is the prefix of a SQLite database's address; what follows it
// is the file's path, as the operating system writes it.
const SQLiteScheme = "sqlite:"

// PostgresScheme and PostgresAltScheme are the prefixes of a Postgres
// database's address, which is the whole postgres:// URL.
const (
	PostgresScheme    = "postgres://"
	PostgresAltScheme = "postgresql://"
)

// OpenMaster opens the master database at address. It does not migrate it;
// the caller calls Migrate. An address of no known type is Invalid, and the
// message never repeats the address.
func OpenMaster(ctx context.Context, address string) (master.Store, error) {
	if path, ok := strings.CutPrefix(address, SQLiteScheme); ok {
		return sqlite.OpenMaster(ctx, path)
	}
	lower := strings.ToLower(address)
	if strings.HasPrefix(lower, PostgresScheme) || strings.HasPrefix(lower, PostgresAltScheme) {
		return postgres.OpenMaster(ctx, address)
	}
	return nil, fault.New(fault.Invalid, "a database address starts with sqlite: or postgres://")
}
