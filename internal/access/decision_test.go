package access_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestStaleDecisionRefused guards a Decision kept past its request: the
// management functions read the key again, so a Decision made while the key
// was an admin opens nothing once the key is revoked (Unauthenticated) or
// no longer admin (Denied), even for a key that never expires; each refusal
// is recorded and nothing changes.
func TestStaleDecisionRefused(t *testing.T) {
	e := newEnv(t)
	admin := e.key(time.Time{}, on(master.Installation, master.RoleAdmin))
	target := e.key(time.Time{})
	stale := e.decide(access.APIKey(admin.full), access.OnInstallation(master.RoleAdmin), "RevokeKey")
	if _, err := e.access.RevokeKey(e.ctx, e.admin("RevokeKey"), admin.id); err != nil {
		t.Fatal(err)
	}
	for name, call := range managementCalls(e, target.id) {
		e.reset()
		err := call(stale)
		wantErr(t, name+" with the Decision of a revoked key", err, fault.Unauthenticated, access.MessageUnauthenticated)
		if _, entries := e.recorded(); len(entries) != 1 || entries[0].Outcome != master.OutcomeRefused || entries[0].Reason != "key revoked since the check" {
			t.Fatalf("%s: entries %+v", name, entries)
		}
	}
	if k, err := e.master.Key(e.ctx, target.id); err != nil || !k.Revoked.IsZero() {
		t.Fatalf("a stale Decision revoked a key: %+v %v", k, err)
	}

	demoted := e.key(time.Time{}, on(master.Installation, master.RoleAdmin))
	d := e.decide(access.APIKey(demoted.full), access.OnInstallation(master.RoleAdmin), "CreateKey")
	if _, err := e.access.RemoveGrant(e.ctx, e.admin("RemoveGrant"), demoted.id, access.InstallationScope); err != nil {
		t.Fatal(err)
	}
	_, err := e.access.CreateKey(e.ctx, d, "h", access.Expiry{}, master.KeyRules{})
	wantErr(t, "CreateKey with the Decision of a key no longer admin", err, fault.Denied, access.MessageDenied)

	expiring := e.key(t0.Add(time.Hour), on(master.Installation, master.RoleAdmin))
	d = e.decide(access.APIKey(expiring.full), access.OnInstallation(master.RoleAdmin), "ListKeys")
	e.clock.Set(t0.Add(2 * time.Hour))
	_, _, err = e.access.ListKeys(e.ctx, d, master.Page{})
	wantErr(t, "ListKeys with the Decision of a key expired since", err, fault.Unauthenticated, access.MessageUnauthenticated)
}

