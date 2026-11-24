package access_test

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestSuspendedKeyRefused guards that a key a restore suspended is refused
// as a key that is no good, with the one answer every such key gets and
// the reason "key suspended" in its entry, both when it is first presented
// and when a credential accepted before the suspension is read again; the
// local socket still passes, since it is known by the socket, never by a
// key.
func TestSuspendedKeyRefused(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleAdmin))
	cred, err := e.access.Authenticate(e.ctx, access.APIKey(k.full), facts("/m"))
	if err != nil {
		t.Fatal(err)
	}
	ks, ok := e.master.(master.KeySuspension)
	if !ok {
		t.Fatal("the test's master cannot suspend keys")
	}
	if n, err := ks.SuspendKeys(e.ctx, t0, e.setupEntry()); err != nil || n != 1 {
		t.Fatalf("SuspendKeys answered %d, %v; want one key suspended", n, err)
	}
	// A second suspension writes its time over the first: whatever a row
	// held is never trusted to mean suspended.
	if n, err := ks.SuspendKeys(e.ctx, t0, e.setupEntry()); err != nil || n != 1 {
		t.Fatalf("a second SuspendKeys answered %d, %v; want the one key suspended again", n, err)
	}
	e.reset()
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), access.OnInstallation(master.RoleAdmin), facts("/m"))
	wantErr(t, "suspended", err, fault.Unauthenticated, access.MessageUnauthenticated)
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Reason != "key suspended" || entries[0].KeyID != k.id {
		t.Fatalf("entries %+v", entries)
	}
	e.reset()
	_, err = e.access.Check(e.ctx, cred.Reread(), access.OnInstallation(master.RoleAdmin), facts("/m"))
	wantErr(t, "suspended since", err, fault.Unauthenticated, access.MessageUnauthenticated)
	got, err := e.master.Key(e.ctx, k.id)
	if err != nil || !got.Suspended.Equal(t0.Truncate(time.Microsecond)) {
		t.Fatalf("the key reads %+v (%v), want suspended at %v", got, err, t0)
	}
}

// TestSuspendedKeyRotatedByLocalOperator guards the way back for a key a
// restore suspended: the local operator, on the socket, rotates it; the
// successor holds the same grants, is not suspended and works at once,
// while the suspended key stays refused.
func TestSuspendedKeyRotatedByLocalOperator(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(72*time.Hour), on(master.TenantScope(e.lab.ID), master.RoleWriter))
	ks, ok := e.master.(master.KeySuspension)
	if !ok {
		t.Fatal("the test's master cannot suspend keys")
	}
	if _, err := ks.SuspendKeys(e.ctx, t0, e.setupEntry()); err != nil {
		t.Fatal(err)
	}
	made, rotated, err := e.access.RotateKey(e.ctx, e.admin("RotateKey"), k.id, time.Hour, maxLife)
	if err != nil {
		t.Fatalf("the local operator cannot rotate a suspended key: %v", err)
	}
	if rotated.Successor != made.Key.ID || !made.Key.Suspended.IsZero() {
		t.Fatalf("rotated %+v, successor %+v: want a successor that is not suspended", rotated, made.Key)
	}
	need := access.OnTenant("lab", master.RoleWriter)
	if _, err := e.access.Check(e.ctx, access.APIKey(made.Secret.Key()), need, facts("M")); err != nil {
		t.Fatalf("the successor of a suspended key is refused: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), need, facts("M"))
	wantErr(t, "the suspended key after its rotation", err, fault.Unauthenticated, access.MessageUnauthenticated)
}
