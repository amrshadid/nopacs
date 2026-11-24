package tenanttest

import (
	"fmt"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// testPagingWithDeletes guards the settings list against settings removed
// ahead of and behind the position reached between pages, with others
// added: each setting present throughout comes back exactly once, none
// twice, in order. Audit entries are never deleted, so their list is paged
// across inserts only (testAuditPaging10000).
func testPagingWithDeletes(e *env) {
	var names []string
	for i := range 30 {
		n := fmt.Sprintf("m.%02d", i)
		e.ok("SetSetting", e.s.SetSetting(e.ctx, n, "v", e.audit(), nil))
		names = append(names, n)
	}
	deleted := map[string]bool{}
	seen := map[string]int{}
	var order []string
	remove := func(n string) {
		_, err := e.s.UnsetSetting(e.ctx, n, e.audit(), nil)
		e.ok("UnsetSetting", err)
		deleted[n] = true
	}
	page := master.Page{Limit: 4}
	for round := 0; round < 100; round++ {
		items, next, err := e.s.ListSettings(e.ctx, page)
		e.ok("ListSettings", err)
		for _, it := range items {
			seen[it.Name]++
			order = append(order, it.Name)
		}
		if next == "" {
			break
		}
		page.Cursor = next
		// One behind the position reached and one ahead of it.
		for _, n := range order {
			if !deleted[n] {
				remove(n)
				break
			}
		}
		for i := len(names) - 1; i >= 0; i-- {
			if n := names[i]; !deleted[n] && seen[n] == 0 {
				remove(n)
				break
			}
		}
		e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("n.%02d", round), "v", e.audit(), nil))
	}
	for _, n := range names {
		if !deleted[n] && seen[n] != 1 {
			e.Errorf("%q, present throughout, came back %d times", n, seen[n])
		}
	}
	for n, c := range seen {
		if c > 1 {
			e.Errorf("%q came back %d times", n, c)
		}
	}
	if !slices.IsSorted(order) {
		e.Errorf("settings out of order: %q", order)
	}
	if len(deleted) < 4 {
		e.Errorf("only %d settings were deleted, so the test tested little", len(deleted))
	}
}

// testTies guards the tie-breaker of the audit list: entries of one time
// come back by id, descending, also when a page ends inside the tie.
func testTies(e *env) {
	tie := T0.Add(time.Hour)
	for _, id := range []string{"t3", "t1", "t5", "t2", "t4"} {
		a := e.audit()
		a.ID, a.Time = id, tie
		e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	}
	var got []string
	page := master.Page{Limit: 2}
	for {
		entries, next, err := e.s.ListAudit(e.ctx, master.AuditFilter{Since: tie}, page)
		e.ok("ListAudit", err)
		for _, a := range entries {
			got = append(got, a.ID)
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if want := []string{"t5", "t4", "t3", "t2", "t1"}; !slices.Equal(got, want) {
		e.Errorf("audit entries of one time in order %q, want %q", got, want)
	}
}
