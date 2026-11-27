package cli_test

import (
	"strings"
	"testing"
)

// TestCorrectionEditPreviewsThenEdits guards the edit's flow: a change that
// is not Tag=Value, no change at all, an instance without its scan and a
// missing reason are refused before anything is sent; --preview shows every
// instance and attribute, its value now hidden unless --show-values, and
// edits nothing; without --preview the command shows the preview and edits
// with its token, the answer naming the correction and how to undo it.
func TestCorrectionEditPreviewsThenEdits(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"correction", "edit", "lab1", "sub-1", "--set", "SeriesDescription", "--reason", "x"},
		{"correction", "edit", "lab1", "sub-1", "--reason", "x"},
		{"correction", "edit", "lab1", "sub-1/ses-1", "--instance", "2.25.1", "--set", "SeriesDescription=x", "--reason", "x"},
		{"correction", "edit", "lab1", "sub-1", "--set", "SeriesDescription=x", "--reason", "  "},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("refused commands sent %d requests", got-before)
	}
	out := mustOut(r, "correction", "edit", "lab1", "sub-1/ses-1/scan-1", "--set", "SeriesDescription=T1 axial", "--remove", "StationName",
		"--reason", "series named wrongly", "--preview")
	if !strings.Contains(out, "SeriesDescription") || !strings.Contains(out, `present -> "T1 axial"`) || !strings.Contains(out, "present -> removed") ||
		strings.Contains(out, `"old"`) {
		t.Errorf("a preview said %q", out)
	}
	if out := mustOut(r, "correction", "list", "lab1"); strings.Contains(out, "edit") {
		t.Errorf("a preview edited: %s", out)
	}
	if out := mustOut(r, "correction", "edit", "lab1", "sub-1/ses-1/scan-1", "--set", "SeriesDescription=T1 axial", "--reason", "r", "--preview",
		"--show-values"); !strings.Contains(out, `"old" -> "T1 axial"`) {
		t.Errorf("a preview with its values said %q", out)
	}
	out = mustOut(r, "correction", "edit", "lab1", "sub-1/ses-1/scan-1", "--set", "SeriesDescription=T1 axial", "--reason", "series named wrongly")
	id := fakeCorrection(1)
	if !strings.Contains(out, "edit "+id+" of sub-1/ses-1/scan-1 is committed") || !strings.Contains(out, "nopacs correction undo lab1 "+id) {
		t.Errorf("the edit said %q", out)
	}
	if out := mustOut(r, "correction", "list", "lab1", "--kind", "edit"); !strings.Contains(out, id) {
		t.Errorf("list said %q", out)
	}
}
