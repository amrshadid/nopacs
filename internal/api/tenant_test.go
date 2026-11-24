package api_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/registry"
)

// createTenant returns a call of CreateTenant.
func createTenant(name string, typ v1.DatabaseType, address string) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.tenants.CreateTenant, &v1.CreateTenantRequest{Name: name, DatabaseType: typ, DatabaseAddress: address})
	}
}

// sqliteType and pgType are the two database types.
const (
	sqliteType = v1.DatabaseType_DATABASE_TYPE_SQLITE
	pgType     = v1.DatabaseType_DATABASE_TYPE_POSTGRES
)

// testCreateTenant guards CreateTenant: admin on the installation; a SQLite
// tenant made ready with its file where the registry places it; the entry
// with the tenant's name and database type; a name taken; every invalid
// request with the limits of a name and an address; and a tenant whose
// database does not answer.
func testCreateTenant(t *testing.T) {
	f := newFixture(t)
	address := pgAddress
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: createTenant("lab2", sqliteType, ""), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: createTenant("lab2", sqliteType, ""), code: connect.CodePermissionDenied, audit: refused()},
		{name: "at the role", as: f.instAdmin, call: createTenant("lab2", sqliteType, ""),
			audit: &wantAudit{outcome: master.OutcomeDone, tenant: "lab2", object: "tenant/lab2", details: map[string]string{"database_type": "sqlite", "new_state": "ready"}},
			check: func(t *testing.T, o outcome) {
				checkCreationEntries(t, f, "at the role", o.requestID, master.OutcomeDone)
				tn := o.res.(*v1.CreateTenantResponse).GetTenant()
				if tn.GetName() != "lab2" || tn.GetState() != v1.TenantState_TENANT_STATE_READY || tn.GetDatabaseType() != sqliteType || tn.GetCreateTime() == nil || tn.GetStateReason() != "" {
					t.Errorf("got %v", tn)
				}
				if _, err := os.Stat(filepath.Join(f.data, "tenants", "lab2.db")); err != nil {
					t.Errorf("the tenant's file: %v", err)
				}
			}},
		// Refused by the registry before it writes anything, so the handler
		// records the request itself, once.
		{name: "a name taken", as: f.instAdmin, call: createTenant("lab2", sqliteType, ""), code: connect.CodeAlreadyExists, audit: failed("exists")},
		{name: "a name taken, as another type", as: f.instAdmin, call: createTenant("lab2", pgType, address), code: connect.CodeAlreadyExists},
		{name: "name at 63", as: f.instAdmin, call: createTenant("a"+strings.Repeat("b", 62), sqliteType, "")},
		{name: "name at 64", as: f.instAdmin, call: createTenant("a"+strings.Repeat("b", 63), sqliteType, ""), code: connect.CodeInvalidArgument, msg: "name", audit: failed("name")},
		{name: "no name", as: f.instAdmin, call: createTenant("", sqliteType, ""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "upper case", as: f.instAdmin, call: createTenant("Lab3", sqliteType, ""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "a digit first", as: f.instAdmin, call: createTenant("3lab", sqliteType, ""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "a path", as: f.instAdmin, call: createTenant("../x", sqliteType, ""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "a device name Windows reserves", as: f.instAdmin, call: createTenant("con", sqliteType, ""), code: connect.CodeInvalidArgument, msg: "name:"},
		{name: "another device name", as: f.instAdmin, call: createTenant("lpt9", sqliteType, ""), code: connect.CodeInvalidArgument, msg: "name:"},
		{name: "a name that only starts like one", as: f.instAdmin, call: createTenant("console", sqliteType, "")},
		{name: "no type", as: f.instAdmin, call: createTenant("lab3", v1.DatabaseType_DATABASE_TYPE_UNSPECIFIED, ""), code: connect.CodeInvalidArgument, msg: "database_type"},
		{name: "an unknown type", as: f.instAdmin, call: createTenant("lab3", v1.DatabaseType(7), ""), code: connect.CodeInvalidArgument, msg: "database_type"},
		{name: "SQLite with an address", as: f.instAdmin, call: createTenant("lab3", sqliteType, "x"), code: connect.CodeInvalidArgument, msg: "database_address"},
		{name: "Postgres without an address", as: f.instAdmin, call: createTenant("lab3", pgType, ""), code: connect.CodeInvalidArgument, msg: "database_address"},
		{name: "address at 4096 bytes", as: f.instAdmin, call: createTenant("lab3", pgType, address+strings.Repeat("a", 4096-len(address))),
			check: func(t *testing.T, o outcome) {
				if tn := o.res.(*v1.CreateTenantResponse).GetTenant(); tn.GetDatabaseType() != pgType || tn.GetState() != v1.TenantState_TENANT_STATE_READY {
					t.Errorf("got %v", tn)
				}
			}},
		{name: "address at 4097 bytes", as: f.instAdmin, call: createTenant("lab4", pgType, address+strings.Repeat("a", 4097-len(address))), code: connect.CodeInvalidArgument, msg: "database_address"},
	})
	if _, err := f.master.TenantByName(ctx, "lab4"); err == nil {
		t.Error("an invalid request made a tenant")
	}

	testCreateWholeOrNotAtAll(t, f)
}

// testCreateWholeOrNotAtAll guards CreateTenant's "whole or not at all": a
// database that does not answer is UNAVAILABLE and an address the database
// refuses is INVALID_ARGUMENT; neither leaves a tenant registered, the
// request is recorded as failed in the installation's log, and the
// registry's undoing of the row it wrote is recorded there too.
func testCreateWholeOrNotAtAll(t *testing.T, f *fixture) {
	f.pg.setDown("down", true)
	cases := []struct {
		name, tenant, address string
		code                  connect.Code
	}{
		{"a database that does not answer", "down", pgAddress, connect.CodeUnavailable},
		{"an address the database refuses", "refused", pgAddress + "?sslmode=refused", connect.CodeInvalidArgument},
	}
	for _, c := range cases {
		o := createTenant(c.tenant, pgType, c.address)(f.as(f.instAdmin))
		if o.code() != c.code {
			t.Errorf("%s: code %v (%v), want %v", c.name, o.code(), o.err, c.code)
		}
		if _, err := f.master.TenantByName(ctx, c.tenant); err == nil {
			t.Errorf("%s: the tenant is registered in the master", c.name)
		}
		if _, ok := f.registry.State(c.tenant); ok {
			t.Errorf("%s: the registry holds the tenant", c.name)
		}
		checkCreationEntries(t, f, c.name, o.requestID, master.OutcomeFailed)
	}
}

// checkCreationEntries fails unless the master's log holds, for the request
// requestID, the request's own CreateTenant entry once, with outcome (and,
// for a failure, the registry's detail undone), and the registry's system
// entry for the row it wrote, which names the request's entry.
func checkCreationEntries(t *testing.T, f *fixture, name, requestID string, outcome master.Outcome) {
	t.Helper()
	var own, started []master.AuditEntry
	for _, en := range entriesOf(f.masterLog(), requestID) {
		switch en.Method {
		case nopacsv1connect.TenantServiceCreateTenantProcedure:
			own = append(own, en)
		case registry.StartMethod:
			started = append(started, en)
		}
	}
	if len(own) != 1 {
		t.Errorf("%s: %d entries of the request, want 1", name, len(own))
		return
	}
	if own[0].Outcome != outcome {
		t.Errorf("%s: the request's entry is %q (%q), want %q", name, own[0].Outcome, own[0].Reason, outcome)
	}
	if outcome == master.OutcomeFailed && own[0].Details[registry.DetailUndone] == "" {
		t.Errorf("%s: the failed request's entry does not say it was undone: %v", name, own[0].Details)
	}
	if len(started) != 1 || started[0].Details[registry.DetailRequestEntry] != own[0].ID {
		t.Errorf("%s: the row's system entries %+v do not name the request's entry %s", name, started, own[0].ID)
	}
}

// testGetTenant guards GetTenant: reader on the tenant or on the
// installation; the same Denied for a tenant that does not exist as for no
// grant, to a caller who may not list every tenant; NotFound and Invalid to
// one who may; a tenant that is not ready still answered.
func testGetTenant(t *testing.T) {
	f := newFixture(t)
	f.downTenant("down")
	get := func(name string) func(clients) outcome {
		return func(c clients) outcome { return call(c.tenants.GetTenant, &v1.GetTenantRequest{Name: name}) }
	}
	var denied string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: get("lab1"), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.noGrant, call: get("lab1"), code: connect.CodePermissionDenied, audit: refused(),
			check: func(t *testing.T, o outcome) { denied = o.message() }},
		{name: "no such tenant, no right to know", as: f.noGrant, call: get("nolab"), code: connect.CodePermissionDenied,
			check: func(t *testing.T, o outcome) {
				if o.message() != denied {
					t.Errorf("message %q, want the same as for no grant, %q", o.message(), denied)
				}
			}},
		{name: "a malformed name, no right to know", as: f.noGrant, call: get("Lab 1"), code: connect.CodePermissionDenied},
		{name: "a tenant reader", as: f.lab1Reader, call: get("lab1"),
			audit: &wantAudit{outcome: master.OutcomeDone, tenant: "lab1"},
			check: func(t *testing.T, o outcome) {
				tn := o.res.(*v1.GetTenantResponse).GetTenant()
				if tn.GetName() != "lab1" || tn.GetState() != v1.TenantState_TENANT_STATE_READY || tn.GetDatabaseType() != sqliteType {
					t.Errorf("got %v", tn)
				}
			}},
		{name: "a reader of another tenant", as: f.lab1Reader, call: get("down"), code: connect.CodePermissionDenied},
		{name: "an installation reader", as: f.instReader, call: get("lab1")},
		{name: "no such tenant, may list", as: f.instReader, call: get("nolab"), code: connect.CodeNotFound, msg: access.MessageNoTenant},
		{name: "a malformed name, may list", as: f.instReader, call: get("Lab 1"), code: connect.CodeInvalidArgument, msg: "name", audit: failed("name")},
		{name: "no name, may list", as: local, call: get(""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "not ready, still answered", as: f.instReader, call: get("down"),
			check: func(t *testing.T, o outcome) {
				tn := o.res.(*v1.GetTenantResponse).GetTenant()
				if tn.GetState() != v1.TenantState_TENANT_STATE_UNREACHABLE || tn.GetStateReason() == "" {
					t.Errorf("got %v", tn)
				}
			}},
	})
}

