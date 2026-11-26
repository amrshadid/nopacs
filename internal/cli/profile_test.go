package cli_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// basicJSON is a document on the Annex E basic profile alone, in its file
// form.
const basicJSON = `{"bases": ["annex-e/basic:2026d"]}`

// keepStudyIDJSON adds one rule of its own: Study ID kept.
const keepStudyIDJSON = `{"bases": ["annex-e/basic:2026d"], "rules": [{"name": "keep-study-id",
  "selector": {"keyword": "StudyID"}, "action": {"keep": {}}, "reason": "the site's study number is needed"}]}`

// writeFile writes text to a file in dir and returns its path.
func writeFile(t *testing.T, dir, name, text string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(text), 0o600); err != nil { // #nosec G703 -- a file of the test's own temporary directory.
		t.Fatal(err)
	}
	return p
}

// TestProfileCommands guards the profile commands that keep and read
// versions: create from a file and from standard input, a document with a
// problem listed with its field and refused with exit 2, the same document
// again making no version, list, versions, get and export (the document
// given back byte for byte in its file form), validate, describe (each
// expanded rule with its origin) and diff, each with --json.
func TestProfileCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	dir := t.TempDir()
	basic := writeFile(t, dir, "basic.json", basicJSON)
	bad := writeFile(t, dir, "bad.json", `{"bases": ["annex-e/basic:2026d", "annex-e/nothing:2026d"]}`)

	create := r.run("profile", "create", "lab1", "research", "--document", basic, "--note", "first")
	if create.code != 0 || !strings.Contains(create.stdout, "Version:        1\n") || !strings.Contains(create.stdout, "Bases:          annex-e/basic:2026d\n") {
		t.Fatalf("create: %+v", create)
	}
	again := r.run("profile", "create", "lab1", "research", "--document", basic)
	if again.code != 0 || !strings.Contains(again.stderr, "equals version 1; no version was made") {
		t.Errorf("again: %+v", again)
	}
	refused := r.run("profile", "create", "lab1", "research", "--document", bad)
	if refused.code != 2 || !strings.Contains(refused.stdout, "bases[2]") || !strings.Contains(refused.stdout, "CODE") || !strings.Contains(refused.stderr, "profile create") {
		t.Errorf("a document with a problem: %+v", refused)
	}
	if res := r.run("profile", "create", "lab1", "research", "--document", filepath.Join(dir, "none.json")); res.code != 2 {
		t.Errorf("a document that is not there: %+v", res)
	}
	stdin := typed(r.env, keepStudyIDJSON, "profile", "create", "lab1", "research", "--document", "-", "--expect-latest", "1")
	if stdin.code != 0 || !strings.Contains(stdin.stdout, "Version:        2\n") || !strings.Contains(stdin.stdout, "Own rules:      1\n") {
		t.Errorf("create from standard input: %+v", stdin)
	}
	if res := typed(r.env, basicJSON, "profile", "create", "lab1", "research", "--document", "-", "--expect-latest", "1"); res.code != 5 {
		t.Errorf("an expectation that does not hold: %+v", res)
	}

	expect(t, r.run("profile", "list", "lab1"), 0, "NAME      LATEST  CREATED\nresearch  2       2026-11-17 15:00:00 +03:00\n", "")
	if list := document(t, r.run("profile", "list", "lab1", "--json")); len(list["profiles"].([]any)) != 1 {
		t.Errorf("list --json: %v", list)
	}
	versions := r.run("profile", "versions", "lab1", "research")
	if versions.code != 0 || !strings.HasPrefix(versions.stdout, "VERSION  CREATED") || strings.Index(versions.stdout, "\n2 ") > strings.Index(versions.stdout, "\n1 ") {
		t.Errorf("versions, newest first: %+v", versions)
	}
	got := document(t, r.run("profile", "get", "lab1", "research@1", "--json"))
	if v := got["version"].(map[string]any); v["version"] != float64(1) || v["note"] != "first" {
		t.Errorf("get --json: %v", got)
	}
	if res := r.run("profile", "get", "lab1", "research@9"); res.code != 4 {
		t.Errorf("a version not there: %+v", res)
	}
	if res := r.run("profile", "get", "lab1", "Research"); res.code != 2 {
		t.Errorf("a malformed name: %+v", res)
	}

	export := r.run("profile", "export", "lab1", "research@2")
	doc, err := profile.ParseDocument([]byte(keepStudyIDJSON))
	if err != nil {
		t.Fatal(err)
	}
	want, _ := profile.FormatDocument(doc)
	if export.code != 0 || export.stdout != string(want) {
		t.Errorf("export:\n%s\nwant:\n%s", export.stdout, want)
	}
	out := filepath.Join(dir, "out.json")
	if res := r.run("profile", "export", "lab1", "research", "--to", out); res.code != 0 {
		t.Errorf("export --to: %+v", res)
	}
	if b, _ := os.ReadFile(out); string(b) != string(want) { // #nosec G304 -- the test's own file.
		t.Errorf("export --to wrote %q", b)
	}

	if res := r.run("profile", "validate", "lab1", "--document", bad); res.code != 2 || !strings.Contains(res.stdout, "bases[2]") || !strings.Contains(res.stderr, "1 problem") {
		t.Errorf("validate a problem: %+v", res)
	}
	if res := r.run("profile", "validate", "lab1", "--document", basic); res.code != 0 || !strings.Contains(res.stdout, "Valid:    yes\n") {
		t.Errorf("validate: %+v", res)
	}
	if v := document(t, r.run("profile", "validate", "lab1", "--document", basic, "--json")); len(v["checksum"].(string)) != 64 {
		t.Errorf("validate --json: %v", v)
	}

	describe := r.run("profile", "describe", "lab1", "research@2")
	last := describe.stdout[strings.LastIndex(strings.TrimSuffix(describe.stdout, "\n"), "\n")+1:]
	if describe.code != 0 || !strings.Contains(describe.stdout, "e1-00100010") || !strings.Contains(describe.stdout, "annex-e/basic:2026d") ||
		!strings.HasPrefix(strings.Fields(last)[1], "own") || !strings.Contains(last, "keep-study-id  tag 00200010  keep") {
		t.Errorf("describe, its last rule: %q", last)
	}
	d := document(t, r.run("profile", "describe", "lab1", "--document", basic, "--json"))
	if rules := d["expanded"].(map[string]any)["rules"].([]any); len(rules) == 0 || len(d["checksum"].(string)) != 64 {
		t.Errorf("describe --json: %v", d["checksum"])
	}
	if res := r.run("profile", "describe", "lab1"); res.code != 2 {
		t.Errorf("describe with nothing named: %+v", res)
	}

	diff := r.run("profile", "diff", "lab1", "research@1", "research@2")
	if diff.code != 0 || !strings.Contains(diff.stdout, "added   own     keep-study-id") || !strings.Contains(diff.stdout, "Same effect:   no\n") || !strings.Contains(diff.stdout, "Effect:") {
		t.Errorf("diff: %s", diff.stdout)
	}
	if dd := document(t, r.run("profile", "diff", "lab1", "research@1", "research@1", "--json")); dd["sameEffect"] != true {
		t.Errorf("diff --json: %v", dd)
	}
}

