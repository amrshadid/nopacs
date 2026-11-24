package tenanttest

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// Driver is how the suite opens a driver's stores: each a migrated store of
// a new database, closed when tb ends.
type Driver struct {
	// Open opens one store for tenantID.
	Open func(tb testing.TB, tenantID string) EventStore
	// OpenPair opens two stores on one new database, as two processes
	// would, for the tests across instances.
	OpenPair func(tb testing.TB, tenantID string) (first, second EventStore)
	// Scratch makes the scratch schema a rebuild names on a driver that
	// needs one; nil for one that needs none.
	Scratch Scratch
}

// Run runs the whole contract suite of the tenant port against d's stores:
// a group per concern (the store, the pipeline, the events, sending,
// de-identification), each also
// across two instances on one database. It is the suite's one entry point,
// so a driver cannot pass part of the suite and seem to pass it all.
func Run(t *testing.T, d Driver) {
	t.Run("Store", func(t *testing.T) {
		runStore(t, func(tb testing.TB, id string) tenant.Store { return d.Open(tb, id) })
	})
	t.Run("StoreShared", func(t *testing.T) {
		runStoreShared(t, func(tb testing.TB, id string) (tenant.Store, tenant.Store) {
			a, b := d.OpenPair(tb, id)
			return a, b
		})
	})
	t.Run("Pipeline", func(t *testing.T) {
		runPipeline(t, func(tb testing.TB, id string) tenant.Pipeline { return d.Open(tb, id) })
	})
	t.Run("PipelineShared", func(t *testing.T) {
		runPipelineShared(t, func(tb testing.TB, id string) (tenant.Pipeline, tenant.Pipeline) {
			a, b := d.OpenPair(tb, id)
			return a, b
		})
	})
	t.Run("Events", func(t *testing.T) {
		runEvents(t, func(tb testing.TB, id string) EventStore { return d.Open(tb, id) })
	})
	t.Run("EventsShared", func(t *testing.T) {
		runEventsShared(t, func(tb testing.TB, id string) (EventStore, EventStore) { return d.OpenPair(tb, id) })
	})
	t.Run("Sending", func(t *testing.T) {
		runSending(t, func(tb testing.TB, id string) EventStore { return d.Open(tb, id) })
	})
	t.Run("Deidentification", func(t *testing.T) {
		runDeidentification(t, func(tb testing.TB, id string) tenant.Pipeline { return d.Open(tb, id) })
	})
	t.Run("SendingShared", func(t *testing.T) {
		runSendingShared(t, func(tb testing.TB, id string) (EventStore, EventStore) { return d.OpenPair(tb, id) })
	})
	t.Run("Importing", func(t *testing.T) {
		runImporting(t, func(tb testing.TB, id string) EventStore { return d.Open(tb, id) })
	})
	t.Run("Scrubbing", func(t *testing.T) {
		runScrubbing(t, func(tb testing.TB, id string) EventStore { return d.Open(tb, id) })
	})
	t.Run("Rebuilding", func(t *testing.T) {
		runRebuilding(t, func(tb testing.TB, id string) EventStore { return d.Open(tb, id) }, d.Scratch)
	})
	t.Run("Corrections", func(t *testing.T) {
		runCorrections(t, func(tb testing.TB, id string) EventStore { return d.Open(tb, id) })
	})
	t.Run("ImportingShared", func(t *testing.T) {
		runImportingShared(t, func(tb testing.TB, id string) (EventStore, EventStore) { return d.OpenPair(tb, id) })
	})
}
