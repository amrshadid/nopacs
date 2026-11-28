package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestSenderShowsAndSetsTheAllowIdentifiedMark guards the mark: the sender's
// screen always says whether it may send what was not de-identified; setting it
// needs the sender's name typed and shows on the screen after; clearing it is the
// same form.
func TestSenderShowsAndSetsTheAllowIdentifiedMark(t *testing.T) {
	r := newRig(t, 120, 40, sendTenant)
	r.key("enter", "enter", "n", "enter")
	r.expect("Sender pacs", "Allow identified:", "no: sends only instances archived de-identified", "i allow identified")
	sets := r.calls("SetSender")
	r.key("i")
	r.expect("Allow identified on sender pacs", "data that still identifies a person")
	r.typeText("web")
	r.key("enter", "enter")
	r.expect("not done: type pacs exactly")
	if r.calls("SetSender") != sets {
		t.Fatalf("a mark was set with the name typed wrong")
	}
	r.key("shift+tab", "ctrl+u")
	r.typeText("pacs")
	r.key("tab", "right", "enter")
	if r.calls("SetSender") != sets+1 {
		t.Fatalf("the mark was not set")
	}
	r.expect("Sender pacs", "Allow identified:", "yes: also sends instances archived as received", "sender pacs may now send instances that are not de-identified")

	// Clearing it is the same form, starting on the mark it holds.
	r.key("i")
	r.typeText("pacs")
	r.key("tab")
	r.expect("[yes]")
	r.key("left", "enter")
	r.expect("Allow identified:", "no: sends only instances archived de-identified", "sends only instances archived de-identified")
}

// TestAllowIdentifiedIsOfferedOnlyToAnInstallationAdmin guards who may set it: an
// admin on the installation, not an admin of one tenant and not a masked key;
// every one of them is shown the mark.
func TestAllowIdentifiedIsOfferedOnlyToAnInstallationAdmin(t *testing.T) {
	open := func(grants []keyGrant, masked bool) *rig {
		setup := sendTenant
		if masked {
			setup = maskedKeySetup(sendTenant)
		}
		r := openKeyTenant(t, grants, setup)
		r.key("n", "enter")
		r.expect("Sender pacs", "Allow identified:")
		helpOf(r, "Sender pacs")
		return r
	}
	r := open([]keyGrant{{role: v1.Role_ROLE_ADMIN}}, false)
	r.expect("allow identified")
	r = open([]keyGrant{{tenant: "t000", role: v1.Role_ROLE_ADMIN}}, false)
	r.expect("test")
	r.refuse("allow identified")
	r = open([]keyGrant{{role: v1.Role_ROLE_ADMIN}}, true)
	r.refuse("allow identified")
}

// TestAllowIdentifiedRefusalsAreShown guards what the server refuses: a masked
// caller's change of where data goes, and a reason of the contract's own, are shown
// on the form with the server's words.
func TestAllowIdentifiedRefusalsAreShown(t *testing.T) {
	r := newRig(t, 120, 40, sendTenant)
	r.fake.SetCallerMasked(true)
	r.key("enter", "enter", "n", "enter", "i")
	r.typeText("pacs")
	r.key("tab", "right", "enter")
	r.expect("a masked key may not choose where archived data goes; a person does", "Allow identified on sender pacs")

	r = newRig(t, 120, 40, sendTenant)
	r.fake.FailWithReason(nopacsv1connect.SenderServiceSetSenderProcedure, fault.Denied, "setting the mark needs admin on the installation", "PERMISSION_DENIED", 0)
	r.key("enter", "enter", "n", "enter", "i")
	r.typeText("pacs")
	r.key("tab", "right", "enter")
	r.expect("setting the mark needs admin on the installation")
}
