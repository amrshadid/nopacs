package api_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// dicomwebPythonEnv names the Python, with dicomweb-client and pydicom,
// that runs the outside check of DICOMweb (docs/develop/testing.md). Set, even to
// nothing, it means the check was asked for, and a missing interpreter
// fails it.
const dicomwebPythonEnv = "NOPACS_CHECK_DICOMWEB_PYTHON"

// TestDICOMwebWithDicomwebClient guards DICOMweb against a client that is
// not NoPACs': scripts/check/dicomweb-client.py searches every study,
// series and instance of an archive made by reception with
// dicomweb-client, retrieves each instance, its metadata, its bulk data
// and frames, and compares each with pydicom's reading of the file
// received. It fails on any difference; the script's output is logged.
// corpusEnv adds files of pydicom's corpus.
func TestDICOMwebWithDicomwebClient(t *testing.T) {
	python, asked := os.LookupEnv(dicomwebPythonEnv)
	if !asked {
		t.Skip(dicomwebPythonEnv + " is not set; it names a Python with dicomweb-client and pydicom")
	}
	if python == "" {
		t.Fatal(dicomwebPythonEnv + " is empty; name an interpreter that has dicomweb-client and pydicom")
	}
	files, _ := metaFiles(t)
	files = append(files, corpusFile(t, "jpeg_3frame_nobot.dcm"), corpusFile(t, "SC_rgb_rle_2frame.dcm"))
	if dirName := os.Getenv(corpusEnv); dirName != "" {
		// Implicit VR, big endian, padded and JPEG-LS files of pydicom's
		// corpus, as the metadata comparison receives them.
		for _, c := range moreCorpus {
			b, err := os.ReadFile(filepath.Join(dirName, c.name)) // #nosec G304 G703 -- the operator's corpus.
			if err != nil {
				t.Fatal(err)
			}
			files = append(files, reUID(t, b, c.last))
		}
	}
	f := newPlaceFixture(t)
	a := receiveArchive(t, f, files)
	s := serveWeb(t, f.env)
	work := t.TempDir()
	args := []string{"-I", filepath.Join("..", "..", "scripts", "check", "dicomweb-client.py"), s.url + "/tenants/lab1/dicom-web"}
	for uid, b := range a.sent {
		name := filepath.Join(work, uid+".dcm")
		if err := os.WriteFile(name, b, 0o600); err != nil {
			t.Fatal(err)
		}
		args = append(args, name)
	}
	cmd := exec.Command(python, args...) // #nosec G204 -- the operator's interpreter.
	cmd.Env = append(os.Environ(), "NOPACS_DICOMWEB_KEY="+f.lab1Reader.key)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("the check with dicomweb-client failed: %v", err)
	}
	if !strings.Contains(string(out), "; 0 differ") {
		t.Fatal("the check did not report that nothing differs")
	}
}
