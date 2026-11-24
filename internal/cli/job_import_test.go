package cli_test

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// importRig is a rig with tenant lab1, a storage main and an import
// receiver old-tree over the root old.
func importRig(t *testing.T) *rig {
	t.Helper()
	r := newRig(t)
	r.must("tenant", "create", "lab1")
	r.must("storage", "add", "lab1", "main", "--type", "dir", "--root", "fast")
	r.must("receiver", "add", "lab1", "old-tree", "--type", "import", "--storage", "main", "--root", "old", "--layout", "pattern",
		"--pattern", "^(?P<project>[^/]+)/(?P<session>[^/]+)/.+$", "--unit-depth", "2", "--map-file", "subjects.csv",
		"--map-key-column", "project", "--map-key-column", "label", "--map-key-part", "project", "--map-key-part", "session",
		"--map-value-column", "subject_label", "--map-gives", "subject", "--enabled")
	return r
}

// TestImportReceiverCommands guards the import receiver's flags: add sends
// the root, layout, pattern, unit depth and map configuration, show words
// them with the receiver idle, set changes one field by its own mask path,
// and flags of another type are refused before anything is sent.
func TestImportReceiverCommands(t *testing.T) {
	r := importRig(t)
	show := r.run("receiver", "show", "lab1", "old-tree")
	for label, want := range map[string]string{"Type": "import", "Health": "idle", "Root": "old", "Layout": "pattern", "Unit depth": "2",
		"Map file": "subjects.csv", "Map": "subject from subject_label by columns project, label of parts project, session"} {
		if got := field(show.stdout, label); show.code != 0 || got != want {
			t.Errorf("receiver show: %s is %q, want %q:\n%s", label, got, want, show.stdout)
		}
	}
	set := document(t, r.run("receiver", "set", "lab1", "old-tree", "--pattern", "^(?P<project>[^/]+)/x/(?P<session>[^/]+)/.+$", "--json"))
	im := set["receiver"].(map[string]any)["importer"].(map[string]any)
	if im["pathPattern"] != "^(?P<project>[^/]+)/x/(?P<session>[^/]+)/.+$" || im["root"] != "old" {
		t.Errorf("receiver set --pattern: %v", im)
	}
	for _, args := range [][]string{
		{"receiver", "add", "lab1", "x", "--type", "import", "--storage", "main", "--root", "old", "--layout", "plain", "--port", "11112"},
		{"receiver", "add", "lab1", "x", "--storage", "main", "--aet", "X", "--root", "old"},
		{"receiver", "add", "lab1", "x", "--type", "import", "--storage", "main", "--root", "old", "--layout", "sideways"},
	} {
		if res := r.run(args...); res.code != 2 {
			t.Errorf("%v: %+v, want a usage error", args, res)
		}
	}
}

