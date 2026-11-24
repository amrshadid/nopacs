package mastertest

import (
	"fmt"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// testAudit guards that an entry reads back exactly, with Unicode and every
// field at its bound, that one may name a tenant that has no row, that an
// id is unique, and that a batch is written all or none.
func testAudit(e *env) {
	full := master.AuditEntry{
		ID:            e.id("a"),
		Time:          T0.Add(time.Second),
		KeyID:         "K1234567890a",
		RemoteAddress: "[2001:db8::1]:51234",
		Method:        "/nopacs.v1.AdminService/CreateTenant",
		TenantName:    "never-existed",
		Object:        "tenant/never-existed",
		Outcome:       master.OutcomeRefused,
		Reason:        strings.Repeat("ü", master.MaxAuditReasonLength/2),
		Details:       map[string]string{},
		RequestID:     "req-0001",
	}
	for i := range master.MaxAuditDetails {
		full.Details[fmt.Sprintf("key-%02d-%s", i, strings.Repeat("k", master.MaxAuditDetailKeyLength-7))] =
			strings.Repeat("値", master.MaxAuditDetailValueLength/3)
	}
	local := e.audit()
	local.Details = nil
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, full, local))
	got := e.allAudit(master.AuditFilter{})
	if len(got) != 2 {
		e.Fatalf("%d entries, want 2", len(got))
	}
	if d := DiffAudit(got[0], full); d != "" {
		e.Errorf("entry: %s", d)
	}
	if d := DiffAudit(got[1], local); d != "" {
		e.Errorf("entry: %s", d)
	}
	byTenant := e.allAudit(master.AuditFilter{TenantName: "never-existed"})
	if len(byTenant) != 1 || byTenant[0].ID != full.ID {
		e.Errorf("entries for a tenant with no row = %d, want the one", len(byTenant))
	}

	e.wantDuplicateAudit("AppendAudit, id taken", e.s.AppendAudit(e.ctx, full))
	fresh := e.audit()
	e.wantDuplicateAudit("AppendAudit, batch with an id taken", e.s.AppendAudit(e.ctx, fresh, full))
	if e.auditIDs()[fresh.ID] {
		e.Error("the batch's first entry was written although the batch failed")
	}
	tooMany := e.audit()
	tooMany.Details = map[string]string{}
	for i := range master.MaxAuditDetails + 1 {
		tooMany.Details[fmt.Sprint(i)] = "v"
	}
	e.wantKind("AppendAudit, 17 details", e.s.AppendAudit(e.ctx, tooMany), fault.Invalid)
	e.ok("AppendAudit, nothing", e.s.AppendAudit(e.ctx))
	batch := make([]master.AuditEntry, master.MaxAuditBatch+1)
	for i := range batch {
		batch[i] = e.audit()
	}
	e.wantKind("AppendAudit, too many", e.s.AppendAudit(e.ctx, batch...), fault.Invalid)
}

