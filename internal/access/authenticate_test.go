package access_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestAuthenticate guards the judgment of a credential on its own: a
// credential that is no good is refused as Check refuses it, with one
// refused entry naming its real reason and the key id when it has one; a
// good key is read once, and Check given the credential Authenticate
// returned reads its roles without reading the key again, and decides as
// it would have; the local operator is accepted without a read.
func TestAuthenticate(t *testing.T) {
	e := newEnv(t)
	good := e.key(time.Time{}, on(master.Installation, master.RoleReader))
	expired := e.key(t0.Add(-time.Second))
	for _, c := range []struct {
		name   string
		cred   access.Credential
		reason string
		keyID  string
	}{
		{"no key", access.APIKey(""), "no key", ""},
		{"malformed", access.APIKey("nopacs_short"), "malformed key", ""},
		{"expired", access.APIKey(expired.full), "key expired", expired.id},
	} {
		e.reset()
		_, err := e.access.Authenticate(e.ctx, c.cred, facts("/m"))
		wantErr(t, c.name, err, fault.Unauthenticated, access.MessageUnauthenticated)
		_, entries := e.recorded()
		if len(entries) != 1 || entries[0].Outcome != master.OutcomeRefused || entries[0].Reason != c.reason ||
			entries[0].KeyID != c.keyID || entries[0].Method != "/m" || entries[0].RequestID != "req-1" {
			t.Errorf("%s: entries %+v", c.name, entries)
		}
	}

	e.reset()
	cred, err := e.access.Authenticate(e.ctx, access.APIKey(good.full), facts("/m"))
	if err != nil {
		t.Fatal(err)
	}
	if calls, entries := e.recorded(); !slices.Equal(calls, []string{"Key"}) || len(entries) != 0 {
		t.Fatalf("authenticating a good key: calls %v, entries %v", calls, entries)
	}
	if _, err := e.access.Check(e.ctx, cred, access.OnInstallation(master.RoleAdmin), facts("/m")); fault.KindOf(err) != fault.Denied {
		t.Fatalf("a reader checked for admin: %v", err)
	}
	d, err := e.access.Check(e.ctx, cred, access.OnInstallation(master.RoleReader), facts("/m"))
	if err != nil || d.Principal().KeyID() != good.id {
		t.Fatalf("a reader checked for reader: %v %v", d.Principal().KeyID(), err)
	}
	calls, entries := e.recorded()
	if !slices.Equal(calls, []string{"Key", "RolesFor", "RolesFor"}) || len(entries) != 1 || entries[0].KeyID != good.id {
		t.Fatalf("checks after Authenticate: calls %v, entries %+v", calls, entries)
	}

	e.reset()
	lc, err := e.access.Authenticate(e.ctx, local, facts("/m"))
	if err != nil {
		t.Fatal(err)
	}
	if d := e.decide(lc, access.OnInstallation(master.RoleAdmin), "/m"); !d.Principal().IsLocalOperator() {
		t.Fatal("the local operator was not accepted as itself")
	}
	if calls, _ := e.recorded(); len(calls) != 0 {
		t.Fatalf("the local operator caused reads: %v", calls)
	}
}

// TestRecordUnchecked guards the entry of a request no check took up: for
// a credential Authenticate accepted, one failed entry with the reason,
// the facts and the caller; for one it did not accept, nothing, so a refused
// caller's request never writes a second entry.
func TestRecordUnchecked(t *testing.T) {
	e := newEnv(t)
	good := e.key(time.Time{})
	cred, err := e.access.Authenticate(e.ctx, access.APIKey(good.full), facts("/m"))
	if err != nil {
		t.Fatal(err)
	}
	e.reset()
	f := facts("/m")
	f.Details = map[string]string{"served": "false"}
	e.access.RecordUnchecked(cred, f, "no handler serves this method")
	e.access.RecordUnchecked(access.APIKey(good.full), f, "not accepted")
	_, entries := e.recorded()
	if len(entries) != 1 {
		t.Fatalf("%d entries, want 1: %+v", len(entries), entries)
	}
	en := entries[0]
	if en.Outcome != master.OutcomeFailed || en.Reason != "no handler serves this method" || en.KeyID != good.id ||
		en.Method != "/m" || en.Details["served"] != "false" || en.RemoteAddress != "192.0.2.7:51000" {
		t.Fatalf("the entry is %+v", en)
	}
	if s := e.access.AuditStats(); s.Failed != 1 {
		t.Fatalf("the credential not accepted was not counted as failed: %+v", s)
	}
}

// TestReread guards the check of an accepted key at the moment of the
// decision: Check given an accepted credential reads no key, and still
// refuses one whose expiry has passed since; given the same credential
// marked by Reread it reads the key once more and refuses one revoked
// since, with the real reason.
func TestReread(t *testing.T) {
	e := newEnv(t)
	k := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleAdmin))
	cred, err := e.access.Authenticate(e.ctx, access.APIKey(k.full), facts("/m"))
	if err != nil {
		t.Fatal(err)
	}
	e.reset()
	e.decide(cred, access.OnInstallation(master.RoleAdmin), "/m")
	e.decide(cred.Reread(), access.OnInstallation(master.RoleAdmin), "/m")
	if calls, _ := e.recorded(); !slices.Equal(calls, []string{"RolesFor", "Key", "RolesFor"}) {
		t.Fatalf("calls %v: the key was not read again only when asked", calls)
	}
	if _, err := e.master.RevokeKey(e.ctx, k.id, t0, e.setupEntry(), master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	e.reset()
	_, err = e.access.Check(e.ctx, cred.Reread(), access.OnInstallation(master.RoleAdmin), facts("/m"))
	wantErr(t, "revoked since", err, fault.Unauthenticated, access.MessageUnauthenticated)
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Reason != "key revoked" || entries[0].KeyID != k.id {
		t.Fatalf("entries %+v", entries)
	}
	e.clock.Set(t0.Add(2 * time.Hour))
	e.reset()
	_, err = e.access.Check(e.ctx, cred, access.OnInstallation(master.RoleAdmin), facts("/m"))
	wantErr(t, "expired since", err, fault.Unauthenticated, access.MessageUnauthenticated)
	if calls, entries := e.recorded(); len(calls) != 0 || len(entries) != 1 || entries[0].Reason != "key expired" {
		t.Fatalf("calls %v, entries %+v", calls, entries)
	}
}

// TestEntered guards the mark the server reads to know an allowed request
// has its entry: false after the check, true once an entry was made from
// the Decision or any copy of it, false for the zero Decision.
func TestEntered(t *testing.T) {
	e := newEnv(t)
	d := e.admin("/m")
	if d.Entered() || (access.Decision{}).Entered() {
		t.Fatal("a Decision with no entry reports one")
	}
	c := d
	e.access.Record(c, master.OutcomeDone, "", access.Facts{})
	if !d.Entered() {
		t.Fatal("an entry made from a copy was not seen")
	}
}

// TestCanceledCheck guards that a store read failing because the request's
// context ended is recorded as the request ending, not as the master
// failing, and that the caller gets the context's error.
func TestCanceledCheck(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{})
	ctx, cancel := context.WithCancel(e.ctx)
	cancel()
	e.reset()
	_, err := e.access.Authenticate(ctx, access.APIKey(k.full), facts("/m"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled check: %v", err)
	}
	if _, entries := e.recorded(); len(entries) != 1 || entries[0].Outcome != master.OutcomeFailed ||
		strings.Contains(entries[0].Reason, "master database") {
		t.Fatalf("entries %+v", entries)
	}
}
