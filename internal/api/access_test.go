package api_test

import (
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/version"
)

// day is a day, for key lifetimes.
const day = 24 * time.Hour

// createKey returns a call of CreateKey.
func createKey(req *v1.CreateKeyRequest) func(clients) outcome {
	return func(c clients) outcome { return call(c.access.CreateKey, req) }
}

// testCreateKey guards CreateKey: admin on the installation; the secret
// answered once, matching the key's id; the default, explicit and absent
// expiry with their limits; the holder's limit; the entry with the new key's
// id and its holder, never its secret.
func testCreateKey(t *testing.T) {
	f := newFixture(t)
	var made *v1.CreateKeyResponse
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: createKey(&v1.CreateKeyRequest{Holder: "a"}), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: createKey(&v1.CreateKeyRequest{Holder: "a"}), code: connect.CodePermissionDenied, audit: refused()},
		{name: "a tenant's admin", as: f.lab1Admin, call: createKey(&v1.CreateKeyRequest{Holder: "a"}), code: connect.CodePermissionDenied},
		{name: "at the role", as: f.instAdmin, call: createKey(&v1.CreateKeyRequest{Holder: "alice"}),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"holder": "alice", "expires": "*"}},
			check: func(t *testing.T, o outcome) {
				made = o.res.(*v1.CreateKeyResponse)
				k := made.GetKey()
				if !strings.HasPrefix(made.GetSecret(), "nopacs_"+k.GetId()+"_") {
					t.Errorf("the secret does not carry the key's id")
				}
				if got := k.GetExpireTime().AsTime().Sub(k.GetCreateTime().AsTime()); got != 90*day {
					t.Errorf("the default expiry is %v after creation", got)
				}
				if k.GetLastUseTime() != nil || k.GetRevokeTime() != nil {
					t.Errorf("a new key has a last use or revoke time: %v", k)
				}
				if k.GetCreateTime().AsTime().Location() != time.UTC {
					t.Errorf("a time is not UTC")
				}
			}},
		{name: "the local operator", as: local, call: createKey(&v1.CreateKeyRequest{Holder: "ops", Expiry: &v1.CreateKeyRequest_NeverExpire{NeverExpire: true}}),
			check: func(t *testing.T, o outcome) {
				if o.res.(*v1.CreateKeyResponse).GetKey().GetExpireTime() != nil {
					t.Errorf("a key that never expires has an expire time")
				}
			}},
		{name: "holder empty", as: f.instAdmin, call: createKey(&v1.CreateKeyRequest{}), code: connect.CodeInvalidArgument, msg: "holder", audit: failed("holder")},
		{name: "holder at 256 bytes", as: f.instAdmin, call: createKey(&v1.CreateKeyRequest{Holder: strings.Repeat("h", 256)})},
		{name: "holder at 257 bytes", as: f.instAdmin, call: createKey(&v1.CreateKeyRequest{Holder: strings.Repeat("h", 257)}), code: connect.CodeInvalidArgument, msg: "holder"},
		{name: "expire_after at 3650 days", as: f.instAdmin,
			call: createKey(&v1.CreateKeyRequest{Holder: "a", Expiry: &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(3650 * day)}}),
			check: func(t *testing.T, o outcome) {
				k := o.res.(*v1.CreateKeyResponse).GetKey()
				if got := k.GetExpireTime().AsTime().Sub(k.GetCreateTime().AsTime()); got != 3650*day {
					t.Errorf("the expiry is %v after creation", got)
				}
			}},
		{name: "expire_after one second past 3650 days", as: f.instAdmin,
			call: createKey(&v1.CreateKeyRequest{Holder: "a", Expiry: &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(3650*day + time.Second)}}),
			code: connect.CodeInvalidArgument, msg: "expire_after"},
		{name: "expire_after zero", as: f.instAdmin,
			call: createKey(&v1.CreateKeyRequest{Holder: "a", Expiry: &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(0)}}),
			code: connect.CodeInvalidArgument, msg: "expire_after"},
		{name: "expire_after negative", as: f.instAdmin,
			call: createKey(&v1.CreateKeyRequest{Holder: "a", Expiry: &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(-time.Hour)}}),
			code: connect.CodeInvalidArgument, msg: "expire_after"},
		{name: "never_expire false", as: f.instAdmin,
			call: createKey(&v1.CreateKeyRequest{Holder: "a", Expiry: &v1.CreateKeyRequest_NeverExpire{NeverExpire: false}}),
			code: connect.CodeInvalidArgument, msg: "never_expire"},
	})
	if made == nil {
		return
	}
	// The entry's object is the new key's id, and no entry anywhere holds
	// the secret.
	for _, en := range f.masterLog() {
		if strings.Contains(en.Reason+en.Object+strings.Join(mapValues(en.Details), " "), made.GetSecret()[len("nopacs_"+made.GetKey().GetId()+"_"):]) {
			t.Errorf("an audit entry holds the key's secret: %+v", en)
		}
		if en.Method == "/nopacs.v1.AccessService/CreateKey" && en.Details["holder"] == "alice" && en.Object != made.GetKey().GetId() {
			t.Errorf("the entry's object is %q, want the new key's id", en.Object)
		}
	}
	// The secret works: the new key is authenticated.
	o := call(f.as(caller{key: made.GetSecret()}).access.WhoAmI, &v1.WhoAmIRequest{})
	if o.err != nil {
		t.Errorf("the new key is refused: %v", o.err)
	}

	// With no expiry asked for, the installation's key.default-lifetime
	// applies, and its built-in default when it is unset again.
	for _, c := range []struct {
		value string
		want  time.Duration
	}{{"30d", 30 * day}, {"2h", 2 * time.Hour}, {"", 90 * day}} {
		admin := f.as(f.instAdmin)
		if c.value == "" {
			call(admin.settings.UnsetSetting, &v1.UnsetSettingRequest{Name: "key.default-lifetime", Scope: installation()})
		} else if o := call(admin.settings.SetSetting, &v1.SetSettingRequest{Setting: &v1.Setting{Name: "key.default-lifetime", Scope: installation(), Value: c.value}}); o.err != nil {
			t.Fatal(o.err)
		}
		o := call(admin.access.CreateKey, &v1.CreateKeyRequest{Holder: "lifetime"})
		if o.err != nil {
			t.Fatal(o.err)
		}
		k := o.res.(*v1.CreateKeyResponse).GetKey()
		if got := k.GetExpireTime().AsTime().Sub(k.GetCreateTime().AsTime()); got != c.want {
			t.Errorf("with key.default-lifetime %q, a key expires %v after it is made, want %v", c.value, got, c.want)
		}
	}
}

