package cli_test

import (
	"os"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// TestDeidCode guards "deid code": --patient-id sends the value as Patient
// ID, and --patient-id - reads it from standard input; --attribute with
// --value - reads the value from standard input; the
// answer shows the keyed hash, each receiver's subject label and whether
// it is held, and each table's code, and --json is the answer; the value
// is never shown; giving no value, or --patient-id with --value, is
// refused before anything is sent.
func TestDeidCode(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.fake.SetCodeAnswer(&v1.CodeResponse{Attribute: "00100020", Code: "abcdefghijklmnop",
		Subjects: []*v1.SubjectLabelCode{{Receiver: "scanner1", SubjectLabel: "sub-abcdefghijklmnop", Held: true}},
		Lookups:  []*v1.LookupTableCode{{Table: "subjects", Code: "P001"}}})
	res := r.run("deid", "code", "lab1", "--patient-id", "SECRET-ID-1")
	if res.code != 0 || !strings.Contains(res.stdout, "abcdefghijklmnop") || !strings.Contains(res.stdout, "scanner1") ||
		!strings.Contains(res.stdout, "sub-abcdefghijklmnop  yes") || !strings.Contains(res.stdout, "subjects      P001") {
		t.Errorf("deid code:\n%+v", res)
	}
	if strings.Contains(res.stdout+res.stderr, "SECRET-ID-1") {
		t.Error("the value was shown")
	}
	in := typed(r.env, "SECRET-ID-2\n", "deid", "code", "lab1", "--attribute", "AccessionNumber", "--value", "-", "--json")
	d := document(t, in)
	if d["code"] != "abcdefghijklmnop" || strings.Contains(in.stdout, "SECRET-ID-2") {
		t.Errorf("deid code --json: %v", d)
	}
	piped := typed(r.env, "SECRET-ID-3\n", "deid", "code", "lab1", "--patient-id", "-")
	if piped.code != 0 || strings.Contains(piped.stdout, "SECRET-ID-3") {
		t.Errorf("--patient-id -: %+v", piped)
	}
	sent := r.fake.CodeRequests()
	if len(sent) != 3 || sent[2].GetAttribute() != "PatientID" || sent[2].GetValue() != "SECRET-ID-3" || sent[0].GetAttribute() != "PatientID" || sent[0].GetValue() != "SECRET-ID-1" ||
		sent[1].GetAttribute() != "AccessionNumber" || sent[1].GetValue() != "SECRET-ID-2" {
		t.Errorf("the requests: %v", sent)
	}
	for _, args := range [][]string{
		{"deid", "code", "lab1"},
		{"deid", "code", "lab1", "--patient-id", "X", "--value", "Y"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("%v: %+v", args, res)
		}
	}
	if n := len(r.fake.CodeRequests()); n != 3 {
		t.Errorf("a refused command sent a request: %d", n)
	}
}

// TestProfileLookupCommands guards "profile lookup import" and "list":
// a CSV with a header imported, the counts shown; again, every entry
// unchanged; a file whose value the table codes otherwise refused with
// exit 5 and nothing stored; a malformed CSV refused naming its line, not
// a value; list shows each table's count.
func TestProfileLookupCommands(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	dir := t.TempDir()
	good := writeFile(t, dir, "codes.csv", "value,code\nPID-0001,P001\nPID-0002,P002\n")
	res := r.run("profile", "lookup", "import", "lab1", "subjects", good)
	if res.code != 0 || !strings.Contains(res.stdout, "Added:              2") || strings.Contains(res.stdout, "PID-0001") {
		t.Fatalf("import:\n%+v", res)
	}
	if d := document(t, r.run("profile", "lookup", "import", "lab1", "subjects", good, "--json")); d["unchanged"] != float64(2) {
		t.Errorf("again: %v", d)
	}
	other := writeFile(t, dir, "other.csv", "PID-0003,P003\nPID-0001,P009\n")
	if res := r.run("profile", "lookup", "import", "lab1", "subjects", other); res.code != 5 {
		t.Errorf("a value coded otherwise: %+v", res)
	}
	bad := writeFile(t, dir, "bad.csv", "PID-0004,P004,extra\n")
	if res := r.run("profile", "lookup", "import", "lab1", "subjects", bad); res.code != 2 || !strings.Contains(res.stderr, "line 1") || strings.Contains(res.stderr, "PID-0004") {
		t.Errorf("a malformed CSV: %+v", res)
	}
	imports := r.fake.ImportRequests()
	if len(imports) != 3 || len(imports[0].GetEntries()) != 2 || imports[0].GetEntries()[0].GetCode() != "P001" {
		t.Errorf("the imports sent: %v", imports)
	}
	list := r.run("profile", "lookup", "list", "lab1")
	if list.code != 0 || !strings.Contains(list.stdout, "subjects  2") {
		t.Errorf("list:\n%+v", list)
	}
	if d := document(t, r.run("profile", "lookup", "list", "lab1", "--json")); len(d["tables"].([]any)) != 1 {
		t.Errorf("list --json: %v", d)
	}
}

// TestRouteExportImport guards "receiver route export" and "import": the
// export holds the receiver, its update time and its routing; edited and
// imported it replaces the routing whole; a file whose update time is
// not the receiver's is refused with exit 5 and changes nothing, unless
// --ignore-update-time; a misspelled field is refused.
func TestRouteExportImport(t *testing.T) {
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")
	r.must("storage", "add", "lab1", "lab-a", "--type", "dir", "--root", "fast")
	r.must("receiver", "add", "lab1", "scanner1", "--storage", "main", "--port", "11112", "--aet", "NOPACS", "--any-caller", "--profile", "research@1")
	r.must("receiver", "route", "add", "lab1", "scanner1", "lab-a", "--storage", "lab-a", "--equals", "calling-aet=LAB_A")
	dir := t.TempDir()
	file := dir + "/routes.json"
	r.must("receiver", "route", "export", "lab1", "scanner1", "--to", file)
	d := document(t, r.run("receiver", "route", "export", "lab1", "scanner1", "--json"))
	if d["receiver"] != "scanner1" || d["updateTime"] == "" || d["routing"] == nil {
		t.Fatalf("export: %v", d)
	}
	text := readText(t, file)
	edited := writeFile(t, dir, "edited.json", strings.Replace(text, "LAB_A", "LAB_B", 1))
	res := r.run("receiver", "route", "import", "lab1", "scanner1", edited)
	if res.code != 0 || !strings.Contains(res.stdout, `"LAB_B"`) {
		t.Fatalf("import:\n%+v", res)
	}
	stale := writeFile(t, dir, "stale.json", strings.Replace(text, d["updateTime"].(string), "2001-01-01T00:00:00Z", 1))
	if res := r.run("receiver", "route", "import", "lab1", "scanner1", stale); res.code != 5 {
		t.Errorf("a stale export: %+v", res)
	}
	if res := r.run("receiver", "route", "list", "lab1", "scanner1"); !strings.Contains(res.stdout, "LAB_B") {
		t.Errorf("a refused import changed the routing:\n%s", res.stdout)
	}
	if res := r.run("receiver", "route", "import", "lab1", "scanner1", stale, "--ignore-update-time"); res.code != 0 || !strings.Contains(res.stdout, "LAB_A") {
		t.Errorf("--ignore-update-time: %+v", res)
	}
	typo := writeFile(t, dir, "typo.json", strings.Replace(text, `"routes"`, `"rutes"`, 1))
	if res := r.run("receiver", "route", "import", "lab1", "scanner1", typo); res.code != 2 {
		t.Errorf("a misspelled field: %+v", res)
	}
}

// readText reads a file the test wrote.
func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path) // #nosec G304 -- the test's own file.
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
