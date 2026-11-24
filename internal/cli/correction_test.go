package cli_test

import (
	"regexp"
	"strings"
	"testing"
)

// mustOut runs a command that must succeed and returns its standard output.
func mustOut(r *rig, args ...string) string { return r.must(args...).stdout }

// TestCorrectionDeleteShowsThePreviewFirst guards the delete's flow: a
// target that is not a path of labels, an instance without its scan and a
// missing reason are refused before anything is sent; without --yes and
// with no terminal the preview is shown, nothing is deleted and the command
// exits 2 with what it would do; --preview shows it and stops (exit 0);
// --yes deletes with the preview's token, and the answer names the
// correction and how to undo it; show, list, undo and purge then act on it.
func TestCorrectionDeleteShowsThePreviewFirst(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"correction", "delete", "lab1", "sub-1//ses", "--reason", "x"},
		{"correction", "delete", "lab1", "sub-1/ses-1", "--instance", "2.25.1", "--reason", "x"},
		{"correction", "delete", "lab1", "sub-1", "--reason", "  "},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("refused commands sent %d requests", got-before)
	}
	res := r.run("correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn")
	if res.code != 2 || !strings.Contains(res.stdout, "Instances") || !strings.Contains(res.stderr, "nothing was deleted") {
		t.Errorf("a delete without --yes: %+v", res)
	}
	if out := mustOut(r, "correction", "list", "lab1"); strings.Contains(out, "delete") {
		t.Errorf("a delete without --yes deleted: %s", out)
	}
	if res := r.run("correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--preview"); res.code != 0 || !strings.Contains(res.stdout, "Held after purge") {
		t.Errorf("a preview: %+v", res)
	}
	out := mustOut(r, "correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--yes")
	id := fakeCorrection(1)
	if !strings.Contains(out, "deleted sub-1: correction "+id) || !strings.Contains(out, "nopacs correction undo lab1 "+id) {
		t.Errorf("the delete said %q", out)
	}
	if out := mustOut(r, "correction", "show", "lab1", id); !strings.Contains(out, "committed") || !strings.Contains(out, "holds subject") {
		t.Errorf("show said %q", out)
	}
	if out := mustOut(r, "correction", "list", "lab1", "--kind", "delete"); !strings.Contains(out, id) {
		t.Errorf("list said %q", out)
	}
	if res := r.run("correction", "list", "lab1", "--kind", "erase"); res.code != 2 {
		t.Errorf("an unknown kind: %+v", res)
	}
	if res := r.run("correction", "purge", "lab1", id); res.code != 2 || !strings.Contains(res.stderr, "nothing was purged") {
		t.Errorf("a purge without --yes: %+v", res)
	}
	if out := mustOut(r, "correction", "undo", "lab1", id, "--reason", "wrong subject"); !strings.Contains(out, "undone") {
		t.Errorf("undo said %q", out)
	}
	if res := r.run("correction", "purge", "lab1", id, "--yes"); res.code != 5 {
		t.Errorf("a purge of an undone delete: %+v, want exit 5", res)
	}
}

// TestCorrectionAllowNamesOneValue guards allow: exactly one value is named,
// or nothing is sent; one value of a purged delete leaves its holds.
func TestCorrectionAllowNamesOneValue(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("correction", "delete", "lab1", "sub-1", "--reason", "consent withdrawn", "--yes")
	id := fakeCorrection(1)
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"correction", "allow", "lab1", id, "--reason", "x"},
		{"correction", "allow", "lab1", id, "--subject", "sub-1", "--study", "2.25.1", "--reason", "x"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("refused commands sent %d requests", got-before)
	}
	if res := r.run("correction", "allow", "lab1", id, "--subject", "sub-1", "--reason", "x"); res.code != 5 {
		t.Errorf("allowing before the purge: %+v, want exit 5", res)
	}
	r.must("correction", "purge", "lab1", id, "--yes")
	if out := mustOut(r, "correction", "allow", "lab1", id, "--subject", "sub-1", "--reason", "consent given again"); !regexp.MustCompile(`Arrivals allowed:\s+yes`).MatchString(out) {
		t.Errorf("allow said %q", out)
	}
}