// TestProfileTryCommand guards "profile try": files sent with their paths
// as names, a directory read for its DICOM files only, records and the
// reception passed on; the elements shown with their values before and
// after with --show-values, and with their lengths and no value without it,
// and the rule that decided them; --changed-only; --json; the
// verdict of a held file and --fail-on-held's exit 5; and the bounds
// checked before anything is sent.
func TestProfileTryCommand(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	dir := t.TempDir()
	ct, err := os.ReadFile(filepath.Join("..", "receive", "testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	one := writeFile(t, dir, "one.dcm", string(ct))
	sub := filepath.Join(dir, "series")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, sub, "two.dcm", string(ct))
	writeFile(t, sub, "README.txt", "not a DICOM file")

	r.fake.SetTrialAnswer(&v1.TryProfileResponse{
		Version: &v1.ProfileVersion{Name: "research", Version: 2}, Checksum: strings.Repeat("a", 64),
		Results: []*v1.TrialResult{
			{Name: one, Verdict: v1.TrialVerdict_TRIAL_VERDICT_PASSED, Elements: []*v1.TrialElement{
				{Path: "00100010", Keyword: "PatientName", Vr: "PN", Action: "empty", Rule: 12, RuleName: "e1-00100010", Origin: "annex-e/basic:2026d",
					Reason: "PS3.15", Changed: true, Before: "Doe^Peter", PresentAfter: true},
				{Path: "00080060", Keyword: "Modality", Vr: "CS", Action: "keep", Before: "CT", After: "CT", PresentAfter: true},
			}},
			{Name: filepath.Join(sub, "two.dcm"), Verdict: v1.TrialVerdict_TRIAL_VERDICT_HELD, Reason: "residual_value", Cause: "name", Path: "00081030"},
		},
		Summary: &v1.TrialSummary{Files: 2, Passed: 1, Held: 1, ByReason: []*v1.TrialCount{{Key: "residual_value/name", Count: 1}}},
	})
	res := r.run("profile", "try", "lab1", "--profile", "research@2", one, sub, "--record", "id-1", "--calling-aet", "PRISMA1", "--show-values")
	if res.code != 0 {
		t.Fatalf("try: %+v", res)
	}
	for _, want := range []string{"Profile:  research@2\n", "PATH", "BEFORE", "Doe^Peter", "e1-00100010 (annex-e/basic:2026d)", "00080060  Modality",
		"held: residual_value (name at 00081030)", "2 file(s): 1 passed, 1 held, 0 undecided\n", "never the archive's"} {
		if !strings.Contains(res.stdout, want) {
			t.Errorf("try lacks %q:\n%s", want, res.stdout)
		}
	}
	reqs := r.fake.TrialRequests()
	if len(reqs) != 1 {
		t.Fatalf("%d requests", len(reqs))
	}
	req := reqs[0]
	if len(req.GetFiles()) != 2 || req.GetFiles()[0].GetName() != one || string(req.GetFiles()[1].GetContent()) != string(ct) ||
		req.GetQuarantineRecords()[0] != "id-1" || req.GetReception().GetCallingAeTitle() != "PRISMA1" || req.GetProfile().GetVersion().GetVersion() != 2 || !req.GetShowValues() {
		t.Errorf("the request: %d files, %v, show_values %v", len(req.GetFiles()), req.GetQuarantineRecords(), req.GetShowValues())
	}
	// Without --show-values no value is printed, in the table or the JSON,
	// even from a server that answered them: each element shows its VR, the
	// lengths of its values and whether it changed.
	hidden := r.run("profile", "try", "lab1", "--profile", "research", one)
	hiddenJSON := r.run("profile", "try", "lab1", "--profile", "research", one, "--json")
	for _, out := range []string{hidden.stdout, hiddenJSON.stdout} {
		if strings.Contains(out, "Doe^Peter") {
			t.Errorf("a value was printed without --show-values:\n%s", out)
		}
	}
	for _, want := range []string{"LENGTH", "CHANGED", "PN", "9 -> 0", "yes"} {
		if !strings.Contains(hidden.stdout, want) {
			t.Errorf("try without values lacks %q:\n%s", want, hidden.stdout)
		}
	}
	if last := r.fake.TrialRequests(); last[len(last)-1].GetShowValues() {
		t.Error("a run without --show-values asked the server for values")
	}
	if changed := r.run("profile", "try", "lab1", "--profile", "research", one, "--changed-only"); strings.Contains(changed.stdout, "Modality") {
		t.Errorf("--changed-only shows an element that does not change:\n%s", changed.stdout)
	}
	doc := document(t, r.run("profile", "try", "lab1", "--profile", "research", one, "--json"))
	if rs := doc["results"].([]any); len(rs) != 2 {
		t.Errorf("try --json: %v", doc)
	}
	if held := r.run("profile", "try", "lab1", "--profile", "research", one, "--fail-on-held"); held.code != 5 || !strings.Contains(held.stderr, "1 of 2 files would be held") {
		t.Errorf("--fail-on-held: %+v", held)
	}
	// No file held, but no receiver could name the profile: a failure too.
	r.fake.SetTrialAnswer(&v1.TryProfileResponse{Checksum: strings.Repeat("a", 64), ReceptionRefusal: "it has pixel rules",
		Results: []*v1.TrialResult{{Name: one, Verdict: v1.TrialVerdict_TRIAL_VERDICT_PASSED, DeidentificationOnly: true}},
		Summary: &v1.TrialSummary{Files: 1, Passed: 1}})
	if refused := r.run("profile", "try", "lab1", "--profile", "research", one, "--fail-on-held"); refused.code != 5 || !strings.Contains(refused.stderr, "no receiver could name") ||
		!strings.Contains(refused.stdout, "de-identification only") {
		t.Errorf("--fail-on-held with a reception refusal: %+v", refused)
	}

	sent := len(r.fake.TrialRequests())
	if res := r.run("profile", "try", "lab1", "--profile", "research"); res.code != 2 {
		t.Errorf("no file: %+v", res)
	}
	if res := r.run("profile", "try", "lab1", one); res.code != 2 {
		t.Errorf("no profile: %+v", res)
	}
	// Past the bound by its size alone: a sparse file, never read.
	big := filepath.Join(dir, "big.dcm")
	bf, err := os.Create(big) // #nosec G304 -- the test's own temporary directory.
	if err != nil {
		t.Fatal(err)
	}
	if err := bf.Truncate(1 << 40); err != nil {
		t.Fatal(err)
	}
	_ = bf.Close()
	if res := r.run("profile", "try", "lab1", "--profile", "research", big); res.code != 2 || !strings.Contains(res.stderr, "3MiB") {
		t.Errorf("a file too large: %+v", res)
	}
	many := make([]string, 0, 33)
	for i := range 33 {
		many = append(many, writeFile(t, dir, "m"+strings.Repeat("x", i)+".dcm", "x"))
	}
	if res := r.run(append([]string{"profile", "try", "lab1", "--profile", "research"}, many...)...); res.code != 2 || !strings.Contains(res.stderr, "more than 32") {
		t.Errorf("33 files: %+v", res)
	}
	if got := len(r.fake.TrialRequests()); got != sent {
		t.Errorf("%d requests sent for refused command lines", got-sent)
	}
	// A link to a directory, named on the command line, is followed.
	link := filepath.Join(dir, "link")
	if err := os.Symlink(sub, link); err != nil {
		t.Fatal(err)
	}
	if res := r.run("profile", "try", "lab1", "--profile", "research", link); res.code != 0 {
		t.Errorf("a link to a directory: %+v", res)
	} else if reqs := r.fake.TrialRequests(); len(reqs[len(reqs)-1].GetFiles()) != 1 {
		t.Errorf("a link to a directory sent %d files", len(reqs[len(reqs)-1].GetFiles()))
	}

	r.fake.SetExplainAnswer(&v1.ExplainProfileResponse{Elements: []*v1.ExplainedElement{{Path: "00200010", Keyword: "StudyID", Action: "keep", Rule: 700,
		RuleName: "keep-study-id", SelectedBy: "tag", Reason: "needed", Others: []*v1.ExplainedCandidate{{Rule: 40, RuleName: "e1-00200010", Origin: "annex-e/basic:2026d", SelectedBy: "tag", WhyNot: "earlier"}}}}})
	explain := r.run("profile", "explain", "lab1", "--profile", "research", one)
	if explain.code != 0 || !strings.Contains(explain.stdout, "e1-00200010 (annex-e/basic:2026d): earlier") || !strings.Contains(explain.stdout, "keep-study-id") {
		t.Errorf("explain: %+v", explain)
	}
	if res := r.run("profile", "explain", "lab1", "--profile", "research", one, "--record", "id-1"); res.code != 2 {
		t.Errorf("explain with a file and a record: %+v", res)
	}
}

// TestReceiverProfileFlags guards a receiver's choice of de-identification
// on the command line: --profile NAME@VERSION sent as the oneof's profile
// and shown, a profile without its version refused before anything is
// sent, --as-received on set, and the two together refused.
func TestReceiverProfileFlags(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")
	add := r.run("receiver", "add", "lab1", "research", "--storage", "main", "--port", "11115", "--aet", "RESEARCH", "--profile", "research@3")
	if add.code != 0 || !strings.Contains(add.stdout, "De-identification: profile research@3\n") {
		t.Errorf("add --profile: %+v", add)
	}
	if res := r.run("receiver", "add", "lab1", "other", "--storage", "main", "--port", "11116", "--aet", "OTHER", "--profile", "research"); res.code != 2 || !strings.Contains(res.stderr, "NAME@VERSION") {
		t.Errorf("a profile with no version: %+v", res)
	}
	set := r.run("receiver", "set", "lab1", "research", "--as-received")
	if set.code != 0 || !strings.Contains(set.stdout, "De-identification: none: archived as received\n") {
		t.Errorf("set --as-received: %+v", set)
	}
	if res := r.run("receiver", "set", "lab1", "research", "--as-received", "--profile", "research@3"); res.code != 2 {
		t.Errorf("both: %+v", res)
	}
}
