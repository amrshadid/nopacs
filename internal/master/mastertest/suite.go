package mastertest

import (
	"context"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Open returns a fresh, empty, migrated store that no other test uses, and
// closes it when tb ends (tb.Cleanup). Closing a store twice must be
// harmless, since a test may close it itself.
type Open func(tb testing.TB) master.Store

// T0 is the time the suite's data starts at: a time with microseconds, so a
// driver that drops them fails.
var T0 = time.Date(2026, 11, 18, 9, 30, 0, 123456000, time.UTC)

// Run runs the whole contract suite against the stores open makes. Each
// test has its own store and runs in parallel with the others.
func Run(t *testing.T, open Open) {
	tests := []struct {
		name string
		fn   func(*env)
	}{
		{"Schema", testSchema},
		{"Closed", testClosed},
		{"Times", testTimes},
		{"Tenants", testTenants},
		{"TenantsListed", testTenantsListed},
		{"TenantNameRace", testTenantNameRace},
		{"TenantHandoff", testTenantHandoff},
		{"Keys", testKeys},
		{"KeysListed", testKeysListed},
		{"KeyHolderUnicode", testKeyHolderUnicode},
		{"MarkKeyUsed", testMarkKeyUsed},
		{"MarkKeyUsedConcurrent", testMarkKeyUsedConcurrent},
		{"Grants", testGrants},
		{"GrantsListed", testGrantsListed},
		{"SetGrantRace", testSetGrantRace},
		{"Settings", testSettings},
		{"SettingsListed", testSettingsListed},
		{"Audit", testAudit},
		{"AuditFilters", testAuditFilters},
		{"AuditPaging10000", testAuditPaging10000},
		{"PageLimits", testPageLimits},
		{"BothOrNeither", testBothOrNeither},
		{"FailuresWriteNoAudit", testFailuresWriteNoAudit},
		{"CursorsBound", testCursorsBound},
		{"ErrorKinds", testErrorKinds},
		{"CloseInFlight", testCloseInFlight},
		{"RolesFor", testRolesFor},
		{"GrantTenantNames", testGrantTenantNames},
		{"MarkKeyUsedEdges", testMarkKeyUsedEdges},
		{"IDsCaseAndOrder", testIDsCaseAndOrder},
		{"ExactText", testExactText},
		{"TimeEdges", testTimeEdges},
		{"PagingWithDeletes", testPagingWithDeletes},
		{"Ties", testTies},
		{"ConcurrentSameRow", testConcurrentSameRow},
		{"SizeBounds", testSizeBounds},
		{"SetGrantRevokeRace", testSetGrantRevokeRace},
		{"ActorChecked", testActorChecked},
		{"ActorOnTenant", testActorOnTenant},
		{"CheckActor", testCheckActor},
		{"ActorRevokeRace", testActorRevokeRace},
		{"CrossedKeyChanges", testCrossedKeyChanges},
		{"KeyRules", testKeyRules},
		{"MarkKeyUsedAddress", testMarkKeyUsedAddress},
		{"RotateKey", testRotateKey},
		{"RotateKeyActor", testRotateKeyActor},
		{"RotatedFrom", testRotatedFrom},
		{"ReinstateKeys", testReinstateKeys},
		{"ReinstateAllKeys", testReinstateAllKeys},
		{"ReinstateRevoked", testReinstateRevoked},
		{"ReinstateRefused", testReinstateRefused},
		{"ReinstateDuplicateAudit", testReinstateDuplicateAudit},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			tt.fn(&env{T: t, s: open(t), ctx: context.Background()})
		})
	}
	t.Run("AuditSeal", func(t *testing.T) {
		RunAuditSeal(t, func(tb testing.TB) SealLog { return open(tb) })
	})
}

// env is one test's store and the helpers that make its data.
type env struct {
	*testing.T
	s   master.Store
	ctx context.Context
	seq int
}

// id returns a new id with the prefix, unique within the test.
func (e *env) id(prefix string) string {
	e.seq++
	return fmt.Sprintf("%s%06d", prefix, e.seq)
}

// audit returns a new audit entry with a unique id, for a change.
func (e *env) audit() master.AuditEntry {
	return master.AuditEntry{
		ID:            e.id("a"),
		Time:          T0,
		LocalUser:     "501",
		RemoteAddress: "local",
		Method:        "Test",
		Outcome:       master.OutcomeDone,
		RequestID:     "r1",
	}
}

