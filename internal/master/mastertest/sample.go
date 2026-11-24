package mastertest

import (
	"context"
	"crypto/sha256"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/settings"
	"github.com/amrshadid/nopacs/internal/master"
)

// SampleSecret is the secret of the sample's key. It is made up for the
// outside check, which searches a dump of the database for it and must not
// find it: only its SHA-256 (SampleHash) is handed to the store, as package
// secret hands a real key's.
const SampleSecret = "SampleSecretNeverStored7f3c9a1e5b2d8f4a6c0e9b7d5a3f1c8e" //nolint:gosec // G101: a made-up value whose absence the outside check proves.

// SampleHash returns the SHA-256 of SampleSecret, the bytes the store keeps.
func SampleHash() []byte {
	h := sha256.Sum256([]byte(SampleSecret))
	return h[:]
}

// SampleClientCert is the client certificate fingerprint the sample's
// admin key must come with, and SampleNetworks the networks it may be used
// from, as the key rules' columns hold them.
const (
	SampleClientCert = "5a6b7c8d9e0f1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f70819203"
	SampleNetworks   = "10.0.0.0/8,192.0.2.7"
)

// WriteSample writes a small installation into s: two tenants, four keys
// (one revoked, one with a Unicode holder, one with networks and a client
// certificate and used from an address, one made by rotating another),
// grants on the installation and on a tenant, settings, and the audit
// entries of each change, so that a tool other than NoPACs can read what the
// driver wrote.
func WriteSample(tb testing.TB, s master.Store) {
	tb.Helper()
	ctx := context.Background()
	n := 0
	audit := func(method string, outcome master.Outcome, details map[string]string) master.AuditEntry {
		n++
		return master.AuditEntry{
			ID: fmt.Sprintf("sample-audit-%02d", n), Time: T0.Add(time.Duration(n) * time.Second),
			LocalUser: "501", RemoteAddress: "local", Method: method, Outcome: outcome,
			Details: details, RequestID: "sample-request",
		}
	}
	must := func(err error) {
		tb.Helper()
		if err != nil {
			tb.Fatal(err)
		}
	}
	lab := master.Tenant{ID: "sample-tenant-lab", Name: "lab", Driver: "sqlite", SealedAddress: []byte{0x01, 0, 0, 0, 1, 0xde, 0xad}, State: master.StateReady, Created: T0}
	clinic := master.Tenant{ID: "sample-tenant-clinic", Name: "clinic", Driver: "postgres", SealedAddress: []byte{0x01, 0, 0, 0, 1, 0xbe, 0xef}, State: master.StateUnreachable, StateReason: "connection refused", Created: T0}
	must(s.CreateTenant(ctx, lab, audit("CreateTenant", master.OutcomeDone, map[string]string{"driver": "sqlite"}), master.LocalOperator()))
	must(s.CreateTenant(ctx, clinic, audit("CreateTenant", master.OutcomeDone, map[string]string{"driver": "postgres"}), master.LocalOperator()))

	networks, err := settings.ParseNetworks(SampleNetworks)
	must(err)
	admin := master.Key{ID: "SampleKey001", Hash: SampleHash(), Holder: "sample admin", Created: T0, Expires: T0.Add(90 * 24 * time.Hour), Rules: master.KeyRules{Networks: networks, ClientCert: SampleClientCert}}
	reader := master.Key{ID: "SampleKey002", Hash: make([]byte, 32), Holder: "Zoë 山田 🧠", Created: T0}
	gone := master.Key{ID: "SampleKey003", Hash: append(make([]byte, 31), 1), Holder: "former", Created: T0}
	for _, k := range []master.Key{admin, reader, gone} {
		must(s.CreateKey(ctx, k, audit("CreateKey", master.OutcomeDone, nil), master.LocalOperator()))
	}
	_, err = s.RevokeKey(ctx, gone.ID, T0.Add(time.Hour), audit("RevokeKey", master.OutcomeDone, nil), master.LocalOperator())
	must(err)
	_, err = s.MarkKeyUsed(ctx, admin.ID, T0.Add(2*time.Hour), "203.0.113.7")
	must(err)

	_, err = s.SetGrant(ctx, master.Grant{KeyID: admin.ID, Scope: master.Installation, Role: master.RoleAdmin}, audit("SetGrant", master.OutcomeDone, map[string]string{"scope": "installation"}), master.LocalOperator())
	must(err)
	_, err = s.SetGrant(ctx, master.Grant{KeyID: reader.ID, Scope: master.TenantScope(lab.ID), Role: master.RoleReader}, audit("SetGrant", master.OutcomeDone, map[string]string{"scope": "tenant/" + lab.ID}), master.LocalOperator())
	must(err)
	_, err = s.SetGrant(ctx, master.Grant{KeyID: reader.ID, Scope: master.TenantScope(lab.ID), Role: master.RoleWriter}, audit("SetGrant", master.OutcomeDone, map[string]string{"scope": "tenant/" + lab.ID}), master.LocalOperator())
	must(err)

	// The reader's key is rotated once its grant is set, so the successor
	// holds a copy of the grant and the rotated key names its successor.
	successor := master.Key{ID: "SampleKey004", Hash: append(make([]byte, 31), 4), Created: T0.Add(3 * time.Hour)}
	_, err = s.RotateKey(ctx, reader.ID, successor, T0.Add(4*time.Hour), audit("RotateKey", master.OutcomeDone, nil), master.LocalOperator())
	must(err)

	must(s.SetSetting(ctx, "api.max_message_bytes", "4194304", audit("SetSetting", master.OutcomeDone, map[string]string{"name": "api.max_message_bytes"}), master.LocalOperator()))
	must(s.SetSetting(ctx, "retention.audit_days", "365", audit("SetSetting", master.OutcomeDone, map[string]string{"name": "retention.audit_days"}), master.LocalOperator()))

	refused := audit("GetTenant", master.OutcomeRefused, nil)
	refused.LocalUser, refused.KeyID, refused.TenantName, refused.Reason = "", reader.ID, "never-existed", "no grant on this tenant"
	must(s.AppendAudit(ctx, refused))
}
