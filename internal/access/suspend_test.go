package access_test

import (
	"fmt"
	"strings"
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

// suspendAll suspends every key in e's master, as a restore does.
func (e *env) suspendAll(t *testing.T) {
	t.Helper()
	ks, ok := e.master.(master.KeySuspension)
	if !ok {
		t.Fatal("the test's master cannot suspend keys")
	}
	if _, err := ks.SuspendKeys(e.ctx, t0, e.setupEntry()); err != nil {
		t.Fatal(err)
	}
}

// TestReinstateKeysByLocalOperator guards the way back the contract names:
// the local operator reinstates a suspended key by id, the key works again
// at once with the grants it had, the key's entry names it as its object,
// and a key not named stays refused.
func TestReinstateKeysByLocalOperator(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(72*time.Hour), on(master.TenantScope(e.lab.ID), master.RoleWriter))
	other := e.key(t0.Add(72*time.Hour), on(master.TenantScope(e.lab.ID), master.RoleWriter))
	e.suspendAll(t)
	need := access.OnTenant("lab", master.RoleWriter)

	keys, n, err := e.access.ReinstateKeys(e.ctx, e.admin("ReinstateKeys"), []string{k.id}, false)
	if err != nil || n != 1 || len(keys) != 1 || keys[0].ID != k.id || !keys[0].Suspended.IsZero() || keys[0].Hash != nil {
		t.Fatalf("ReinstateKeys answered %+v, %d, %v; want the one key, not suspended, without its hash", keys, n, err)
	}
	if _, err := e.access.Check(e.ctx, access.APIKey(k.full), need, facts("M")); err != nil {
		t.Fatalf("the reinstated key is refused: %v", err)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(other.full), need, facts("M"))
	wantErr(t, "a key not reinstated", err, fault.Unauthenticated, access.MessageUnauthenticated)

	entries, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, a := range entries {
		if a.Method == "ReinstateKeys" && a.Outcome == master.OutcomeDone {
			found++
			if a.Object != k.id || a.LocalUser != "501" || a.Details["all"] != "false" {
				t.Errorf("the reinstatement's entry reads %+v; want the key as its object, the local user and all=false", a)
			}
		}
	}
	if found != 1 {
		t.Errorf("%d entries for the reinstatement; want one per key reinstated", found)
	}
}

// TestReinstateKeysRefusesKeys guards that a key, even one with admin on the
// installation, may not reinstate keys: it is answered permission denied,
// the refusal is recorded with its reason, and nothing changes.
func TestReinstateKeysRefusesKeys(t *testing.T) {
	e := newEnv(t)
	suspended := e.key(t0.Add(72*time.Hour), on(master.Installation, master.RoleAdmin))
	e.suspendAll(t)
	admin := e.key(t0.Add(72*time.Hour), on(master.Installation, master.RoleAdmin)) // made after: not suspended
	d := e.decide(access.APIKey(admin.full), access.OnInstallation(master.RoleAdmin), "ReinstateKeys")
	e.reset()
	for _, all := range []bool{false, true} {
		var ids []string
		if !all {
			ids = []string{suspended.id}
		}
		_, _, err := e.access.ReinstateKeys(e.ctx, d, ids, all)
		wantErr(t, "an admin key reinstating", err, fault.Denied, access.MessageDenied)
	}
	if _, entries := e.recorded(); len(entries) != 2 || entries[0].Reason != "only the local operator reinstates keys" || entries[0].KeyID != admin.id ||
		entries[0].Outcome != master.OutcomeRefused {
		t.Fatalf("entries %+v; want each refusal recorded", entries)
	}
	got, err := e.master.Key(e.ctx, suspended.id)
	if err != nil || got.Suspended.IsZero() {
		t.Fatalf("the key reads %+v (%v); want it still suspended", got, err)
	}
}