// tenant returns a new valid tenant with the name.
func (e *env) tenant(name string) master.Tenant {
	return master.Tenant{
		ID:            e.id("t"),
		Name:          name,
		Driver:        "sqlite",
		SealedAddress: []byte("sealed:" + name),
		State:         master.StateReady,
		Created:       T0,
	}
}

// key returns a new valid key.
func (e *env) key() master.Key {
	id := e.id("k")
	hash := make([]byte, master.KeyHashLength)
	copy(hash, id)
	return master.Key{ID: id, Hash: hash, Holder: "holder of " + id, Created: T0, Expires: T0.Add(90 * 24 * time.Hour)}
}

// mustTenant creates a tenant named name and returns it.
func (e *env) mustTenant(name string) master.Tenant {
	e.Helper()
	t := e.tenant(name)
	if err := e.s.CreateTenant(e.ctx, t, e.audit(), master.LocalOperator()); err != nil {
		e.Fatalf("CreateTenant(%s): %v", name, err)
	}
	return t
}

// mustKey creates a key and returns it.
func (e *env) mustKey() master.Key {
	e.Helper()
	k := e.key()
	if err := e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()); err != nil {
		e.Fatalf("CreateKey: %v", err)
	}
	return k
}

// mustGrant sets a grant.
func (e *env) mustGrant(keyID string, scope master.Scope, role master.Role) {
	e.Helper()
	if _, err := e.s.SetGrant(e.ctx, master.Grant{KeyID: keyID, Scope: scope, Role: role}, e.audit(), master.LocalOperator()); err != nil {
		e.Fatalf("SetGrant: %v", err)
	}
}

// allAudit returns every audit entry, newest first, by paging through.
func (e *env) allAudit(filter master.AuditFilter) []master.AuditEntry {
	e.Helper()
	var out []master.AuditEntry
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		entries, next, err := e.s.ListAudit(e.ctx, filter, page)
		if err != nil {
			e.Fatalf("ListAudit: %v", err)
		}
		out = append(out, entries...)
		if next == "" {
			return out
		}
		page.Cursor = next
	}
}

// auditIDs returns the set of every audit entry's id.
func (e *env) auditIDs() map[string]bool {
	e.Helper()
	ids := map[string]bool{}
	for _, a := range e.allAudit(master.AuditFilter{}) {
		ids[a.ID] = true
	}
	return ids
}

// wantKind fails unless err has the kind want.
func (e *env) wantKind(what string, err error, want fault.Kind) {
	e.Helper()
	if err == nil {
		e.Errorf("%s: no error, want %v", what, want)
		return
	}
	if got := fault.KindOf(err); got != want {
		e.Errorf("%s: kind %v (%v), want %v", what, got, err, want)
	}
}

// ok fails the test now when err is not nil.
func (e *env) ok(what string, err error) {
	e.Helper()
	if err != nil {
		e.Fatalf("%s: %v", what, err)
	}
}

// SameTime reports whether a and b are the same instant, and a is in UTC
// unless it is the zero time.
func SameTime(a, b time.Time) bool {
	if a.IsZero() || b.IsZero() {
		return a.IsZero() && b.IsZero()
	}
	return a.Equal(b) && a.Location() == time.UTC
}

// diffTenant describes how got differs from want, or returns "".
func diffTenant(got, want master.Tenant) string {
	var d []string
	if got.ID != want.ID || got.Name != want.Name || got.Driver != want.Driver {
		d = append(d, fmt.Sprintf("id/name/driver %s/%s/%s, want %s/%s/%s", got.ID, got.Name, got.Driver, want.ID, want.Name, want.Driver))
	}
	if string(got.SealedAddress) != string(want.SealedAddress) {
		d = append(d, "sealed address differs")
	}
	if got.State != want.State || got.StateReason != want.StateReason {
		d = append(d, fmt.Sprintf("state %s %q, want %s %q", got.State, got.StateReason, want.State, want.StateReason))
	}
	if !SameTime(got.Created, want.Created) {
		d = append(d, fmt.Sprintf("created %v, want %v", got.Created, want.Created))
	}
	return strings.Join(d, "; ")
}

