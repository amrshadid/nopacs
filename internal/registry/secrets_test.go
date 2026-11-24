package registry

import (
	"bytes"
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// hashOf returns the label part h makes of v.
func hashOf(t *testing.T, h naming.Hasher, v string) string {
	t.Helper()
	return h.Hash([]byte(v))
}

// withoutSecrets is a driver whose stores keep no secrets, as a store of a
// server older than the tenant port's secrets: a tenant it makes has no
// naming key.
type withoutSecrets struct{ Driver }

// Open opens the database with the driver, hiding every method but
// tenant.Store's.
func (w withoutSecrets) Open(ctx context.Context, id, name, address string) (tenant.Store, error) {
	s, err := w.Driver.Open(ctx, id, name, address)
	if err != nil {
		return nil, err
	}
	return struct{ tenant.Store }{s}, nil
}

// TestATenantIsMadeWithItsNamingKey guards the naming key's life: a tenant
// is ready only with its key kept, sealed so that it opens with the
// installation key under its own id and under no other; the key is the
// same after the registry starts again (labels made before and after agree);
// two tenants have different keys; and the log holds no byte of it.
func TestATenantIsMadeWithItsNamingKey(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	e.create(r, "id-other", "other", driver.SQLiteDriver, "")

	st, err := r.Store("lab")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := st.(tenant.Secrets).TenantSecret(ctx, tenant.SecretNaming)
	if err != nil {
		t.Fatalf("a ready tenant has no naming key: %v", err)
	}
	key, err := e.keys.Open(SecretContext("id-lab", tenant.SecretNaming), sealed)
	if err != nil || len(key) != namingKeyLength {
		t.Fatalf("the key opens to %d bytes, %v", len(key), err)
	}
	if _, err := e.keys.Open(SecretContext("id-other", tenant.SecretNaming), sealed); err == nil {
		t.Fatal("the key opens under another tenant's id")
	}
	if bytes.Contains([]byte(e.logBuf.String()), key) {
		t.Fatal("the log holds the key")
	}

	h, err := r.NamingHasher(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	other, err := r.NamingHasher(ctx, "other")
	if err != nil {
		t.Fatal(err)
	}
	if hashOf(t, h, "JaneDoe1") == hashOf(t, other, "JaneDoe1") {
		t.Fatal("two tenants hash a value alike")
	}
	before := hashOf(t, h, "JaneDoe1")
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}

	again := e.newRegistry(e.config())
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	again.checkAll().Wait()
	h2, err := again.NamingHasher(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if after := hashOf(t, h2, "JaneDoe1"); after != before {
		t.Fatalf("the label changed across a restart: %s, then %s", before, after)
	}
}

// TestATenantWithoutAKeyIsGivenOneAtOpen guards an older tenant: one whose
// database was made before tenants had a naming key is given one when it is
// next opened, before it is ready, and keeps it from then on.
func TestATenantWithoutAKeyIsGivenOneAtOpen(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	old := e.config()
	old.Drivers = map[string]Driver{driver.SQLiteDriver: withoutSecrets{driver.SQLiteTenants{DataDir: e.data}}}
	r := e.newRegistry(old)
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	if _, err := r.NamingHasher(ctx, "lab"); fault.KindOf(err) != fault.Internal {
		t.Fatalf("a store without secrets: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := (driver.SQLiteTenants{DataDir: e.data}).Open(ctx, "id-lab", "lab", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.(tenant.Secrets).TenantSecret(ctx, tenant.SecretNaming); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("the older tenant has a key already: %v", err)
	}
	_ = s.Close()

	again := e.newRegistry(e.config())
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	again.checkAll().Wait()
	if _, err := again.NamingHasher(ctx, "lab"); err != nil {
		t.Fatalf("the older tenant has no naming key after its open: %v", err)
	}
}

// TestNamingHasherOfATenantNotReady guards the refusal: a tenant not known
// is NotFound, as Store answers.
func TestNamingHasherOfATenantNotReady(t *testing.T) {
	e := newEnv(t)
	r := e.newRegistry(e.config())
	if _, err := r.NamingHasher(context.Background(), "nobody"); fault.KindOf(err) != fault.NotFound {
		t.Fatalf("an unknown tenant: %v", err)
	}
}

// TestATenantIsMadeWithItsDeidentificationKeys guards the keys of UIDs and
// dates: a ready tenant has each, sealed under its own name and the
// tenant's id so that it opens there and nowhere else (not under another
// name, not under another tenant), each 32 bytes and different from the
// naming key and from each other; the same after the registry starts
// again, so a UID remapped or a date shifted before a restart is the same
// after; and the log holds no byte of either.
func TestATenantIsMadeWithItsDeidentificationKeys(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	read := func(r *Registry) map[tenant.SecretName][]byte {
		t.Helper()
		st, err := r.Store("lab")
		if err != nil {
			t.Fatal(err)
		}
		keys := map[tenant.SecretName][]byte{}
		for _, name := range tenant.SecretNames() {
			sealed, err := st.(tenant.Secrets).TenantSecret(ctx, name)
			if err != nil {
				t.Fatalf("a ready tenant has no %s key: %v", name, err)
			}
			key, err := e.keys.Open(SecretContext("id-lab", name), sealed)
			if err != nil || len(key) != namingKeyLength {
				t.Fatalf("the %s key opens to %d bytes, %v", name, len(key), err)
			}
			if _, err := e.keys.Open(SecretContext("id-other", name), sealed); err == nil {
				t.Fatalf("the %s key opens under another tenant's id", name)
			}
			for _, other := range tenant.SecretNames() {
				if _, err := e.keys.Open(SecretContext("id-lab", other), sealed); other != name && err == nil {
					t.Fatalf("the %s key opens under the name %s", name, other)
				}
			}
			keys[name] = key
		}
		return keys
	}
	first := read(r)
	if len(first) != 3 {
		t.Fatalf("%d keys", len(first))
	}
	if bytes.Equal(first[tenant.SecretUID], first[tenant.SecretDate]) || bytes.Equal(first[tenant.SecretUID], first[tenant.SecretNaming]) ||
		bytes.Equal(first[tenant.SecretDate], first[tenant.SecretNaming]) {
		t.Fatal("two of the tenant's keys are equal")
	}
	for name, key := range first {
		if bytes.Contains([]byte(e.logBuf.String()), key) {
			t.Fatalf("the log holds the %s key", name)
		}
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	again := e.newRegistry(e.config())
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	again.checkAll().Wait()
	for name, key := range read(again) {
		if !bytes.Equal(key, first[name]) {
			t.Fatalf("the %s key changed across a restart", name)
		}
	}
}
