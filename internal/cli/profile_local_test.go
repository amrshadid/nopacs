package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestProfileTryLocal guards "profile try --local": with no server at
// all, a corpus file tried under a document passes on de-identification
// alone, element by element, the same answer a server gives; a version's
// export (the --json answer of "profile export") is tried as that
// version; --profile, --receiver and --record are refused, as is a file
// that is not there; --fail-on-held exits 5 for a file a receiver would
// hold.
func TestProfileTryLocal(t *testing.T) {
	dir := t.TempDir()
	ct, err := os.ReadFile(filepath.Join("..", "receive", "testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	dcm := writeFile(t, dir, "one.dcm", string(ct))
	doc := writeFile(t, dir, "basic.json", basicJSON)
	none := map[string]string{}
	res := runWith(none, "profile", "try", "--local", "--document", doc, dcm)
	if res.code != 0 || !strings.Contains(res.stdout, "passed (de-identification only") || !strings.Contains(res.stdout, "1 file(s): 1 passed") ||
		!strings.Contains(res.stdout, "unsaved document") || !strings.Contains(res.stdout, "00100020") {
		t.Fatalf("try --local:\n%+v", res)
	}
	d := document(t, runWith(none, "profile", "try", "--local", "--document", doc, dir, "--json"))
	results, _ := d["results"].([]any)
	if len(results) != 1 || results[0].(map[string]any)["verdict"] != "TRIAL_VERDICT_PASSED" {
		t.Errorf("try --local --json over a directory: %v", d)
	}

	// A version's export, as the server answers it.
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("profile", "create", "lab1", "research", "--from", doc)
	exported := r.run("profile", "export", "lab1", "research@1", "--json")
	export := writeFile(t, dir, "research-1.json", exported.stdout)
	res = runWith(none, "profile", "try", "--local", "--document", export, dcm, "--json")
	v, _ := document(t, res)["version"].(map[string]any)
	if res.code != 0 || v["name"] != "research" || v["version"] != float64(1) {
		t.Errorf("try --local of an export: %+v", res)
	}

	for _, args := range [][]string{
		{"profile", "try", "--local", "--profile", "research", dcm},
		{"profile", "try", "--local", "--document", doc, "--receiver", "scanner1", dcm},
		{"profile", "try", "--local", "--document", doc, "--record", "01J9Z3K4", dcm},
		{"profile", "try", "--local", "--document", doc, filepath.Join(dir, "gone.dcm")},
	} {
		if res := runWith(none, args...); res.code != 2 {
			t.Errorf("%v: %+v", args[3:], res)
		}
	}
	text := writeFile(t, dir, "notes.dcm", "not a DICOM file")
	if res := runWith(none, "profile", "try", "--local", "--document", doc, text, "--fail-on-held"); res.code != 5 || !strings.Contains(res.stdout, "held: not_dicom") {
		t.Errorf("--fail-on-held for a file a receiver would hold: %+v", res)
	}
}

// TestProfileDescribePlainWords guards the description "profile describe"
// prints: the classes in plain words, the sentences for the whole profile
// and the method record a Conformance Statement needs, with the basic
// profile's code; --no-rules leaves the rules out; --json holds the
// classes, the notes and the method.
func TestProfileDescribePlainWords(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	doc := writeFile(t, t.TempDir(), "basic.json", basicJSON)
	r.must("profile", "create", "lab1", "research", "--document", doc)
	res := r.run("profile", "describe", "lab1", "research@1", "--no-rules")
	for _, want := range []string{"Names of persons", "UIDs", "remapped", "Method, for a Conformance Statement",
		`(113100, DCM, "Basic Application Confidentiality Profile")`, "Patient Identity Removed (0012,0062): YES", "never kept"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("describe does not say %q:\n%s", want, res.stdout)
		}
	}
	if strings.Contains(res.stdout, "Rules, in order") {
		t.Error("--no-rules printed the rules")
	}
	d := document(t, r.run("profile", "describe", "lab1", "research@1", "--json"))
	m, _ := d["method"].(map[string]any)
	if len(d["classes"].([]any)) < 4 || len(d["notes"].([]any)) < 3 || m["identityRemoved"] != "YES" {
		t.Errorf("describe --json: classes %v, method %v", d["classes"], m)
	}
}
