package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// transferSetup is two tenants and, in the first, a catalog.
func transferSetup(f *fakeserver.Server) {
	catalogSetup(f)
	f.AddTenant(&v1.Tenant{Name: "t001", DatabaseType: v1.DatabaseType_DATABASE_TYPE_SQLITE, State: v1.TenantState_TENANT_STATE_READY, CreateTime: timestamp(-5)})
}

// TestTransferCopyShowsItsPreviewThenItsStepsAndOutcomes guards a copy: the form
// asks the source, the target tenant, a reason and the move choice; the preview
// says what it covers and whether the receiver de-identifies; the confirmation is a
// word; and the transfer is shown with its step and how many instances are in each
// state, and listed.
func TestTransferCopyShowsItsPreviewThenItsStepsAndOutcomes(t *testing.T) {
	r := newRig(t, 120, 40, transferSetup)
	r.key("enter", "enter", "y")
	r.expect("Transfers of t000", "no transfer yet")
	r.key("n")
	r.expect("Transfer data out of t000", "Needs admin on this tenant and writer on the target")
	creates := r.calls("CreateTransfer")
	// No target tenant: refused before anything is sent.
	r.fill("sub-a/ses-1", "", "", "study belongs to the trial")
	r.key("enter", "enter")
	r.expect("name the tenant to transfer into")
	if r.calls("CreateTransfer") != creates {
		t.Fatalf("a preview was asked for with no target tenant")
	}
	r.key("shift+tab", "shift+tab", "shift+tab")
	r.typeText("t001")
	r.key("tab", "tab", "tab", "enter")
	if r.calls("CreateTransfer") != creates+1 {
		t.Fatalf("CreateTransfer was called %d times for the preview", r.calls("CreateTransfer")-creates)
	}
	r.expect("Transfer sub-a to t001", "Transfers:", "sub-a/ses-1 as a copy to t001", "Instances:", "3", "Through the receiver:", "xfer",
		"The receiver de-identifies:", "no: archived as received", "Instances it would refuse:", "0")

	r.key("c")
	r.expect("This copies 3 instances into t001")
	r.key("enter")
	r.expect("choose yes to start the copy")
	if r.calls("CreateTransfer") != creates+1 {
		t.Fatalf("a copy was started without the word")
	}
	r.key("right", "enter")
	if r.calls("CreateTransfer") != creates+2 {
		t.Fatalf("the copy was not started")
	}
	r.expect("Transfer job_1", "Transfers:", "sub-a/ses-1 as a copy to t001 through xfer", "Step:", "listing", "Confirmed in the target:", "Failed:", "r refresh")

	// The list shows it, and a second transfer of the same subject is refused with
	// the server's reason on the form.
	r.key("esc")
	r.expect("Transfers of t000", "job_1", "sub-a/ses-1", "copy", "t001", "listing", "0/3")
	r.key("n")
	r.fill("sub-a", "t001", "", "again")
	r.key("enter", "enter")
	r.key("c", "right", "enter")
	r.expect("a transfer of this subject has not ended; one runs at a time", "This copies 3 instances")
	// Opening a row shows its steps.
	r.key("esc", "esc")
	r.key("enter")
	r.expect("Transfer job_1", "Not yet copied:")
}

// TestTransferMoveNeedsTheTenantNameTyped guards a move: it deletes the source, so
// its confirmation needs the source tenant's name typed, and the screen says what
// it will delete.
func TestTransferMoveNeedsTheTenantNameTyped(t *testing.T) {
	r := newRig(t, 120, 40, transferSetup)
	r.key("enter", "enter", "c", "t")
	r.expect("Transfer data out of t000", "sub-a")
	r.key("tab")
	r.typeText("t001")
	r.key("tab", "tab")
	r.typeText("consent moved to the other trial")
	r.key("tab", "right", "enter")
	r.expect("sub-a as a move to t001")
	r.key("c")
	r.expect("moves 3 instances into t001 and deletes them from t000")
	creates := r.calls("CreateTransfer")
	r.key("enter")
	r.expect("not done: type t000 exactly")
	if r.calls("CreateTransfer") != creates {
		t.Fatalf("a move was started with the tenant's name not typed")
	}
	r.typeText("t000")
	r.key("enter")
	r.expect("Transfer job_1", "as a move to t001", "Delete of the source:")
}

// TestTransferRefusedToAMaskedKeyShowsItsReason guards a masked key's transfer:
// the server refuses where data goes, and the form says why.
func TestTransferRefusedToAMaskedKeyShowsItsReason(t *testing.T) {
	r := newRig(t, 120, 40, transferSetup)
	r.fake.SetCallerMasked(true)
	r.key("enter", "enter", "y", "n")
	r.fill("sub-a", "t001", "", "study belongs to the trial")
	r.key("enter", "enter")
	r.expect("a masked key may not choose where archived data goes; a person does", "Transfer data out of t000")
}

// TestTransferKeysFollowTheRoleAndTheMask guards who is offered the transfer
// screens: a reader none, an operator the list and not the key that starts one, an
// admin both, a masked key the list only.
func TestTransferKeysFollowTheRoleAndTheMask(t *testing.T) {
	list := func(grants []keyGrant, setup func(*fakeserver.Server)) *rig {
		r := openKeyTenant(t, grants, setup)
		r.key("y")
		r.expect("Transfers of t000")
		helpOf(r, "Transfers of t000")
		return r
	}
	r := list([]keyGrant{{tenant: "t000", role: v1.Role_ROLE_OPERATOR}}, transferSetup)
	r.refuse("n  new")
	r = list([]keyGrant{{tenant: "t000", role: v1.Role_ROLE_ADMIN}}, transferSetup)
	r.expect("new")
	r = list([]keyGrant{{role: v1.Role_ROLE_ADMIN}}, maskedKeySetup(transferSetup))
	r.refuse("n  new")
}