// mapValues returns the values of a map.
func mapValues(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

// testGetKey guards GetKey: admin on the installation, the key's object in
// the entry, revoked keys answered, an unknown id not found, a malformed id
// invalid.
func testGetKey(t *testing.T) {
	f := newFixture(t)
	get := func(id string) func(clients) outcome {
		return func(c clients) outcome { return call(c.access.GetKey, &v1.GetKeyRequest{Id: id}) }
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: get(f.noGrantID), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: get(f.noGrantID), code: connect.CodePermissionDenied},
		{name: "at the role", as: f.instAdmin, call: get(f.noGrantID),
			audit: &wantAudit{outcome: master.OutcomeDone, object: f.noGrantID},
			check: func(t *testing.T, o outcome) {
				if k := o.res.(*v1.GetKeyResponse).GetKey(); k.GetId() != f.noGrantID || k.GetHolder() != "test" {
					t.Errorf("got %v", k)
				}
			}},
		{name: "not found", as: f.instAdmin, call: get("nokey0000000"), code: connect.CodeNotFound, audit: failed("no key")},
		{name: "empty id", as: f.instAdmin, call: get(""), code: connect.CodeInvalidArgument, msg: "id", audit: failed("id")},
		{name: "malformed id", as: f.instAdmin, call: get("no/such"), code: connect.CodeInvalidArgument, msg: "id"},
	})
	call(f.as(f.instAdmin).access.RevokeKey, &v1.RevokeKeyRequest{Id: f.noGrantID})
	o := call(f.as(f.instAdmin).access.GetKey, &v1.GetKeyRequest{Id: f.noGrantID})
	if o.err != nil || o.res.(*v1.GetKeyResponse).GetKey().GetRevokeTime() == nil {
		t.Errorf("a revoked key is not answered with its revoke time: %v", o.err)
	}
}

