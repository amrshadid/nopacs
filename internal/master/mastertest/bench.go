package mastertest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// The benchmarks of the per-request path: what every API call costs the
// master. They are the baseline each driver reports; no claim is made from
// them yet, as there is no target to hold them to.

// benchStore makes a store holding 1,000 keys and 50 tenants, with one key
// granted on the installation and on three tenants, so the reads below meet
// a table and an index that are not trivially small.
func benchStore(b *testing.B, open Open) (master.Store, string) {
	b.Helper()
	s := open(b)
	ctx := context.Background()
	n := 0
	audit := func() master.AuditEntry {
		n++
		return master.AuditEntry{ID: fmt.Sprintf("bench-setup-%06d", n), Time: T0, Method: "Setup", Outcome: master.OutcomeDone}
	}
	var tenants []string
	for i := range 50 {
		t := master.Tenant{ID: fmt.Sprintf("bt%03d", i), Name: fmt.Sprintf("bench%03d", i), Driver: "sqlite", SealedAddress: []byte{1}, State: master.StateReady, Created: T0}
		if err := s.CreateTenant(ctx, t, audit(), master.LocalOperator()); err != nil {
			b.Fatal(err)
		}
		tenants = append(tenants, t.ID)
	}
	for i := range 1000 {
		k := master.Key{ID: fmt.Sprintf("BenchKey%04d", i), Hash: make([]byte, 32), Holder: "bench", Created: T0}
		if err := s.CreateKey(ctx, k, audit(), master.LocalOperator()); err != nil {
			b.Fatal(err)
		}
	}
	const id = "BenchKey0500"
	for _, scope := range []master.Scope{master.Installation, master.TenantScope(tenants[3]), master.TenantScope(tenants[17]), master.TenantScope(tenants[42])} {
		if _, err := s.SetGrant(ctx, master.Grant{KeyID: id, Scope: scope, Role: master.RoleReader}, audit(), master.LocalOperator()); err != nil {
			b.Fatal(err)
		}
	}
	return s, id
}

// BenchKeyByID measures reading a key by its id, as every request with a
// key does first.
func BenchKeyByID(b *testing.B, open Open) {
	s, id := benchStore(b, open)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if _, err := s.Key(ctx, id); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchGrantsByKey measures reading a key's four grants, as every request
// with a key does second.
func BenchGrantsByKey(b *testing.B, open Open) {
	s, id := benchStore(b, open)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		g, _, err := s.GrantsByKey(ctx, id, master.Page{Limit: master.MaxPageLimit})
		if err != nil || len(g) != 4 {
			b.Fatalf("GrantsByKey = %d grants, %v", len(g), err)
		}
	}
}

// BenchRolesFor measures the access check's read of a key's roles on the
// installation and on one tenant, named, as every request with a key does.
func BenchRolesFor(b *testing.B, open Open) {
	s, id := benchStore(b, open)
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		r, err := s.RolesFor(ctx, id, "bench017")
		if err != nil || r.Installation != master.RoleReader || r.Tenant != master.RoleReader || !r.TenantFound {
			b.Fatalf("RolesFor = %+v, %v", r, err)
		}
	}
}

// BenchAppendAudit measures writing one audit entry in its own
// transaction, synced to disk as the driver syncs every commit, as every
// request does last.
func BenchAppendAudit(b *testing.B, open Open) {
	s, id := benchStore(b, open)
	ctx := context.Background()
	entry := master.AuditEntry{
		Time: T0, KeyID: id, RemoteAddress: "192.0.2.10:50000", Method: "/nopacs.v1.AdminService/ListTenants",
		Outcome: master.OutcomeDone, Details: map[string]string{"limit": "100"}, RequestID: "bench",
	}
	i := 0
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		i++
		entry.ID = fmt.Sprintf("bench-%09d", i)
		if err := s.AppendAudit(ctx, entry); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchMarkKeyUsedNoWrite measures marking a key used within the minute,
// the case of nearly every request of a busy key: the store matches no row
// and writes nothing.
func BenchMarkKeyUsedNoWrite(b *testing.B, open Open) {
	s, id := benchStore(b, open)
	ctx := context.Background()
	if wrote, err := s.MarkKeyUsed(ctx, id, T0, ""); err != nil || !wrote {
		b.Fatalf("the first MarkKeyUsed = %v, %v; want a write", wrote, err)
	}
	at := T0.Add(30 * time.Second)
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		wrote, err := s.MarkKeyUsed(ctx, id, at, "")
		if err != nil || wrote {
			b.Fatalf("MarkKeyUsed = %v, %v; want no write", wrote, err)
		}
	}
}

// BenchSetSettingByKey measures one change, a setting and its audit entry in
// one synced transaction, made by a key that is admin on the installation:
// the store checks the key inside the transaction, so this is what a change
// on the request's path costs.
func BenchSetSettingByKey(b *testing.B, open Open) {
	s, id := benchStore(b, open)
	if _, err := s.SetGrant(context.Background(), master.Grant{KeyID: id, Scope: master.Installation, Role: master.RoleAdmin},
		master.AuditEntry{ID: "bench-admin", Time: T0, Method: "Setup", Outcome: master.OutcomeDone}, master.LocalOperator()); err != nil {
		b.Fatal(err)
	}
	benchSetSetting(b, s, master.ActingKey(id, master.Installation, master.RoleAdmin, at(T0)))
}

// BenchSetSettingByLocalOperator is BenchSetSettingByKey by the local
// operator, whom no store checks: the difference between the two is the
// cost of the check of a key inside a change.
func BenchSetSettingByLocalOperator(b *testing.B, open Open) {
	s, _ := benchStore(b, open)
	benchSetSetting(b, s, master.LocalOperator())
}

// benchSetSetting sets one setting by by, again and again.
func benchSetSetting(b *testing.B, s master.Store, by master.Actor) {
	ctx := context.Background()
	entry := master.AuditEntry{Time: T0, KeyID: by.KeyID(), Method: "/nopacs.v1.SettingsService/SetSetting", Outcome: master.OutcomeDone, RequestID: "bench"}
	i := 0
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		i++
		entry.ID = fmt.Sprintf("bench-%09d", i)
		if err := s.SetSetting(ctx, "bench.setting", "v", entry, by); err != nil {
			b.Fatal(err)
		}
	}
}
