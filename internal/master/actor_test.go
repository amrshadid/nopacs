package master

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestJudge guards the one statement of the rule every driver applies to
// what it read of a change's actor: which state of a key is refused, with
// which kind and reason, in which order the causes are told (a key that is
// gone or no good is Unauthenticated before a missing role is Denied), the
// exact moment of an expiry, and the actors that are never refused.
func TestJudge(t *testing.T) {
	exp := t0.Add(time.Hour)
	key := func(role Role, when time.Time) Actor { return ActingKey("k1", Installation, role, at(when)) }
	// none stands for no error: no kind is negative.
	none := fault.Kind(-1)
	for _, c := range []struct {
		name             string
		by               Actor
		found            bool
		revoked, expires time.Time
		held             Role
		kind             fault.Kind
		reason           string
	}{
		{"the local operator, whatever was read", LocalOperator(), false, t0, t0, "", none, ""},
		{"the server, whatever was read", Server(), false, t0, t0, "", none, ""},
		{"admin needing admin", key(RoleAdmin, t0), true, time.Time{}, exp, RoleAdmin, none, ""},
		{"admin needing reader", key(RoleReader, t0), true, time.Time{}, exp, RoleAdmin, none, ""},
		{"operator needing admin", key(RoleAdmin, t0), true, time.Time{}, exp, RoleOperator, fault.Denied, RefusedNoRole},
		{"no grant needing reader", key(RoleReader, t0), true, time.Time{}, exp, "", fault.Denied, RefusedNoRole},
		{"no grant, no role asked", key("", t0), true, time.Time{}, exp, "", none, ""},
		{"never expires", key(RoleAdmin, t0.Add(1000*time.Hour)), true, time.Time{}, time.Time{}, RoleAdmin, none, ""},
		{"a microsecond before the expiry", key(RoleAdmin, exp.Add(-time.Microsecond)), true, time.Time{}, exp, RoleAdmin, none, ""},
		{"at the expiry", key(RoleAdmin, exp), true, time.Time{}, exp, RoleAdmin, fault.Unauthenticated, RefusedExpired},
		{"revoked", key(RoleAdmin, t0), true, t0, exp, RoleAdmin, fault.Unauthenticated, RefusedRevoked},
		{"revoked and expired", key(RoleAdmin, exp), true, t0, exp, RoleAdmin, fault.Unauthenticated, RefusedRevoked},
		{"revoked, no grant", key(RoleAdmin, t0), true, t0, exp, "", fault.Unauthenticated, RefusedRevoked},
		{"expired, no grant", key(RoleAdmin, exp), true, time.Time{}, exp, "", fault.Unauthenticated, RefusedExpired},
		{"not found", key(RoleAdmin, t0), false, time.Time{}, time.Time{}, "", fault.Unauthenticated, RefusedUnknownKey},
	} {
		err := c.by.Judge(c.found, c.revoked, c.expires, c.held)
		if c.kind == none {
			if err != nil {
				t.Errorf("%s: %v, want nil", c.name, err)
			}
			continue
		}
		if fault.KindOf(err) != c.kind {
			t.Errorf("%s: kind %v (%v), want %v", c.name, fault.KindOf(err), err, c.kind)
		}
		if r, ok := RefusalOf(err); !ok || r.Reason != c.reason {
			t.Errorf("%s: %v, want a Refusal for %q", c.name, err, c.reason)
		}
	}
}

