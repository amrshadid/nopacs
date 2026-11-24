package mastertest

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/master"
)

// checkChurn lists with list, limit at a time, calling mutate between pages,
// which may insert and delete and returns the keys it deleted. Each item
// present before and never deleted must come back exactly once; no item may
// come back twice; and the keys must increase.
func checkChurn[T any](e *env, what string, list func(master.Page) ([]T, string, error), key func(T) string, mutate func(page int, seen []string) []string, limit int) {
	e.Helper()
	all, _, err := list(master.Page{Limit: master.MaxPageLimit})
	e.ok(what, err)
	before := map[string]bool{}
	for _, it := range all {
		before[key(it)] = true
	}
	deleted := map[string]bool{}
	seen := map[string]int{}
	var order []string
	page := master.Page{Limit: limit}
	for n := 0; n < 1000; n++ {
		items, next, err := list(page)
		e.ok(what, err)
		for _, it := range items {
			seen[key(it)]++
			order = append(order, key(it))
		}
		if next == "" {
			break
		}
		page.Cursor = next
		for _, d := range mutate(n, order) {
			deleted[d] = true
		}
	}
	for k := range before {
		if !deleted[k] && seen[k] != 1 {
			e.Errorf("%s: %q, present throughout, came back %d times", what, k, seen[k])
		}
	}
	for k, n := range seen {
		if n > 1 {
			e.Errorf("%s: %q came back %d times", what, k, n)
		}
	}
	for i := 1; i < len(order); i++ {
		if order[i] <= order[i-1] {
			e.Errorf("%s: %q after %q, out of order", what, order[i], order[i-1])
		}
	}
	if len(deleted) == 0 {
		e.Errorf("%s: nothing was deleted, so the test tested nothing", what)
	}
}

// pick returns, ahead, the largest of keys not yet seen and not yet used, or
// otherwise the smallest seen and not yet used, so that deletes land on both
// sides of the position reached. It returns "" when none is left.
func pick(keys []string, seen []string, used map[string]bool, ahead bool) string {
	s := map[string]bool{}
	for _, k := range seen {
		s[k] = true
	}
	sorted := slices.Clone(keys)
	sort.Strings(sorted)
	if ahead {
		slices.Reverse(sorted)
	}
	for _, k := range sorted {
		if !used[k] && s[k] != ahead {
			used[k] = true
			return k
		}
	}
	return ""
}

