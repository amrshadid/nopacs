package tui

import (
	tea "charm.land/bubbletea/v2"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// roleChoices are the four roles a grant can give, in the order of the
// contract.
var roleChoices = []string{"reader", "writer", "operator", "admin"}

// roleFromChoice is the role a choice word stands for.
func roleFromChoice(word string) v1.Role {
	switch word {
	case "reader":
		return v1.Role_ROLE_READER
	case "writer":
		return v1.Role_ROLE_WRITER
	case "operator":
		return v1.Role_ROLE_OPERATOR
	case "admin":
		return v1.Role_ROLE_ADMIN
	}
	return v1.Role_ROLE_UNSPECIFIED
}

// newGrantList makes the list of the grants of a key or on a scope.
func newGrantList(c *ctx, f grantFilter) (screen, tea.Cmd) {
	heading := "Grants of key " + clean(f.keyID)
	if f.scope != nil {
		heading = "Grants on " + scopeName(f.scope)
	}
	l := newList(c, heading, []column{{"KEY", 12, 14}, {"SCOPE", 10, 40}, {"ROLE", 4, 8}}, "no grant")
	l.params = f
	l.fetch = func(c *ctx, l listScreen, token string) tea.Cmd {
		return grantsPage(c, l.id, l.gen, token, l.params.(grantFilter))
	}
	if f.scope != nil && c.acc.onInstallation(v1.Role_ROLE_ADMIN) {
		// On a tenant's grants, enter opens the key that holds the grant,
		// which is where its holder is shown. Only an admin on the
		// installation may read a key; a tenant's admin sees the grant.
		l.open = func(c *ctx, _ listScreen, r row) tea.Cmd {
			g, _ := r.ref.(*v1.Grant)
			s, cmd := newKeyDetail(c, &v1.Key{Id: g.GetKeyId()}, true)
			return push(s, cmd)
		}
	}
	l.actions = []action{
		{key: "s", help: "set a role", do: func(c *ctx, _ screen, _ any) tea.Cmd {
			s, cmd := newSetGrant(c, f)
			return push(s, cmd)
		}},
		{key: "d", help: "remove", needsRow: true, do: func(c *ctx, _ screen, ref any) tea.Cmd {
			g, _ := ref.(*v1.Grant)
			s, cmd := newRemoveGrant(c, g)
			return push(s, cmd)
		}},
	}
	nl, cmd := l.start(c)
	return nl, cmd
}

// newSetGrant makes the form that gives a key a role: the scope is asked for
// when the list is a key's, and the key when it is a scope's.
func newSetGrant(c *ctx, filter grantFilter) (screen, tea.Cmd) {
	first := formField{label: "Key id", hint: "the id from the key list, not the key itself", keyID: true}
	heading := "Set a role on " + scopeName(filter.scope)
	if filter.scope == nil {
		first = formField{label: "Scope", hint: "the word installation, or a tenant's name", in: field{}.set("installation")}
		heading = "Set a role for key " + clean(filter.keyID)
	}
	f := formScreen{
		base:    base{c.newID()},
		heading: heading,
		fields:  []formField{first, {label: "Role", choices: roleChoices}},
		verb:    "set the role",
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		keyID, scope := filter.keyID, filter.scope
		if scope == nil {
			if f.value(0) == "" {
				return nil, fault.New(fault.Invalid, "the scope is required")
			}
			scope = scopeFromText(f.value(0))
		} else {
			keyID = f.value(0)
			if err := output.CheckKeyID(keyID); err != nil {
				return nil, err
			}
		}
		return setGrant(c, f.id, keyID, scope, roleFromChoice(f.value(1))), nil
	}
	return f, nil
}

// newRemoveGrant asks for the key's id to be typed before a grant is taken
// back.
func newRemoveGrant(c *ctx, g *v1.Grant) (screen, tea.Cmd) {
	f := formScreen{
		base:    base{c.newID()},
		heading: "Remove a grant",
		intro: []string{
			"Take away the " + output.Role(g.GetRole()) + " role of key " + g.GetKeyId() + " on " + output.Scope(g.GetScope()) + ".",
			"The key keeps working for whatever its other grants allow.",
		},
		fields: []formField{{label: "Type the key id", hint: g.GetKeyId()}},
		want:   g.GetKeyId(),
		verb:   "remove",
	}
	f.submit = func(c *ctx, f formScreen) (tea.Cmd, error) {
		return removeGrant(c, f.id, g.GetKeyId(), g.GetScope()), nil
	}
	return f, nil
}
