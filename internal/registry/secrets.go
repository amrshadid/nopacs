package registry

import (
	"context"
	"crypto/rand"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// namingKeyLength is the length of each of a tenant's secrets (the naming
// key and the keys of UIDs and dates): the output size of SHA-256, the most
// HMAC-SHA-256 uses at full strength (RFC 2104 section 3), and
// naming.MinKeyLength.
const namingKeyLength = naming.MinKeyLength

// SecretContext is what a tenant's secret is sealed under (package secret),
// which binds it to the tenant's id and to the secret's name: a sealed
// secret copied into another tenant's database, or under another name, does
// not open there.
func SecretContext(id string, name tenant.SecretName) string {
	return "tenant/" + id + "/secret/" + string(name)
}

// errNoSecrets is a tenant whose store keeps no secrets: one opened by a
// driver older than the tenant port's secrets, which this server does not
// have.
var errNoSecrets = fault.New(fault.Internal, "the tenant's database keeps no secrets")

// ensureSecrets gives the tenant whose store is st each of its secrets it
// has none of (tenant.SecretNames: the naming key, and the keys of UIDs and
// dates de-identification uses): 32 bytes each from the system's
// cryptographic source, sealed with the installation key under
// SecretContext and kept by the store, which keeps the first if two
// servers make one at once and never replaces one. It runs at every open,
// after the migration, so a tenant made before tenants had a secret gets it
// before anything is made with it, and a tenant made now gets them all
// before it is ready. A secret the store has is only read. Each is its own
// key, made apart, so one can be destroyed alone (a data set closed) and
// none can be worked out from another.
func (r *Registry) ensureSecrets(ctx context.Context, id string, st tenant.Store) error {
	ks, ok := st.(tenant.Secrets)
	if !ok {
		// A store of a test's own, or of a driver without secrets: nothing
		// it serves can name with a keyed hash, which NamingHasher says.
		return nil
	}
	for _, name := range tenant.SecretNames() {
		if err := r.ensureSecret(ctx, id, ks, name); err != nil {
			return err
		}
	}
	return nil
}

// ensureSecret makes, seals and keeps the tenant's secret of that name
// when its store has none.
func (r *Registry) ensureSecret(ctx context.Context, id string, ks tenant.Secrets, name tenant.SecretName) error {
	_, err := ks.TenantSecret(ctx, name)
	if err == nil || fault.KindOf(err) != fault.NotFound {
		return err
	}
	key := make([]byte, namingKeyLength)
	if _, err := rand.Read(key); err != nil {
		return fault.Wrap(fault.Internal, err, "registry: the tenant's "+string(name)+" key could not be made")
	}
	sealed, err := r.cfg.Keys.Seal(SecretContext(id, name), key)
	clear(key)
	if err != nil {
		return fault.Wrap(fault.Internal, err, "registry: the tenant's "+string(name)+" key could not be sealed")
	}
	_, err = ks.KeepTenantSecret(ctx, name, sealed, r.cfg.Now())
	return err
}

// NamingHasher returns the keyed hash of the tenant named name, for its
// naming rules' {#X} (core/naming): its naming key, read sealed from its
// database and opened with the installation key. The key is never returned,
// logged or put in an error. A tenant not ready is Unavailable, as Store
// answers; one whose store keeps no secrets, or whose key does not open with
// this key file, is Internal.
func (r *Registry) NamingHasher(ctx context.Context, name string) (naming.Hasher, error) {
	st, err := r.Store(name)
	if err != nil {
		return nil, err
	}
	e := (*r.entries.Load())[name]
	if e == nil {
		return nil, fault.New(fault.NotFound, "no such tenant")
	}
	ks, ok := st.(tenant.Secrets)
	if !ok {
		return nil, errNoSecrets
	}
	sealed, err := ks.TenantSecret(ctx, tenant.SecretNaming)
	if err != nil {
		return nil, err
	}
	key, err := r.cfg.Keys.Open(SecretContext(e.id, tenant.SecretNaming), sealed)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "registry: the tenant's naming key does not open with this key file")
	}
	h, err := naming.NewHasher(key)
	clear(key)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "registry: the tenant's naming key is not a key")
	}
	return h, nil
}