// TestWholeKeyGivenAsIDNeverLogged guards that a whole key pasted where a
// key id goes, to ReinstateKeys and to the other operations that take a key
// id from a caller, is refused as Invalid without repeating it, and that
// the failed request's audit entry, which is hash-chained and exported,
// holds it neither in its reason nor its object nor its details.
func TestWholeKeyGivenAsIDNeverLogged(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(72*time.Hour), on(master.Installation, master.RoleAdmin))
	e.suspendAll(t)
	secretPart := k.full[len(k.full)-44:]
	calls := map[string]func() error{
		"ReinstateKeys": func() error {
			_, _, err := e.access.ReinstateKeys(e.ctx, e.admin("ReinstateKeys"), []string{k.full}, false)
			return err
		},
		"GetKey":    func() error { _, err := e.access.GetKey(e.ctx, e.admin("GetKey"), k.full); return err },
		"RevokeKey": func() error { _, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), k.full); return err },
		"SetKeyRules": func() error {
			_, err := e.access.SetKeyRules(e.ctx, e.admin("SetKeyRules"), k.full, master.KeyRules{})
			return err
		},
		"RotateKey": func() error {
			_, _, err := e.access.RotateKey(e.ctx, e.admin("RotateKey"), k.full, time.Hour, maxLife)
			return err
		},
		"SetGrant": func() error {
			_, _, err := e.access.SetGrant(e.ctx, e.admin("SetGrant"), k.full, access.TenantNamed("lab"), master.RoleReader)
			return err
		},
		"RemoveGrant": func() error {
			_, err := e.access.RemoveGrant(e.ctx, e.admin("RemoveGrant"), k.full, access.TenantNamed("lab"))
			return err
		},
		"ListGrants": func() error {
			_, _, err := e.access.ListGrants(e.ctx, e.admin("ListGrants"), access.GrantsOfKey(k.full), master.Page{})
			return err
		},
	}
	for name, call := range calls {
		err := call()
		wantErr(t, name+" with a whole key", err, fault.Invalid, "")
		if strings.Contains(fault.Message(err), secretPart) || strings.Contains(err.Error(), secretPart) {
			t.Errorf("%s: the error repeats the whole key: %v", name, err)
		}
	}
	e.flush()
	entries, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{}, master.Page{Limit: master.MaxPageLimit})
	if err != nil {
		t.Fatal(err)
	}
	failed := 0
	for _, a := range entries {
		if strings.Contains(fmt.Sprintf("%v %v %v", a.Reason, a.Object, a.Details), secretPart) {
			t.Errorf("an audit entry holds the whole key: %+v", a)
		}
		if a.Outcome == master.OutcomeFailed {
			failed++
		}
	}
	if failed != len(calls) {
		t.Errorf("%d failed entries; want each refused request recorded", failed)
	}
	if got, err := e.master.Key(e.ctx, k.id); err != nil || got.Suspended.IsZero() || !got.Revoked.IsZero() {
		t.Errorf("the key reads %+v (%v); want it untouched", got, err)
	}
}

// TestReinstatedRevokedKeyStaysRefused guards that reinstating clears only
// the suspension: a key revoked after the backup, then suspended by the
// restore and reinstated by mistake, is still refused, now as revoked.
func TestReinstatedRevokedKeyStaysRefused(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(72*time.Hour), on(master.Installation, master.RoleAdmin))
	if _, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), k.id); err != nil {
		t.Fatal(err)
	}
	e.suspendAll(t)
	if _, n, err := e.access.ReinstateKeys(e.ctx, e.admin("ReinstateKeys"), nil, true); err != nil || n != 1 {
		t.Fatalf("ReinstateKeys, all answered %d, %v; want the one key", n, err)
	}
	e.reset()
	_, err := e.access.Check(e.ctx, access.APIKey(k.full), access.OnInstallation(master.RoleAdmin), facts("/m"))
	wantErr(t, "a revoked key reinstated", err, fault.Unauthenticated, access.MessageUnauthenticated)
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Reason != "key revoked" {
		t.Fatalf("entries %+v; want the refusal recorded as revoked", entries)
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
