package mastertest

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// This file holds the tests that pin the values a driver must keep exactly,
// which a second driver could otherwise get subtly wrong and still pass:
// ids compared whole and case-sensitively, text ordered by its bytes and
// kept byte for byte, the edges of time, and the edges of every size bound.

// testIDsCaseAndOrder guards that ids are compared whole and case-sensitively
// and that lists order text by its bytes, with mixed case, "-" and "_".
func testIDsCaseAndOrder(e *env) {
	upper, lower := e.key(), e.key()
	upper.ID, lower.ID = "CaseKey", "casekey"
	upper.Holder, lower.Holder = "upper", "lower"
	e.ok("CreateKey CaseKey", e.s.CreateKey(e.ctx, upper, e.audit(), master.LocalOperator()))
	e.ok("CreateKey casekey", e.s.CreateKey(e.ctx, lower, e.audit(), master.LocalOperator()))
	for _, k := range []master.Key{upper, lower} {
		got, err := e.s.Key(e.ctx, k.ID)
		if err != nil || got.Holder != k.Holder {
			e.Errorf("Key(%s) = %q, %v; want %q", k.ID, got.Holder, err, k.Holder)
		}
	}
	for _, id := range []string{"CASEKEY", "caseKey", "CaseKe", "CaseKeyX"} {
		if _, err := e.s.Key(e.ctx, id); fault.KindOf(err) != fault.NotFound {
			e.Errorf("Key(%s) = %v, want NotFound: ids are compared whole and by case", id, err)
		}
	}
	t1, t2 := e.tenant("upper"), e.tenant("lower")
	t1.ID, t2.ID = "Tid", "tid"
	e.ok("CreateTenant Tid", e.s.CreateTenant(e.ctx, t1, e.audit(), master.LocalOperator()))
	e.ok("CreateTenant tid", e.s.CreateTenant(e.ctx, t2, e.audit(), master.LocalOperator()))
	if got, err := e.s.Tenant(e.ctx, "tid"); err != nil || got.Name != "lower" {
		e.Errorf("Tenant(tid) = %q, %v; want lower", got.Name, err)
	}

	// Keys of one creation time, so the list orders them by id alone.
	ids := []string{"a_b", "a-b", "A-b", "B", "a", "_x", "-x", "Z", "z9", "Z_", "AB", "Ab"}
	later := T0.Add(24 * time.Hour)
	for _, id := range ids {
		k := e.key()
		k.ID, k.Created, k.Expires = id, later, later.Add(time.Hour)
		e.ok("CreateKey "+id, e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	}
	want := slices.Clone(ids)
	sort.Strings(want) // byte order
	keys, _, err := e.s.ListKeys(e.ctx, master.Page{Limit: 1000})
	e.ok("ListKeys", err)
	var got []string
	for _, k := range keys {
		if k.Created.Equal(later) {
			got = append(got, k.ID)
		}
	}
	if !slices.Equal(got, want) {
		e.Errorf("keys of one time in order %q, want byte order %q", got, want)
	}
	scope := master.TenantScope(t1.ID)
	for _, id := range ids {
		e.mustGrant(id, scope, master.RoleReader)
	}
	grants, _, err := e.s.GrantsByScope(e.ctx, scope, master.Page{Limit: 1000})
	e.ok("GrantsByScope", err)
	got = got[:0]
	for _, g := range grants {
		got = append(got, g.KeyID)
	}
	if !slices.Equal(got, want) {
		e.Errorf("grants on one scope in order %q, want byte order %q", got, want)
	}

	// Tenant ids for the list by key, and names and setting names.
	tids := []string{"T_1", "T-1", "t1", "T1", "_t"}
	k := e.mustKey()
	for i, id := range tids {
		t := e.tenant(fmt.Sprintf("o%d", i))
		t.ID = id
		e.ok("CreateTenant "+id, e.s.CreateTenant(e.ctx, t, e.audit(), master.LocalOperator()))
		e.mustGrant(k.ID, master.TenantScope(id), master.RoleReader)
	}
	wantT := append([]string{""}, tids...)
	sort.Strings(wantT)
	byKey, _, err := e.s.GrantsByKey(e.ctx, k.ID, master.Page{Limit: 1000})
	e.ok("GrantsByKey", err)
	got = got[:0]
	for _, g := range byKey {
		got = append(got, g.Scope.TenantID)
	}
	if !slices.Equal(got, wantT[1:]) { // no installation grant here
		e.Errorf("grants of one key in order %q, want byte order %q", got, wantT[1:])
	}
	names := []string{"n-b", "n0", "nb", "n", "n-"}
	for _, n := range names {
		e.mustTenant(n)
	}
	tenants, _, err := e.s.ListTenants(e.ctx, master.Page{Limit: 1000})
	e.ok("ListTenants", err)
	got = got[:0]
	for _, t := range tenants {
		if strings.HasPrefix(t.Name, "n") {
			got = append(got, t.Name)
		}
	}
	wantN := slices.Clone(names)
	sort.Strings(wantN)
	if !slices.Equal(got, wantN) {
		e.Errorf("tenant names in order %q, want byte order %q", got, wantN)
	}
	settings := []string{"a.b", "a-b", "a_b", "a0", "ab", "a"}
	for _, n := range settings {
		e.ok("SetSetting", e.s.SetSetting(e.ctx, n, "v", e.audit(), master.LocalOperator()))
	}
	list, _, err := e.s.ListSettings(e.ctx, master.Page{Limit: 1000})
	e.ok("ListSettings", err)
	got = got[:0]
	for _, s := range list {
		got = append(got, s.Name)
	}
	wantS := slices.Clone(settings)
	sort.Strings(wantS)
	if !slices.Equal(got, wantS) {
		e.Errorf("settings in order %q, want byte order %q", got, wantS)
	}
}

// testExactText guards that text is kept and matched byte for byte: the
// audit tenant-name filter is exact (case, spaces, Unicode normal forms),
// and a holder's bytes come back exactly, trailing spaces and NFC or NFD
// form included.
func testExactText(e *env) {
	const nfc, nfd = "läb", "läb" // the same text to a reader, different bytes
	names := []string{"lab", "Lab", "lab ", " lab", nfc, nfd}
	for _, n := range names {
		a := e.audit()
		a.TenantName = n
		e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	}
	for _, n := range names {
		got := e.allAudit(master.AuditFilter{TenantName: n})
		if len(got) != 1 || got[0].TenantName != n {
			var seen []string
			for _, a := range got {
				seen = append(seen, a.TenantName)
			}
			e.Errorf("filter %q matched %q, want exactly that name once", n, seen)
		}
	}
	holders := []string{"name  ", "  name", "Zoé", "Zoé", "tab\there"}
	for _, h := range holders {
		k := e.key()
		k.Holder = h
		e.ok("CreateKey", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
		got, err := e.s.Key(e.ctx, k.ID)
		e.ok("Key", err)
		if got.Holder != h {
			e.Errorf("holder %q came back as %q", h, got.Holder)
		}
	}
}

// testTimeEdges guards the first and the last time the store keeps, read back
// exactly, and their order in a list.
func testTimeEdges(e *env) {
	first := time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	last := time.Date(9999, 12, 31, 23, 59, 59, 999999000, time.UTC)
	k := e.key()
	k.Created, k.Expires = first, last
	e.ok("CreateKey", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	got, err := e.s.Key(e.ctx, k.ID)
	e.ok("Key", err)
	if !SameTime(got.Created, first) || !SameTime(got.Expires, last) {
		e.Errorf("created %v and expires %v, want %v and %v", got.Created, got.Expires, first, last)
	}
	w, err := e.s.MarkKeyUsed(e.ctx, k.ID, last, "")
	if err != nil || !w {
		e.Errorf("MarkKeyUsed at the last time = %v, %v", w, err)
	}
	keys, _, err := e.s.ListKeys(e.ctx, master.Page{Limit: 1})
	e.ok("ListKeys", err)
	if len(keys) != 1 || keys[0].ID != k.ID {
		e.Errorf("the key created at 1970 is not first in the list")
	}
	a1, a2 := e.audit(), e.audit()
	a1.Time, a2.Time = first, last
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a1, a2))
	all := e.allAudit(master.AuditFilter{})
	if !SameTime(all[0].Time, last) || !SameTime(all[len(all)-1].Time, first) {
		e.Errorf("audit newest %v and oldest %v, want %v and %v", all[0].Time, all[len(all)-1].Time, last, first)
	}
	since := e.allAudit(master.AuditFilter{Since: last})
	if len(since) != 1 || since[0].ID != a2.ID {
		e.Errorf("entries since the last time = %d, want the one at it", len(since))
	}
}

// detailsOf returns the details of the audit entry with id.
func (e *env) detailsOf(id string) map[string]string {
	e.Helper()
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if a.ID == id {
			return a.Details
		}
	}
	e.Errorf("no audit entry %s", id)
	return nil
}

