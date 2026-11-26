package holds

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// activatedStore is a tenant's database whose catalog a test activates
// again (the history id it answers moves, as a rebuild's replace or a
// restore moves it), counting each listing of its storages: one per
// adoption of the holds found on them.
type activatedStore struct {
	Store
	history atomic.Value
	listed  atomic.Int64
}

// EventHistory answers the history id the test set.
func (a *activatedStore) EventHistory(context.Context) (string, error) {
	h, _ := a.history.Load().(string)
	return h, nil
}

// ListStorages counts the listing, then lists.
func (a *activatedStore) ListStorages(ctx context.Context, page master.Page) ([]tenant.Storage, string, error) {
	a.listed.Add(1)
	return a.Store.ListStorages(ctx, page)
}

// TestTheWorkerAdoptsAgainAfterEachActivation guards the hold-record
// worker's adoption: it adopts the holds found on storage once after its
// start, not again at every pass while the catalog stays as it is, and
// again after the catalog is activated anew, when the database may lack a
// hold its records hold.
func TestTheWorkerAdoptsAgainAfterEachActivation(t *testing.T) {
	r := newRig(t)
	h := r.place(r.writer(), r.subjects[0])
	fresh := openTenant(t, 1)
	st := &activatedStore{Store: fresh}
	st.history.Store("first")
	w, err := New(r.config(st))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(r.ctx)
	done := make(chan struct{})
	go func() { defer close(done); w.Run(ctx) }()
	defer func() { cancel(); <-done }()
	wait := func(what string, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("%s did not happen", what)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	wait("the adoption after the start", func() bool {
		got, err := fresh.Hold(r.ctx, h.ID)
		return err == nil && got.State == tenant.HoldActive
	})
	first := st.listed.Load()
	time.Sleep(10 * w.cfg.Interval)
	if n := st.listed.Load(); n != first {
		t.Fatalf("with no activation the worker read the storages' records %d more times", n-first)
	}
	st.history.Store("made-again")
	wait("an adoption after the activation", func() bool { return st.listed.Load() > first })
}
