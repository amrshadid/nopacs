package cli_test

import (
	"strings"
	"testing"
)

// TestRecallPreviewsThenAsks guards "recall": it shows the preview (the
// storages, the target's objects and bytes, those online and being recalled
// already, those it would ask for) and, with --dry-run, stops there; with no
// terminal and no --yes it asks for nothing and exits 2; typed on a terminal
// a wrong target stops it and the right one asks; --yes asks at once with
// the preview's token and shows what was asked; a storage not of the
// archive tier is refused (exit 5), and days outside 1 to 365 are refused
// before any request.
func TestRecallPreviewsThenAsks(t *testing.T) {
	r := replicaRig(t)
	res := r.run("recall", "lab1", "sub-01/ses-01", "--dry-run")
	if res.code != 0 {
		t.Fatalf("the preview: %+v", res)
	}
	for _, want := range []string{"sub-01/ses-01", "cold", "3 (", "Being recalled:", "To ask for:", "2 ("} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("the preview lacks %q:\n%s", want, res.stdout)
		}
	}
	before := r.fake.Requests()
	if res := r.run("recall", "lab1", "sub-01/ses-01"); res.code != 2 || !strings.Contains(res.stderr, "nothing was recalled") || r.fake.Requests() != before+1 {
		t.Errorf("a recall with no terminal and no --yes: %+v, %d requests (want the preview alone)", res, r.fake.Requests()-before)
	}
	if res := typed(r.env, "sub-02\n", "recall", "lab1", "sub-01/ses-01"); res.code != 2 || !strings.Contains(res.stderr, "not the id given") {
		t.Errorf("a wrong target typed: %+v", res)
	}
	if res := typed(r.env, "sub-01/ses-01\n", "recall", "lab1", "sub-01/ses-01"); res.code != 0 || !strings.Contains(res.stdout, "Asked for:") {
		t.Errorf("the right target typed: %+v", res)
	}
	res = r.run("recall", "lab1", "sub-01", "--days", "7", "--yes")
	if res.code != 0 || !strings.Contains(res.stdout, "Asked for:") || !strings.Contains(res.stdout, "2 (") {
		t.Errorf("a recall with --yes: %+v", res)
	}
	doc := document(t, r.run("recall", "lab1", "sub-01", "--yes", "--json"))
	if doc["objectsRequested"] != "2" && doc["objectsRequested"] != float64(2) {
		t.Errorf("recall --json: %v", doc)
	}
	if res := r.run("recall", "lab1", "sub-01", "--storage", "main", "--yes"); res.code != 5 || !strings.Contains(res.stderr, "not of the archive tier") {
		t.Errorf("a recall from an online storage: %+v", res)
	}
	before = r.fake.Requests()
	if res := r.run("recall", "lab1", "sub-01", "--days", "0"); res.code != 2 || r.fake.Requests() != before {
		t.Errorf("a recall of 0 days: %+v", res)
	}
	if res := r.run("recall", "lab1", "sub-01/ses-01/t1w/extra"); res.code != 2 {
		t.Errorf("a target of four levels: %+v", res)
	}
}

// TestReplicaVerifyDrawsAgain guards "replica verify": a tab-separated
// table of the sample (key, size, SHA-256, revision and labels) under a
// line that says how many of how many; the same seed draws the same sample
// again; --json holds the answer; a count outside 1 to 10000 is refused
// before any request.
func TestReplicaVerifyDrawsAgain(t *testing.T) {
	r := replicaRig(t)
	first := r.run("replica", "verify", "lab1", "copy", "--count", "2", "--seed", "7")
	if first.code != 0 {
		t.Fatalf("verify: %+v", first)
	}
	lines := strings.Split(strings.TrimSuffix(first.stdout, "\n"), "\n")
	if len(lines) != 4 || lines[0] != "# 2 of 1000 objects of copy" || lines[1] != "key\tsize\tsha256\trevision\tsubject\tsession\tscan" ||
		!strings.HasPrefix(lines[2], "data/sub-01/ses-01/t1w/1.2.3.007.dcm\t1007\t") || len(strings.Split(lines[3], "\t")) != 7 {
		t.Errorf("the sample's table:\n%s", first.stdout)
	}
	if again := r.run("replica", "verify", "lab1", "copy", "--count", "2", "--seed", "7"); again.stdout != first.stdout {
		t.Errorf("the same seed drew\n%s\nthe first draw\n%s", again.stdout, first.stdout)
	}
	doc := document(t, r.run("replica", "verify", "lab1", "copy", "--count", "3", "--json"))
	if objs, _ := doc["objects"].([]any); len(objs) != 3 {
		t.Errorf("verify --json: %v", doc)
	}
	before := r.fake.Requests()
	if res := r.run("replica", "verify", "lab1", "copy", "--count", "0"); res.code != 2 || r.fake.Requests() != before {
		t.Errorf("a count of 0: %+v", res)
	}
	if res := r.run("replica", "verify", "lab1", "nowhere"); res.code == 0 {
		t.Errorf("a storage the tenant does not have: %+v", res)
	}
}
