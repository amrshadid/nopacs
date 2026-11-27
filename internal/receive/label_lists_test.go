package receive

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestLabelHoldingAListTermIsAResidual guards the labels against the
// profile's text lists: a label a plain naming rule made from a value as
// received, holding a term of the lists standing as a word, is a residual
// of kind list_term at its level, as one holding a removed value is; a
// label holding the term inside a longer word is not.
func TestLabelHoldingAListTermIsAResidual(t *testing.T) {
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{"Zebulonquist"}}})
	if problems != nil {
		t.Fatal(problems)
	}
	needles := engine.Needles{{Kind: engine.NeedleList, Lists: l}}
	rcv := tenant.Receiver{Naming: &naming.Spec{Subject: "{(0010,4000)}", Session: "ses-{#StudyInstanceUID}", Scan: "scan-{#SeriesInstanceUID}"}}
	if level, kind := labelResidual(rcv, label.Path{Subject: "Zebulonquist-01", Session: "ses-x", Scan: "scan-y"}, needles); level != "subject" || kind != "list_term" {
		t.Errorf("a label holding a list term: %q %q", level, kind)
	}
	if level, _ := labelResidual(rcv, label.Path{Subject: "Zebulonquists01", Session: "ses-x", Scan: "scan-y"}, needles); level != "" {
		t.Errorf("a label holding the term inside a word: %q", level)
	}
}
