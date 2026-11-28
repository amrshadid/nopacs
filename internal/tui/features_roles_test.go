package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// TestChangingKeysFollowTheRoleAndTheMaskOfTheCaller guards the keys that change
// archived data: the corrections list offers edit, move, merge and batch delete
// to an admin on the tenant and to no one less, and to no masked key whatever its
// role; the catalog lists offer the same, and mark and transfer, on the same terms;
// the server refuses the rest whatever the keys offer.
func TestChangingKeysFollowTheRoleAndTheMaskOfTheCaller(t *testing.T) {
	setup := func(f *fakeserver.Server) {
		correctionSetup(f)
		catalogSetup(f)
	}
	changing := []string{"edit headers", "batch delete", "merge"}
	corrections := func(r *rig) {
		r.key("x")
		r.expect("Corrections of t000")
		helpOf(r, "Corrections of t000")
	}
	catalog := func(r *rig) {
		r.key("c")
		r.expect("Subjects of t000")
		helpOf(r, "Subjects of t000")
	}

	r := openKeyTenant(t, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_OPERATOR}}, setup)
	corrections(r)
	r.expect("up/down")
	r.refuse(changing...)
	r.key("x", "esc")
	r.key("c")
	helpOf(r, "Subjects of t000")
	r.expect("download")
	r.refuse("edit headers", "mark", "batch delete", "merge", "transfer")

	r = openKeyTenant(t, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_ADMIN}}, setup)
	corrections(r)
	r.expect(append([]string{"move", "delete"}, changing...)...)
	r.key("x", "esc")
	catalog(r)
	r.expect("download", "edit headers", "mark", "batch delete", "merge", "transfer")

	r = openKeyTenant(t, []keyGrant{{role: v1.Role_ROLE_ADMIN}}, maskedKeySetup(setup))
	corrections(r)
	r.expect("delete", "undo")
	r.refuse(changing...)
	r.key("x", "esc")
	catalog(r)
	r.expect("download")
	r.refuse("edit headers", "mark", "batch delete", "merge", "transfer")

	// The tenant's own screen offers transfers to an operator and not to a reader.
	r = openKeyTenant(t, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_READER}}, setup)
	helpOf(r, "Tenant t000")
	r.refuse("transfers")
	r = openKeyTenant(t, []keyGrant{{tenant: "t000", role: v1.Role_ROLE_OPERATOR}}, setup)
	helpOf(r, "Tenant t000")
	r.expect("transfers")
}