// TestReadThenChangeWithOneDecision guards a Decision used for more than one
// entry of the same request: each entry has its own fresh id and the same
// request id, so a read and then a change both reach the log.
func TestReadThenChangeWithOneDecision(t *testing.T) {
	e := newEnv(t)
	target := e.key(time.Time{})
	d := e.admin("ReadThenRevoke")
	e.reset()
	if _, err := e.access.GetKey(e.ctx, d, target.id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.access.RevokeKey(e.ctx, d, target.id); err != nil {
		t.Fatal(err)
	}
	_, entries := e.recorded()
	if len(entries) != 2 || entries[0].ID == entries[1].ID || entries[0].RequestID != entries[1].RequestID {
		t.Fatalf("entries %+v, want two with their own ids and one request id", entries)
	}
	one, two := d.Entry(master.OutcomeDone, "", access.Facts{}), d.Entry(master.OutcomeDone, "", access.Facts{})
	if one.ID == two.ID {
		t.Fatalf("two entries of one Decision share the id %s", one.ID)
	}
}

// TestEntryDetailsAreItsOwn guards that each entry has its own details map:
// a handler that adds a detail to one changes no other entry.
func TestEntryDetailsAreItsOwn(t *testing.T) {
	e := newEnv(t)
	f := facts("m")
	f.Details = map[string]string{"limit": "100"}
	d, err := e.access.Check(e.ctx, local, access.Authenticated(), f)
	if err != nil {
		t.Fatal(err)
	}
	one := d.Entry(master.OutcomeDone, "", access.Facts{})
	one.Details["added"] = "x"
	two := d.Entry(master.OutcomeDone, "", access.Facts{})
	if _, ok := two.Details["added"]; ok || two.Details["limit"] != "100" {
		t.Fatalf("the second entry's details are %v", two.Details)
	}
}

// TestHandlerFacts guards the facts a handler adds after the check: a
// change's entry, a failed request's and a read's each carry the handler's
// object, tenant name and details, cleaned and bounded; and the identity of
// the request (key, local user, time, method, address, request id) stays
// the check's.
func TestHandlerFacts(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{}, on(master.Installation, master.RoleAdmin))
	d := e.decide(access.APIKey(k.full), access.OnInstallation(master.RoleAdmin), "/nopacs.v1.TenantService/CreateTenant")
	hf := access.Facts{Object: "obj\nx", TenantName: "lab", Details: map[string]string{"driver": "sqlite"}}

	change := d.Entry(master.OutcomeDone, "", hf)
	if err := e.master.SetSetting(e.ctx, "audit.test", "v", change, master.LocalOperator()); err != nil {
		t.Fatal(err)
	}
	e.access.Record(d, master.OutcomeFailed, "the tenant's database does not answer", hf)
	e.access.Record(d, master.OutcomeDone, "", hf)
	e.flush()
	stored, _, err := e.master.ListAudit(e.ctx, master.AuditFilter{KeyID: k.id}, master.Page{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 3 {
		t.Fatalf("%d entries stored, want 3", len(stored))
	}
	for _, s := range stored {
		if s.Object != `obj\nx` || s.TenantName != "lab" || s.Details["driver"] != "sqlite" {
			t.Errorf("entry %s lacks the handler's facts: %+v", s.ID, s)
		}
		if s.KeyID != k.id || s.LocalUser != "" || !s.Time.Equal(t0) || s.Method != "/nopacs.v1.TenantService/CreateTenant" ||
			s.RemoteAddress != "192.0.2.7:51000" || s.RequestID != "req-1" {
			t.Errorf("entry %s has another identity than the check's: %+v", s.ID, s)
		}
	}
	// A tenant name that could not be a tenant's is quoted, as the check's.
	if got := d.Entry(master.OutcomeDone, "", access.Facts{TenantName: "Bad\u202eName"}).TenantName; got != "\"Bad\\u202eName\"" {
		t.Fatalf("a bad tenant name from a handler is written %q", got)
	}
	many := map[string]string{}
	for i := range 30 {
		many["k"+strconv.Itoa(i)] = "v"
	}
	if got := len(d.Entry(master.OutcomeDone, "", access.Facts{Details: many}).Details); got != access.MaxRequestDetails {
		t.Fatalf("%d details kept of 30, want %d", got, access.MaxRequestDetails)
	}
}

// TestUnknownKeyCostsAComparison guards the dummy comparison: a key with an
// unknown id is compared once, as a wrong secret is, and a malformed key,
// which reads nothing, not at all.
func TestUnknownKeyCostsAComparison(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{})
	count := access.CountVerifications(e.access)
	unknown := "nopacs_" + strings.Repeat("A", 12) + k.full[len("nopacs_")+12:]
	for _, tc := range []struct {
		key  string
		want int
	}{
		{"nopacs_malformed", 0},
		{unknown, 1},
		{"nopacs_" + k.id + "_" + strings.Repeat("A", 44), 1},
	} {
		before := count()
		_, err := e.access.Check(e.ctx, access.APIKey(tc.key), access.Authenticated(), facts("m"))
		wantErr(t, "check", err, fault.Unauthenticated, access.MessageUnauthenticated)
		if got := count() - before; got != tc.want {
			t.Errorf("%q: %d comparisons, want %d", tc.key, got, tc.want)
		}
	}
}

// TestDuplicateIDFreshOnFailure guards the record of a change that failed
// because the id source repeated an id: the failure is recorded under a
// fresh id, not lost as a second duplicate.
func TestDuplicateIDFreshOnFailure(t *testing.T) {
	e := newEnv(t)
	e.store.mu.Lock()
	e.store.failCreateKey = fault.Wrap(fault.Internal, master.ErrDuplicateAuditID, "test: duplicate")
	e.store.mu.Unlock()
	e.reset()
	_, err := e.access.CreateKey(e.ctx, e.admin("CreateKey"), "h", access.Expiry{}, master.KeyRules{})
	wantErr(t, "create", err, fault.Internal, "")
	_, entries := e.recorded()
	e.store.mu.Lock()
	attempted := e.store.attempted
	e.store.mu.Unlock()
	if len(entries) != 1 || entries[0].Outcome != master.OutcomeFailed || entries[0].ID == attempted.ID {
		t.Fatalf("failure entries %+v, attempted id %s", entries, attempted.ID)
	}
}

// TestMaxRequestDetails guards the bound on the facts' details: the most
// allowed passes, one more is a server error, recorded.
func TestMaxRequestDetails(t *testing.T) {
	e := newEnv(t)
	f := facts("m")
	f.Details = map[string]string{}
	for i := range access.MaxRequestDetails {
		f.Details["k"+strconv.Itoa(i)] = "v"
	}
	if _, err := e.access.Check(e.ctx, local, access.Authenticated(), f); err != nil {
		t.Fatalf("%d details: %v", len(f.Details), err)
	}
	f.Details["one-more"] = "v"
	_, err := e.access.Check(e.ctx, local, access.Authenticated(), f)
	wantErr(t, "one detail too many", err, fault.Internal, "internal error")
}

// TestLocalUserMustBeClean guards the local user check: a user id the entry
// could not hold as it is (too long, or with a control character) is not
// one the socket reported, and is refused.
func TestLocalUserMustBeClean(t *testing.T) {
	e := newEnv(t)
	for _, uid := range []string{strings.Repeat("5", master.MaxAuditFieldLength+1), "501\n", "501\u202e"} {
		_, err := e.access.Check(e.ctx, access.LocalOperator(uid), access.Authenticated(), facts("m"))
		wantErr(t, strconv.Quote(uid), err, fault.Unauthenticated, access.MessageUnauthenticated)
	}
	if _, err := e.access.Check(e.ctx, access.LocalOperator(strings.Repeat("5", master.MaxAuditFieldLength)), access.Authenticated(), facts("m")); err != nil {
		t.Fatalf("a user id of the longest length: %v", err)
	}
}

// TestDecisionTenantRef guards what a Decision says of its tenant: the id
// and the name, from the one read of the check.
func TestDecisionTenantRef(t *testing.T) {
	e := newEnv(t)
	k := e.key(time.Time{}, on(master.TenantScope(e.lab.ID), master.RoleReader))
	d := e.decide(access.APIKey(k.full), access.OnTenant("lab", master.RoleReader), "m")
	if ref, ok := d.Tenant(); !ok || ref != (access.TenantRef{ID: e.lab.ID, Name: "lab"}) {
		t.Fatalf("tenant %+v %v", ref, ok)
	}
	ld := e.decide(local, access.OnTenant("clinic", master.RoleAdmin), "m")
	if ref, ok := ld.Tenant(); !ok || ref != (access.TenantRef{ID: e.clinic.ID, Name: "clinic"}) {
		t.Fatalf("local operator's tenant %+v %v", ref, ok)
	}
	if _, ok := e.decide(local, access.Authenticated(), "m").Tenant(); ok {
		t.Fatal("a requirement naming no tenant has a tenant")
	}
}

// TestLocalOperatorOnlyFromServer guards the one credential that is admin
// everywhere: no Go file of the repository outside internal/server, tests
// excepted, calls access.LocalOperator. A package that needs it is reviewed
// as a change to who is admin.
func TestLocalOperatorOnlyFromServer(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	files := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "bin" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") ||
			strings.HasPrefix(filepath.ToSlash(rel), "internal/server/") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if ok && sel.Sel.Name == "LocalOperator" {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "access" {
					t.Errorf("%s calls access.LocalOperator; only internal/server may", rel)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 20 {
		t.Fatalf("only %d Go files walked: the walk did not see the repository", files)
	}
}

// TestChangeCarriesItsKey guards how a management call checks a key after
// Check: every call asks the master first, by the check every change makes
// (CheckActor), before it reads anything the request names, and a change
// then hands the key to the master's change, which checks it again in its
// transaction; the key itself is never read again (no Key of the caller, no
// RolesFor). A Decision that Confirm already checked spares a change the
// first CheckActor, but not a read, whose CheckActor is its only check.
func TestChangeCarriesItsKey(t *testing.T) {
	e := newEnv(t)
	admin := e.key(time.Time{}, on(master.Installation, master.RoleAdmin))
	want := map[string][]string{
		"CreateKey":   {"CheckActor", "CreateKey"},
		"RevokeKey":   {"CheckActor", "Key", "RevokeKey"},
		"SetGrant":    {"CheckActor", "SetGrant"},
		"RemoveGrant": {"CheckActor", "RemoveGrant"},
		"GetKey":      {"CheckActor", "Key"},
		"ListKeys":    {"CheckActor"},
		"ListGrants":  {"CheckActor", "GrantsByKey"},
		"WhoAmI":      {"CheckActor", "GrantsByKey"},
	}
	for _, confirmed := range []bool{false, true} {
		for name := range want {
			call := managementCalls(e, e.key(time.Time{}).id)[name]
			d := e.decide(access.APIKey(admin.full), access.OnInstallation(master.RoleAdmin), name)
			if confirmed {
				var err error
				if d, err = e.access.Confirm(e.ctx, d); err != nil {
					t.Fatal(err)
				}
			}
			e.reset()
			if err := call(d); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			calls, _ := e.recorded()
			expect := want[name]
			if confirmed && name != "GetKey" && name != "ListKeys" && name != "ListGrants" && name != "WhoAmI" {
				expect = expect[1:]
			}
			if strings.Join(calls, ",") != strings.Join(expect, ",") {
				t.Errorf("%s (confirmed %v) made the store calls %v, want %v", name, confirmed, calls, expect)
			}
		}
	}
}