// TestActorValidate guards what an Actor must be to be taken: the zero
// Actor is no one and is Invalid, so a change that forgot its actor fails;
// a key needs a valid id, scope, role (or none) and time; the local operator
// and the server need nothing.
func TestActorValidate(t *testing.T) {
	invalid := fault.Invalid
	for _, c := range []struct {
		name string
		by   Actor
		want *fault.Kind
	}{
		{"the zero Actor", Actor{}, &invalid},
		{"the local operator", LocalOperator(), nil},
		{"the server", Server(), nil},
		{"a key", ActingKey("k1", TenantScope("t1"), RoleWriter, at(t0)), nil},
		{"a key, no role", ActingKey("k1", Installation, "", at(t0)), nil},
		{"a key with no id", ActingKey("", Installation, RoleAdmin, at(t0)), &invalid},
		{"a key with a bad id", ActingKey("k/1", Installation, RoleAdmin, at(t0)), &invalid},
		{"a key with a bad role", ActingKey("k1", Installation, "owner", at(t0)), &invalid},
		{"a key with a bad scope", ActingKey("k1", TenantScope("t 1"), RoleAdmin, at(t0)), &invalid},
		{"a key with no clock", ActingKey("k1", Installation, RoleAdmin, nil), &invalid},
	} {
		wantKind(t, c.name, c.by.Validate(), c.want)
	}
	if LocalOperator().IsKey() || Server().IsKey() || (Actor{}).IsKey() || !ActingKey("k1", Installation, "", at(t0)).IsKey() {
		t.Error("IsKey is true for a key only")
	}
}

// at returns a clock that always reads t.
func at(t time.Time) func() time.Time { return func() time.Time { return t } }

// TestRoleIncludes guards the one order of roles: each includes the ones
// before it, no role includes an unknown one, and none includes nothing.
func TestRoleIncludes(t *testing.T) {
	order := []Role{RoleReader, RoleWriter, RoleOperator, RoleAdmin}
	for i, have := range order {
		for j, need := range order {
			if got := have.Includes(need); got != (i >= j) {
				t.Errorf("%s includes %s: %v", have, need, got)
			}
		}
		if have.Includes("") || have.Includes("owner") || Role("").Includes(have) || Role("owner").Includes(have) {
			t.Errorf("%s: an empty or unknown role is included or includes", have)
		}
	}
}

// TestUncheckedActorsNamedOnlyByTheirPackages guards the two actors no store
// refuses: outside tests and the contract suites, only internal/access, which
// gives a Decision of the local operator its actor, and internal/backup,
// whose import registers a tenant for the local operator on the command
// line with the server stopped, name LocalOperator; only internal/registry
// and internal/servecmd, which change the master for the server
// itself, name Server; and only internal/registry, which gives back a
// tenant whose export failed, names ExportGiveBack. Any other package that
// names one makes changes no key is checked for: adding one changes who may
// change the installation, and has to be added here on purpose.
func TestUncheckedActorsNamedOnlyByTheirPackages(t *testing.T) {
	const masterPath = "github.com/amrshadid/nopacs/internal/master"
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	allowed := map[string][]string{
		"LocalOperator":  {"internal/access/", "internal/backup/", "internal/master/mastertest/", "internal/tenant/tenanttest/"},
		"Server":         {"internal/registry/", "internal/servecmd/", "internal/master/mastertest/"},
		"ExportGiveBack": {"internal/registry/", "internal/master/mastertest/"},
	}
	files := 0
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "bin" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		files++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		// The name this file gives the package, whatever it is: an alias
		// would otherwise hide a call from the check.
		local := ""
		for _, imp := range f.Imports {
			if strings.Trim(imp.Path.Value, `"`) != masterPath {
				continue
			}
			local = "master"
			if imp.Name != nil {
				local = imp.Name.Name
			}
		}
		switch local {
		case "":
			return nil
		case ".":
			t.Errorf("%s imports the master package into its own names, which hides what it calls", rel)
			return nil
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, ok := sel.X.(*ast.Ident)
			places, named := allowed[sel.Sel.Name]
			if !ok || x.Name != local || !named {
				return true
			}
			for _, p := range places {
				if strings.HasPrefix(rel, p) {
					return true
				}
			}
			t.Errorf("%s names master.%s; only %v may", rel, sel.Sel.Name, places)
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if files < 20 {
		t.Fatalf("only %d Go files walked: the walk did not see the repository", files)
	}
}
