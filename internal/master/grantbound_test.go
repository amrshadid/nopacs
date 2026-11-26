package master

import (
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestCheckGrantChange guards the bounds of a tenant's admin on grants,
// one refusal each: another tenant's grants or the installation's, its own
// key, giving admin, and touching a key that is admin there or on the
// installation are refused as Denied with ErrGrantBound; a reader, writer
// or operator grant on its tenant is let through, also for a key that is
// an operator on the installation; and an installation's admin, the local
// operator and the server are not held to these bounds.
func TestCheckGrantChange(t *testing.T) {
	clock := func() time.Time { return time.Unix(0, 0) }
	lab1, lab2 := TenantScope("t1"), TenantScope("t2")
	tenantAdmin := ActingKey("kadmin", lab1, RoleAdmin, clock)
	cases := []struct {
		name            string
		by              Actor
		key             string
		scope           Scope
		old, next, inst Role
		refused         bool
	}{
		{"a reader on its tenant", tenantAdmin, "kother", lab1, "", RoleReader, "", false},
		{"a writer raised to operator", tenantAdmin, "kother", lab1, RoleWriter, RoleOperator, "", false},
		{"an operator removed", tenantAdmin, "kother", lab1, RoleOperator, "", "", false},
		{"an installation's operator given reader", tenantAdmin, "kother", lab1, "", RoleReader, RoleOperator, false},
		{"another tenant's grant", tenantAdmin, "kother", lab2, "", RoleReader, "", true},
		{"a grant on the installation", tenantAdmin, "kother", Installation, "", RoleReader, "", true},
		{"its own key", tenantAdmin, "kadmin", lab1, RoleAdmin, RoleOperator, "", true},
		{"making an admin", tenantAdmin, "kother", lab1, RoleReader, RoleAdmin, "", true},
		{"lowering an admin", tenantAdmin, "kother", lab1, RoleAdmin, RoleReader, "", true},
		{"removing an admin", tenantAdmin, "kother", lab1, RoleAdmin, "", "", true},
		{"an installation's admin given reader", tenantAdmin, "kother", lab1, "", RoleReader, RoleAdmin, true},
		{"an installation's admin lowered", tenantAdmin, "kother", lab1, RoleOperator, RoleReader, RoleAdmin, true},
		{"an installation's admin removed", tenantAdmin, "kother", lab1, RoleOperator, "", RoleAdmin, true},
		{"the installation's admin makes an admin", ActingKey("kinst", Installation, RoleAdmin, clock), "kother", lab1, RoleReader, RoleAdmin, "", false},
		{"the local operator removes an admin", LocalOperator(), "kother", lab1, RoleAdmin, "", "", false},
	}
	for _, c := range cases {
		err := CheckGrantChange(c.by, c.key, c.scope, c.old, c.next, c.inst)
		switch {
		case c.refused && (fault.KindOf(err) != fault.Denied || !errors.Is(err, ErrGrantBound)):
			t.Errorf("%s: %v, want a refusal with ErrGrantBound", c.name, err)
		case !c.refused && err != nil:
			t.Errorf("%s: %v, want it let through", c.name, err)
		}
	}
}
