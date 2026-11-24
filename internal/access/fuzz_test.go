package access_test

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// FuzzCheck guards the check against arbitrary presented keys and tenant
// names: it never panics, never returns a principal for a key that is not the
// stored one, always answers Unauthenticated with the one fixed message, and
// always writes one refusal entry the master would take. The store is in
// memory, so the fuzzer runs fast; the stored key's id is a seed, so
// well-formed keys with a known id and a wrong secret are explored too, but
// the stored secret is never given.
func FuzzCheck(f *testing.F) {
	m, full := newMem(f)
	a := newMemAccess(f, m, 0)
	id := m.key.ID
	for _, seed := range []string{
		"", "nopacs_", "nopacs__", "Bearer x", "nopacs_" + id + "_",
		"nopacs_" + id + "_" + strings.Repeat("1", 44),
		"nopacs_" + strings.Repeat("1", 12) + "_" + strings.Repeat("z", 44),
		strings.Repeat("\x00", 64), "nopacs_" + id + "_" + strings.Repeat("é", 22),
	} {
		f.Add(seed, "lab")
	}
	f.Add("nopacs_"+id+"_"+strings.Repeat("A", 44), strings.Repeat("x", 300))
	f.Fuzz(func(t *testing.T, key, tenant string) {
		if key == full {
			t.Skip("the stored key itself")
		}
		m.mu.Lock()
		before := len(m.audit)
		m.mu.Unlock()
		d, err := a.Check(context.Background(), access.APIKey(key), access.OnTenant(tenant, master.RoleReader), facts("fuzz"))
		if err == nil || !reflect.DeepEqual(d, access.Decision{}) {
			t.Fatalf("a principal for %q: %+v, %v", key, d, err)
		}
		if fault.KindOf(err) != fault.Unauthenticated || err.Error() != access.MessageUnauthenticated {
			t.Fatalf("%q: %v (%s)", key, err, fault.KindOf(err))
		}
		if err := a.FlushAudit(context.Background()); err != nil {
			t.Fatal(err)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.audit) != before+1 {
			t.Fatalf("%q: %d entries written, want 1", key, len(m.audit)-before)
		}
		if m.badEntry != nil {
			t.Fatalf("%q: an entry the master would refuse: %v", key, m.badEntry)
		}
		// Keep memory flat over a long run.
		m.audit = m.audit[:0]
	})
}

// FuzzCheckSQLite guards the check end to end on the real SQLite master,
// with fuzzed requirements as well as keys: whatever the requirement kind,
// role and tenant name, a key that is not the stored one gets no Decision;
// the stored key, which holds reader on the installation and writer on
// "lab", is allowed exactly when an oracle written from the contract's
// rules says so, and otherwise refused with the right kind; and no entry
// the check makes is one the master would refuse.
func FuzzCheckSQLite(f *testing.F) {
	e := newEnv(f)
	good := e.key(time.Time{}, on(master.Installation, master.RoleReader), on(master.TenantScope(e.lab.ID), master.RoleWriter))
	f.Add("nopacs_x", false, uint8(1), uint8(1), "lab")
	f.Add("", true, uint8(3), uint8(2), "lab")
	f.Add("", true, uint8(3), uint8(3), "lab")
	f.Add("", true, uint8(3), uint8(1), "clinic")
	f.Add("", true, uint8(3), uint8(1), "nosuch")
	f.Add("", true, uint8(4), uint8(1), "Bad Name")
	f.Add("", true, uint8(2), uint8(4), "")
	f.Add(good.full[:len(good.full)-1]+"1", false, uint8(2), uint8(1), "")
	roles := []master.Role{"", master.RoleReader, master.RoleWriter, master.RoleOperator, master.RoleAdmin, "boss"}
	f.Fuzz(func(t *testing.T, key string, useGood bool, kind, roleIndex uint8, tenant string) {
		role := roles[int(roleIndex)%len(roles)]
		var req access.Requirement
		switch kind % 5 {
		case 1:
			req = access.Authenticated()
		case 2:
			req = access.OnInstallation(role)
		case 3:
			req = access.OnTenant(tenant, role)
		case 4:
			req = access.TenantRegistration(tenant)
		}
		validReq := kind%5 == 1 || kind%5 == 4 || (kind%5 != 0 && role.Valid())
		cred := access.APIKey(key)
		if useGood {
			cred = access.APIKey(good.full)
		} else if key == good.full {
			t.Skip("the stored key itself")
		}
		d, err := e.access.Check(e.ctx, cred, req, facts("fuzz"))
		if s := e.access.AuditStats(); s.Failed != 0 {
			t.Fatalf("an entry the master would refuse: %+v", s)
		}
		if !validReq {
			wantErr(t, "an invalid requirement", err, fault.Internal, "internal error")
			return
		}
		if !useGood {
			if err == nil || !reflect.DeepEqual(d, access.Decision{}) || fault.KindOf(err) != fault.Unauthenticated {
				t.Fatalf("%q: %+v, %v", key, d, err)
			}
			return
		}
		// The oracle, from the contract: reader on the installation, writer on
		// lab; an installation reader is told NotFound for a missing tenant.
		exists := tenant == "lab" || tenant == "clinic"
		var want fault.Kind = -1 // allowed
		switch kind % 5 {
		case 2:
			if rankOf(role) > 1 {
				want = fault.Denied
			}
		case 3:
			switch {
			case !exists:
				want = fault.NotFound
			case tenant != "lab" || rankOf(role) > 2:
				want = fault.Denied
			}
		case 4:
			if !exists {
				want = fault.NotFound
			}
		}
		if want == -1 {
			if err != nil {
				t.Fatalf("kind %d role %q tenant %q: refused (%v), the oracle allows", kind%5, role, tenant, err)
			}
			return
		}
		if fault.KindOf(err) != want {
			t.Fatalf("kind %d role %q tenant %q: %v (%s), the oracle says %s", kind%5, role, tenant, err, fault.KindOf(err), want)
		}
	})
}

// rankOf orders the roles as the contract does, for FuzzCheckSQLite's oracle.
func rankOf(r master.Role) int {
	switch r {
	case master.RoleReader:
		return 1
	case master.RoleWriter:
		return 2
	case master.RoleOperator:
		return 3
	case master.RoleAdmin:
		return 4
	}
	return 0
}