// diffKey describes how got differs from want, or returns "".
func diffKey(got, want master.Key) string {
	var d []string
	if got.ID != want.ID || got.Holder != want.Holder {
		d = append(d, fmt.Sprintf("id/holder %s/%q, want %s/%q", got.ID, got.Holder, want.ID, want.Holder))
	}
	if string(got.Hash) != string(want.Hash) {
		d = append(d, "hash differs")
	}
	if got.Rules.Networks.String() != want.Rules.Networks.String() || got.Rules.ClientCert != want.Rules.ClientCert ||
		got.LastAddress != want.LastAddress || got.Successor != want.Successor {
		d = append(d, fmt.Sprintf("rules/address/successor %+v/%q/%q, want %+v/%q/%q",
			got.Rules, got.LastAddress, got.Successor, want.Rules, want.LastAddress, want.Successor))
	}
	for _, f := range []struct {
		name      string
		got, want time.Time
	}{
		{"created", got.Created, want.Created},
		{"expires", got.Expires, want.Expires},
		{"last used", got.LastUsed, want.LastUsed},
		{"revoked", got.Revoked, want.Revoked},
	} {
		if !SameTime(f.got, f.want) {
			d = append(d, fmt.Sprintf("%s %v, want %v", f.name, f.got, f.want))
		}
	}
	return strings.Join(d, "; ")
}

// DiffAudit describes how got differs from want, or returns "". No details
// and empty details are the same.
func DiffAudit(got, want master.AuditEntry) string {
	var d []string
	gt, wt := got.Time, want.Time
	got.Time, want.Time = time.Time{}, time.Time{}
	gd, wd := got.Details, want.Details
	got.Details, want.Details = nil, nil
	if !reflect.DeepEqual(got, want) {
		d = append(d, fmt.Sprintf("%+v, want %+v", got, want))
	}
	if !SameTime(gt, wt) {
		d = append(d, fmt.Sprintf("time %v, want %v", gt, wt))
	}
	if len(gd) != 0 || len(wd) != 0 {
		if !maps.Equal(gd, wd) {
			d = append(d, fmt.Sprintf("details %v, want %v", gd, wd))
		}
	}
	return strings.Join(d, "; ")
}

// testSchema guards Migrate and Version: a migrated store is at a version
// above 0, and migrating again changes nothing.
func testSchema(e *env) {
	v, err := e.s.Version(e.ctx)
	e.ok("Version", err)
	if v < 1 {
		e.Errorf("Version = %d after Migrate, want at least 1", v)
	}
	e.ok("Migrate again", e.s.Migrate(e.ctx))
	again, err := e.s.Version(e.ctx)
	e.ok("Version", err)
	if again != v {
		e.Errorf("Version after a second Migrate = %d, want %d", again, v)
	}
	e.ok("Ping", e.s.Ping(e.ctx))
}

// testClosed guards that a closed store fails every call as Unavailable
// instead of panicking, and that Close may be called again.
func testClosed(e *env) {
	k := e.mustKey()
	e.ok("Close", e.s.Close())
	e.ok("second Close", e.s.Close())
	_, err := e.s.Key(e.ctx, k.ID)
	e.wantKind("Key after Close", err, fault.Unavailable)
	e.wantKind("CreateKey after Close", e.s.CreateKey(e.ctx, e.key(), e.audit(), master.LocalOperator()), fault.Unavailable)
	e.wantKind("Ping after Close", e.s.Ping(e.ctx), fault.Unavailable)
}

