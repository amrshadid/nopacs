package api_test

import (
	"testing"

	connect "connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// setGrantCall is a call of SetGrant.
func setGrantCall(keyID string, scope *v1.Scope, role v1.Role) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: keyID, Scope: scope, Role: role})
	}
}

// removeGrantCall is a call of RemoveGrant.
func removeGrantCall(keyID string, scope *v1.Scope) func(clients) outcome {
	return func(c clients) outcome {
		return call(c.access.RemoveGrant, &v1.RemoveGrantRequest{KeyId: keyID, Scope: scope})
	}
}

// TestTenantAdminGrants guards what a tenant's admin may do with grants:
// set, change and remove reader, writer and operator grants on its tenant,
// each audited in the installation's log; never admin, never on its own
// key, never on a key that is admin there, never on another tenant or the
// installation, never a key's rules; list only the keys of its tenant, each
// by id and holder alone, and the grants on its tenant, never a key's
// grants on every scope. The installation's admin still sets any grant.
func TestTenantAdminGrants(t *testing.T) {
	f := newPlaceFixture(t)
	admin, adminID := f.key(f.onTenant("lab1", master.RoleAdmin))
	_, otherAdminID := f.key(f.onTenant("lab1", master.RoleAdmin))
	_, personID := f.key()
	lab1, lab2 := tenantScope("lab1"), tenantScope("lab2")
	f.run(t, []tcase{
		{name: "a reader let in", as: admin, call: setGrantCall(personID, lab1, v1.Role_ROLE_READER),
			audit: &wantAudit{outcome: master.OutcomeDone, object: personID, details: map[string]string{"scope": "tenant/lab1", "new_role": "reader"}},
			check: func(t *testing.T, o outcome) {
				if g := o.res.(*v1.SetGrantResponse).GetGrant(); g.GetRole() != v1.Role_ROLE_READER || g.GetScope().GetTenant() != "lab1" {
					t.Errorf("grant %v", g)
				}
			}},
		{name: "raised to operator", as: admin, call: setGrantCall(personID, lab1, v1.Role_ROLE_OPERATOR), check: func(t *testing.T, o outcome) {
			if r := o.res.(*v1.SetGrantResponse); r.GetPreviousRole() != v1.Role_ROLE_READER || r.GetGrant().GetRole() != v1.Role_ROLE_OPERATOR {
				t.Errorf("answer %v", r)
			}
		}},
		{name: "made an admin", as: admin, call: setGrantCall(personID, lab1, v1.Role_ROLE_ADMIN), code: connect.CodePermissionDenied,
			msg: "reader, writer or operator", audit: &wantAudit{outcome: master.OutcomeRefused, object: personID}},
		{name: "its own key", as: admin, call: setGrantCall(adminID, lab1, v1.Role_ROLE_READER), code: connect.CodePermissionDenied, msg: "its own key"},
		{name: "another admin lowered", as: admin, call: setGrantCall(otherAdminID, lab1, v1.Role_ROLE_READER), code: connect.CodePermissionDenied,
			msg: "admin on the tenant"},
		{name: "another admin removed", as: admin, call: removeGrantCall(otherAdminID, lab1), code: connect.CodePermissionDenied, msg: "admin on the tenant"},
		{name: "another tenant", as: admin, call: setGrantCall(personID, lab2, v1.Role_ROLE_READER), code: connect.CodePermissionDenied},
		{name: "the installation", as: admin, call: setGrantCall(personID, installation(), v1.Role_ROLE_READER), code: connect.CodePermissionDenied},
		{name: "a key's rules", as: admin, call: func(c clients) outcome {
			return call(c.access.SetKeyRules, &v1.SetKeyRulesRequest{Id: personID, Networks: []string{"10.0.0.0/8"}})
		}, code: connect.CodePermissionDenied},
		{name: "the keys of its tenant", as: admin, call: func(c clients) outcome {
			return call(c.access.ListKeys, &v1.ListKeysRequest{Tenant: "lab1"})
		}, check: func(t *testing.T, o outcome) {
			keys := o.res.(*v1.ListKeysResponse).GetKeys()
			seen := map[string]bool{}
			for _, k := range keys {
				seen[k.GetId()] = true
				if k.GetHolder() == "" || k.GetCreateTime() != nil || k.GetExpireTime() != nil || len(k.GetNetworks()) != 0 || k.GetLastUseAddress() != "" {
					t.Errorf("a tenant's admin read more than a key's id and holder: %v", k)
				}
			}
			if !seen[personID] || !seen[adminID] || !seen[otherAdminID] {
				t.Errorf("keys %v lack those holding grants on lab1", keys)
			}
		}},
		{name: "every key", as: admin, call: func(c clients) outcome { return call(c.access.ListKeys, &v1.ListKeysRequest{}) }, code: connect.CodePermissionDenied},
		{name: "another tenant's keys", as: admin, call: func(c clients) outcome { return call(c.access.ListKeys, &v1.ListKeysRequest{Tenant: "lab2"}) },
			code: connect.CodePermissionDenied},
		{name: "the grants on its tenant", as: admin, call: func(c clients) outcome {
			return call(c.access.ListGrants, &v1.ListGrantsRequest{Filter: &v1.ListGrantsRequest_Scope{Scope: lab1}})
		}, check: func(t *testing.T, o outcome) {
			for _, g := range o.res.(*v1.ListGrantsResponse).GetGrants() {
				if g.GetScope().GetTenant() != "lab1" {
					t.Errorf("a grant of another scope: %v", g)
				}
			}
		}},
		{name: "a key's grants on every scope", as: admin, call: func(c clients) outcome {
			return call(c.access.ListGrants, &v1.ListGrantsRequest{Filter: &v1.ListGrantsRequest_KeyId{KeyId: personID}})
		}, code: connect.CodePermissionDenied},
		{name: "an operator removed", as: admin, call: removeGrantCall(personID, lab1), check: func(t *testing.T, o outcome) {
			if r := o.res.(*v1.RemoveGrantResponse); r.GetPreviousRole() != v1.Role_ROLE_OPERATOR {
				t.Errorf("answer %v", r)
			}
		}},
		{name: "the installation's admin makes an admin", as: f.instAdmin, call: setGrantCall(personID, lab1, v1.Role_ROLE_ADMIN)},
		{name: "a tenant's operator", as: f.lab1Operator, call: setGrantCall(personID, lab1, v1.Role_ROLE_READER), code: connect.CodePermissionDenied},
	})
}
