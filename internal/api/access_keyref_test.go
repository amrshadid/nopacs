package api_test

import (
	"fmt"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// TestWholeKeyInAKeyIDField guards every request field that takes a key id
// against a whole key pasted in its place: GetKey, RevokeKey, SetKeyRules
// and RotateKey's id, SetGrant and RemoveGrant's key_id, the key_id filters
// of ListGrants and ListAuditEntries, and a stow receiver's key_ids, when it
// is added and when it is changed. Each is answered invalid argument; the
// secret is in no answer and in no audit entry's reason, object or details,
// in the installation's log or the tenant's; and no receiver is stored or
// changed with it. A whole key is 64 characters an id may hold, so a check
// of the characters alone would let it through to the log, which is
// hash-chained and exported and cannot be cleaned.
func TestWholeKeyInAKeyIDField(t *testing.T) {
	t.Parallel()
	f := newPlaceFixture(t)
	whole := "nopacs_" + strings.Repeat("Q", 12) + "_" + strings.Repeat("w", 44)
	secretPart := strings.Repeat("w", 44)
	mustCall[*v1.AddStorageResponse](t, "add main", addStorage("lab1", dirStorage("main", "fast", "mri"))(f.as(local)))
	stow := func(name string, keys ...string) *v1.Receiver {
		return &v1.Receiver{Name: name, Storage: "main", Enabled: true, Type: &v1.Receiver_Stow{Stow: &v1.StowReceiver{KeyIds: keys}}}
	}
	mustCall[*v1.AddReceiverResponse](t, "add web2", addReceiver("lab1", stow("web2", f.noGrantID))(f.as(local)))

	calls := map[string]func(clients) outcome{
		"GetKey":    func(c clients) outcome { return call(c.access.GetKey, &v1.GetKeyRequest{Id: whole}) },
		"RevokeKey": func(c clients) outcome { return call(c.access.RevokeKey, &v1.RevokeKeyRequest{Id: whole}) },
		"SetKeyRules": func(c clients) outcome {
			return call(c.access.SetKeyRules, &v1.SetKeyRulesRequest{Id: whole})
		},
		"RotateKey": func(c clients) outcome { return call(c.access.RotateKey, &v1.RotateKeyRequest{Id: whole}) },
		"SetGrant": func(c clients) outcome {
			return call(c.access.SetGrant, &v1.SetGrantRequest{KeyId: whole, Scope: installation(), Role: v1.Role_ROLE_READER})
		},
		"RemoveGrant": func(c clients) outcome {
			return call(c.access.RemoveGrant, &v1.RemoveGrantRequest{KeyId: whole, Scope: installation()})
		},
		"ListGrants": func(c clients) outcome {
			return call(c.access.ListGrants, &v1.ListGrantsRequest{Filter: &v1.ListGrantsRequest_KeyId{KeyId: whole}})
		},
		"ListAuditEntries": func(c clients) outcome {
			return call(c.audit.ListAuditEntries, &v1.ListAuditEntriesRequest{Scope: installation(), KeyId: whole})
		},
		"AddReceiver": addReceiver("lab1", stow("web", whole)),
		"SetReceiver": func(c clients) outcome {
			return call(c.receivers.SetReceiver, &v1.SetReceiverRequest{Tenant: "lab1", Receiver: stow("web2", whole), UpdateMask: mask("stow.key_ids")})
		},
	}
	for name, do := range calls {
		as := f.instAdmin
		if name == "AddReceiver" || name == "SetReceiver" {
			as = local // a tenant's receivers are its admins', not the installation's
		}
		o := do(f.as(as))
		if o.code() != connect.CodeInvalidArgument {
			t.Errorf("%s with a whole key: %v (%v), want invalid argument", name, o.code(), o.err)
		}
		if strings.Contains(o.message(), secretPart) || (o.err != nil && strings.Contains(o.err.Error(), secretPart)) {
			t.Errorf("%s: the answer repeats the whole key: %q", name, o.message())
		}
	}

	f.flush()
	for log, entries := range map[string][]master.AuditEntry{"installation": f.masterLog(), "lab1": f.tenantLog("lab1")} {
		for _, en := range entries {
			if strings.Contains(fmt.Sprintf("%s %s %v", en.Reason, en.Object, en.Details), secretPart) {
				t.Errorf("an entry of the %s log holds the whole key: %+v", log, en)
			}
		}
	}
	if o := call(f.as(local).receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "web"}); o.code() != connect.CodeNotFound {
		t.Errorf("the receiver added with a whole key: %v (%v), want not found", o.code(), o.err)
	}
	got := mustCall[*v1.GetReceiverResponse](t, "get web2", call(f.as(local).receivers.GetReceiver, &v1.GetReceiverRequest{Tenant: "lab1", Name: "web2"}))
	if keys := got.GetReceiver().GetStow().GetKeyIds(); len(keys) != 1 || keys[0] != f.noGrantID {
		t.Errorf("web2's keys are %v, want [%s] as before", keys, f.noGrantID)
	}
}
