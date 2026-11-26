package checkedread

import (
	"context"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/place"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Replicas is what a read that turns from its primary asks the tenant's
// database: the scan's series when the reader knows only its labels, the
// replicas in step for it now, whether an instance's key is still the
// catalog's, and where to record an object of a replica found wrong. A
// read that its primary serves asks none of it.
type Replicas interface {
	// CatalogScan answers the scan with the labels of path: its series, and
	// the digest of its metadata file now, asked before a replica's file is
	// recorded as damaged.
	CatalogScan(ctx context.Context, path label.Path) (tenant.CatalogScan, error)
	// InStepReplicas answers, per series, the replicas in step for its scan
	// at the catalog's manifest digest now.
	InStepReplicas(ctx context.Context, seriesUIDs []string) (map[string][]string, error)
	// RecordReplicaDamage records an object of a replica found missing or
	// wrong.
	RecordReplicaDamage(ctx context.Context, d tenant.ReplicaDamageRecord) (bool, error)
	// HeldInstance answers the catalog's row of an instance now, asked
	// before a replica's object is recorded as damage, so a key a new
	// revision replaced is never recorded.
	HeldInstance(ctx context.Context, path label.Path, sopInstanceUID string) (tenant.ScanInstance, error)
}

// Recalls is what a read of an object not online asks the tenant's
// database: the object's recall, so the answer can say whether waiting
// brings it back (tenant.Recalls).
type Recalls interface {
	// Recall answers the row of the object at key on storage, and whether
	// there is one.
	Recall(ctx context.Context, storage, key string) (tenant.Recall, bool, error)
}

// Stores opens the storages a read reaches, each once, for the tenant
// whose pipeline it holds, and closes them at its end. Its Open, Stat and
// Source read an archived object from its primary, or from a replica in
// step when the primary cannot serve it (Open).
type Stores struct {
	opener   place.Opener
	tenantID func(ctx context.Context, name string) (string, error)
	p        tenant.Pipeline
	replicas Replicas
	// recalls is the tenant's database's recalls, nil for one that keeps
	// none: a read not online then says nothing of a recall.
	recalls    Recalls
	tenantName string
	tenant     place.Tenant
	open       map[string]blob.Store
	// served names the storages that served a read, in the order first
	// served; counts, when set, counts the reads a replica served.
	served []string
	counts *Counts
	seams  storesSeams
}

// storesSeams are what a test changes in Stores.
type storesSeams struct {
	// wrap, when set, wraps each storage as it is opened.
	wrap func(name string, s blob.Store) blob.Store
	// now, when set, is the clock of a damage record.
	now func() time.Time
}

// NewStores returns the storages of the tenant named name, none open yet.
// tenantID finds the tenant's identifier from its name, once, when the first
// storage is opened; opener opens a storage for reading.
func NewStores(opener place.Opener, tenantID func(ctx context.Context, name string) (string, error), p tenant.Pipeline, name string) *Stores {
	s := &Stores{opener: opener, tenantID: tenantID, p: p, replicas: p, tenantName: name, open: map[string]blob.Store{}}
	if rc, ok := p.(Recalls); ok {
		s.recalls = rc
	}
	return s
}

// WithCounts makes s count the reads a replica served, and the replicas'
// objects found wrong, in c, and returns s.
func (s *Stores) WithCounts(c *Counts) *Stores {
	s.counts = c
	return s
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
	if w := s.seams.wrap; w != nil {
		st = w(name, st)
	}
	s.open[name] = st
	return st, nil
}

// Served returns the names of the storages that served a read through
// Open, Stat or a Source, in the order each first served, for the audit
// entry of the request: more than one when a replica served some of its
// instances.
func (s *Stores) Served() []string { return slices.Clone(s.served) }

// servedBy notes that the storage named name served a read.
func (s *Stores) servedBy(name string) {
	if !slices.Contains(s.served, name) {
		s.served = append(s.served, name)
	}
}

// now is the time of a damage record.
func (s *Stores) now() time.Time {
	if f := s.seams.now; f != nil {
		return f()
	}
	return time.Now()
}

// Close closes every storage opened.
func (s *Stores) Close() {
	for _, st := range s.open {
		_ = st.Close() // Only read from; nothing to lose.
	}
}
