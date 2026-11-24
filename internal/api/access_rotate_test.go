package api_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/master"
)

// testRotateKey guards RotateKey: the installation's admin rotates any key,
// a key rotates itself and no other, never ending itself in under an hour, the successor's secret works with the
// rotated key's grants, the rotated key names its successor, a key is rotated
// once, and each invalid request is answered after the caller is judged.
func testRotateKey(t *testing.T) {
	f := newFixture(t)
	rotate := func(id string, overlap *durationpb.Duration) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.RotateKey, &v1.RotateKeyRequest{Id: id, Overlap: overlap})
		}
	}
	victim, victimID := f.key(f.onTenant("lab1", master.RoleReader))
	var successor caller
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: rotate(victimID, nil), code: connect.CodeUnauthenticated},
		{name: "another key, without admin", as: f.lab1Admin, call: rotate(victimID, nil), code: connect.CodePermissionDenied,
			audit: &wantAudit{outcome: master.OutcomeRefused, object: victimID}},
		{name: "another key's malformed request, without admin", as: f.instOperator, call: rotate("", durationpb.New(-time.Hour)), code: connect.CodePermissionDenied},
		{name: "itself, ending itself in under an hour", as: victim, call: rotate(victimID, durationpb.New(59*time.Minute)), code: connect.CodeInvalidArgument},
		{name: "itself", as: victim, call: rotate(victimID, durationpb.New(time.Hour)),
			audit: &wantAudit{outcome: master.OutcomeDone, object: victimID, details: map[string]string{master.DetailSuccessor: "*", master.DetailOldExpires: "*", "overlap": "1h0m0s"}},
			check: func(t *testing.T, o outcome) {
				r := o.res.(*v1.RotateKeyResponse)
				if !strings.HasPrefix(r.GetSecret(), "nopacs_"+r.GetKey().GetId()+"_") || r.GetKey().GetHolder() != "test" {
					t.Errorf("successor %v", r.GetKey())
				}
				if r.GetRotated().GetSuccessorId() != r.GetKey().GetId() || r.GetRotated().GetExpireTime() == nil {
					t.Errorf("rotated %v", r.GetRotated())
				}
				successor = caller{key: r.GetSecret()}
			}},
		{name: "the successor holds the grants", as: caller{}, call: func(c clients) outcome {
			return call(f.as(successor).storages.ListStorages, &v1.ListStoragesRequest{Tenant: "lab1"})
		}},
		{name: "again", as: f.instAdmin, call: rotate(victimID, nil), code: connect.CodeAlreadyExists},
		{name: "overlap past 30 days", as: f.instAdmin, call: rotate(f.noGrantID, durationpb.New(31*24*time.Hour)), code: connect.CodeInvalidArgument},
		{name: "unknown", as: f.instAdmin, call: rotate("nokey0000000", nil), code: connect.CodeNotFound},
		{name: "empty id", as: f.instAdmin, call: rotate("", nil), code: connect.CodeInvalidArgument},
		{name: "by the admin", as: f.instAdmin, call: rotate(f.noGrantID, nil), audit: done()},
	})
}

