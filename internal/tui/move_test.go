package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/testkit/fakeserver"
)

// TestMoveShowsItsPreviewAndTheStateOfItsIdentityRewrite guards a move: the form
// needs the session, the other subject and a reason; the preview names what moves
// and says whether the identity is rewritten; the confirmation needs the session
// typed again and carries the token; and the correction it made says its identity
// rewrite is asked and made in the background.
func TestMoveShowsItsPreviewAndTheStateOfItsIdentityRewrite(t *testing.T) {
	r := newRig(t, 120, 40, correctionSetup)
	r.key("enter", "enter", "x", "o")
	r.expect("Move a session in t000", "Undo files it back until then")
	moves := r.calls("Move")
	// A session that is only a subject is refused before anything is sent.
	r.fill("sub-3", "sub-4", "study filed under the wrong patient")
	r.key("enter", "enter")
	r.expect("a session is subject/session")
	r.key("shift+tab", "shift+tab", "shift+tab")
	r.typeText("/ses-9")
	r.key("tab", "tab", "tab", "right", "enter")
	if r.calls("Move") != moves+1 {
		t.Fatalf("Move was called %d times for the preview", r.calls("Move")-moves)
	}
	r.expect("Move sub-3/ses-9 under sub-4", "Moves:", "sub-3/ses-9", "Under:", "sub-4", "Rewrite identity:", "yes: once the move is published", "Confirm token:", "fake.sub-3/ses-9/")

	r.key("c")
	r.expect("files 1 instances under sub-4")
	r.key("enter")
	r.expect("not done: type sub-3/ses-9 exactly")
	if r.calls("Move") != moves+1 {
		t.Fatalf("a move was sent with the session not typed")
	}
	r.typeText("sub-3/ses-9")
	r.key("enter")
	if r.calls("Move") != moves+2 {
		t.Fatalf("the move was not sent")
	}
	r.expect("Correction 01jd", "Kind:", "move", "Identity rewrite:", "asked: made in the background", "move 01jd")
	if cs := r.fake.Corrections("t000"); len(cs) != 3 || cs[2].GetToLabel() != "sub-4" {
		t.Errorf("the corrections: %v", cs)
	}
}

// TestMoveWithoutARewriteSaysSo guards the other state of the identity rewrite: a
// move that did not ask for it says the instances keep the identity they have.
func TestMoveWithoutARewriteSaysSo(t *testing.T) {
	r := newRig(t, 120, 40, correctionSetup)
	r.key("enter", "enter", "x", "o")
	r.fill("sub-3/ses-9", "sub-4", "study filed under the wrong patient")
	r.key("enter", "enter")
	r.expect("Rewrite identity:", "no")
	r.key("c")
	r.typeText("sub-3/ses-9")
	r.key("enter")
	r.expect("Identity rewrite:", "not asked: the moved instances keep the identity they have")
}

// TestAMoveWhoseRewriteWasRefusedShowsTheRefusal guards a correction whose
// identity rewrite the server refused for good: the correction's screen says the
// move stands and why the rewrite did not happen.
func TestAMoveWhoseRewriteWasRefusedShowsTheRefusal(t *testing.T) {
	r := newRig(t, 120, 40, func(f *fakeserver.Server) {
		correctionSetup(f)
		f.AddCorrection("t000", &v1.Correction{Id: "cor_7", Kind: v1.CorrectionKind_CORRECTION_KIND_MOVE, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
			Target: &v1.CorrectionTarget{SubjectLabel: "sub-3", SessionLabel: "ses-9"}, ToLabel: "sub-4", Reason: "wrong patient", CreateTime: timestamp(-1),
			IdentityRewriteRefused: "the destination's files hold no Patient ID"})
	})
	r.key("enter", "enter", "x")
	r.expect("cor_7", "move")
	r.key("enter")
	r.expect("Correction cor_7", "Identity rewrite:", "refused: the destination's files hold no Patient ID", "the move stands")
}

// TestMergeNeedsTheSubjectTypedAndAMaskedKeyIsRefused guards a merge: the form's
// answer is a preview, the confirmation needs the subject typed, and a masked
// caller's merge is refused by the server with its reason, shown on the form.
func TestMergeNeedsTheSubjectTypedAndAMaskedKeyIsRefused(t *testing.T) {
	r := newRig(t, 120, 40, correctionSetup)
	r.key("enter", "enter", "x", "g")
	r.expect("Merge a subject in t000")
	merges := r.calls("Merge")
	r.fill("sub-3", "sub-4", "one patient under two ids")
	r.key("enter", "enter")
	r.expect("Merge sub-3 under sub-4", "Moves:", "sub-3")
	r.key("c")
	r.expect("Merge sub-3 into sub-4")
	r.typeText("sub-4")
	r.key("enter")
	r.expect("not done: type sub-3 exactly")
	if r.calls("Merge") != merges+1 {
		t.Fatalf("a merge was sent with the subject typed wrong")
	}
	r.key("ctrl+u")
	r.typeText("sub-3")
	r.key("enter")
	if r.calls("Merge") != merges+2 {
		t.Fatalf("the merge was not sent")
	}

	r = newRig(t, 120, 40, correctionSetup)
	r.fake.SetCallerMasked(true)
	r.key("enter", "enter", "x", "g")
	r.fill("sub-3", "sub-4", "one patient under two ids")
	r.key("enter", "enter")
	r.expect("a masked key may not choose where archived data goes; a person does")
	r.refuse("Confirm token")
}

// TestMoveRefusalShowsItsReasonCode guards a move the server refuses for a reason of
// the contract's own, such as a hold on the subject: the code and the message show
// on the form beside what was typed.
func TestMoveRefusalShowsItsReasonCode(t *testing.T) {
	r := newRig(t, 120, 40, correctionSetup)
	r.key("enter", "enter", "x", "o")
	r.fake.FailWithReason(nopacsv1connect.CorrectionServiceMoveProcedure, fault.Precondition, "a hold covers sub-3", fault.ReasonHeld, 0)
	r.fill("sub-3/ses-9", "sub-4", "study filed under the wrong patient")
	r.key("enter", "enter")
	r.expect("HELD: a hold covers sub-3", "Move a session in t000")
}

// TestMoveAndMergeKeysOfTheCatalogStartFilledIn guards the keys of the catalog: a
// session offers a move with its subject and session typed, a subject a merge.
func TestMoveAndMergeKeysOfTheCatalogStartFilledIn(t *testing.T) {
	r := newRig(t, 120, 40, catalogSetup)
	r.key("enter", "enter", "c", "g")
	r.expect("Merge a subject in t000", "sub-a")
	r.key("esc", "enter", "o")
	r.expect("Move a session in t000", "sub-a/ses-1")
}