// testTimes guards that times come back in UTC with their microseconds,
// whatever zone they went in with, and that what is finer than a
// microsecond is dropped.
func testTimes(e *env) {
	zone := time.FixedZone("UTC+4", 4*60*60)
	in := time.Date(2026, 11, 18, 13, 30, 0, 123456789, zone)
	k := e.key()
	k.Created = in
	k.Expires = time.Date(2099, 12, 31, 23, 59, 59, 999999000, time.UTC)
	e.ok("CreateKey", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	got, err := e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	want := time.Date(2026, 11, 18, 9, 30, 0, 123456000, time.UTC)
	if !got.Created.Equal(want) || got.Created.Location() != time.UTC || got.Created.Nanosecond() != 123456000 {
		t := got.Created
		e.Errorf("created = %v (%v), want %v in UTC", t, t.Location(), want)
	}
	if !SameTime(got.Expires, k.Expires) {
		e.Errorf("expires = %v, want %v", got.Expires, k.Expires)
	}
	if !got.LastUsed.IsZero() || !got.Revoked.IsZero() {
		e.Errorf("a new key has last used %v and revoked %v, want zero", got.LastUsed, got.Revoked)
	}
}

// checkPaging lists with list, limit at a time, calling insert between
// pages, and checks that each of the first want items (those present
// before the listing) is returned exactly once and that the keys come in
// increasing order.
func checkPaging[T any](e *env, what string, list func(master.Page) ([]T, string, error), key func(T) string, insert func(), want, limit int) {
	e.Helper()
	before := map[string]bool{}
	page := master.Page{Limit: master.MaxPageLimit}
	items, _, err := list(page)
	e.ok(what, err)
	if len(items) != want {
		e.Fatalf("%s: %d items before paging, want %d", what, len(items), want)
	}
	for _, it := range items {
		before[key(it)] = true
	}
	seen := map[string]int{}
	var keys []string
	page = master.Page{Limit: limit}
	for range 1000 {
		items, next, err := list(page)
		e.ok(what, err)
		if len(items) > limit {
			e.Fatalf("%s: a page of %d items with limit %d", what, len(items), limit)
		}
		for _, it := range items {
			seen[key(it)]++
			keys = append(keys, key(it))
		}
		if next == "" {
			break
		}
		page.Cursor = next
		insert()
	}
	for k := range before {
		if seen[k] != 1 {
			e.Errorf("%s: item %q returned %d times, want once", what, k, seen[k])
		}
	}
	for i := 1; i < len(keys); i++ {
		if keys[i] <= keys[i-1] {
			e.Errorf("%s: %q after %q, out of order", what, keys[i], keys[i-1])
		}
	}
}

// testPageLimits guards that every list refuses a limit of 0 or above 1000,
// and a cursor it did not make, as Invalid.
func testPageLimits(e *env) {
	k := e.mustKey()
	t := e.mustTenant("lab")
	lists := map[string]func(master.Page) error{
		"ListTenants": func(p master.Page) error { _, _, err := e.s.ListTenants(e.ctx, p); return err },
		"ListKeys":    func(p master.Page) error { _, _, err := e.s.ListKeys(e.ctx, p); return err },
		"GrantsByKey": func(p master.Page) error { _, _, err := e.s.GrantsByKey(e.ctx, k.ID, p); return err },
		"GrantsByScope": func(p master.Page) error {
			_, _, err := e.s.GrantsByScope(e.ctx, master.TenantScope(t.ID), p)
			return err
		},
		"ListSettings": func(p master.Page) error { _, _, err := e.s.ListSettings(e.ctx, p); return err },
		"ListAudit": func(p master.Page) error {
			_, _, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, p)
			return err
		},
	}
	// A cursor some list really returned, to hand to the others.
	for i := range 3 {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("s%d", i), "v", e.audit(), master.LocalOperator()))
	}
	_, settingsCursor, err := e.s.ListSettings(e.ctx, master.Page{Limit: 1})
	e.ok("ListSettings", err)
	if settingsCursor == "" {
		e.Fatal("ListSettings with more to come returned no cursor")
	}
	for name, list := range lists {
		e.ok(name+" limit 1", list(master.Page{Limit: 1}))
		e.ok(name+" limit 1000", list(master.Page{Limit: 1000}))
		e.wantKind(name+" limit 0", list(master.Page{Limit: 0}), fault.Invalid)
		e.wantKind(name+" limit 1001", list(master.Page{Limit: 1001}), fault.Invalid)
		e.wantKind(name+" garbage cursor", list(master.Page{Limit: 5, Cursor: "%%%not-a-cursor"}), fault.Invalid)
		if name != "ListSettings" {
			e.wantKind(name+" another list's cursor", list(master.Page{Limit: 5, Cursor: settingsCursor}), fault.Invalid)
		}
	}
}

