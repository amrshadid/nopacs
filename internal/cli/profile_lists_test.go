package cli_test

import (
	"regexp"
	"strings"
	"testing"
)

// regexpMatch reports whether pattern matches s.
func regexpMatch(pattern, s string) bool { return regexp.MustCompile(pattern).MatchString(s) }

// listedTerm is a term no other text of the tests holds.
const listedTerm = "Zebulonquist"

// listsJSON is a document with two text lists and rules naming them.
const listsJSON = `{"bases": ["annex-e/basic:2026d"],
 "textLists": [{"name": "staff", "version": 3, "terms": ["Ann Lee", "` + listedTerm + `"]},
               {"name": "sites", "version": 1, "terms": ["North Clinic"]}],
 "rules": [{"name": "clean-desc", "selector": {"keyword": "StudyDescription"}, "action": {"clean": {"lists": ["staff", "sites"]}}},
           {"name": "cut-comments", "selector": {"keyword": "ImageComments"}, "action": {"redact": {"lists": ["staff"], "replacement": "[x]"}}}]}`

// TestProfileCommandsShowListSummaries guards what "profile describe" and
// "profile diff" print of text lists, in a person's view and with --json:
// describe, of a stored version and of a document, each list's name,
// version, count of terms and checksum, the expanded form in --json given
// without its lists; diff, between two versions differing only in a
// list's terms and version, that list with both sides' summaries; and
// never a term.
func TestProfileCommandsShowListSummaries(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	path := writeFile(t, t.TempDir(), "lists.json", listsJSON)
	if res := r.run("profile", "create", "lab1", "lists", "--document", path); res.code != 0 {
		t.Fatalf("create: %+v", res)
	}
	text := r.run("profile", "describe", "lab1", "lists@1")
	if text.code != 0 || !strings.Contains(text.stdout, "Text lists (terms not shown):") ||
		!strings.Contains(text.stdout, "list staff v3: 2 terms, sha256 ") || !strings.Contains(text.stdout, "list sites v1: 1 terms, sha256 ") {
		t.Errorf("describe: %+v", text)
	}
	for _, args := range [][]string{
		{"profile", "describe", "lab1", "lists@1", "--json"},
		{"profile", "describe", "lab1", "--document", path, "--json"},
	} {
		res := r.run(args...)
		d := document(t, res)
		lists, _ := d["textLists"].([]any)
		if len(lists) != 2 {
			t.Fatalf("%v: textLists %v", args, d["textLists"])
		}
		first, _ := lists[0].(map[string]any)
		if first["name"] != "staff" || first["version"] != float64(3) || first["terms"] != float64(2) || len(first["sha256"].(string)) != 64 {
			t.Errorf("%v: the first summary %v", args, first)
		}
		if exp, _ := d["expanded"].(map[string]any); exp["textLists"] != nil {
			t.Errorf("%v: the expanded form holds the lists", args)
		}
	}
	// A second version whose only change is the staff list's terms, and a
	// new version number for it: profile diff names the list with both
	// sides' summaries, in a person's view and with --json.
	changed := strings.Replace(strings.Replace(listsJSON, `"`+listedTerm+`"`, `"Bea Moss", "Cy Nolan"`, 1), `"version": 3`, `"version": 4`, 1)
	if res := r.run("profile", "create", "lab1", "lists", "--document", writeFile(t, t.TempDir(), "lists2.json", changed)); res.code != 0 {
		t.Fatalf("create version 2: %+v", res)
	}
	diff := r.run("profile", "diff", "lab1", "lists@1", "lists@2")
	if diff.code != 0 || !strings.Contains(diff.stdout, "Text lists (terms not shown):") || !regexpMatch(`changed\s+staff\s+v3, 2 terms, [0-9a-f]{12}\s+v4, 3 terms, [0-9a-f]{12}`, diff.stdout) {
		t.Errorf("diff: %+v", diff)
	}
	dj := r.run("profile", "diff", "lab1", "lists@1", "lists@2", "--json")
	lists, _ := document(t, dj)["textLists"].([]any)
	if len(lists) != 1 {
		t.Fatalf("diff --json: %s", dj.stdout)
	}
	c, _ := lists[0].(map[string]any)
	from, _ := c["from"].(map[string]any)
	to, _ := c["to"].(map[string]any)
	if c["name"] != "staff" || c["change"] != "PROFILE_CHANGE_CHANGED" || from["terms"] != float64(2) || to["terms"] != float64(3) ||
		to["version"] != float64(4) || from["sha256"] == to["sha256"] {
		t.Errorf("diff --json: %v", c)
	}
	for _, res := range []result{text, diff, dj, r.run("profile", "describe", "lab1", "lists@1", "--json"), r.run("profile", "describe", "lab1", "--document", path, "--json")} {
		if strings.Contains(strings.ToLower(res.stdout+res.stderr), strings.ToLower(listedTerm)) {
			t.Errorf("describe printed a term: %s", res.stdout)
		}
	}
}
