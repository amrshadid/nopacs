package dirimport

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/layout"
)

// pythonEnv names the Python, with pydicom, the outside test reads the
// archive with (docs/testing.md).
const pythonEnv = "NOPACS_PYTHON"

// readBack is the outside check's script: given the storage's data
// directory and a JSON list of [tree file, labels], it reads every
// archived file and every listed tree file with pydicom and prints one line
// "labels SOP-Instance-UID Study-UID Series-UID" for each, under "stored"
// and "expected", so the two can be compared.
const readBack = `
import json, os, sys
import pydicom
data, listing = sys.argv[1], json.load(open(sys.argv[2]))
out = {"stored": [], "expected": []}
for d, _, names in os.walk(data):
    for n in names:
        if n.endswith(".dcm"):
            ds = pydicom.dcmread(os.path.join(d, n))
            out["stored"].append(" ".join([os.path.relpath(d, data), ds.SOPInstanceUID, ds.StudyInstanceUID, ds.SeriesInstanceUID]))
for path, labels in listing:
    ds = pydicom.dcmread(path)
    out["expected"].append(" ".join([labels, ds.SOPInstanceUID, ds.StudyInstanceUID, ds.SeriesInstanceUID]))
print(json.dumps(out))
`

// python returns a Python with pydicom, or skips the test.
func python(t *testing.T) string {
	t.Helper()
	p := os.Getenv(pythonEnv)
	if p == "" {
		var err error
		if p, err = exec.LookPath("python3"); err != nil {
			t.Skip("no python3, and " + pythonEnv + " is not set")
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := exec.CommandContext(ctx, p, "-I", "-c", "import pydicom").Run(); err != nil { // #nosec G204 G702 -- the test's own interpreter, from its environment.
		t.Skip("the Python at " + p + " has no pydicom; set " + pythonEnv)
	}
	return p
}

// TestOutsidePydicomReadsTheImport guards the import against another
// implementation: pydicom reads every file the import archived, and the
// SOP Instance, Study and Series UIDs it finds under each subject, session
// and scan label are exactly those of the tree's files that the tree's
// paths and the map name so; a file held twice in the tree is archived
// once.
func TestOutsidePydicomReadsTheImport(t *testing.T) {
	py := python(t)
	e, files := runImport(t, "")
	var listing [][2]string
	for _, f := range files {
		if f.labels[0] != "" {
			listing = append(listing, [2]string{filepath.Join(e.tree, filepath.FromSlash(f.path)), strings.Join(f.labels[:], "/")})
		}
	}
	list, err := json.Marshal(listing)
	if err != nil {
		t.Fatal(err)
	}
	listFile := filepath.Join(t.TempDir(), "listing.json")
	if err := os.WriteFile(listFile, list, 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	data := filepath.Join(e.storeDir, filepath.FromSlash(layout.DataRoot))
	out, err := exec.CommandContext(ctx, py, "-I", "-c", readBack, data, listFile).Output() // #nosec G204 G702 -- the test's own interpreter and files.
	if err != nil {
		t.Fatalf("pydicom could not read the archive back: %v", err)
	}
	var got struct{ Stored, Expected []string }
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	slices.Sort(got.Stored)
	slices.Sort(got.Expected)
	if len(got.Stored) != 9 || !slices.Equal(got.Stored, got.Expected) {
		t.Errorf("pydicom read the archive as\n%s\nand the tree as\n%s", strings.Join(got.Stored, "\n"), strings.Join(got.Expected, "\n"))
	}
	t.Logf("pydicom read %d archived files, each under the labels its tree path names:\n%s", len(got.Stored), strings.Join(got.Stored, "\n"))
}