// TestImportJobCommands guards "job import", "job files" and "job units":
// the job is made with its path and the map read from a local file, shown
// with its counts, and its files and units listed by path with why, in both
// forms; a map file and a local map together are refused.
func TestImportJobCommands(t *testing.T) {
	r := importRig(t)
	mapPath := writeFile(t, t.TempDir(), "subjects.csv", "project,label,subject_label\nproj1,sess1,subj1\n")
	made := document(t, r.run("job", "import", "lab1", "old-tree", "proj1", "--map-csv", mapPath, "--json"))
	ij := made["job"].(map[string]any)["importJob"].(map[string]any)
	if ij["receiver"] != "old-tree" || ij["path"] != "proj1" || ij["mapCsv"] == nil {
		t.Errorf("job import --json: %v", ij)
	}
	show := r.run("job", "show", "lab1", "job_1")
	for label, want := range map[string]string{"Imports": "proj1", "Receiver": "old-tree", "Units": "0 of 1 walked", "Found": "0"} {
		if got := field(show.stdout, label); show.code != 0 || got != want {
			t.Errorf("job show: %s is %q, want %q:\n%s", label, got, want, show.stdout)
		}
	}
	if res := r.run("job", "import", "lab1", "old-tree", "proj1", "--map-file", "a.csv", "--map-csv", mapPath); res.code != 2 {
		t.Errorf("both maps: %+v", res)
	}
	if res := r.run("job", "import", "lab1", "old-tree", "proj1", "--map-csv", filepath.Join(t.TempDir(), "none.csv")); res.code != 2 {
		t.Errorf("a local map not there: %+v", res)
	}
	// A device or a directory has no size to bound and may never end (a
	// FIFO, /dev/zero): it is refused before it is read, not sent empty.
	notRegular := []string{t.TempDir()}
	if runtime.GOOS != "windows" {
		notRegular = append(notRegular, "/dev/null", "/dev/zero")
	}
	for _, p := range notRegular {
		if res := r.run("job", "import", "lab1", "old-tree", "proj1", "--map-csv", p); res.code != 2 || !strings.Contains(res.stderr, "not a regular file") {
			t.Errorf("a local map that is not a regular file (%s): %+v", p, res)
		}
	}
	big := writeFile(t, t.TempDir(), "big.csv", strings.Repeat("a", 3<<20+1))
	if res := r.run("job", "import", "lab1", "old-tree", "proj1", "--map-csv", big); res.code != 2 || !strings.Contains(res.stderr, "3 MiB") {
		t.Errorf("a local map past 3 MiB: %+v", res)
	}

	at := timestamppb.New(fixedNow)
	r.fake.AddImportFile("lab1", "job_1", &v1.ImportFile{Path: "proj1/notes.txt", Kind: v1.ImportFileKind_IMPORT_FILE_KIND_SKIPPED,
		SkipCode: v1.ImportSkipCode_IMPORT_SKIP_CODE_NOT_PART10, Unit: "u0", SizeBytes: 24, Time: at})
	r.fake.AddImportFile("lab1", "job_1", &v1.ImportFile{Path: "proj1/sess1/a.dcm", Kind: v1.ImportFileKind_IMPORT_FILE_KIND_QUARANTINED,
		QuarantineReason: v1.QuarantineReason_QUARANTINE_REASON_BAD_LABEL, QuarantinedFile: "q-1", Unit: "u1", SizeBytes: 2048, Time: at})
	r.fake.AddImportUnit("lab1", "job_1", &v1.ImportUnit{Id: "u0", Folder: "proj1", State: v1.ImportUnitState_IMPORT_UNIT_STATE_DONE,
		Counts: &v1.ImportCounts{Found: 1, Skipped: []*v1.ImportSkipCount{{Code: v1.ImportSkipCode_IMPORT_SKIP_CODE_NOT_PART10, Count: 1}}, Units: 1, UnitsWalked: 1}})
	r.fake.AddImportUnit("lab1", "job_1", &v1.ImportUnit{Id: "u1", Folder: "proj1/sess1", State: v1.ImportUnitState_IMPORT_UNIT_STATE_WALKED,
		Counts: &v1.ImportCounts{Found: 1, HandedIn: 1, Processing: 1, Units: 1, UnitsWalked: 1}})

	files := r.run("job", "files", "lab1", "job_1")
	if files.code != 0 || !strings.Contains(files.stdout, "proj1/notes.txt") || !strings.Contains(files.stdout, "skipped: not_part10") ||
		!strings.Contains(files.stdout, "quarantined: bad_label") || !strings.Contains(files.stdout, "q-1") {
		t.Errorf("job files:\n%s", files.stdout)
	}
	only := document(t, r.run("job", "files", "lab1", "job_1", "--kind", "quarantined", "--json"))
	if list := only["files"].([]any); len(list) != 1 || list[0].(map[string]any)["path"] != "proj1/sess1/a.dcm" {
		t.Errorf("job files --kind quarantined: %v", only)
	}
	units := r.run("job", "units", "lab1", "job_1", "--state", "walked")
	if units.code != 0 || !strings.Contains(units.stdout, "proj1/sess1") || strings.Contains(units.stdout, "u0 ") {
		t.Errorf("job units --state walked:\n%s", units.stdout)
	}
	if res := r.run("job", "files", "lab1", "job_1", "--kind", "sideways"); res.code != 2 {
		t.Errorf("job files --kind sideways: %+v", res)
	}
}