// testSetKeyRules guards SetKeyRules: admin on the installation, the rules
// answered and kept, and each malformed network or fingerprint refused.
func testSetKeyRules(t *testing.T) {
	f := newFixture(t)
	set := func(id string, nets []string, cert string) func(clients) outcome {
		return func(c clients) outcome {
			return call(c.access.SetKeyRules, &v1.SetKeyRulesRequest{Id: id, Networks: nets, ClientCertSha256: cert})
		}
	}
	cert := strings.Repeat("ab", 32)
	f.run(t, []tcase{
		{name: "no key", as: nobody, call: set(f.noGrantID, nil, ""), code: connect.CodeUnauthenticated},
		{name: "just below the role", as: f.instOperator, call: set(f.noGrantID, nil, ""), code: connect.CodePermissionDenied},
		{name: "at the role", as: f.instAdmin, call: set(f.noGrantID, []string{"10.0.0.0/8", "2001:db8::1"}, cert),
			audit: &wantAudit{outcome: master.OutcomeDone, object: f.noGrantID, details: map[string]string{"networks": "10.0.0.0/8,2001:db8::1", "client_cert_sha256": cert}},
			check: func(t *testing.T, o outcome) {
				k := o.res.(*v1.SetKeyRulesResponse).GetKey()
				if strings.Join(k.GetNetworks(), ",") != "10.0.0.0/8,2001:db8::1" || k.GetClientCertSha256() != cert {
					t.Errorf("key %v", k)
				}
			}},
		{name: "kept", as: f.instAdmin, call: func(c clients) outcome { return call(c.access.GetKey, &v1.GetKeyRequest{Id: f.noGrantID}) },
			check: func(t *testing.T, o outcome) {
				if k := o.res.(*v1.GetKeyResponse).GetKey(); len(k.GetNetworks()) != 2 || k.GetClientCertSha256() != cert {
					t.Errorf("key %v", k)
				}
			}},
		{name: "host bits set", as: f.instAdmin, call: set(f.noGrantID, []string{"10.1.2.3/8"}, ""), code: connect.CodeInvalidArgument, msg: "networks"},
		{name: "two in one", as: f.instAdmin, call: set(f.noGrantID, []string{"10.0.0.0/8,192.0.2.1"}, ""), code: connect.CodeInvalidArgument, msg: "networks"},
		{name: "twice", as: f.instAdmin, call: set(f.noGrantID, []string{"192.0.2.1", "192.0.2.1"}, ""), code: connect.CodeInvalidArgument, msg: "networks"},
		{name: "fingerprint in capitals", as: f.instAdmin, call: set(f.noGrantID, nil, strings.ToUpper(cert)), code: connect.CodeInvalidArgument, msg: "client_cert_sha256"},
		{name: "unknown", as: f.instAdmin, call: set("nokey0000000", nil, ""), code: connect.CodeNotFound},
		{name: "cleared", as: f.instAdmin, call: set(f.noGrantID, nil, ""), audit: done(),
			check: func(t *testing.T, o outcome) {
				if k := o.res.(*v1.SetKeyRulesResponse).GetKey(); len(k.GetNetworks()) != 0 || k.GetClientCertSha256() != "" {
					t.Errorf("key %v", k)
				}
			}},
	})
}

// TestCreateKeyRulesAndMaximum guards CreateKey's rules and the
// installation's key.max-lifetime: a key made with networks has them, an
// expiry past the maximum is refused, and a key made with no expiry asked
// for lives no longer than the maximum.
func TestCreateKeyRulesAndMaximum(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	admin := f.as(f.instAdmin)
	res, err := admin.access.CreateKey(ctx, &v1.CreateKeyRequest{Holder: "robot", Networks: []string{"192.0.2.0/24"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := res.GetKey().GetNetworks(); len(got) != 1 || got[0] != "192.0.2.0/24" {
		t.Errorf("networks %v", got)
	}
	if _, err := f.as(local).settings.SetSetting(ctx, &v1.SetSettingRequest{Setting: &v1.Setting{Name: "key.max-lifetime", Scope: installation(), Value: "2d"}}); err != nil {
		t.Fatal(err)
	}
	_, err = admin.access.CreateKey(ctx, &v1.CreateKeyRequest{Holder: "long", Expiry: &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(72 * time.Hour)}})
	if connect.CodeOf(err) != connect.CodeInvalidArgument || !strings.Contains(err.Error(), "key.max-lifetime") {
		t.Errorf("an expiry past the maximum: %v", err)
	}
	res, err = admin.access.CreateKey(ctx, &v1.CreateKeyRequest{Holder: "default"})
	if err != nil {
		t.Fatal(err)
	}
	if life := res.GetKey().GetExpireTime().AsTime().Sub(res.GetKey().GetCreateTime().AsTime()); life != 48*time.Hour {
		t.Errorf("a key with the default lifetime lives %v, want the maximum, 48h", life)
	}
	_, err = admin.access.CreateKey(ctx, &v1.CreateKeyRequest{Holder: "bad", ClientCertSha256: "abc"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("a malformed fingerprint: %v", err)
	}
}

// TestStatusExpiringKeys guards the status of every tenant: it lists the
// keys about to expire, and nothing for one tenant.
func TestStatusExpiringKeys(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	res, err := f.as(local).access.CreateKey(ctx, &v1.CreateKeyRequest{Holder: "soon", Expiry: &v1.CreateKeyRequest_ExpireAfter{ExpireAfter: durationpb.New(48 * time.Hour)}})
	if err != nil {
		t.Fatal(err)
	}
	st, err := f.as(local).monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: installation()})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, k := range st.GetExpiringKeys() {
		found = found || k.GetId() == res.GetKey().GetId()
	}
	if !found {
		t.Errorf("expiring keys %v lack %s", st.GetExpiringKeys(), res.GetKey().GetId())
	}
	one, err := f.as(local).monitor.GetStatus(ctx, &v1.GetStatusRequest{Scope: tenantScope("lab1")})
	if err != nil {
		t.Fatal(err)
	}
	if len(one.GetExpiringKeys()) != 0 {
		t.Errorf("a tenant's status lists keys: %v", one.GetExpiringKeys())
	}
}