// testBothOrNeither guards the rule that a change and its audit entry are
// written both or neither. For every changing operation, the audit entry is
// made to fail by giving it an id already taken; the operation must fail
// with ErrDuplicateAuditID and leave the change unmade. A driver that writes
// the change first and the entry second, outside one transaction, fails
// here.
func testBothOrNeither(e *env) {
	taken := e.audit()
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, taken))
	dup := func() master.AuditEntry {
		a := e.audit()
		a.ID = taken.ID
		return a
	}
	before := len(e.auditIDs())
	check := func(op string, err error, unchanged func() string) {
		e.Helper()
		e.wantDuplicateAudit(op+" with a taken audit id", err)
		if d := unchanged(); d != "" {
			e.Errorf("%s with a failed audit entry changed the master: %s", op, d)
		}
		if n := len(e.auditIDs()); n != before {
			e.Errorf("%s with a failed audit entry: %d audit entries, want %d", op, n, before)
		}
	}

	// CreateTenant.
	nt := e.tenant("never")
	check("CreateTenant", e.s.CreateTenant(e.ctx, nt, dup(), master.LocalOperator()), func() string {
		if _, err := e.s.TenantByName(e.ctx, "never"); fault.KindOf(err) != fault.NotFound {
			return fmt.Sprintf("the tenant exists (%v)", err)
		}
		return ""
	})

	t := e.mustTenant("lab")
	k := e.mustKey()
	e.mustGrant(k.ID, master.TenantScope(t.ID), master.RoleReader)
	e.ok("SetSetting", e.s.SetSetting(e.ctx, "kept", "old", e.audit(), master.LocalOperator()))
	before = len(e.auditIDs())

	// SetTenantState.
	check("SetTenantState", e.s.SetTenantState(e.ctx, t.ID, master.StateFailed, "x", dup(), master.LocalOperator()), func() string {
		got, err := e.s.Tenant(e.ctx, t.ID)
		if err != nil || got.State != master.StateReady {
			return fmt.Sprintf("state %s (%v)", got.State, err)
		}
		return ""
	})
	// RemoveTenant, which also removes grants.
	check("RemoveTenant", e.s.RemoveTenant(e.ctx, t.ID, dup(), master.LocalOperator()), func() string {
		if _, err := e.s.Tenant(e.ctx, t.ID); err != nil {
			return fmt.Sprintf("the tenant is gone (%v)", err)
		}
		g, _, err := e.s.GrantsByScope(e.ctx, master.TenantScope(t.ID), master.Page{Limit: 10})
		if err != nil || len(g) != 1 {
			return fmt.Sprintf("its grants are %v (%v)", g, err)
		}
		return ""
	})
	// CreateKey.
	nk := e.key()
	check("CreateKey", e.s.CreateKey(e.ctx, nk, dup(), master.LocalOperator()), func() string {
		if _, err := e.s.Key(e.ctx, nk.ID); fault.KindOf(err) != fault.NotFound {
			return fmt.Sprintf("the key exists (%v)", err)
		}
		return ""
	})
	// RevokeKey.
	_, err := e.s.RevokeKey(e.ctx, k.ID, T0.Add(time.Hour), dup(), master.LocalOperator())
	check("RevokeKey", err, func() string {
		got, err := e.s.Key(e.ctx, k.ID)
		if err != nil || !got.Revoked.IsZero() {
			return fmt.Sprintf("revoked %v (%v)", got.Revoked, err)
		}
		return ""
	})
	// SetGrant, new and replacing.
	grantIs := func(scope master.Scope, want master.Role) func() string {
		return func() string {
			g, _, err := e.s.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 10})
			if err != nil {
				return err.Error()
			}
			for _, x := range g {
				if x.Scope == scope {
					if x.Role != want {
						return fmt.Sprintf("role %s, want %s", x.Role, want)
					}
					return ""
				}
			}
			if want != "" {
				return "the grant is gone"
			}
			return ""
		}
	}
	_, err = e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.Installation, Role: master.RoleAdmin}, dup(), master.LocalOperator())
	check("SetGrant (new)", err, grantIs(master.Installation, ""))
	_, err = e.s.SetGrant(e.ctx, master.Grant{KeyID: k.ID, Scope: master.TenantScope(t.ID), Role: master.RoleAdmin}, dup(), master.LocalOperator())
	check("SetGrant (replace)", err, grantIs(master.TenantScope(t.ID), master.RoleReader))
	// RemoveGrant.
	_, err = e.s.RemoveGrant(e.ctx, k.ID, master.TenantScope(t.ID), dup(), master.LocalOperator())
	check("RemoveGrant", err, grantIs(master.TenantScope(t.ID), master.RoleReader))
	// SetSetting, new and replacing.
	check("SetSetting (new)", e.s.SetSetting(e.ctx, "fresh", "v", dup(), master.LocalOperator()), func() string {
		if _, err := e.s.Setting(e.ctx, "fresh"); fault.KindOf(err) != fault.NotFound {
			return fmt.Sprintf("the setting exists (%v)", err)
		}
		return ""
	})
	settingIs := func() string {
		v, err := e.s.Setting(e.ctx, "kept")
		if err != nil || v != "old" {
			return fmt.Sprintf("value %q (%v)", v, err)
		}
		return ""
	}
	check("SetSetting (replace)", e.s.SetSetting(e.ctx, "kept", "new", dup(), master.LocalOperator()), settingIs)
	// UnsetSetting.
	_, err = e.s.UnsetSetting(e.ctx, "kept", dup(), master.LocalOperator())
	check("UnsetSetting", err, settingIs)
	// AppendAudit: a batch whose second entry fails writes neither.
	first := e.audit()
	check("AppendAudit (batch)", e.s.AppendAudit(e.ctx, first, dup()), func() string {
		if e.auditIDs()[first.ID] {
			return "the batch's first entry was written"
		}
		return ""
	})
}
