package api_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/api"
)

// The variables of the pydicom comparison, listed in docs/testing.md.
const (
	// pythonEnv names the Python, with pydicom, that runs the comparison.
	// Set, even to nothing, it means the comparison was asked for, and a
	// missing interpreter fails it.
	pythonEnv = "NOPACS_CHECK_METADATA_PYTHON"
	// corpusEnv names pydicom's directory of public test files, from which
	// more files (implicit VR, big endian, deflated and others) are
	// received for the comparison.
	corpusEnv = "NOPACS_PYDICOM_CORPUS"
)

// moreCorpus are the files of pydicom's corpus the comparison adds when
// corpusEnv names it, each with the digit its SOP Instance UID is made to
// end in, since each shares its SOP Instance UID with another file.
var moreCorpus = []struct {
	name string
	last byte
}{
	{"MR_small_implicit.dcm", '1'}, {"MR_small_bigendian.dcm", '2'}, {"MR_small_padded.dcm", '3'},
	{"MR_small_jpeg_ls_lossless.dcm", '4'}, {"MR_small_expb.dcm", '5'},
}

// TestMetadataEqualsPydicom guards that the DICOM JSON the service answers
// for every instance of an archive made by reception equals pydicom's
// Dataset.to_json_dict() of the file that was received, but for the bulk
// data references, which must name exactly pydicom's bytes. It runs
// scripts/check/metadata-pydicom.py and fails on any difference; the
// script's output is logged. It needs the Python that pythonEnv names.
func TestMetadataEqualsPydicom(t *testing.T) {
	python, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skip(pythonEnv + " is not set; `make check-metadata` runs this comparison")
	}
	if python == "" {
		t.Fatal(pythonEnv + " is empty; name an interpreter that has pydicom")
	}
	files, _ := metaFiles(t)
	if dirName := os.Getenv(corpusEnv); dirName != "" {
		for _, c := range moreCorpus {
			b, err := os.ReadFile(filepath.Join(dirName, c.name)) // #nosec G304 G703 -- the operator's corpus.
			if err != nil {
				t.Fatal(err)
			}
			b = reUID(t, b, c.last)
			files = append(files, b)
		}
	}
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, files)
	md := f.as(f.lab1Reader).metadata
	work := t.TempDir()
	type entry struct {
		File    string `json:"file"`
		ID      string `json:"id"`
		JSON    string `json:"json"`
		Omitted bool   `json:"omitted"`
	}
	var entries []entry
	for _, path := range a.scans(t) {
		res := mustCall[*v1.GetScanMetadataResponse](t, "scan", call(md.GetScanMetadata, scanRequest(path)))
		for _, m := range res.GetInstances() {
			if m.GetJsonTooLarge() {
				t.Fatalf("%s: too large", m.GetInstanceId())
			}
			name := filepath.Join(work, m.GetInstanceId()+".dcm")
			if err := os.WriteFile(name, a.sent[m.GetInstanceId()], 0o600); err != nil { // #nosec G703 -- an instance id the test's own server answered, under its temporary directory.
				t.Fatal(err)
			}
			entries = append(entries, entry{File: name, ID: m.GetInstanceId(), JSON: m.GetDicomJson(), Omitted: m.GetBulkDataOmitted()})
		}
	}
	// A deflated file cannot be archived (its pixel data lies outside the
	// compressed file), so its answer is made from a storage that holds it.
	dfl := deflatedFile(t, corpusFile(t, "MR_small.dcm"), '0')
	store, path, held, line := storedInstance(t, dfl)
	m, err := api.MetaInstance(context.Background(), store, "lab1", path, 1<<20, held, line)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(work, "deflated.dcm")
	if err := os.WriteFile(name, dfl, 0o600); err != nil {
		t.Fatal(err)
	}
	entries = append(entries, entry{File: name, ID: held.SOPInstanceUID, JSON: m.GetDicomJson(), Omitted: m.GetBulkDataOmitted()})
	if len(entries) != len(files)+1 {
		t.Fatalf("%d instances answered, %d received", len(entries), len(files))
	}
	manifest := filepath.Join(work, "answers.json")
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join("..", "..", "scripts", "check", "metadata-pydicom.py")
	out, err := exec.Command(python, "-I", script, manifest).CombinedOutput() // #nosec G204 -- the operator's interpreter.
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the comparison with pydicom failed: %v", err)
	}
	if !strings.Contains(string(out), "0 differ") {
		t.Fatal("the comparison did not report that no instance differs")
	}
}
