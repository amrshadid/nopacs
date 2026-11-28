package tui

import (
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/fault"
)

// editRig opens the header edit form over the corrections of tenant t000.
func editRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t, 120, 40, correctionSetup)
	r.key("enter", "enter", "x", "e")
	r.expect("Edit headers in t000", "Nothing is changed until you confirm it there")
	return r
}

// TestEditShowsItsPreviewWithValuesHiddenAndThenTheCorrection guards the edit
// flow: the form needs a change and a reason before anything is sent; the
// preview names each instance and attribute with no value but the new one; the
// confirmation asks for the target typed again and carries the preview's token;
// and the correction the edit made is shown.
func TestEditShowsItsPreviewWithValuesHiddenAndThenTheCorrection(t *testing.T) {
	r := editRig(t)
	edits := r.calls("Edit")
	// A change and no reason: refused before anything is sent.
	r.fill("sub-1/ses-1/scan-1", "", "SeriesDescription=T1 axial")
	r.key("enter", "enter", "enter", "enter")
	r.expect("say why the headers are edited")
	if r.calls("Edit") != edits {
		t.Fatalf("a preview was asked for with no reason")
	}
	r.key("shift+tab")
	r.typeText("series named wrongly")
	r.key("enter", "enter")
	if r.calls("Edit") != edits+1 {
		t.Fatalf("Edit was called %d times for the preview", r.calls("Edit")-edits)
	}
	r.expect("Edit sub-1/ses-1/scan-1", "Instances:", "Values:", "hidden", "sub-1/ses-1/scan-1 1.2.3.4:", "revision 1",
		`SeriesDescription:`, `present -> "T1 axial"`, "Confirm token:", "fake.sub-1/ses-1/scan-1/")
	r.refuse(`"old"`)

	r.key("c")
	r.expect("This writes a new revision of 1 instances")
	r.key("enter")
	r.expect("not done: type sub-1/ses-1/scan-1 exactly")
	if r.calls("Edit") != edits+1 {
		t.Fatalf("an edit was sent with the target not typed")
	}
	r.typeText("sub-1/ses-1/scan-1")
	r.key("enter")
	if r.calls("Edit") != edits+2 {
		t.Fatalf("the edit was not sent")
	}
	r.expect("Correction 01jd", "Kind:", "edit", "State:", "committed", "Target:", "sub-1/ses-1/scan-1", "Reason:", "series named wrongly", "edit 01jd")
	if cs := r.fake.Corrections("t000"); len(cs) != 3 || cs[2].GetKind() != v1.CorrectionKind_CORRECTION_KIND_EDIT {
		t.Errorf("the corrections: %v", cs)
	}
	r.key("esc")
	r.expect("Corrections of t000", "edit")
}

// TestEditShowsValuesOnlyWhenAsked guards the values of a preview: the form's
// choice defaults to no, which shows no value that is there now; yes shows the
// values and the preview says the request is recorded in the audit log.
func TestEditShowsValuesOnlyWhenAsked(t *testing.T) {
	r := editRig(t)
	r.fill("sub-1/ses-1/scan-1", "", "SeriesDescription=T1 axial", "", "named wrongly")
	r.key("enter", "right")
	r.expect("[yes]")
	r.key("enter")
	r.expect("shown, and recorded in the audit log", `"old" -> "T1 axial"`)
	r.refuse("hidden")
}

// TestEditRefusedToAMaskedKeyShowsItsReason guards a masked key's edit: the
// server refuses it, and the form shows the refusal beside the fields so the
// person sees why.
func TestEditRefusedToAMaskedKeyShowsItsReason(t *testing.T) {
	r := editRig(t)
	r.fake.SetCallerMasked(true)
	r.fill("sub-1", "", "SeriesDescription=T1 axial", "", "named wrongly")
	r.key("enter", "enter")
	r.expect("a masked key may not write values into archived files; a person does", "Edit headers in t000")
	r.refuse("Confirm token")
}

// TestEditRefusalAtTheConfirmationShowsItsReasonCode guards what the server
// refuses after the preview, such as a revision the tenant's profile refuses: the
// confirmation shows the server's message beside the fields, without the code's own
// name, which says no more than the message.
func TestEditRefusalAtTheConfirmationShowsItsMessage(t *testing.T) {
	r := editRig(t)
	r.fill("sub-1", "", "", "StationName", "station named by the site")
	r.key("enter", "enter")
	r.expect("sub-1", "Confirm token:")
	r.key("c")
	r.fake.FailWithReason(nopacsv1connect.CorrectionServiceEditProcedure, fault.Precondition, "the tenant's profile removes this attribute", "FAILED_PRECONDITION", 0)
	r.typeText("sub-1")
	r.key("enter")
	r.expect("the tenant's profile removes this attribute", "This writes a new revision")
	r.refuse("FAILED_PRECONDITION: the tenant's")
}

// TestEditFromACatalogRowStartsFilledIn guards the edit keys of the catalog: on
// a scan's instance the form holds the scan and the instance, so a person types
// only the change.
func TestEditFromACatalogRowStartsFilledIn(t *testing.T) {
	r := newRig(t, 120, 40, catalogSetup)
	r.key("enter", "enter", "c", "enter", "enter", "enter", "m")
	r.expect("Headers of scan-1")
	r.key("e")
	r.expect("Edit headers in t000", "sub-a/ses-1/scan-1", "1.2.3.1")
	r.key("esc", "esc", "e")
	r.expect("Edit headers in t000", "sub-a/ses-1/scan-1")
}