// testListTenants guards ListTenants: any authenticated caller; every
// tenant for one who may list them all; for a key with grants on two of
// five, those two, paged with a token that works and is refused with
// another key's list; an empty list for a key with no grant.
func testListTenants(t *testing.T) {
	f := newFixture(t)
	for _, n := range []string{"lab2", "lab3", "lab4", "lab5"} {
		f.tenant(n)
	}
	two, twoID := f.key(f.onTenant("lab2", master.RoleReader), f.onTenant("lab4", master.RoleAdmin))
	_ = twoID
	list := func(size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.tenants.ListTenants, &v1.ListTenantsRequest{PageSize: size, PageToken: token})
		}
	}
	names := func(o outcome) string {
		var out []string
		for _, tn := range o.res.(*v1.ListTenantsResponse).GetTenants() {
			out = append(out, tn.GetName())
		}
		return strings.Join(out, ",")
	}
	var grantedToken, allToken string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(0, ""), code: connect.CodeUnauthenticated},
		{name: "a key with no grant", as: f.noGrant, call: list(0, ""), audit: done(),
			check: func(t *testing.T, o outcome) {
				if got := names(o); got != "" {
					t.Errorf("got %q", got)
				}
			}},
		{name: "two of five", as: two, call: list(0, ""),
			check: func(t *testing.T, o outcome) {
				if got := names(o); got != "lab2,lab4" {
					t.Errorf("got %q, want lab2,lab4", got)
				}
			}},
		{name: "two of five, a page of 1", as: two, call: list(1, ""),
			check: func(t *testing.T, o outcome) {
				grantedToken = o.res.(*v1.ListTenantsResponse).GetNextPageToken()
				if got := names(o); got != "lab2" || grantedToken == "" {
					t.Errorf("got %q, token %q", got, grantedToken)
				}
			}},
		{name: "two of five, the next page", as: two, call: func(c clients) outcome { return list(1, grantedToken)(c) },
			check: func(t *testing.T, o outcome) {
				if got, next := names(o), o.res.(*v1.ListTenantsResponse).GetNextPageToken(); got != "lab4" || next != "" {
					t.Errorf("got %q, token %q", got, next)
				}
			}},
		{name: "another key with that token", as: f.lab1Reader, call: func(c clients) outcome { return list(1, grantedToken)(c) },
			code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "an installation reader", as: f.instReader, call: list(0, ""),
			check: func(t *testing.T, o outcome) {
				if got := names(o); got != "lab1,lab2,lab3,lab4,lab5" {
					t.Errorf("got %q", got)
				}
			}},
		{name: "the local operator, a page of 2", as: local, call: list(2, ""),
			check: func(t *testing.T, o outcome) {
				allToken = o.res.(*v1.ListTenantsResponse).GetNextPageToken()
				if got := names(o); got != "lab1,lab2" || allToken == "" {
					t.Errorf("got %q, token %q", got, allToken)
				}
			}},
		{name: "the next page of all", as: local, call: func(c clients) outcome { return list(2, allToken)(c) },
			check: func(t *testing.T, o outcome) {
				if got := names(o); got != "lab3,lab4" {
					t.Errorf("got %q", got)
				}
			}},
		{name: "a token of all, to a key of two", as: two, call: func(c clients) outcome { return list(2, allToken)(c) },
			code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "a token of two, to a reader of all", as: f.instReader, call: func(c clients) outcome { return list(1, grantedToken)(c) },
			code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "page_size at 1000", as: two, call: list(1000, "")},
		{name: "page_size at 1001", as: two, call: list(1001, ""), code: connect.CodeInvalidArgument, msg: "page_size", audit: failed("page_size")},
		{name: "page_size negative", as: f.instReader, call: list(-1, ""), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "page_token at 1025 bytes", as: two, call: list(0, strings.Repeat("g", 1025)), code: connect.CodeInvalidArgument, msg: "page_token"},
	})
}