// testAuditFilters guards each filter of the audit listing, alone and
// together, and the order newest first with the id breaking ties.
func testAuditFilters(e *env) {
	var all []master.AuditEntry
	tenants := []string{"lab", "clinic", ""}
	keys := []string{"KA", "KB", ""}
	outcomes := []master.Outcome{master.OutcomeDone, master.OutcomeRefused, master.OutcomeFailed}
	for i := range 90 {
		a := e.audit()
		a.Time = T0.Add(time.Duration(i/2) * time.Second) // pairs share a time
		a.TenantName = tenants[i%3]
		a.KeyID, a.LocalUser = keys[(i/3)%3], ""
		a.Outcome = outcomes[(i/9)%3]
		all = append(all, a)
	}
	e.ok("AppendAudit", e.s.AppendAudit(e.ctx, all...))

	filters := []master.AuditFilter{
		{},
		{TenantName: "lab"},
		{KeyID: "KB"},
		{Outcome: master.OutcomeRefused},
		{Since: T0.Add(10 * time.Second)},
		{Until: T0.Add(20 * time.Second)},
		{Since: T0.Add(5 * time.Second), Until: T0.Add(25 * time.Second)},
		{TenantName: "clinic", KeyID: "KA"},
		{TenantName: "lab", Outcome: master.OutcomeFailed, Since: T0.Add(30 * time.Second)},
		{KeyID: "KA", Since: T0.Add(3 * time.Second), Until: T0.Add(40 * time.Second)},
		{TenantName: "nobody"},
	}
	for _, f := range filters {
		var want []master.AuditEntry
		for i := len(all) - 1; i >= 0; i-- {
			a := all[i]
			if (f.TenantName == "" || a.TenantName == f.TenantName) &&
				(f.KeyID == "" || a.KeyID == f.KeyID) &&
				(f.Outcome == "" || a.Outcome == f.Outcome) &&
				(f.Since.IsZero() || !a.Time.Before(f.Since)) &&
				(f.Until.IsZero() || a.Time.Before(f.Until)) {
				want = append(want, a)
			}
		}
		// Newest first; within a time, the larger id first. The ids grow
		// with i, so walking all backward is already that order.
		var got []master.AuditEntry
		page := master.Page{Limit: 7}
		for {
			entries, next, err := e.s.ListAudit(e.ctx, f, page)
			e.ok("ListAudit", err)
			got = append(got, entries...)
			if next == "" {
				break
			}
			page.Cursor = next
		}
		if len(got) != len(want) {
			e.Errorf("filter %+v: %d entries, want %d", f, len(got), len(want))
			continue
		}
		for i := range want {
			if got[i].ID != want[i].ID {
				e.Errorf("filter %+v: entry %d is %s, want %s", f, i, got[i].ID, want[i].ID)
				break
			}
		}
	}
	_, _, err := e.s.ListAudit(e.ctx, master.AuditFilter{Since: T0, Until: T0}, master.Page{Limit: 5})
	e.wantKind("ListAudit, empty range", err, fault.Invalid)
}

// testAuditPaging10000 guards that a listing of 10,000 entries, paged, with
// entries inserted between pages before, after and at the position reached,
// returns each of the 10,000 exactly once, in order.
func testAuditPaging10000(e *env) {
	const n = 10000
	originals := make([]master.AuditEntry, n)
	for i := range originals {
		a := e.audit()
		a.ID = fmt.Sprintf("o%05d", i)
		a.Time = T0.Add(time.Duration(i/3) * time.Millisecond) // threes share a time
		originals[i] = a
	}
	e.ok("AppendAudit 10,000", e.s.AppendAudit(e.ctx, originals...))

	seen := map[string]int{}
	var order []master.AuditEntry
	page := master.Page{Limit: 97}
	pages := 0
	for {
		entries, next, err := e.s.ListAudit(e.ctx, master.AuditFilter{}, page)
		e.ok("ListAudit", err)
		pages++
		for _, a := range entries {
			seen[a.ID]++
		}
		order = append(order, entries...)
		if next == "" {
			break
		}
		page.Cursor = next
		// Between pages: one entry newer than all, one older than all, and
		// one at the exact time of the last entry returned, with ids on
		// both sides of it.
		last := entries[len(entries)-1]
		ins := []master.AuditEntry{e.audit(), e.audit(), e.audit(), e.audit()}
		ins[0].ID, ins[0].Time = fmt.Sprintf("new-%04d", pages), T0.Add(time.Hour)
		ins[1].ID, ins[1].Time = fmt.Sprintf("old-%04d", pages), T0.Add(-time.Hour)
		ins[2].ID, ins[2].Time = last.ID+"-after", last.Time
		ins[3].ID, ins[3].Time = "a"+fmt.Sprintf("%04d", pages), last.Time
		e.ok("AppendAudit between pages", e.s.AppendAudit(e.ctx, ins...))
		if pages > 2*n {
			e.Fatal("the listing does not end")
		}
	}
	for _, a := range originals {
		if seen[a.ID] != 1 {
			e.Fatalf("entry %s returned %d times, want once", a.ID, seen[a.ID])
		}
	}
	for id, count := range seen {
		if count != 1 {
			e.Errorf("entry %s returned %d times", id, count)
		}
	}
	for i := 1; i < len(order); i++ {
		p, c := order[i-1], order[i]
		if c.Time.After(p.Time) || (c.Time.Equal(p.Time) && c.ID >= p.ID) {
			e.Fatalf("entry %d (%s at %v) is not before entry %d (%s at %v)", i, c.ID, c.Time, i-1, p.ID, p.Time)
		}
	}
	e.Logf("10,000 entries in %d pages, %d entries returned with the ones inserted", pages, len(order))
}
