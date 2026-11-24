package access_test

import (
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// maxLife is the installation's maximum key lifetime the rotation tests
// pass.
const maxLife = 3650 * 24 * time.Hour

// TestRotateKeyByAdmin guards a rotation by the installation's admin: the
// successor works at once with the same grants, the rotated key keeps
// working through the overlap and is refused after it, the successor lives
// as long as the rotated key was made to, and an event names both.
func TestRotateKeyByAdmin(t *testing.T) {
	s := &sink{}
	e := newEnv(t, withEvents(s))
	old := e.key(t0.Add(72*time.Hour), on(master.TenantScope(e.lab.ID), master.RoleWriter))
	made, rotated, err := e.access.RotateKey(e.ctx, e.admin("RotateKey"), old.id, time.Hour, maxLife)
	if err != nil {
		t.Fatal(err)
	}
	if rotated.Successor != made.Key.ID || !rotated.Expires.Equal(t0.Add(time.Hour)) {
		t.Errorf("rotated key %+v", rotated)
	}
	// The rotated key was made 48 hours before t0 to expire 72 hours after:
	// a lifetime of 120 hours, which the successor gets from now.
	if !made.Key.Expires.Equal(t0.Add(120*time.Hour)) || made.Key.Holder != "test" {
		t.Errorf("successor %+v", made.Key)
	}
	need := access.OnTenant("lab", master.RoleWriter)
	for _, full := range []string{made.Secret.Key(), old.full} {
		if _, err := e.access.Check(e.ctx, access.APIKey(full), need, facts("M")); err != nil {
			t.Errorf("during the overlap: %v", err)
		}
	}
	e.clock.Set(t0.Add(time.Hour + time.Second))
	_, err = e.access.Check(e.ctx, access.APIKey(old.full), need, facts("M"))
	wantErr(t, "rotated key past its overlap", err, fault.Unauthenticated, access.MessageUnauthenticated)
	if _, err := e.access.Check(e.ctx, access.APIKey(made.Secret.Key()), need, facts("M")); err != nil {
		t.Errorf("successor after the overlap: %v", err)
	}
	ev := s.all()
	if len(ev) != 1 || ev[0].Kind != access.EventKeyRotated || ev[0].KeyID != old.id || ev[0].Detail != made.Key.ID {
		t.Errorf("events %+v", ev)
	}
}

// TestRotateKeySelf guards self-rotation: a key with no role on the
// installation may rotate itself, never another key, with an overlap of at
// least an hour, and its successor is bound by the installation's maximum
// lifetime.
func TestRotateKeySelf(t *testing.T) {
	e := newEnv(t)
	self := e.key(t0.Add(72*time.Hour), on(master.TenantScope(e.lab.ID), master.RoleReader))
	other := e.key(t0.Add(72 * time.Hour))
	d := e.decide(access.APIKey(self.full), access.Authenticated(), "RotateKey")
	e.reset()
	_, _, err := e.access.RotateKey(e.ctx, d, other.id, time.Hour, maxLife)
	wantErr(t, "another key", err, fault.Denied, access.MessageDenied)
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Outcome != master.OutcomeRefused || entries[0].Object != other.id {
		t.Errorf("entries %+v", entries)
	}
	// A key rotating itself cannot end itself sooner than an hour, so a
	// thief holding it cannot cut its holder off at once.
	for _, overlap := range []time.Duration{0, time.Hour - time.Second} {
		_, _, err = e.access.RotateKey(e.ctx, d, self.id, overlap, 10*time.Hour)
		wantErr(t, "self-rotation with overlap "+overlap.String(), err, fault.Invalid, "")
	}
	made, rotated, err := e.access.RotateKey(e.ctx, d, self.id, access.MinSelfRotationOverlap, 10*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !made.Key.Expires.Equal(t0.Add(10 * time.Hour)) {
		t.Errorf("successor expires %v, want the maximum lifetime from now", made.Key.Expires)
	}
	if !rotated.Expires.Equal(t0.Add(time.Hour)) {
		t.Errorf("the rotated key ends %v, want an hour from now", rotated.Expires)
	}
	if _, err := e.access.Check(e.ctx, access.APIKey(self.full), access.OnTenant("lab", master.RoleReader), facts("M")); err != nil {
		t.Errorf("a key rotated by itself stopped at once: %v", err)
	}
	// The successor may rotate itself in turn; the rotated key may not
	// rotate again.
	d2 := e.decide(access.APIKey(made.Secret.Key()), access.Authenticated(), "RotateKey")
	if _, _, err := e.access.RotateKey(e.ctx, d2, made.Key.ID, time.Hour, maxLife); err != nil {
		t.Errorf("successor rotating itself: %v", err)
	}
	_, _, err = e.access.RotateKey(e.ctx, e.admin("RotateKey"), self.id, time.Hour, maxLife)
	wantErr(t, "a key rotated twice", err, fault.Conflict, "")
}

// TestRotateKeyRefusals guards the bounds of a rotation: an overlap out of
// range, an unknown key, a revoked key, and a key that never expires making
// a successor that never expires.
func TestRotateKeyRefusals(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour))
	_, _, err := e.access.RotateKey(e.ctx, e.admin("RotateKey"), k.id, 31*24*time.Hour, maxLife)
	wantErr(t, "overlap past 30 days", err, fault.Invalid, "")
	_, _, err = e.access.RotateKey(e.ctx, e.admin("RotateKey"), k.id, -time.Second, maxLife)
	wantErr(t, "negative overlap", err, fault.Invalid, "")
	_, _, err = e.access.RotateKey(e.ctx, e.admin("RotateKey"), "nope", time.Hour, maxLife)
	wantErr(t, "unknown", err, fault.NotFound, "no key has this id")
	gone := e.key(t0.Add(time.Hour))
	if _, err := e.master.RevokeKey(e.ctx, gone.id, t0, e.setupEntry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	_, _, err = e.access.RotateKey(e.ctx, e.admin("RotateKey"), gone.id, time.Hour, maxLife)
	wantErr(t, "revoked", err, fault.Conflict, "")
	never := e.key(time.Time{})
	made, rotated, err := e.access.RotateKey(e.ctx, e.admin("RotateKey"), never.id, time.Hour, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !made.Key.Expires.IsZero() || !rotated.Expires.Equal(t0.Add(time.Hour)) {
		t.Errorf("successor of a key with no expiry expires %v; rotated ends %v", made.Key.Expires, rotated.Expires)
	}
	_, _, err = e.access.RotateKey(e.ctx, access.Decision{}, k.id, time.Hour, maxLife)
	wantErr(t, "zero decision", err, fault.Denied, access.MessageDenied)
}

// TestSetKeyRulesAccess guards SetKeyRules: admin only, valid rules only, and
// the key's next request judged by the new rules.
func TestSetKeyRulesAccess(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleAdmin))
	reader := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleReader))
	rules := master.KeyRules{Networks: networks(t, "10.0.0.0/8")}
	_, err := e.access.SetKeyRules(e.ctx, e.decide(access.APIKey(reader.full), access.OnInstallation(master.RoleReader), "SetKeyRules"), k.id, rules)
	wantErr(t, "a reader", err, fault.Denied, access.MessageDenied)
	got, err := e.access.SetKeyRules(e.ctx, e.admin("SetKeyRules"), k.id, rules)
	if err != nil {
		t.Fatal(err)
	}
	if got.Rules.Networks.String() != "10.0.0.0/8" || got.Hash != nil {
		t.Errorf("answered %+v", got)
	}
	_, err = e.access.Check(e.ctx, access.APIKey(k.full), access.OnInstallation(master.RoleAdmin), facts("M"))
	wantErr(t, "outside its new networks", err, fault.Unauthenticated, access.MessageUnauthenticated)
	_, err = e.access.SetKeyRules(e.ctx, e.admin("SetKeyRules"), k.id, master.KeyRules{ClientCert: "nothex"})
	wantErr(t, "malformed", err, fault.Invalid, "")
	_, err = e.access.SetKeyRules(e.ctx, e.admin("SetKeyRules"), "nope", rules)
	wantErr(t, "unknown", err, fault.NotFound, "no key has this id")
}

// TestCreateKeyRules guards that a key made with rules has them, and that a
// key without an expiry is the installation admin's alone to make.
func TestCreateKeyRules(t *testing.T) {
	e := newEnv(t)
	rules := master.KeyRules{Networks: networks(t, "192.0.2.0/24")}
	made, err := e.access.CreateKey(e.ctx, e.admin("CreateKey"), "robot", access.ExpireAfter(time.Hour), rules)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := e.master.Key(e.ctx, made.Key.ID)
	if err != nil || stored.Rules.Networks.String() != "192.0.2.0/24" {
		t.Fatalf("stored %+v, %v", stored, err)
	}
	tenantAdmin := e.key(t0.Add(time.Hour), on(master.TenantScope(e.lab.ID), master.RoleAdmin))
	d := e.decide(access.APIKey(tenantAdmin.full), access.OnTenant("lab", master.RoleAdmin), "CreateKey")
	_, err = e.access.CreateKey(e.ctx, d, "forever", access.NeverExpire(), master.KeyRules{})
	wantErr(t, "a tenant's admin asking for a key with no expiry", err, fault.Denied, access.MessageDenied)
}