// testSizeBounds guards every size bound through the store: a value at
// exactly its bound is stored and read back, and one byte over is Invalid.
func testSizeBounds(e *env) {
	t := master.Tenant{
		ID:            strings.Repeat("T", master.MaxIDLength),
		Name:          "n" + strings.Repeat("x", master.MaxTenantNameLength-1),
		Driver:        strings.Repeat("d", master.MaxDriverLength),
		SealedAddress: []byte(strings.Repeat("s", master.MaxSealedAddressLength)),
		State:         master.StateFailed,
		StateReason:   strings.Repeat("r", master.MaxStateReasonLength),
		Created:       T0,
	}
	e.ok("CreateTenant at every bound", e.s.CreateTenant(e.ctx, t, e.audit(), master.LocalOperator()))
	got, err := e.s.Tenant(e.ctx, t.ID)
	e.ok("Tenant", err)
	if d := diffTenant(got, t); d != "" {
		e.Errorf("tenant at every bound: %s", d)
	}
	over := map[string]func(*master.Tenant){
		"id":             func(x *master.Tenant) { x.ID += "T" },
		"name":           func(x *master.Tenant) { x.Name += "x" },
		"driver":         func(x *master.Tenant) { x.Driver += "d" },
		"sealed address": func(x *master.Tenant) { x.SealedAddress = append(x.SealedAddress, 's') },
		"state reason":   func(x *master.Tenant) { x.StateReason += "r" },
	}
	for name, change := range over {
		x := t
		x.ID, x.Name = "other", "other"
		x.SealedAddress = slices.Clone(t.SealedAddress)
		if name == "id" {
			x.ID = t.ID
		}
		if name == "name" {
			x.Name = t.Name
		}
		change(&x)
		e.wantKind("CreateTenant, "+name+" one byte over", e.s.CreateTenant(e.ctx, x, e.audit(), master.LocalOperator()), fault.Invalid)
	}
	e.wantKind("SetTenantState, reason one byte over",
		e.s.SetTenantState(e.ctx, t.ID, master.StateFailed, strings.Repeat("r", master.MaxStateReasonLength+1), e.audit(), master.LocalOperator()), fault.Invalid)

	k := e.key()
	k.ID = strings.Repeat("K", master.MaxIDLength)
	e.ok("CreateKey, id at its bound", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	k2 := e.key()
	k2.ID = k.ID + "K"
	e.wantKind("CreateKey, id one byte over", e.s.CreateKey(e.ctx, k2, e.audit(), master.LocalOperator()), fault.Invalid)

	name := strings.Repeat("s", master.MaxSettingNameLength)
	e.ok("SetSetting, name at its bound", e.s.SetSetting(e.ctx, name, "v", e.audit(), master.LocalOperator()))
	e.wantKind("SetSetting, name one byte over", e.s.SetSetting(e.ctx, name+"s", "v", e.audit(), master.LocalOperator()), fault.Invalid)

	a := e.audit()
	a.ID = strings.Repeat("A", master.MaxIDLength)
	a.Method = strings.Repeat("m", master.MaxAuditFieldLength)
	a.Object = strings.Repeat("o", master.MaxAuditFieldLength)
	a.RemoteAddress = strings.Repeat("r", master.MaxAuditFieldLength)
	a.RequestID = strings.Repeat("q", master.MaxAuditFieldLength)
	a.TenantName = strings.Repeat("t", master.MaxAuditFieldLength)
	a.LocalUser = strings.Repeat("u", master.MaxAuditFieldLength)
	a.Reason = strings.Repeat("x", master.MaxAuditReasonLength)
	e.ok("AppendAudit at every bound", e.s.AppendAudit(e.ctx, a))
	all := e.allAudit(master.AuditFilter{TenantName: a.TenantName})
	if len(all) != 1 || DiffAudit(all[0], a) != "" {
		e.Errorf("audit entry at every bound did not read back exactly")
	}
	fields := map[string]func(*master.AuditEntry){
		"id":             func(x *master.AuditEntry) { x.ID += "A" },
		"method":         func(x *master.AuditEntry) { x.Method += "m" },
		"object":         func(x *master.AuditEntry) { x.Object += "o" },
		"remote address": func(x *master.AuditEntry) { x.RemoteAddress += "r" },
		"request id":     func(x *master.AuditEntry) { x.RequestID += "q" },
		"tenant name":    func(x *master.AuditEntry) { x.TenantName += "t" },
		"local user":     func(x *master.AuditEntry) { x.LocalUser += "u" },
		"reason":         func(x *master.AuditEntry) { x.Reason += "x" },
	}
	for name, change := range fields {
		x := a
		if name != "id" {
			x.ID = e.id("a")
		}
		change(&x)
		e.wantKind("AppendAudit, "+name+" one byte over", e.s.AppendAudit(e.ctx, x), fault.Invalid)
	}
}
