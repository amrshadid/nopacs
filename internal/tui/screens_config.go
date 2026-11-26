package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newFindingList makes the list of what scrubs found on a tenant's storages,
// every kind, held now. s starts a scrub of one storage or all of them, b a
// rebuild of the catalog from the storages; both ask first.
func newFindingList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Scrub findings of "+clean(tenant), []column{{"ID", 8, 26}, {"KIND", 6, 18}, {"STORAGE", 7, 40}, {"SCAN", 8, 60}, {"INSTANCE", 8, 26}, {"FOUND", 8, 19}},
		"no finding holds: every object read so far matched the catalog")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return findingsPage(c, l.id, l.gen, token, tenant, "", false)
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		f, _ := r.ref.(*v1.ScrubFinding)
		d := detailScreen{base: base{c.newID()}, heading: "Finding " + clean(f.GetId()), ref: f, pairs: findingPairs}
		return push(d, nil)
	}
	if c.acc.onTenant(tenant, v1.Role_ROLE_OPERATOR) {
		l.actions = []action{
			{key: "s", help: "scrub", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newScrubForm(c, tenant), nil) }},
			{key: "b", help: "rebuild", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newRebuildForm(c, tenant), nil) }},
		}
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// findingPairs are the lines of one scrub finding.
func findingPairs(c *ctx, ref any) []pair {
	f, _ := ref.(*v1.ScrubFinding)
	ps := []pair{
		{"Id", f.GetId()}, {"Kind", output.FindingKind(f.GetKind())}, {"Storage", f.GetStorage()},
		{"Scan", f.GetSubjectLabel() + "/" + f.GetSessionLabel() + "/" + f.GetScanLabel()}, {"Instance", output.Dash(f.GetInstanceId())},
		{"Revision", strconv.Itoa(int(f.GetRevision()))}, {"Found", output.Time(f.GetFindTime(), c.loc)},
		{"Found by job", output.Dash(f.GetJob())}, {"Cleared", output.Time(f.GetClearTime(), c.loc)},
	}
	if f.ExpectedSize != nil {
		ps = append(ps, pair{"Expected size", itoa(f.GetExpectedSize())})
	}
	if f.FoundSize != nil {
		ps = append(ps, pair{"Found size", itoa(f.GetFoundSize())})
	}
	if f.GetRepairJob() != "" {
		ps = append(ps, pair{"Repair job", f.GetRepairJob()})
	}
	return ps
}

// newScrubForm asks which storage to scrub; a scrub only reads.
func newScrubForm(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Scrub " + clean(tenant),
		intro: []string{
			"Every stored object is read again and compared with the catalog's length and SHA-256. A scrub only reads; what it finds is listed here.",
			"On an archive-tier storage objects are checked by presence, and none is recalled.",
		},
		fields: []formField{{label: "Storage", hint: "empty: every storage of the tenant"}},
		verb:   "scrub",
		pop:    1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return createScrubJob(c, f.id, tenant, f.value(0)), nil }
	return f
}

// newRebuildForm asks how to rebuild the catalog. Comparing changes nothing;
// replacing swaps the catalog in use, so it asks for the tenant's name.
func newRebuildForm(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Rebuild the catalog of " + clean(tenant),
		intro: []string{
			"The catalog is made again from the storages. compare builds a shadow catalog and lists the differences, and changes nothing;",
			"replace swaps the catalog in use for the rebuilt one, and needs the tenant's name typed.",
		},
		fields: []formField{
			{label: "Mode", choices: []string{"compare", "replace"}},
			{label: "From storage", hint: "compare only: build from this one storage alone"},
			{label: "Type the name", hint: tenant + " (only for replace)"},
		},
		verb: "rebuild",
		pop:  1,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		mode := v1.RebuildJobMode_REBUILD_JOB_MODE_COMPARE
		if f.value(0) == "replace" {
			mode = v1.RebuildJobMode_REBUILD_JOB_MODE_REPLACE
			if f.value(2) != tenant {
				return nil, fault.New(fault.Invalid, "not done: type "+tenant+" exactly, as shown, to replace the catalog")
			}
			if f.value(1) != "" {
				return nil, fault.New(fault.Invalid, "a replace builds from every storage: leave the storage empty")
			}
		}
		return createRebuildJob(c, f.id, tenant, mode, f.value(1)), nil
	}
	return f
}

