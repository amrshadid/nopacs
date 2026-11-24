package checkedread

import (
	"context"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Stores opens the storages a read reaches, each once, for the tenant
// whose pipeline it holds, and closes them at its end.
type Stores struct {
	opener     place.Opener
	tenantID   func(ctx context.Context, name string) (string, error)
	p          tenant.Pipeline
	tenantName string
	tenant     place.Tenant
	open       map[string]blob.Store
}

// NewStores returns the storages of the tenant named name, none open yet.
// tenantID finds the tenant's identifier from its name, once, when the first
// storage is opened; opener opens a storage for reading.
func NewStores(opener place.Opener, tenantID func(ctx context.Context, name string) (string, error), p tenant.Pipeline, name string) *Stores {
	return &Stores{opener: opener, tenantID: tenantID, p: p, tenantName: name, open: map[string]blob.Store{}}
}

// Get returns the storage named name, opening it on first use. A storage
// that cannot be opened is the storage not answering or the server's
// fault, as the opener says.
func (s *Stores) Get(ctx context.Context, name string) (blob.Store, error) {
	if st, ok := s.open[name]; ok {
		return st, nil
	}
	if s.tenant.ID == "" {
		id, err := s.tenantID(ctx, s.tenantName)
		if err != nil {
			return nil, err
		}
		s.tenant = place.TenantOf(id, s.tenantName, s.p)
	}
	srow, err := s.p.Storage(ctx, name)
	if err != nil {
		return nil, err
	}
	st, err := s.opener.OpenForReading(ctx, s.tenant, srow)
	if err != nil {
		return nil, err
	}
	s.open[name] = st
	return st, nil
}

// Close closes every storage opened.
func (s *Stores) Close() {
	for _, st := range s.open {
		_ = st.Close() // Only read from; nothing to lose.
	}
}
