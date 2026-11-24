package mastertest

import (
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// sampleCert is a fingerprint of a client certificate in the form a key
// holds it.
var sampleCert = strings.Repeat("ab", master.ClientCertLength/2)

// mustNetworks parses a list of networks the test wrote.
func (e *env) mustNetworks(v string) settings.Networks {
	e.Helper()
	n, err := settings.ParseNetworks(v)
	if err != nil {
		e.Fatalf("ParseNetworks(%q): %v", v, err)
	}
	return n
}

// testKeyRules guards a key's rules: made with the key and read back
// exactly, replaced by SetKeyRules with its audit entry, cleared, refused
// when malformed, and refused on an unknown or a revoked key.
func testKeyRules(e *env) {
	k := e.key()
	k.Rules = master.KeyRules{Networks: e.mustNetworks("10.0.0.0/8,2001:db8::/32,192.0.2.7"), ClientCert: sampleCert}
	e.ok("CreateKey with rules", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	got, err := e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if got.Rules.Networks.String() != "10.0.0.0/8,2001:db8::/32,192.0.2.7" || got.Rules.ClientCert != sampleCert {
		e.Errorf("rules read back as %q, %q", got.Rules.Networks.String(), got.Rules.ClientCert)
	}
	plain := e.mustKey()
	got, err = e.s.Key(e.ctx, plain.ID)
	e.ok("Key", err)
	if !got.Rules.IsZero() || got.LastAddress != "" || got.Successor != "" {
		e.Errorf("a key made without rules reads back with %+v, %q, %q", got.Rules, got.LastAddress, got.Successor)
	}

	set := e.audit()
	next := master.KeyRules{Networks: e.mustNetworks("198.51.100.0/24")}
	e.ok("SetKeyRules", e.s.SetKeyRules(e.ctx, plain.ID, next, set, master.LocalOperator()))
	got, err = e.s.Key(e.ctx, plain.ID)
	e.ok("Key", err)
	if got.Rules.Networks.String() != "198.51.100.0/24" || got.Rules.ClientCert != "" {
		e.Errorf("after SetKeyRules the rules are %q, %q", got.Rules.Networks.String(), got.Rules.ClientCert)
	}
	if !e.auditIDs()[set.ID] {
		e.Error("SetKeyRules wrote no audit entry")
	}
	e.ok("SetKeyRules, cleared", e.s.SetKeyRules(e.ctx, k.ID, master.KeyRules{}, e.audit(), master.LocalOperator()))
	got, err = e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if !got.Rules.IsZero() {
		e.Errorf("cleared rules read back as %+v", got.Rules)
	}

	e.wantKind("SetKeyRules, unknown key", e.s.SetKeyRules(e.ctx, "nope", next, e.audit(), master.LocalOperator()), fault.NotFound)
	bad := []master.KeyRules{
		{ClientCert: "AB" + sampleCert[2:]},
		{ClientCert: sampleCert[:62]},
		{Networks: manyNetworks(master.MaxKeyNetworks + 1)},
		{Networks: settings.Networks{netip.MustParsePrefix("10.1.2.3/8")}},
	}
	for i, r := range bad {
		before := e.auditIDs()
		e.wantKind("SetKeyRules, malformed "+string(rune('a'+i)), e.s.SetKeyRules(e.ctx, k.ID, r, e.audit(), master.LocalOperator()), fault.Invalid)
		if len(e.auditIDs()) != len(before) {
			e.Error("a refused SetKeyRules wrote an audit entry")
		}
		withRules := e.key()
		withRules.Rules = r
		e.wantKind("CreateKey, malformed rules", e.s.CreateKey(e.ctx, withRules, e.audit(), master.LocalOperator()), fault.Invalid)
	}
	_, err = e.s.RevokeKey(e.ctx, plain.ID, T0.Add(time.Hour), e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	e.wantKind("SetKeyRules, revoked key", e.s.SetKeyRules(e.ctx, plain.ID, next, e.audit(), master.LocalOperator()), fault.Conflict)
}

// testMarkKeyUsedAddress guards the last-used address: written with the
// time, under the same throttle, so a use within the minute changes neither,
// and bounded like an audit entry's address.
func testMarkKeyUsedAddress(e *env) {
	k := e.mustKey()
	wrote, err := e.s.MarkKeyUsed(e.ctx, k.ID, T0, "192.0.2.10")
	e.ok("MarkKeyUsed", err)
	if !wrote {
		e.Fatal("the first use did not write")
	}
	wrote, err = e.s.MarkKeyUsed(e.ctx, k.ID, T0.Add(time.Second), "198.51.100.20")
	e.ok("MarkKeyUsed", err)
	if wrote {
		e.Error("a use within the minute wrote")
	}
	got, err := e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if got.LastAddress != "192.0.2.10" || !SameTime(got.LastUsed, T0) {
		e.Errorf("last use %v from %q, want %v from 192.0.2.10", got.LastUsed, got.LastAddress, T0)
	}
	later := T0.Add(2 * time.Minute)
	_, err = e.s.MarkKeyUsed(e.ctx, k.ID, later, "2001:db8::7")
	e.ok("MarkKeyUsed", err)
	got, err = e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if got.LastAddress != "2001:db8::7" || !SameTime(got.LastUsed, later) {
		e.Errorf("last use %v from %q, want %v from 2001:db8::7", got.LastUsed, got.LastAddress, later)
	}
	// A use on the local socket has no address: it moves the time and
	// keeps the last address known, so the operator's own use never hides
	// where the key was last used from on the network.
	local := later.Add(2 * time.Minute)
	wrote, err = e.s.MarkKeyUsed(e.ctx, k.ID, local, "")
	e.ok("MarkKeyUsed", err)
	got, err = e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if !wrote || got.LastAddress != "2001:db8::7" || !SameTime(got.LastUsed, local) {
		e.Errorf("a local use: wrote %v, last use %v from %q; want %v from 2001:db8::7", wrote, got.LastUsed, got.LastAddress, local)
	}
	_, err = e.s.MarkKeyUsed(e.ctx, k.ID, later.Add(time.Hour), strings.Repeat("a", master.MaxAddressLength+1))
	e.wantKind("MarkKeyUsed, address too long", err, fault.Invalid)
	_, err = e.s.MarkKeyUsed(e.ctx, k.ID, later.Add(time.Hour), "caf\xe9")
	e.wantKind("MarkKeyUsed, address not UTF-8", err, fault.Invalid)
}

// successorOf returns a successor key for a rotation at at.
func (e *env) successorOf(at time.Time, lifetime time.Duration) master.Key {
	s := e.key()
	s.Holder, s.Created, s.Expires = "", at, time.Time{}
	if lifetime > 0 {
		s.Expires = at.Add(lifetime)
	}
	return s
}

// testRotateKey guards a rotation: the successor carries the rotated key's
// holder, rules and every grant; the rotated key ends at the overlap's end,
// never later than it would have; its successor is named; the audit entry
// records both; and each Conflict.
func testRotateKey(e *env) {
	ta := e.mustTenant("rot-a")
	tb := e.mustTenant("rot-b")
	old := e.key()
	old.Rules = master.KeyRules{Networks: e.mustNetworks("10.0.0.0/8"), ClientCert: sampleCert}
	e.ok("CreateKey", e.s.CreateKey(e.ctx, old, e.audit(), master.LocalOperator()))
	e.mustGrant(old.ID, master.Installation, master.RoleReader)
	e.mustGrant(old.ID, master.TenantScope(ta.ID), master.RoleWriter)
	e.mustGrant(old.ID, master.TenantScope(tb.ID), master.RoleAdmin)

	at := T0.Add(time.Hour)
	succ := e.successorOf(at, 30*24*time.Hour)
	ends := at.Add(24 * time.Hour)
	entry := e.audit()
	made, err := e.s.RotateKey(e.ctx, old.ID, succ, ends, entry, master.LocalOperator())
	e.ok("RotateKey", err)
	if made.ID != succ.ID || made.Holder != old.Holder || made.Rules.Networks.String() != "10.0.0.0/8" || made.Rules.ClientCert != sampleCert || len(made.Hash) != 0 {
		e.Errorf("RotateKey returned %+v", made)
	}
	stored, err := e.s.Key(e.ctx, succ.ID)
	e.ok("Key successor", err)
	want := succ
	want.Holder, want.Rules = old.Holder, old.Rules
	if d := diffKey(stored, want); d != "" || stored.Rules.Networks.String() != "10.0.0.0/8" || stored.Rules.ClientCert != sampleCert {
		e.Errorf("successor as stored: %s %+v", d, stored.Rules)
	}
	rotated, err := e.s.Key(e.ctx, old.ID)
	e.ok("Key rotated", err)
	if !SameTime(rotated.Expires, ends) || rotated.Successor != succ.ID {
		e.Errorf("rotated key expires %v with successor %q, want %v and %s", rotated.Expires, rotated.Successor, ends, succ.ID)
	}
	grants, _, err := e.s.GrantsByKey(e.ctx, succ.ID, master.Page{Limit: 10})
	e.ok("GrantsByKey", err)
	roles := map[string]master.Role{}
	for _, g := range grants {
		roles[g.Scope.String()] = g.Role
	}
	if len(roles) != 3 || roles["installation"] != master.RoleReader || roles["tenant/"+ta.ID] != master.RoleWriter || roles["tenant/"+tb.ID] != master.RoleAdmin {
		e.Errorf("successor's grants %v, want the rotated key's three", roles)
	}
	var found bool
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if a.ID == entry.ID {
			found = true
			if a.Details[master.DetailSuccessor] != succ.ID || a.Details[master.DetailOldExpires] == "" {
				e.Errorf("rotation's entry details %v", a.Details)
			}
		}
	}
	if !found {
		e.Error("RotateKey wrote no audit entry")
	}

	// A key rotated once is not rotated again; its successor may be.
	_, err = e.s.RotateKey(e.ctx, old.ID, e.successorOf(at, time.Hour), ends, e.audit(), master.LocalOperator())
	e.wantKind("RotateKey, already rotated", err, fault.Conflict)
	_, err = e.s.RotateKey(e.ctx, succ.ID, e.successorOf(at.Add(time.Minute), 0), at.Add(time.Minute), e.audit(), master.LocalOperator())
	e.ok("RotateKey of the successor, at once", err)

	// A rotation never makes a key live longer than it would have.
	short := e.key()
	short.Expires = at.Add(time.Hour)
	e.ok("CreateKey", e.s.CreateKey(e.ctx, short, e.audit(), master.LocalOperator()))
	_, err = e.s.RotateKey(e.ctx, short.ID, e.successorOf(at, time.Hour), at.Add(48*time.Hour), e.audit(), master.LocalOperator())
	e.ok("RotateKey, overlap past the expiry", err)
	got, err := e.s.Key(e.ctx, short.ID)
	e.ok("Key", err)
	if !SameTime(got.Expires, short.Expires) {
		e.Errorf("a rotation moved an expiry from %v to %v", short.Expires, got.Expires)
	}

	// A key that never expired ends at the overlap's end.
	never := e.key()
	never.Expires = time.Time{}
	e.ok("CreateKey", e.s.CreateKey(e.ctx, never, e.audit(), master.LocalOperator()))
	_, err = e.s.RotateKey(e.ctx, never.ID, e.successorOf(at, 0), ends, e.audit(), master.LocalOperator())
	e.ok("RotateKey, no expiry", err)
	got, err = e.s.Key(e.ctx, never.ID)
	e.ok("Key", err)
	if !SameTime(got.Expires, ends) {
		e.Errorf("a key with no expiry rotated ends %v, want %v", got.Expires, ends)
	}

	// Each refusal writes nothing.
	revoked := e.mustKey()
	_, err = e.s.RevokeKey(e.ctx, revoked.ID, T0.Add(time.Minute), e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	expired := e.key()
	expired.Expires = T0.Add(time.Minute)
	e.ok("CreateKey", e.s.CreateKey(e.ctx, expired, e.audit(), master.LocalOperator()))
	fresh := e.mustKey()
	for _, tc := range []struct {
		name string
		id   string
		succ master.Key
		ends time.Time
		kind fault.Kind
	}{
		{"unknown", "nope", e.successorOf(at, time.Hour), ends, fault.NotFound},
		{"revoked", revoked.ID, e.successorOf(at, time.Hour), ends, fault.Conflict},
		{"expired", expired.ID, e.successorOf(at, time.Hour), ends, fault.Conflict},
		{"successor id taken", fresh.ID, func() master.Key { s := e.successorOf(at, time.Hour); s.ID = old.ID; return s }(), ends, fault.Conflict},
		{"ends before the successor", fresh.ID, e.successorOf(at, time.Hour), at.Add(-time.Second), fault.Invalid},
		{"successor with a short hash", fresh.ID, func() master.Key { s := e.successorOf(at, time.Hour); s.Hash = s.Hash[:3]; return s }(), ends, fault.Invalid},
	} {
		before := e.auditIDs()
		_, err := e.s.RotateKey(e.ctx, tc.id, tc.succ, tc.ends, e.audit(), master.LocalOperator())
		e.wantKind("RotateKey, "+tc.name, err, tc.kind)
		if len(e.auditIDs()) != len(before) {
			e.Errorf("RotateKey, %s: wrote an audit entry", tc.name)
		}
		if _, err := e.s.Key(e.ctx, tc.succ.ID); tc.succ.ID != old.ID && fault.KindOf(err) != fault.NotFound {
			e.Errorf("RotateKey, %s: the successor was written (%v)", tc.name, err)
		}
	}
	got, err = e.s.Key(e.ctx, fresh.ID)
	e.ok("Key", err)
	if got.Successor != "" || !SameTime(got.Expires, fresh.Expires) {
		e.Errorf("a refused rotation changed the key: %+v", got)
	}
}

// testRotatedFrom guards the backward read of a rotation: a key made by a
// rotation names the key it replaced, revoked or not; a key made new, an
// unknown id and the start of a line are NotFound; and FindRotatedFrom
// follows a line back, nearest first, no further than MaxRotations.
func testRotatedFrom(e *env) {
	first := e.mustKey()
	at := T0.Add(time.Hour)
	line := []string{first.ID}
	for i := range master.MaxRotations + 1 {
		next := e.successorOf(at.Add(time.Duration(i)*time.Minute), 0)
		_, err := e.s.RotateKey(e.ctx, line[len(line)-1], next, at.Add(time.Duration(i+1)*time.Minute), e.audit(), master.LocalOperator())
		e.ok("RotateKey", err)
		line = append(line, next.ID)
	}
	for i := 1; i < len(line); i++ {
		k, err := e.s.RotatedFrom(e.ctx, line[i])
		e.ok("RotatedFrom", err)
		if k.ID != line[i-1] || k.Successor != line[i] {
			e.Errorf("RotatedFrom(%s) = %s with successor %s, want %s", line[i], k.ID, k.Successor, line[i-1])
		}
	}
	_, err := e.s.RevokeKey(e.ctx, line[1], at.Add(time.Hour), e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	if k, err := e.s.RotatedFrom(e.ctx, line[2]); err != nil || k.ID != line[1] || k.Revoked.IsZero() {
		e.Errorf("RotatedFrom of a revoked key's successor: %+v %v", k, err)
	}
	for _, id := range []string{first.ID, e.mustKey().ID, "nope"} {
		_, err := e.s.RotatedFrom(e.ctx, id)
		e.wantKind("RotatedFrom("+id+")", err, fault.NotFound)
	}
	_, err = e.s.RotatedFrom(e.ctx, "")
	e.wantKind("RotatedFrom of no id", err, fault.Invalid)

	last := line[len(line)-1]
	var asked []string
	got, err := master.FindRotatedFrom(e.ctx, e.s, last, func(id string) bool { asked = append(asked, id); return id == line[len(line)-3] })
	if err != nil || got != line[len(line)-3] || len(asked) != 2 || asked[0] != line[len(line)-2] {
		e.Errorf("FindRotatedFrom two back: %q %v, asked %v", got, err, asked)
	}
	// The line is MaxRotations+1 long: its first key is one rotation past
	// the bound.
	if got, err := master.FindRotatedFrom(e.ctx, e.s, last, func(id string) bool { return id == line[1] }); err != nil || got != line[1] {
		e.Errorf("FindRotatedFrom at the bound: %q %v", got, err)
	}
	if got, err := master.FindRotatedFrom(e.ctx, e.s, last, func(id string) bool { return id == first.ID }); err != nil || got != "" {
		e.Errorf("FindRotatedFrom past the bound: %q %v", got, err)
	}
	if got, err := master.FindRotatedFrom(e.ctx, e.s, first.ID, func(string) bool { return true }); err != nil || got != "" {
		e.Errorf("FindRotatedFrom of a key never rotated to: %q %v", got, err)
	}
}

// testRotateKeyActor guards that a rotation checks its actor: a key may
// rotate itself while it is valid, a revoked key cannot, and two rotations
// of one key racing make one successor.
func testRotateKeyActor(e *env) {
	self := e.mustKey()
	clock := func() time.Time { return T0.Add(time.Hour) }
	by := master.ActingKey(self.ID, master.Installation, "", clock)
	at := T0.Add(time.Hour)
	_, err := e.s.RotateKey(e.ctx, self.ID, e.successorOf(at, time.Hour), at.Add(time.Hour), e.audit(), by)
	e.ok("RotateKey by itself", err)

	gone := e.mustKey()
	_, err = e.s.RevokeKey(e.ctx, gone.ID, T0.Add(time.Minute), e.audit(), master.LocalOperator())
	e.ok("RevokeKey", err)
	victim := e.mustKey()
	_, err = e.s.RotateKey(e.ctx, victim.ID, e.successorOf(at, time.Hour), at.Add(time.Hour), e.audit(),
		master.ActingKey(gone.ID, master.Installation, "", clock))
	e.wantKind("RotateKey by a revoked key", err, fault.Unauthenticated)
	if _, ok := master.RefusalOf(err); !ok {
		e.Errorf("RotateKey by a revoked key: %v carries no refusal", err)
	}

	racer := e.mustKey()
	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		succ := e.successorOf(at, time.Hour)
		entry := e.audit()
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = e.s.RotateKey(e.ctx, racer.ID, succ, at.Add(time.Hour), entry, master.LocalOperator())
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case fault.KindOf(err) != fault.Conflict:
			e.Errorf("a racing rotation failed with %v, want Conflict", err)
		}
	}
	if won != 1 {
		e.Errorf("%d racing rotations of one key succeeded, want 1", won)
	}
}

// manyNetworks returns n distinct single addresses.
func manyNetworks(n int) settings.Networks {
	out := make(settings.Networks, n)
	for i := range out {
		out[i] = netip.MustParsePrefix("10.0." + strconv.Itoa(i/256) + "." + strconv.Itoa(i%256) + "/32")
	}
	return out
}