// testListKeys guards ListKeys: admin on the installation, paging with its
// limits, a token that works and one that is refused.
func testListKeys(t *testing.T) {
	f := newFixture(t)
	list := func(size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.ListKeys, &v1.ListKeysRequest{PageSize: size, PageToken: token})
		}
	}
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: list(0, ""), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: list(0, ""), code: connect.CodePermissionDenied},
		{name: "at the role, default page", as: f.instAdmin, call: list(0, ""), audit: done(),
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ListKeysResponse)
				if len(r.GetKeys()) != 7 || r.GetNextPageToken() != "" {
					t.Errorf("got %d keys, token %q; want the fixture's 7 and no token", len(r.GetKeys()), r.GetNextPageToken())
				}
			}},
		{name: "a page of 3", as: f.instAdmin, call: list(3, ""),
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ListKeysResponse)
				token = r.GetNextPageToken()
				if len(r.GetKeys()) != 3 || token == "" {
					t.Errorf("got %d keys, token %q", len(r.GetKeys()), token)
				}
			}},
		{name: "page_size at 1000", as: f.instAdmin, call: list(1000, "")},
		{name: "page_size at 1001", as: f.instAdmin, call: list(1001, ""), code: connect.CodeInvalidArgument, msg: "page_size", audit: failed("page_size")},
		{name: "page_size negative", as: f.instAdmin, call: list(-1, ""), code: connect.CodeInvalidArgument, msg: "page_size"},
		{name: "page_token at 1024 bytes", as: f.instAdmin, call: list(0, strings.Repeat("x", 1024)), code: connect.CodeInvalidArgument, msg: "page_token: is not one"},
		{name: "page_token at 1025 bytes", as: f.instAdmin, call: list(0, strings.Repeat("x", 1025)), code: connect.CodeInvalidArgument, msg: "page_token: is at most"},
	})
	seen := 3
	for token != "" {
		o := call(f.as(f.instAdmin).access.ListKeys, &v1.ListKeysRequest{PageSize: 3, PageToken: token})
		if o.err != nil {
			t.Fatalf("the next page: %v", o.err)
		}
		seen += len(o.res.(*v1.ListKeysResponse).GetKeys())
		token = o.res.(*v1.ListKeysResponse).GetNextPageToken()
	}
	if seen != 7 {
		t.Errorf("the pages hold %d keys, want 7", seen)
	}
}

// testRevokeKey guards RevokeKey: admin on the installation, the revoke
// time, the same time on a repeat, the revoked key refused afterwards.
func testRevokeKey(t *testing.T) {
	f := newFixture(t)
	revoke := func(id string) func(clients) outcome {
		return func(c clients) outcome { return call(c.access.RevokeKey, &v1.RevokeKeyRequest{Id: id}) }
	}
	var first time.Time
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: revoke(f.noGrantID), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: revoke(f.noGrantID), code: connect.CodePermissionDenied},
		{name: "at the role", as: f.instAdmin, call: revoke(f.noGrantID),
			audit: &wantAudit{outcome: master.OutcomeDone, object: f.noGrantID, details: map[string]string{"already_revoked": "false"}},
			check: func(t *testing.T, o outcome) {
				first = o.res.(*v1.RevokeKeyResponse).GetKey().GetRevokeTime().AsTime()
				if first.IsZero() {
					t.Error("no revoke time")
				}
			}},
		{name: "again", as: f.instAdmin, call: revoke(f.noGrantID),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"already_revoked": "true"}},
			check: func(t *testing.T, o outcome) {
				if got := o.res.(*v1.RevokeKeyResponse).GetKey().GetRevokeTime().AsTime(); !got.Equal(first) {
					t.Errorf("revoke time %v, want the first, %v", got, first)
				}
			}},
		{name: "the revoked key", as: f.noGrant, call: func(c clients) outcome { return call(c.access.WhoAmI, &v1.WhoAmIRequest{}) }, code: connect.CodeUnauthenticated},
		{name: "not found", as: f.instAdmin, call: revoke("nokey0000000"), code: connect.CodeNotFound},
		{name: "empty id", as: f.instAdmin, call: revoke(""), code: connect.CodeInvalidArgument, msg: "id"},
	})
}

