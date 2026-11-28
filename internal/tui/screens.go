package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// This file builds the screens: for each, its columns, what a page of it asks
// for, and what its keys open. Every function returns the screen and the
// command that loads it; none of them calls the API.

// installationScope is the scope of the installation itself.
func installationScope() *v1.Scope {
	return &v1.Scope{Kind: &v1.Scope_Installation{Installation: &v1.Installation{}}}
}

// tenantScope is the scope of one tenant.
func tenantScope(name string) *v1.Scope {
	return &v1.Scope{Kind: &v1.Scope_Tenant{Tenant: name}}
}

// scopeFromText reads what a person typed for a scope: the word installation,
// or a tenant's name.
func scopeFromText(s string) *v1.Scope {
	if strings.EqualFold(s, "installation") {
		return installationScope()
	}
	return tenantScope(s)
}

// scopeName words a scope for a heading.
func scopeName(sc *v1.Scope) string {
	if t := sc.GetTenant(); t != "" {
		return "tenant " + clean(t)
	}
	return "the installation"
}

// listDate is a time as a date in the person's zone, for a list, where a cell
// has little room; a detail screen shows the whole time with its offset.
func listDate(ts *timestamppb.Timestamp, loc *time.Location, absent string) string {
	if ts == nil {
		return absent
	}
	return ts.AsTime().In(loc).Format("2006-01-02")
}

// listTime is a time to the second in the person's zone, for the audit log.
func listTime(ts *timestamppb.Timestamp, loc *time.Location) string {
	if ts == nil {
		return "-"
	}
	return ts.AsTime().In(loc).Format("2006-01-02 15:04:05")
}

// newList makes the common part of every list.
func newList(c *ctx, heading string, cols []column, empty string) listScreen {
	return listScreen{base: base{c.newID()}, heading: heading, cols: cols, empty: empty}
}

// ---- tenants ----

// newTenantList makes the list of tenants. It refreshes itself on a timer, so
// a tenant whose database goes down shows its state and reason without a key
// being pressed.
func newTenantList(c *ctx) (screen, tea.Cmd) {
	l := newList(c, "Tenants", []column{{"NAME", 8, 40}, {"DB", 2, 8}, {"STATE", 8, 40}, {"CREATED", 19, 19}},
		"no tenant yet; \"nopacs tenant create\" makes one")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return tenantsPage(c, l.id, l.gen, token) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		t, _ := r.ref.(*v1.Tenant)
		// The list leaves out the storage quota, which only a read of one tenant
		// carries.
		s, cmd := newTenantDetail(c, t, true)
		return push(s, cmd)
	}
	l.note = func(r row) string {
		t, _ := r.ref.(*v1.Tenant)
		if t.GetState() == v1.TenantState_TENANT_STATE_READY {
			return ""
		}
		return t.GetName() + " is " + output.State(t.GetState()) + ": " + t.GetStateReason()
	}
	if c.acc.onInstallation(v1.Role_ROLE_ADMIN) {
		l.actions = []action{{key: "d", help: "remove", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			t, _ := ref.(*v1.Tenant)
			s, cmd := newRemoveTenant(c, t.GetName(), 1)
			return push(s, cmd)
		}}}
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newTenantDetail makes the screen of one tenant: its registration and state,
// with a key for each of its grants, settings and audit log. fetch is true when
// only the name is known and the rest must be asked for.
func newTenantDetail(c *ctx, t *v1.Tenant, fetch bool) (screen, tea.Cmd) {
	d := detailScreen{base: base{c.newID()}, heading: "Tenant " + clean(t.GetName()), ref: t}
	d.pairs = func(c *ctx, ref any) []pair {
		t, _ := ref.(*v1.Tenant)
		ps := []pair{
			{"Name", t.GetName()},
			{"Database", output.Database(t.GetDatabaseType())},
			{"State", output.State(t.GetState())},
		}
		if t.GetStateReason() != "" {
			ps = append(ps, pair{"Reason", t.GetStateReason()})
		}
		ps = append(ps, pair{"Created", output.Time(t.GetCreateTime(), c.loc)})
		return append(ps, quotaPairs(t.GetQuota())...)
	}
	d.refetch = func(c *ctx, id int, ref any) tea.Cmd {
		t, _ := ref.(*v1.Tenant)
		return getTenant(c, id, t.GetName())
	}
	d.actions = []action{
		{key: "g", help: "grants", need: v1.Role_ROLE_ADMIN, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			t, _ := ref.(*v1.Tenant)
			s, cmd := newGrantList(c, grantFilter{scope: tenantScope(t.GetName())})
			return push(s, cmd)
		}},
		{key: "s", help: "settings", need: v1.Role_ROLE_READER, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			t, _ := ref.(*v1.Tenant)
			s, cmd := newSettingList(c, tenantScope(t.GetName()))
			return push(s, cmd)
		}},
		{key: "a", help: "audit", need: v1.Role_ROLE_ADMIN, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			t, _ := ref.(*v1.Tenant)
			s, cmd := newAuditList(c, tenantScope(t.GetName()))
			return push(s, cmd)
		}},
		{key: "d", help: "remove", installation: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			t, _ := ref.(*v1.Tenant)
			s, cmd := newRemoveTenant(c, t.GetName(), 2)
			return push(s, cmd)
		}},
	}
	d.actions = append(d.actions, tenantActions(c, t.GetName())...)
	d.actions = allowed(c, t.GetName(), d.actions)
	if fetch {
		d.loading = true
		return d, d.refetch(c, d.id, t)
	}
	return d, nil
}

// quotaPairs are the labeled lines of a tenant's storage quota: its use against
// its bounds, what its purges will give back, and its warning; none when the
// server sent no quota.
func quotaPairs(q *v1.TenantQuota) []pair {
	if q == nil {
		return nil
	}
	ps := []pair{{"Quota", output.Quota(q)}}
	if purge := output.QuotaPurge(q); purge != "" {
		ps = append(ps, pair{"To purge", purge})
	}
	if warn := output.QuotaWarning(q); warn != "" {
		ps = append(ps, pair{"Quota warning", warn})
	}
	return ps
}

// newRemoveTenant asks for the tenant's name to be typed before it is
// unregistered, and says what that does and does not do.
func newRemoveTenant(c *ctx, name string, pop int) (screen, tea.Cmd) {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Remove tenant " + clean(name),
		intro: []string{
			"This unregisters tenant " + name + " from the installation.",
			"Nothing is deleted: its database and files stay where they are, and the program says where.",
			"Keys that were granted a role on it keep the grant on the record.",
		},
		fields:   []formField{{label: "Type the name", hint: name}},
		want:     name,
		confirms: true,
		verb:     "remove",
		pop:      pop,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return removeTenant(c, f.id, name), nil }
	return f, nil
}

// ---- keys ----

// ---- grants ----

// ---- settings ----

// ---- audit ----
