package servecmd

import (
	"context"
	"errors"
	"sync"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// tenantStorages opens one tenant's storages for its reception pipeline, by
// the rules the API's handlers apply (package place). The pipeline never
// closes a store it is handed (receive.OpenStorage), so an open store is
// kept per storage and handed to every step while the storage's row is
// unchanged; every use still applies the rules again (place.Opener.Check),
// so a storage that is no longer allowed is refused at its next use. A
// store replaced by a changed row may still be in a step's hands, so it is
// closed only when the pipeline stops (Close).
type tenantStorages struct {
	opener place.Opener
	master master.Store
	name   string
	store  tenant.Pipeline

	mu      sync.Mutex
	id      string
	open    map[string]openStore
	retired []blob.Store
	closed  bool
}

// openStore is a store open for a storage's row as it was when opened.
type openStore struct {
	row   tenant.Storage
	store blob.Store
}

// errStoragesClosed is a use after the pipeline stopped.
var errStoragesClosed = fault.New(fault.Unavailable, "the tenant's reception pipeline has stopped")

// writingOpener returns the opener of every store the server keeps open to
// write a tenant's storage: each reception pipeline's, which a pause or a
// changed row opens again while the server runs, and each corrections
// engine's. They all share cleanups, the server's one, so only the first
// open of a storage in the process removes what a crash left there and
// every later one opens beside it; otherwise the open of one would remove
// the temporary file of another's Put in progress, or its probe file, and
// fail it (place.Cleanups).
func writingOpener(values place.Values, keys place.Keys, cleanups *place.Cleanups) place.Opener {
	return place.Opener{Values: values, Keys: keys, Cleanups: cleanups}
}

// newTenantStorages returns the opener of the storages of the tenant named
// name, whose database is st.
func newTenantStorages(opener place.Opener, m master.Store, name string, st tenant.Pipeline) *tenantStorages {
	return &tenantStorages{opener: opener, master: m, name: name, store: st, open: map[string]openStore{}}
}

// tenant returns the tenant as package place opens its storages; its id,
// which a sealed secret is bound to, is read from the master once.
func (s *tenantStorages) tenant(ctx context.Context) (place.Tenant, error) {
	s.mu.Lock()
	id := s.id
	s.mu.Unlock()
	if id == "" {
		t, err := s.master.TenantByName(ctx, s.name)
		if err != nil {
			return place.Tenant{}, err
		}
		id = t.ID
		s.mu.Lock()
		s.id = id
		s.mu.Unlock()
	}
	return place.TenantOf(id, s.name, s.store), nil
}

// Open returns the tenant's storage named name, open, as receive.OpenStorage
// asks: the same store for every step while its row is unchanged.
func (s *tenantStorages) Open(ctx context.Context, name string) (blob.Store, error) {
	t, err := s.tenant(ctx)
	if err != nil {
		return nil, err
	}
	row, err := s.store.Storage(ctx, name)
	if err != nil {
		return nil, err
	}
	if err := s.opener.Check(ctx, t, row); err != nil {
		return nil, err
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errStoragesClosed
	}
	if o, ok := s.open[name]; ok && o.row.Updated.Equal(row.Updated) {
		s.mu.Unlock()
		return o.store, nil
	}
	s.mu.Unlock()
	bs, err := s.opener.Open(ctx, t, row)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		_ = bs.Close()
		return nil, errStoragesClosed
	}
	if o, ok := s.open[name]; ok {
		if o.row.Updated.Equal(row.Updated) {
			// Another step opened the same row meanwhile: keep one.
			_ = bs.Close()
			return o.store, nil
		}
		s.retired = append(s.retired, o.store)
	}
	s.open[name] = openStore{row: row, store: bs}
	return bs, nil
}

// Close closes every store opened, current and replaced. It is called once
// the pipeline has stopped, so no step holds one.
func (s *tenantStorages) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var errs []error
	for _, o := range s.open {
		errs = append(errs, o.store.Close())
	}
	for _, bs := range s.retired {
		errs = append(errs, bs.Close())
	}
	s.open, s.retired = nil, nil
	return errors.Join(errs...)
}
