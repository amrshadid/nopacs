package registry

import (
	"bytes"
	"context"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/driver"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestDeidKeysAreTheTenantsSecrets guards the keys of de-identification: a
// tenant's UID, date and naming keys, each of full length and each its own;
// the hash key is the naming key, so a hashed identifier equals the label
// code of the same value; the same keys after the registry starts again;
// another tenant's are others; and a tenant not known is NotFound.
func TestDeidKeysAreTheTenantsSecrets(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	e.create(r, "id-other", "other", driver.SQLiteDriver, "")
	k, err := r.DeidKeys(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	for name, key := range map[string][]byte{"uid": k.UID, "date": k.Date, "hash": k.Hash} {
		if len(key) != namingKeyLength {
			t.Errorf("the %s key is %d bytes", name, len(key))
		}
	}
	if bytes.Equal(k.UID, k.Date) || bytes.Equal(k.UID, k.Hash) || bytes.Equal(k.Date, k.Hash) {
		t.Error("two of the keys are equal")
	}
	h, err := r.NamingHasher(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	if hashOf(t, h, "98890234") != naming.HashText(k.Hash, []byte("98890234")) {
		t.Error("the hash key is not the naming key")
	}
	other, err := r.DeidKeys(ctx, "other")
	if err != nil || bytes.Equal(other.UID, k.UID) || bytes.Equal(other.Date, k.Date) {
		t.Errorf("another tenant's keys are the same (%v)", err)
	}
	if _, err := r.DeidKeys(ctx, "nobody"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a tenant not known: %v", err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatal(err)
	}
	again := e.newRegistry(e.config())
	if err := again.Start(ctx); err != nil {
		t.Fatal(err)
	}
	again.checkAll().Wait()
	k2, err := again.DeidKeys(ctx, "lab")
	if err != nil || !bytes.Equal(k2.UID, k.UID) || !bytes.Equal(k2.Date, k.Date) || !bytes.Equal(k2.Hash, k.Hash) {
		t.Errorf("the keys after a start again differ (%v)", err)
	}
}

// TestUIDKeyIsTheUIDKeyAlone guards the key a check of the archive reads:
// the tenant's UID key, the same DeidKeys answers, and nothing of the
// others; a tenant not known is NotFound.
func TestUIDKeyIsTheUIDKeyAlone(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	r := e.newRegistry(e.config())
	e.create(r, "id-lab", "lab", driver.SQLiteDriver, "")
	k, err := r.DeidKeys(ctx, "lab")
	if err != nil {
		t.Fatal(err)
	}
	u, err := r.UIDKey(ctx, "lab")
	if err != nil || !bytes.Equal(u, k.UID) || bytes.Equal(u, k.Date) || bytes.Equal(u, k.Hash) {
		t.Errorf("UIDKey = %d bytes, %v; not the UID key alone", len(u), err)
	}
	if _, err := r.UIDKey(ctx, "nobody"); fault.KindOf(err) != fault.NotFound {
		t.Errorf("a tenant not known: %v", err)
	}
}