// testRemoveTenant guards RemoveTenant: admin on the installation;
// confirm_name; the database type and the file's place in the answer and
// the entry; the registration gone and the file left; a repeat not found.
func testRemoveTenant(t *testing.T) {
	f := newFixture(t)
	f.tenant("lab2")
	remove := func(name, confirm string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.tenants.RemoveTenant, &v1.RemoveTenantRequest{Name: name, ConfirmName: confirm})
		}
	}
	file := filepath.Join(f.data, "tenants", "lab2.db")
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: remove("lab2", "lab2"), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: remove("lab2", "lab2"), code: connect.CodePermissionDenied},
		{name: "confirm_name differs", as: f.instAdmin, call: remove("lab2", "lab1"), code: connect.CodeInvalidArgument, msg: "confirm_name", audit: failed("confirm_name")},
		{name: "confirm_name empty", as: f.instAdmin, call: remove("lab2", ""), code: connect.CodeInvalidArgument, msg: "confirm_name"},
		{name: "no name", as: f.instAdmin, call: remove("", ""), code: connect.CodeInvalidArgument, msg: "name"},
		{name: "at the role", as: f.instAdmin, call: remove("lab2", "lab2"),
			audit: &wantAudit{outcome: master.OutcomeDone, tenant: "lab2", details: map[string]string{"database_type": "sqlite", "data_location": file}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.RemoveTenantResponse)
				if r.GetDatabaseType() != sqliteType || r.GetDataLocation() != file {
					t.Errorf("got %v, want the file %s", r, file)
				}
			}},
		{name: "repeated", as: f.instAdmin, call: remove("lab2", "lab2"), code: connect.CodeNotFound, audit: failed("")},
		{name: "never was", as: f.instAdmin, call: remove("nolab", "nolab"), code: connect.CodeNotFound},
	})
	if _, err := os.Stat(file); err != nil {
		t.Errorf("the removed tenant's file is gone: %v", err)
	}
	if _, ok := f.registry.State("lab2"); ok {
		t.Error("the registry still holds the removed tenant")
	}
}
