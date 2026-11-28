package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// markAction is the key of a catalog list that marks the row under the cursor
// for a batch delete, or unmarks it. ref says the subject, then the session and
// scan when they apply.
func markAction(tenant string, ref func(any) []string) action {
	return action{key: "x", help: "mark", needsRow: true, plain: true, do: func(c *ctx, _ screen, r any) tea.Cmd {
		path := strings.Join(ref(r), "/")
		if c.marked.toggle(tenant, path) {
			return say(path + " marked for a batch delete (" + itoa(int64(len(c.marked.marked(tenant)))) + " marked); b starts it")
		}
		return say(path + " unmarked (" + itoa(int64(len(c.marked.marked(tenant)))) + " marked)")
	}}
}

// batchAction is the key that opens the batch delete form over the rows
// marked in the catalog.
func batchAction(tenant string) action {
	return action{key: "b", help: "batch delete", plain: true, do: func(c *ctx, _ screen, _ any) tea.Cmd {
		return push(newBatchForm(c, tenant), nil)
	}}
}

// newBatchForm asks which targets to delete and why: the rows marked in the
// catalog and any typed here, or the scans a catalog filter selects. Its answer
// is the preview, which says what each target holds back; nothing is deleted
// until it is confirmed there.
func newBatchForm(c *ctx, tenant string) screen {
	marked := c.marked.marked(tenant)
	f := formScreen{
		base:    base{c.newID()},
		heading: "Delete a batch in " + clean(tenant),
		intro: []string{
			"Name the targets: " + itoa(int64(len(marked))) + " marked in the catalog (x on a row) are included, and more may be typed; or give a filter instead.",
			"The next screen shows each target with its holds, the time a retention rule keeps it until, its replicas and where it was sent. Nothing is deleted until you confirm it there.",
			"A target a hold covers, a rule keeps, or that changed since the preview is skipped with its reason, never forced.",
		},
		fields: []formField{
			{label: "More targets", hint: "subject[/session[/scan]]; subject[/session[/scan]]"},
			{label: "Filter", hint: "instead of targets: modality=MR description-prefix=T1 receiver=R archived-since=2024-01-01 archived-until=2025-01-01"},
			{label: "Reason", hint: "why, kept with each delete"},
		},
		verb:    "preview",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		if f.value(2) == "" {
			return nil, fault.New(fault.Invalid, "say why the data is deleted")
		}
		filter, err := parseScanFilter(f.value(1), c.loc)
		if err != nil {
			return nil, err
		}
		refs := append([]string{}, marked...)
		for _, s := range strings.Split(f.value(0), ";") {
			if s = strings.TrimSpace(s); s != "" {
				refs = append(refs, s)
			}
		}
		switch {
		case filter != nil && len(refs) > 0:
			return nil, fault.New(fault.Invalid, "name the targets or give a filter, one of the two")
		case filter == nil && len(refs) == 0:
			return nil, fault.New(fault.Invalid, "name the targets (mark rows in the catalog with x, or type them) or give a filter")
		}
		var targets []*v1.CorrectionTarget
		for _, r := range refs {
			t, err := parseTarget(r, "")
			if err != nil {
				return nil, err
			}
			targets = append(targets, t)
		}
		return previewBatch(c, f.id, tenant, f.value(2), targets, filter), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(batchPreviewMsg)
		if !ok {
			return nil, nil, false
		}
		return newBatchPreview(c, m), nil, true
	}
	return f
}

// newBatchPreview shows what a batch delete would remove, with the key that
// goes on to the confirmation.
func newBatchPreview(c *ctx, m batchPreviewMsg) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Delete " + itoa(int64(len(m.prev.GetItems()))) + " targets of " + clean(m.req.GetTenant()), ref: m, pairs: batchPreviewLines}
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, _ screen, _ any) tea.Cmd {
		if m.prev.GetConfirm() == "" {
			return nil
		}
		return push(newBatchConfirm(c, m), nil)
	}}}
	return d
}

// newBatchConfirm asks for the tenant's name to be typed, then starts the
// batch with the preview's token and follows its job.
func newBatchConfirm(c *ctx, m batchPreviewMsg) screen {
	tenant := m.req.GetTenant()
	f := formScreen{
		base:    base{c.newID()},
		heading: "Delete " + itoa(int64(len(m.prev.GetItems()))) + " targets of " + clean(tenant),
		intro: []string{
			"This starts a job that deletes each target in turn: " + itoa(m.prev.GetCounts().GetInstances()) + " instances hidden from every read, their bytes purged after the grace period.",
			"Each delete is undone and purged as any delete. The job can be paused, resumed and canceled; one batch runs per tenant at a time.",
		},
		fields:   []formField{{label: "Type the tenant", hint: tenant}},
		want:     tenant,
		confirms: true,
		verb:     "delete",
		pop:      2,
		refusal:  true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return startBatch(c, f.id, m.req, m.prev.GetConfirm()), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		started, ok := msg.(batchStartedMsg)
		if !ok {
			return nil, nil, false
		}
		c.marked.clear()
		job, cmd := newJobDetail(c, tenant, started.job.GetId())
		return f, backThen(f.pop, "batch job "+started.job.GetId()+" started: "+itoa(started.job.GetDeleteBatch().GetTargets())+" targets, deleted one at a time",
			push(job, cmd)), true
	}
	return f
}

// newBatchTargets makes the list of what became of each target of a batch
// delete job: pending, deleted, or skipped and why, with the delete made for it.
// It refreshes itself on a timer, so a target's outcome shows as it is reached.
func newBatchTargets(c *ctx, tenant, job string) (screen, tea.Cmd) {
	l := newList(c, "Targets of job "+clean(job), []column{{"#", 1, 6}, {"TARGET", 8, 60}, {"OUTCOME", 7, 18}, {"CORRECTION", 10, 30}, {"INSTANCES", 9, 10}},
		"the job has no target")
	l.live = true
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return batchTargetsPage(c, l.id, l.gen, token, tenant, job)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}