// testSetGrant guards SetGrant: admin on the installation, the previous
// role, the entry's scope and roles, an unknown key or tenant, a revoked
// key, and each invalid request.
func testSetGrant(t *testing.T) {
	f := newFixture(t)
	revoked, revokedID := f.key()
	_ = revoked
	call(f.as(local).access.RevokeKey, &v1.RevokeKeyRequest{Id: revokedID})
	set := func(id string, s *v1.Scope, r v1.Role) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: id, Scope: s, Role: r})
		}
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: set(f.noGrantID, tenantScope("lab1"), v1.Role_ROLE_READER), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: set(f.noGrantID, tenantScope("lab1"), v1.Role_ROLE_READER), code: connect.CodePermissionDenied},
		{name: "at the role", as: f.instAdmin, call: set(f.noGrantID, tenantScope("lab1"), v1.Role_ROLE_READER),
			audit: &wantAudit{outcome: master.OutcomeDone, object: f.noGrantID, details: map[string]string{"scope": "tenant/lab1", "old_role": "none", "new_role": "reader"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.SetGrantResponse)
				if r.GetPreviousRole() != v1.Role_ROLE_UNSPECIFIED || r.GetGrant().GetRole() != v1.Role_ROLE_READER || r.GetGrant().GetScope().GetTenant() != "lab1" {
					t.Errorf("got %v", r)
				}
			}},
		{name: "replaced", as: f.instAdmin, call: set(f.noGrantID, tenantScope("lab1"), v1.Role_ROLE_ADMIN),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"old_role": "reader", "new_role": "admin"}},
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.SetGrantResponse); r.GetPreviousRole() != v1.Role_ROLE_READER {
					t.Errorf("previous role %v", r.GetPreviousRole())
				}
			}},
		{name: "repeated", as: f.instAdmin, call: set(f.noGrantID, tenantScope("lab1"), v1.Role_ROLE_ADMIN),
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.SetGrantResponse); r.GetPreviousRole() != v1.Role_ROLE_ADMIN {
					t.Errorf("previous role %v", r.GetPreviousRole())
				}
			}},
		{name: "on the installation", as: f.instAdmin, call: set(f.noGrantID, installation(), v1.Role_ROLE_OPERATOR)},
		{name: "unknown key", as: f.instAdmin, call: set("nokey0000000", installation(), v1.Role_ROLE_READER), code: connect.CodeNotFound, audit: failed("")},
		{name: "unknown tenant", as: f.instAdmin, call: set(f.noGrantID, tenantScope("nolab"), v1.Role_ROLE_READER), code: connect.CodeNotFound, msg: "no tenant"},
		{name: "revoked key", as: f.instAdmin, call: set(revokedID, installation(), v1.Role_ROLE_READER), code: connect.CodeAlreadyExists},
		{name: "no scope", as: f.instAdmin, call: set(f.noGrantID, nil, v1.Role_ROLE_READER), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "malformed tenant", as: f.instAdmin, call: set(f.noGrantID, tenantScope("Lab1"), v1.Role_ROLE_READER), code: connect.CodeInvalidArgument, msg: "scope.tenant"},
		{name: "no role", as: f.instAdmin, call: set(f.noGrantID, installation(), v1.Role_ROLE_UNSPECIFIED), code: connect.CodeInvalidArgument, msg: "role"},
		{name: "unknown role", as: f.instAdmin, call: set(f.noGrantID, installation(), v1.Role(9)), code: connect.CodeInvalidArgument, msg: "role"},
		{name: "writer on the installation", as: f.instAdmin, call: set(f.noGrantID, installation(), v1.Role_ROLE_WRITER), code: connect.CodeInvalidArgument, msg: "role: writer"},
		{name: "no key id", as: f.instAdmin, call: set("", installation(), v1.Role_ROLE_READER), code: connect.CodeInvalidArgument, msg: "key_id"},
	})
}