// testPagingWithDeletes guards every list that has a delete (tenants, grants
// by key, grants by scope, settings) against items removed ahead of and
// behind the position reached between pages. Keys and audit entries are
// never deleted, so their lists are paged across inserts only (elsewhere).
func testPagingWithDeletes(e *env) {
	// Tenants.
	var names []string
	ids := map[string]string{}
	for i := range 30 {
		t := e.mustTenant(fmt.Sprintf("m%02d", i))
		names = append(names, t.Name)
		ids[t.Name] = t.ID
	}
	used := map[string]bool{}
	checkChurn(e, "ListTenants", func(p master.Page) ([]master.Tenant, string, error) { return e.s.ListTenants(e.ctx, p) },
		func(t master.Tenant) string { return t.Name },
		func(n int, seen []string) []string {
			var del []string
			for _, ahead := range []bool{true, false} {
				if name := pick(names, seen, used, ahead); name != "" {
					e.ok("RemoveTenant", e.s.RemoveTenant(e.ctx, ids[name], e.audit(), master.LocalOperator()))
					del = append(del, name)
				}
			}
			e.mustTenant(fmt.Sprintf("n%02d", n))
			return del
		}, 4)

	// Grants by key and by scope.
	k := e.mustKey()
	tenant := e.mustTenant("shared")
	var scopes, keyIDs []string
	for i := range 20 {
		t := e.mustTenant(fmt.Sprintf("g%02d", i))
		e.mustGrant(k.ID, master.TenantScope(t.ID), master.RoleReader)
		scopes = append(scopes, t.ID)
		other := e.mustKey()
		e.mustGrant(other.ID, master.TenantScope(tenant.ID), master.RoleReader)
		keyIDs = append(keyIDs, other.ID)
	}
	checkChurnGrants := func(what string, list func(master.Page) ([]master.Grant, string, error), key func(master.Grant) string,
		pool []string, remove func(id string)) {
		used := map[string]bool{}
		checkChurn(e, what, list, key, func(_ int, seen []string) []string {
			var del []string
			for _, ahead := range []bool{true, false} {
				if id := pick(pool, seen, used, ahead); id != "" {
					remove(id)
					del = append(del, id)
				}
			}
			return del
		}, 3)
	}
	checkChurnGrants("GrantsByKey", func(p master.Page) ([]master.Grant, string, error) { return e.s.GrantsByKey(e.ctx, k.ID, p) },
		func(g master.Grant) string { return g.Scope.TenantID }, scopes,
		func(id string) {
			_, err := e.s.RemoveGrant(e.ctx, k.ID, master.TenantScope(id), e.audit(), master.LocalOperator())
			e.ok("RemoveGrant", err)
		})
	checkChurnGrants("GrantsByScope", func(p master.Page) ([]master.Grant, string, error) {
		return e.s.GrantsByScope(e.ctx, master.TenantScope(tenant.ID), p)
	}, func(g master.Grant) string { return g.KeyID }, keyIDs,
		func(id string) {
			_, err := e.s.RemoveGrant(e.ctx, id, master.TenantScope(tenant.ID), e.audit(), master.LocalOperator())
			e.ok("RemoveGrant", err)
		})

	// Settings.
	var settings []string
	for i := range 30 {
		n := fmt.Sprintf("m.%02d", i)
		e.ok("SetSetting", e.s.SetSetting(e.ctx, n, "v", e.audit(), master.LocalOperator()))
		settings = append(settings, n)
	}
	used = map[string]bool{}
	checkChurn(e, "ListSettings", func(p master.Page) ([]master.Setting, string, error) { return e.s.ListSettings(e.ctx, p) },
		func(s master.Setting) string { return s.Name },
		func(n int, seen []string) []string {
			var del []string
			for _, ahead := range []bool{true, false} {
				if name := pick(settings, seen, used, ahead); name != "" {
					_, err := e.s.UnsetSetting(e.ctx, name, e.audit(), master.LocalOperator())
					e.ok("UnsetSetting", err)
					del = append(del, name)
				}
			}
			e.ok("SetSetting", e.s.SetSetting(e.ctx, fmt.Sprintf("n.%02d", n), "v", e.audit(), master.LocalOperator()))
			return del
		}, 4)
}

// testTies guards the tie-breaker of the two lists sorted by time: keys of
// one creation time come back by id, ascending, and audit entries of one
// time by id, descending, also when a page ends inside the tie.
func testTies(e *env) {
	ids := []string{"k5", "k1", "k4", "k2", "k3"}
	for _, id := range ids {
		k := e.key()
		k.ID = id
		e.ok("CreateKey", e.s.CreateKey(e.ctx, k, e.audit(), master.LocalOperator()))
	}
	var got []string
	page := master.Page{Limit: 2}
	for {
		keys, next, err := e.s.ListKeys(e.ctx, page)
		e.ok("ListKeys", err)
		for _, k := range keys {
			if strings.HasPrefix(k.ID, "k") && len(k.ID) == 2 {
				got = append(got, k.ID)
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}
	if want := []string{"k1", "k2", "k3", "k4", "k5"}; !slices.Equal(got, want) {
		e.Errorf("keys of one time in order %q, want %q", got, want)
	}
	tie := T0.Add(time.Hour)
	for _, id := range []string{"t3", "t1", "t5", "t2", "t4"} {
		a := e.audit()
		a.ID, a.Time = id, tie
		e.ok("AppendAudit", e.s.AppendAudit(e.ctx, a))
	}
	got = got[:0]
	page = master.Page{Limit: 2}
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
