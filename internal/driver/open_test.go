package driver

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestOpenMasterByScheme guards that a sqlite: address opens a working
// master store, and that an unknown scheme is Invalid without the address,
// which may hold a password, in the error.
func TestOpenMasterByScheme(t *testing.T) {
	ctx := context.Background()
	store, err := OpenMaster(ctx, SQLiteScheme+filepath.Join(t.TempDir(), "master.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := store.Version(ctx); err != nil || v < 1 {
		t.Errorf("Version = %d, %v; want at least 1", v, err)
	}

	const address = "mysql://admin:hunter2@db.example/master" //nolint:gosec // G101: a made-up password whose absence from the error is the point.
	_, err = OpenMaster(ctx, address)
	if fault.KindOf(err) != fault.Invalid {
		t.Errorf("unknown scheme: %v, want Invalid", err)
	}
	if err != nil && (strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "db.example")) {
		t.Errorf("the error repeats the address: %v", err)
	}
}

// TestOpenMasterPostgres guards that a postgres:// address, in either
// spelling of the scheme, is handed to the Postgres driver, and that a
// database that cannot be reached is Unavailable with the address, which
// holds a password, not in the error. It needs no Postgres: nothing listens
// on port 1.
func TestOpenMasterPostgres(t *testing.T) {
	for _, scheme := range []string{"postgres://", "postgresql://", "POSTGRES://"} {
		address := scheme + "admin:hunter2@127.0.0.1:1/master?sslmode=disable&connect_timeout=2" //nolint:gosec // G101: a made-up password whose absence from the error is the point.
		_, err := OpenMaster(context.Background(), address)
		if fault.KindOf(err) != fault.Unavailable {
			t.Errorf("%s: %v, want Unavailable", scheme, err)
		}
		if err != nil && (strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "127.0.0.1")) {
			t.Errorf("%s: the error repeats the address: %v", scheme, err)
		}
	}
}
