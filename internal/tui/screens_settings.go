package tui

import (
	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
)

// newSettingList makes the list of the settings set on a scope.
func newSettingList(c *ctx, scope *v1.Scope) (screen, tea.Cmd) {
	l := newList(c, "Settings of "+scopeName(scope), []column{{"NAME", 8, 40}, {"VALUE", 5, 60}, {"FROM", 4, 30}},
		"nothing is set here")
	l.params = scope
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return settingsPage(c, l.id, l.gen, token, l.params.(*v1.Scope))
	}
	l.actions = []action{{key: "s", help: "set", do: func(c *ctx, _ screen, _ any) tea.Cmd {
		s, cmd := newSetSetting(c, scope)
		return push(s, cmd)
	}}}
	// A value set on either scope can be unset: a tenant's override, or the
	// installation's value, after which the built-in default applies.
	l.actions = append(l.actions, action{key: "u", help: "unset", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
		st, _ := ref.(*v1.Setting)
		s, cmd := newUnsetSetting(c, st.GetName(), scope)
		return push(s, cmd)
	}})
	nl, cmd := l.start(c)
	return nl, cmd
}

// newSetSetting makes the form that sets a setting on a scope.
func newSetSetting(c *ctx, scope *v1.Scope) (screen, tea.Cmd) {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Set a setting on " + scopeName(scope),
		fields: []formField{
			{label: "Name", hint: "such as key.default-lifetime"},
			{label: "Value"},
		},
		verb: "set",
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		// The value may be empty: whether a setting takes an empty value is
		// the setting's own rule, which the server applies.
		if f.value(0) == "" {
			return nil, fault.New(fault.Invalid, "the name is required")
		}
		return setSetting(c, f.id, f.value(0), scope, f.value(1)), nil
	}
	return f, nil
}

// newUnsetSetting asks for the setting's name to be typed before the value
// set on a scope is removed.
func newUnsetSetting(c *ctx, name string, scope *v1.Scope) (screen, tea.Cmd) {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Unset " + clean(name),
		intro:   []string{unsetIntro(scope, name)},
		fields:  []formField{{label: "Type the name", hint: name}},
		want:    name,
		verb:    "unset",
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) { return unsetSetting(c, f.id, name, scope), nil }
	return f, nil
}

// unsetIntro says what unsetting name on scope does.
func unsetIntro(scope *v1.Scope, name string) string {
	if scope.GetTenant() != "" {
		return "Remove " + scopeName(scope) + "'s override of " + name + "; what applies on the installation applies again."
	}
	return "Remove the installation's value of " + name + "; the built-in default applies again."
}
