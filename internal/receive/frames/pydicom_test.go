package frames

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// The variables of the pydicom comparison, listed in docs/testing.md.
const (
	// pythonEnv names the Python, with pydicom, that runs the comparison.
	// Set, even to nothing, it means the comparison was asked for, and a
	// missing interpreter fails it.
	pythonEnv = "NOPACS_CHECK_FRAMES_PYTHON"
	// corpusEnv names pydicom's directory of public test files: every file
	// of it that is a DICOM file with pixel data is compared, and the files
	// the corpus lacks are made from it.
	corpusEnv = "NOPACS_PYDICOM_CORPUS"
)

// TestTableEqualsPydicom guards the frame table of every file it is given
// against pydicom: the files of internal/receive/testdata, and with
// corpusEnv set every file of pydicom's corpus with pixel data (JPEG
// baseline and extended, JPEG lossless, JPEG-LS, JPEG 2000, RLE, the
// multi-frame ones, native files in each byte order) and the files
// scripts/check/frames-pydicom.py builds from it (an empty Basic Offset
// Table with three fragments a frame; an Extended Offset Table). The script
// compares each table, range for range, with the fragments pydicom divides
// each frame into, found by walking the items itself, and the bytes each
// range names with pydicom's; a file whose frames are not byte ranges must
// be one pydicom cannot divide either. It needs the Python that pythonEnv
// names.
func TestTableEqualsPydicom(t *testing.T) {
	python, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skip(pythonEnv + " is not set; this comparison runs when it names a Python with pydicom")
	}
	if python == "" {
		t.Fatal(pythonEnv + " is empty; name an interpreter that has pydicom")
	}
	script := filepath.Join("..", "..", "..", "scripts", "check", "frames-pydicom.py")
	names, _ := filepath.Glob(filepath.Join("..", "testdata", "*.dcm"))
	if corpus := os.Getenv(corpusEnv); corpus != "" {
		built := t.TempDir()
		out, err := exec.Command(python, "-I", script, "build", corpus, built).CombinedOutput() // #nosec G204 G702 -- the operator's interpreter and corpus.
		if err != nil {
			t.Fatalf("building the files the corpus lacks: %v\n%s", err, out)
		}
		more, _ := filepath.Glob(filepath.Join(corpus, "*.dcm"))
		made, _ := filepath.Glob(filepath.Join(built, "*.dcm"))
		names = append(append(names, more...), made...)
	}
	type entry struct {
		File   string            `json:"file"`
		Frames [][]sidecar.Range `json:"frames"`
	}
	var entries []entry
	encapsulated := 0
	for _, name := range names {
		data, err := os.ReadFile(name) // #nosec G304 G703 -- the test's own files and the operator's corpus.
		if err != nil {
			t.Fatal(err)
		}
		h, err := header.Read(bytes.NewReader(data), int64(len(data)))
		if err != nil || !h.HasPixelData {
			// Not a whole DICOM file with pixel data: nothing has a table.
			continue
		}
		table, err := Table(h, bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		e := entry{File: name}
		if len(table) > 0 {
			e.Frames = table
		}
		if h.TransferSyntaxUID != header.TransferSyntaxDeflated && table != nil && len(table) > 0 && len(table[0]) > 0 &&
			table[0][0].Offset > h.PixelData.Offset+12 {
			encapsulated++
		}
		entries = append(entries, e)
	}
	if encapsulated < 3 {
		t.Fatalf("only %d encapsulated files were compared", encapsulated)
	}
	work := filepath.Join(t.TempDir(), "tables.json")
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(work, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(python, "-I", script, "check", work).CombinedOutput() // #nosec G204 -- the operator's interpreter.
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the comparison with pydicom failed: %v", err)
	}
	if !strings.Contains(string(out), " 0 differ") {
		t.Fatal("the comparison did not report that no file differs")
	}
}
