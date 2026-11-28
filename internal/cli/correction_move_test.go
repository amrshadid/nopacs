package cli_test

import (
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestCorrectionMoveAndMerge guards the move's and the merge's flow: a
// session that is not <subject>/<session>, a destination that is not a
// label, a missing destination and a blank reason are refused before
// anything is sent; --preview shows what moves and under which subject and
// moves nothing; without --preview the command shows the preview and moves
// with its token, the answer naming the correction; a merge goes the same
// way; both are listed by their kind.
func TestCorrectionMoveAndMerge(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"correction", "move", "lab1", "sub-1", "--to", "sub-2", "--reason", "x"},
		{"correction", "move", "lab1", "sub-1/ses-1", "--to", "a/b", "--reason", "x"},
		{"correction", "move", "lab1", "sub-1/ses-1", "--reason", "x"},
		{"correction", "move", "lab1", "sub-1/ses-1", "--to", "sub-2", "--reason", "  "},
		{"correction", "merge", "lab1", "sub-1/ses-1", "--into", "sub-2", "--reason", "x"},
		{"correction", "merge", "lab1", "sub-1", "--reason", "x"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("refused commands sent %d requests", got-before)
	}
	out := mustOut(r, "correction", "move", "lab1", "sub-1/ses-1", "--to", "sub-2", "--reason", "filed under the wrong patient", "--preview")
	if !strings.Contains(out, "sub-1/ses-1") || !strings.Contains(out, "sub-2") || !strings.Contains(out, "Confirm") {
		t.Errorf("a preview said %q", out)
	}
	if out := mustOut(r, "correction", "list", "lab1"); strings.Contains(out, "move") {
		t.Errorf("a preview moved: %s", out)
	}
	out = mustOut(r, "correction", "move", "lab1", "sub-1/ses-1", "--to", "sub-2", "--reason", "filed under the wrong patient", "--rewrite-identity")
	moved := fakeCorrection(1)
	if !strings.Contains(out, "move "+moved+" of sub-1/ses-1 under sub-2 is committed") {
		t.Errorf("the move said %q", out)
	}
	out = mustOut(r, "correction", "merge", "lab1", "sub-3", "--into", "sub-2", "--reason", "one patient under two ids")
	merged := fakeCorrection(2)
	if !strings.Contains(out, "move "+merged+" of sub-3 under sub-2 is committed") {
		t.Errorf("the merge said %q", out)
	}
	if out := mustOut(r, "correction", "list", "lab1", "--kind", "move"); !strings.Contains(out, moved) || !strings.Contains(out, merged) {
		t.Errorf("list said %q", out)
	}
}

// TestCorrectionShowSaysAnIdentityRewriteWasRefused guards that show names
// a move's refused identity rewrite with its reason, and a move whose
// rewrite was not refused says nothing of one.
func TestCorrectionShowSaysAnIdentityRewriteWasRefused(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	move := &v1.Correction{Id: "cor_1", Kind: v1.CorrectionKind_CORRECTION_KIND_MOVE, State: v1.CorrectionState_CORRECTION_STATE_COMMITTED,
		Target: &v1.CorrectionTarget{SubjectLabel: "sub-1", SessionLabel: "ses-1"}, ToLabel: "sub-2", Counts: &v1.CorrectionCounts{Instances: 2},
		IdentityRewriteRefused: "the profile or its check after refused the rewritten files"}
	r.fake.AddCorrection("lab1", move)
	out := mustOut(r, "correction", "show", "lab1", "cor_1")
	if got := field(out, "Identity rewrite"); got != "refused: the profile or its check after refused the rewritten files" {
		t.Errorf("Identity rewrite is %q:\n%s", got, out)
	}
	r.fake.AddCorrection("lab1", &v1.Correction{Id: "cor_2", Kind: v1.CorrectionKind_CORRECTION_KIND_MOVE,
		State: v1.CorrectionState_CORRECTION_STATE_COMMITTED, Target: &v1.CorrectionTarget{SubjectLabel: "sub-1", SessionLabel: "ses-2"}})
	if out := mustOut(r, "correction", "show", "lab1", "cor_2"); strings.Contains(out, "Identity rewrite") {
		t.Errorf("a move not refused says:\n%s", out)
	}
}
