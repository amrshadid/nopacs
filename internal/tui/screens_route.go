package tui

import (
	"strconv"

	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newRuleSetList makes the list of a receiver's rule set versions, the newest
// first, with the one it runs marked. A version opens on its rules; the keys
// activate one, explain what one decides for a file or an archived instance, try
// one on the latest sessions and count the receptions each rule routed.
// Creating a version is left to "nopacs route create": the program does not
// edit the JSON file a rule set is written in.
func newRuleSetList(c *ctx, tenant, receiver string) (screen, tea.Cmd) {
	l := newList(c, "Rule sets of "+clean(receiver), []column{{"VERSION", 7, 8}, {"RUNNING", 7, 7}, {"MADE", 8, 19}, {"BY", 6, 26}, {"ARCHIVE", 7, 8}, {"FORWARD", 7, 8}, {"COMMENT", 7, 60}},
		"no rule set yet; \"nopacs route create\" makes one from a JSON file: the program does not edit one")
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return ruleSetsPage(c, l.id, l.gen, token, tenant, receiver)
	}
	l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
		rs, _ := r.ref.(*v1.RuleSet)
		return push(newRuleSetDetail(c, tenant, rs), nil)
	}
	l.note = func(row) string {
		return "versions are made with \"nopacs route create\"; this screen reads, activates and tries them"
	}
	version := func(ref any) int32 {
		rs, _ := ref.(*v1.RuleSet)
		return rs.GetVersion()
	}
	l.actions = append(l.actions, action{key: "h", help: "hits", need: v1.Role_ROLE_READER, do: func(c *ctx, _ screen, ref any) tea.Cmd {
		s, cmd := newRouteHits(c, tenant, receiver, version(ref))
		return push(s, cmd)
	}})
	if c.acc.onTenant(tenant, v1.Role_ROLE_OPERATOR) {
		l.actions = append(l.actions,
			action{key: "x", help: "explain", do: func(c *ctx, _ screen, ref any) tea.Cmd {
				return push(newExplainForm(c, tenant, receiver, version(ref)), nil)
			}},
			action{key: "d", help: "dry run", do: func(c *ctx, _ screen, ref any) tea.Cmd {
				return push(newDryRunForm(c, tenant, receiver, version(ref)), nil)
			}})
	}
	if c.acc.onTenant(tenant, v1.Role_ROLE_ADMIN) {
		l.actions = append(l.actions,
			action{key: "a", help: "activate", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
				return push(newActivateForm(c, tenant, receiver, version(ref)), nil)
			}},
			action{key: "z", help: "run none", do: func(c *ctx, _ screen, _ any) tea.Cmd { return push(newActivateForm(c, tenant, receiver, 0), nil) }})
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newRuleSetDetail makes the screen of one rule set version: its archive rules
// in the order tried, what happens to a study none chooses, and its forward
// rules.
func newRuleSetDetail(c *ctx, tenant string, rs *v1.RuleSet) screen {
	d := detailScreen{base: base{c.newID()}, heading: "Rule set " + clean(rs.GetReceiver()) + "@" + strconv.Itoa(int(rs.GetVersion())), ref: rs, pairs: ruleSetLines}
	d.refetch = func(c *ctx, id int, ref any) tea.Cmd {
		rs, _ := ref.(*v1.RuleSet)
		return getRuleSet(c, id, tenant, rs.GetReceiver(), rs.GetVersion())
	}
	return d
}

// newActivateForm asks for the version's number to be typed, then makes the
// receiver run it, or none (0). It applies to receptions identified from then
// on; a session already made keeps where it went.
func newActivateForm(c *ctx, tenant, receiver string, version int32) screen {
	word := strconv.Itoa(int(version))
	heading := "Run rule set " + word + " on " + clean(receiver)
	about := "The receiver runs this version for receptions identified from now on; a session already made keeps where it went."
	if version == 0 {
		heading = "Run no rule set on " + clean(receiver)
		about = "The receiver then runs no rule set: every study goes as the receiver says."
	}
	f := formScreen{
		base:     base{c.newID()},
		heading:  heading,
		intro:    []string{about, "An archive rule that names a storage or a profile the tenant lacks refuses the activation."},
		fields:   []formField{{label: "Type the version", hint: word}},
		want:     word,
		confirms: true,
		verb:     "activate",
		pop:      1,
		refusal:  true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return activateRuleSet(c, f.id, tenant, receiver, version), nil
	}
	return f
}

