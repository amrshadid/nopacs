package postgres

import (
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/master"

	"github.com/amrshadid/nopacs/internal/master/mastertest"
)

// TestMasterContract runs the master port's contract suite on Postgres.
func TestMasterContract(t *testing.T) {
	mastertest.Run(t, openTest)
}

// openPair opens two stores, each with its own pool, on one new migrated
// schema: the two instances of one installation that RunShared races.
func openPair(tb testing.TB) (first, second master.Store) {
	tb.Helper()
	address := newSchema(tb)
	one := openAt(tb, address)
	if err := one.Migrate(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return one, openAt(tb, address)
}

// TestMasterContractShared runs the tests of the master port that need two
// instances of the store on one database.
func TestMasterContractShared(t *testing.T) {
	mastertest.RunShared(t, openPair)
}