// testListGrants guards ListGrants: admin on the installation, exactly one
// filter, an unknown tenant, paging, and a token refused with another
// filter.
func testListGrants(t *testing.T) {
	f := newFixture(t)
	byKey := func(id string, size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.ListGrants, &v1.ListGrantsRequest{Filter: &v1.ListGrantsRequest_KeyId{KeyId: id}, PageSize: size, PageToken: token})
		}
	}
	byScope := func(s *v1.Scope, size int32, token string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.ListGrants, &v1.ListGrantsRequest{Filter: &v1.ListGrantsRequest_Scope{Scope: s}, PageSize: size, PageToken: token})
		}
	}
	var token string
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: byKey(f.lab1ReaderID, 0, ""), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: byKey(f.lab1ReaderID, 0, ""), code: connect.CodePermissionDenied},
		{name: "by key", as: f.instAdmin, call: byKey(f.lab1ReaderID, 0, ""),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"filter": "key/" + f.lab1ReaderID}},
			check: func(t *testing.T, o outcome) {
				g := o.res.(*v1.ListGrantsResponse).GetGrants()
				if len(g) != 1 || g[0].GetScope().GetTenant() != "lab1" || g[0].GetRole() != v1.Role_ROLE_READER {
					t.Errorf("got %v", g)
				}
			}},
		{name: "by scope, a page of 2", as: f.instAdmin, call: byScope(tenantScope("lab1"), 2, ""),
			audit: &wantAudit{outcome: master.OutcomeDone, details: map[string]string{"filter": "tenant/lab1"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.ListGrantsResponse)
				token = r.GetNextPageToken()
				if len(r.GetGrants()) != 2 || token == "" {
					t.Errorf("got %d grants, token %q", len(r.GetGrants()), token)
				}
			}},
		{name: "the next page", as: f.instAdmin, call: func(c clients) outcome { return byScope(tenantScope("lab1"), 2, token)(c) },
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.ListGrantsResponse); len(r.GetGrants()) != 1 || r.GetNextPageToken() != "" {
					t.Errorf("got %d grants, token %q", len(r.GetGrants()), r.GetNextPageToken())
				}
			}},
		{name: "a token with another filter", as: f.instAdmin, call: func(c clients) outcome { return byScope(installation(), 2, token)(c) },
			code: connect.CodeInvalidArgument, msg: "page_token"},
		{name: "by the installation", as: f.instAdmin, call: byScope(installation(), 0, ""),
			check: func(t *testing.T, o outcome) {
				if g := o.res.(*v1.ListGrantsResponse).GetGrants(); len(g) != 3 {
					t.Errorf("got %d grants on the installation, want 3", len(g))
				}
			}},
		{name: "no filter", as: f.instAdmin, call: func(c clients) outcome { return call(c.access.ListGrants, &v1.ListGrantsRequest{}) },
			code: connect.CodeInvalidArgument, msg: "filter", audit: failed("filter")},
		{name: "empty key id", as: f.instAdmin, call: byKey("", 0, ""), code: connect.CodeInvalidArgument, msg: "key_id"},
		{name: "empty scope", as: f.instAdmin, call: byScope(&v1.Scope{}, 0, ""), code: connect.CodeInvalidArgument, msg: "scope"},
		{name: "unknown tenant", as: f.instAdmin, call: byScope(tenantScope("nolab"), 0, ""), code: connect.CodeNotFound},
		{name: "page_size at 1001", as: f.instAdmin, call: byKey(f.lab1ReaderID, 1001, ""), code: connect.CodeInvalidArgument, msg: "page_size"},
	})
}

