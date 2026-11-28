package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newRecallForm asks which subject, session or scan to bring back from an
// archive tier. Its answer is the preview of the recall, which says how many
// objects it would ask the store for; recalling costs what the store charges, so
// nothing is asked until it is confirmed there.
func newRecallForm(c *ctx, tenant string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Recall from an archive tier in " + clean(tenant),
		intro: []string{
			"The archive-tier storages that hold the target are asked to bring its objects back, so later reads and jobs find them online. A read of an object that is not online starts no recall.",
			"The next screen shows what would be asked for. Days above a storage's own need admin on the tenant.",
		},
		fields: []formField{
			{label: "Target", hint: "subject[/session[/scan]]"},
			{label: "Storage", hint: "optional: one archive-tier storage; every one that holds the target when empty"},
			{label: "Days", hint: "optional: how long the objects stay readable, 1 to 365; each storage's own when empty"},
		},
		verb:    "preview",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		t, err := parseTarget(f.value(0), "")
		if err != nil {
			return nil, err
		}
		var days int32
		if f.value(2) != "" {
			if days, err = wholeIn(f.value(2), "days", 1, 365); err != nil {
				return nil, err
			}
		}
		req := &v1.RecallRequest{Tenant: tenant, SubjectLabel: t.GetSubjectLabel(), SessionLabel: t.GetSessionLabel(), ScanLabel: t.GetScanLabel(), Storage: f.value(1), Days: days}
		return previewRecall(c, f.id, req, f.value(0)), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(recallPreviewMsg)
		if !ok {
			return nil, nil, false
		}
		return newRecallPreview(c, m), nil, true
	}
	return f
}

// newRecallPreview shows what a recall would ask the store for, with the key
// that goes on to the confirmation when there is something to ask for.
func newRecallPreview(c *ctx, m recallPreviewMsg) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Recall " + clean(m.target), ref: m, pairs: recallLines}
	d.actions = []action{{key: "c", help: "confirm", do: func(c *ctx, _ screen, _ any) tea.Cmd {
		if m.res.GetConfirm() == "" || recallToAsk(m.res) <= 0 {
			return say("nothing to ask for: every object is online or being recalled already")
		}
		return push(newRecallConfirm(c, m), nil)
	}}}
	return d
}

// newRecallConfirm asks for the target to be typed again, then asks the store to
// recall, with the preview's token: when the objects to ask for changed since,
// the server refuses and the person previews again.
func newRecallConfirm(c *ctx, m recallPreviewMsg) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Recall " + clean(m.target),
		intro: []string{
			"This asks the store to recall " + itoa(recallToAsk(m.res)) + " objects, at its charge.",
		},
		fields:   []formField{{label: "Type the target", hint: m.target}},
		want:     m.target,
		confirms: true,
		verb:     "recall",
		pop:      2,
		refusal:  true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return recallObjects(c, f.id, m), nil }
	return f
}

// newVerifyForm asks how many objects of a storage to sample, and a seed to
// draw the same sample again.
func newVerifyForm(c *ctx, tenant, storage string) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Sample " + clean(storage),
		intro: []string{
			"A random sample of the objects the storage holds is listed with each one's key, size and SHA-256 as the catalog holds them, so a tool outside the server can read them, hash them and compare.",
			"The server reads no object itself. The keys hold identifiers as received: keep what you read with the care the archive needs.",
		},
		fields: []formField{
			filled("Count", "how many objects, 1 to 10000", "200"),
			{label: "Seed", hint: "optional: the same seed draws the same sample again"},
		},
		verb:    "sample",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		n, err := wholeIn(f.value(0), "count", 1, 10000)
		if err != nil {
			return nil, err
		}
		var seed *int64
		if f.value(1) != "" {
			v, err := strconv.ParseInt(f.value(1), 10, 64)
			if err != nil {
				return nil, fault.New(fault.Invalid, "the seed is a whole number")
			}
			seed = &v
		}
		start := startSampleMsg{count: n, seed: seed}
		return func() tea.Msg { return deliverMsg{to: f.id, msg: start} }, nil
	}
	f.result = func(c *ctx, _ formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(startSampleMsg)
		if !ok {
			return nil, nil, false
		}
		s, cmd := newSampleList(c, tenant, storage, m.count, m.seed)
		return s, cmd, true
	}
	return f
}

// startSampleMsg is what a sample's form hands the root once its fields are
// good: no call has been made yet, the list it opens makes it.
type startSampleMsg struct {
	count int32
	seed  *int64
}

// newSampleList makes the list of a sample of a storage's objects.
func newSampleList(c *ctx, tenant, storage string, count int32, seed *int64) (screen, tea.Cmd) {
	l := newList(c, "Sample of "+clean(storage), []column{{"KEY", 8, 70}, {"SIZE", 4, 12}, {"SHA-256", 8, 64}, {"REV", 3, 4}, {"SCAN", 8, 60}},
		"the storage holds no object to sample")
	l.fetch = func(c *ctx, l listScreen, _ string) tea.Cmd {
		return samplePage(c, l.id, l.gen, tenant, storage, count, seed)
	}
	l.note = func(row) string {
		return "keys hold identifiers as received: keep what you read here with the care the archive needs"
	}
	l.onMsg = func(c *ctx, l listScreen, msg tea.Msg) (listScreen, tea.Cmd, bool) {
		m, ok := msg.(sampleMsg)
		if !ok {
			return l, nil, false
		}
		l.heading = "Sample of " + clean(storage) + ": " + itoa(int64(len(m.page.rows))) + " of " + itoa(m.population) + " objects"
		next, cmd := l.page(c, m.page)
		nl, _ := next.(listScreen)
		return nl, cmd, true
	}
	nl, cmd := l.start(c)
	return nl, cmd
}