// newExplainForm asks what to explain: an archived instance, or a DICOM file on
// this machine as the receiver would receive it. Values are hidden in the answer
// unless they are asked for, and asking is audited.
func newExplainForm(c *ctx, tenant, receiver string, version int32) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Explain rule set " + strconv.Itoa(int(version)) + " of " + clean(receiver),
		intro: []string{
			"Each rule of the version is tried on one file, and the answer says its result and the comparison that decided it.",
			"An archived header is de-identified, so a comparison on an attribute its profile removed is unknown. Give an instance or a file, one of the two.",
		},
		fields: []formField{
			{label: "Instance", hint: "subject/session/scan or subject/session/scan/uid: an archived instance"},
			{label: "File", hint: "or the path of a DICOM file, up to 64 MiB"},
			{label: "Calling AE", hint: "a file only: the AE title it is received from"},
			{label: "Called AE", hint: "a file only: the AE title it is received with"},
			{label: "Show values", choices: valuesChoices(c), hint: "yes: the values each comparison saw; recorded in the audit log, refused to a masked key"},
		},
		verb:    "explain",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		instance, file := f.value(0), f.value(1)
		if (instance == "") == (file == "") {
			return nil, fault.New(fault.Invalid, "give an instance or a file, one of the two")
		}
		req := &v1.ExplainRouteRequest{Tenant: tenant, Receiver: receiver, Version: version, CallingAeTitle: f.value(2), CalledAeTitle: f.value(3), ShowValues: f.value(4) == "yes"}
		if instance != "" {
			t, err := parseTarget(instance, "")
			if err != nil || t.GetScanLabel() == "" {
				return nil, fault.New(fault.Invalid, "an instance is subject/session/scan, with /uid for one instance")
			}
			req.Source = &v1.ExplainRouteRequest_Instance{Instance: &v1.RouteInstance{SubjectLabel: t.GetSubjectLabel(), SessionLabel: t.GetSessionLabel(), ScanLabel: t.GetScanLabel()}}
		}
		return explainRoute(c, f.id, req, file), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(explainMsg)
		if !ok {
			return nil, nil, false
		}
		d := detailScreen{base: base{c.newID()}, heading: "Explanation by " + clean(receiver) + "@" + strconv.Itoa(int(m.res.GetVersion())), ref: m, pairs: explainLines}
		return d, nil, true
	}
	return f
}

// newDryRunForm asks how many of the latest sessions a version is tried on; a
// dry run only reads.
func newDryRunForm(c *ctx, tenant, receiver string, version int32) screen {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Try rule set " + strconv.Itoa(int(version)) + " of " + clean(receiver),
		intro: []string{
			"The version is tried on the receiver's latest sessions, and each is shown with the rules that routed it beside those the version would choose. Nothing is changed.",
			"A session whose result depends on a value its profile removed is unknown, never counted as matching or not.",
		},
		fields:  []formField{filled("Sessions", "how many of the latest, 1 to 1000", "100")},
		verb:    "try",
		pop:     1,
		refusal: true,
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		n, err := wholeIn(f.value(0), "sessions", 1, 1000)
		if err != nil {
			return nil, err
		}
		return dryRunRoutes(c, f.id, tenant, receiver, version, n), nil
	}
	f.result = func(c *ctx, f formScreen, msg tea.Msg) (screen, tea.Cmd, bool) {
		m, ok := msg.(dryRunMsg)
		if !ok {
			return nil, nil, false
		}
		d := detailScreen{base: base{c.newID()}, heading: "Dry run of " + clean(receiver) + "@" + strconv.Itoa(int(m.res.GetVersion())), ref: m, pairs: dryRunLines}
		return d, nil, true
	}
	return f
}

// newRouteHits makes the list of the receptions each rule routed, from the
// record each reception keeps; "(default)" is a reception no archive rule chose.
func newRouteHits(c *ctx, tenant, receiver string, version int32) (screen, tea.Cmd) {
	l := newList(c, "Route hits of "+clean(receiver)+"@"+strconv.Itoa(int(version)), []column{{"VERSION", 7, 8}, {"PHASE", 5, 8}, {"RULE", 6, 40}, {"RECEPTIONS", 10, 12}, {"SERIES", 6, 12}, {"LAST", 8, 19}},
		"no rule routed a reception of this version")
	l.fetch = func(c *ctx, l listScreen, _ string) tea.Cmd {
		return routeHitsPage(c, l.id, l.gen, tenant, receiver, version)
	}
	nl, cmd := l.start(c)
	return nl, cmd
}