// testRemoveGrant guards RemoveGrant: admin on the installation, the role
// removed in the answer and the entry, a repeat that succeeds, an unknown
// key or tenant.
func testRemoveGrant(t *testing.T) {
	f := newFixture(t)
	remove := func(id string, s *v1.Scope) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.RemoveGrant, &v1.RemoveGrantRequest{KeyId: id, Scope: s})
		}
	}
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: remove(f.lab1ReaderID, tenantScope("lab1")), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: remove(f.lab1ReaderID, tenantScope("lab1")), code: connect.CodePermissionDenied},
		{name: "at the role", as: f.instAdmin, call: remove(f.lab1ReaderID, tenantScope("lab1")),
			audit: &wantAudit{outcome: master.OutcomeDone, object: f.lab1ReaderID, details: map[string]string{"scope": "tenant/lab1", "old_role": "reader", "new_role": "none"}},
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.RemoveGrantResponse); r.GetPreviousRole() != v1.Role_ROLE_READER {
					t.Errorf("previous role %v", r.GetPreviousRole())
				}
			}},
		{name: "again, nothing to remove", as: f.instAdmin, call: remove(f.lab1ReaderID, tenantScope("lab1")),
			check: func(t *testing.T, o outcome) {
				if r := o.res.(*v1.RemoveGrantResponse); r.GetPreviousRole() != v1.Role_ROLE_UNSPECIFIED {
					t.Errorf("previous role %v", r.GetPreviousRole())
				}
			}},
		{name: "the key has lost its grant", as: f.lab1Reader, call: func(c clients) outcome {
			return call(c.tenants.GetTenant, &v1.GetTenantRequest{Name: "lab1"})
		}, code: connect.CodePermissionDenied},
		{name: "unknown key", as: f.instAdmin, call: remove("nokey0000000", installation()), code: connect.CodeNotFound},
		{name: "unknown tenant", as: f.instAdmin, call: remove(f.lab1ReaderID, tenantScope("nolab")), code: connect.CodeNotFound},
		{name: "no scope", as: f.instAdmin, call: remove(f.lab1ReaderID, nil), code: connect.CodeInvalidArgument, msg: "scope", audit: failed("scope")},
		{name: "no key id", as: f.instAdmin, call: remove("", installation()), code: connect.CodeInvalidArgument, msg: "key_id"},
	})
}

// testWhoAmI guards WhoAmI: any authenticated caller, the key with its
// grants and the server's version, the local operator with none.
func testWhoAmI(t *testing.T) {
	f := newFixture(t)
	who := func(c clients) outcome { return call(c.access.WhoAmI, &v1.WhoAmIRequest{}) }
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: who, code: connect.CodeUnauthenticated},
		{name: "an unknown key", as: caller{key: "nopacs_AAAAAAAAAAAA_" + strings.Repeat("B", 44)}, call: who, code: connect.CodeUnauthenticated},
		{name: "a key with no grant", as: f.noGrant, call: who, audit: done(),
			check: func(t *testing.T, o outcome) {
				p := o.res.(*v1.WhoAmIResponse).GetPrincipal()
				if p.GetKey().GetId() != f.noGrantID || len(p.GetGrants()) != 0 {
					t.Errorf("got %v", p)
				}
			}},
		{name: "a key with a grant", as: f.lab1Reader, call: who,
			check: func(t *testing.T, o outcome) {
				p := o.res.(*v1.WhoAmIResponse).GetPrincipal()
				g := p.GetGrants()
				if p.GetKey().GetId() != f.lab1ReaderID || len(g) != 1 || g[0].GetScope().GetTenant() != "lab1" || g[0].GetRole() != v1.Role_ROLE_READER || g[0].GetKeyId() != f.lab1ReaderID {
					t.Errorf("got %v", p)
				}
				// The answer names the build, as the server's version command
				// prints it, so a client can warn on a mismatch.
				res := o.res.(*v1.WhoAmIResponse)
				if info := version.Get(); res.GetServerVersion() != info.Version || res.GetServerCommit() != info.Commit || res.GetServerVersion() == "" {
					t.Errorf("server version %q commit %q, want %q %q", res.GetServerVersion(), res.GetServerCommit(), info.Version, info.Commit)
				}
			}},
		{name: "the local operator", as: local, call: who,
			check: func(t *testing.T, o outcome) {
				p := o.res.(*v1.WhoAmIResponse).GetPrincipal()
				if p.GetLocalOperator().GetUserId() != "501" || p.GetKey() != nil || len(p.GetGrants()) != 0 {
					t.Errorf("got %v", p)
				}
			}},
	})
}
