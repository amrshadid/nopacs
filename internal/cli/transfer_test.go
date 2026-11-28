package cli_test

import (
	"strings"
	"testing"
)

// TestTransferCommands guards the transfer commands' flow: a missing --to, a
// reference of more than two labels and a missing reason are refused before
// anything is sent; --preview shows what the transfer covers, the receiver
// and whether it de-identifies, and makes no job; a copy starts at once and
// names how to follow it; a move without --yes and with no terminal moves
// nothing (exit 2), and with --yes starts; a second transfer of the subject
// while one runs is refused (exit 5); show and list answer what was made, in
// a table and as JSON.
func TestTransferCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("tenant", "create", "lab2")
	before := r.fake.Requests()
	for _, args := range [][]string{
		{"transfer", "create", "lab1", "sub-a", "--reason", "x"},
		{"transfer", "create", "lab1", "sub-a/ses-1/scan", "--to", "lab2", "--reason", "x"},
		{"transfer", "create", "lab1", "sub-a", "--to", "lab2", "--reason", " "},
		{"transfer", "create", "lab1", "sub-a", "--to", "lab2"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("nopacs %v: %+v, want exit 2", args, res)
		}
	}
	if got := r.fake.Requests(); got != before {
		t.Errorf("refused commands sent %d requests", got-before)
	}
	res := r.run("transfer", "create", "lab1", "sub-a/ses-1", "--to", "lab2", "--reason", "study belongs to lab2", "--preview")
	if res.code != 0 || !strings.Contains(res.stdout, "Instances") || !strings.Contains(res.stdout, "xfer") || !strings.Contains(res.stdout, "de-identifies") {
		t.Errorf("a preview: %+v", res)
	}
	if out := mustOut(r, "transfer", "list", "lab1"); strings.Contains(out, "sub-a") {
		t.Errorf("a preview made a transfer: %s", out)
	}
	out := mustOut(r, "transfer", "create", "lab1", "sub-a/ses-1", "--to", "lab2", "--via", "from-lab1", "--reason", "study belongs to lab2")
	if !strings.Contains(out, "transfer job_1 is queued: 3 instances to tenant lab2") || !strings.Contains(out, "nopacs transfer show lab1 job_1") {
		t.Errorf("the transfer said %q", out)
	}
	if res := r.run("transfer", "create", "lab1", "sub-a", "--to", "lab2", "--move", "--reason", "x", "--yes"); res.code != 5 {
		t.Errorf("a move while a transfer of the subject runs: %+v, want exit 5", res)
	}
	r.must("tenant", "create", "lab3")
	res = r.run("transfer", "create", "lab1", "sub-b", "--to", "lab3", "--move", "--reason", "consent moved")
	if res.code != 2 || !strings.Contains(res.stderr, "nothing was moved") {
		t.Errorf("a move without --yes: %+v", res)
	}
	out = mustOut(r, "transfer", "create", "lab1", "sub-b", "--to", "lab3", "--move", "--reason", "consent moved", "--yes")
	if !strings.Contains(out, "transfer job_2 is queued") {
		t.Errorf("the move said %q", out)
	}
	if res := r.run("transfer", "create", "lab1", "sub-b", "--to", "lab3", "--reason", "again", "--yes"); res.code != 5 {
		t.Errorf("a second transfer of the subject: %+v, want exit 5", res)
	}
	show := mustOut(r, "transfer", "show", "lab1", "job_2")
	for _, want := range []string{"sub-b as a move to lab3 through xfer", "listing", "Delete of the source"} {
		if !strings.Contains(show, want) {
			t.Errorf("transfer show holds no %q:\n%s", want, show)
		}
	}
	list := mustOut(r, "transfer", "list", "lab1")
	if !strings.Contains(list, "job_2") || !strings.Contains(list, "move") || !strings.Contains(list, "sub-a/ses-1") || strings.Index(list, "job_2") > strings.Index(list, "job_1") {
		t.Errorf("transfer list, newest first:\n%s", list)
	}
	doc := document(t, r.run("transfer", "show", "lab1", "job_1", "--json"))
	if tr, _ := doc["transfer"].(map[string]any); tr == nil || tr["targetTenant"] != "lab2" || tr["receiver"] != "from-lab1" {
		t.Errorf("transfer show --json: %v", doc)
	}
	if res := r.run("transfer", "show", "lab1", "nojob"); res.code != 4 {
		t.Errorf("a transfer that does not exist: %+v, want exit 4", res)
	}
}