// newProfileList makes the list of a tenant's de-identification profiles.
func newProfileList(c *ctx, tenant string) (screen, tea.Cmd) {
	l := newList(c, "Profiles of "+clean(tenant), []column{{"NAME", 8, 40}, {"LATEST", 6, 8}, {"CREATED", 8, 19}},
		"no profile yet; \"nopacs profile create\" makes one")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd { return profilesPage(c, l.id, l.gen, token, tenant) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		p, _ := r.ref.(*v1.ProfileSummary)
		d := detailScreen{base: base{c.newID()}, heading: "Profile " + clean(p.GetName()), ref: p, pairs: func(c *ctx, ref any) []pair {
			p, _ := ref.(*v1.ProfileSummary)
			return []pair{{"Name", p.GetName()}, {"Latest version", strconv.Itoa(int(p.GetLatestVersion()))}, {"Created", output.Time(p.GetLatestCreateTime(), c.loc)}}
		}}
		return push(d, nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newRouteList makes the list of a receiver's routes, in the order the
// receiver tries them.
func newRouteList(c *ctx, tenant, receiver string) (screen, tea.Cmd) {
	l := newList(c, "Routes of "+clean(receiver), []column{{"#", 1, 3}, {"ROUTE", 8, 40}, {"WHEN", 8, 20}, {"STORAGE", 7, 40}, {"PROFILE", 7, 40}},
		"no route: every study goes to the receiver's storage")
	l.fetch = func(c *ctx, l listScreen, _ string) tea.Cmd { return routesPage(c, l.id, l.gen, tenant, receiver) }
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		rt, _ := r.ref.(*v1.Route)
		d := detailScreen{base: base{c.newID()}, heading: "Route " + clean(rt.GetName()), ref: rt, pairs: func(c *ctx, ref any) []pair {
			rt, _ := ref.(*v1.Route)
			ps := []pair{{"Name", rt.GetName()}, {"Storage", output.Dash(rt.GetStorage())}}
			if p := rt.GetProfile(); p != nil {
				ps = append(ps, pair{"Profile", p.GetName() + "@" + strconv.Itoa(int(p.GetVersion()))})
			}
			ps = append(ps, pair{"Conditions", strconv.Itoa(len(rt.GetConditions())) + ", all of which must hold; \"nopacs receiver route list\" words them"})
			return ps
		}}
		return push(d, nil)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newSealVerify verifies a scope's audit log and shows what the verification
// found, an intact log or the problems.
func newSealVerify(c *ctx, scope *v1.Scope) (screen, tea.Cmd) {
	d := detailScreen{base: base{c.newID()}, heading: "Audit seal of " + scopeName(scope), ref: &v1.VerifyAuditResponse{}, loading: true, pairs: sealLines}
	d.refetch = func(c *ctx, id int, _ any) tea.Cmd { return verifySeal(c, id, scope) }
	s := sealScreen{detailScreen: d}
	return s, verifySeal(c, s.id, scope)
}

// sealScreen is a verification: the detail screen with the answer of the
// verifying call.
type sealScreen struct{ detailScreen }

// update takes the verification's answer; the rest is the detail screen's.
func (s sealScreen) update(c *ctx, msg tea.Msg) (screen, tea.Cmd) {
	if m, ok := msg.(sealVerifiedMsg); ok {
		s.ref, s.loading = m.res, false
		return s, nil
	}
	next, cmd := s.detailScreen.update(c, msg)
	if d, ok := next.(detailScreen); ok {
		s.detailScreen = d
		return s, cmd
	}
	return next, cmd
}
