package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// The texts the list test writes into each file, and what clean must
// leave of them: the terms cut where they stand as words, a term inside a
// longer word ("Zebulonquistian") kept, every other character kept.
const (
	listComments     = "Reviewed by Zebulonquist Okonkwo-Hale at NORTH RIVERBEND CLINIC; zebulonquistian method"
	listCommentsWant = "Reviewed by  at ; zebulonquistian method"
	listDesc         = "CT head, read by zebulonquist"
	listDescWant     = "CT head, read by"
)

// listTerms are the list of the test.
var listTerms = []string{"Zebulonquist Okonkwo-Hale", "North Riverbend Clinic", "Zebulonquist"}

// TestListCleanAgainstPydicom guards clean with a list over public files,
// judged by pydicom: each of the receiver's test files (and, with
// NOPACS_PYDICOM_CORPUS set, every file of pydicom's corpus) gets an Image
// Comments and a Study Description holding the list's terms, is
// de-identified by a profile that cleans both with the list and keeps what
// no rule names, and is read back by pydicom
// (scripts/check/deid-lists-pydicom.py),
// which finds no term standing as a word anywhere, the two texts as
// expected, and every element the engine kept as it was. It runs with
// NOPACS_CHECK_DEID_PYTHON set to a Python with pydicom.
func TestListCleanAgainstPydicom(t *testing.T) {
	py, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skipf("%s is not set: the outside check of the lists needs a Python with pydicom", pythonEnv)
	}
	files := corpusFiles(t, receiveTestdata)
	if dir := os.Getenv(corpusEnv); dir != "" {
		files = append(files, corpusFiles(t, dir)...)
	}
	en := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{
		TextLists: []*v1.TextList{{Name: "staff", Version: 1, Terms: listTerms}},
		Rules: []*v1.ProfileRule{
			rule("comments", kw("ImageComments"), aCleanLists("staff")),
			rule("desc", kw("StudyDescription"), aCleanLists("staff")),
		},
		PixelRules: []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the attributes are judged here"}},
	})
	dir := t.TempDir()
	type pair struct {
		Original string            `json:"original"`
		Result   string            `json:"result"`
		Cleaned  map[string]string `json:"cleaned"`
		Kept     []string          `json:"kept"`
	}
	var pairs []pair
	for i, path := range files {
		ds, err := readPath(t, path)
		if err != nil {
			continue
		}
		if ts := ds.TransferSyntaxUID(); ts == "1.2.840.10008.1.2.2" || ts == "1.2.840.10008.1.2.1.99" {
			continue
		}
		if cs, ok := text(ds, tSpecificCharacterSet); ok && cs != "" && cs != "ISO_IR 100" && cs != "ISO_IR 192" {
			// The texts written here are ASCII, but the file's own
			// declaration is the reader's to apply; kept to the sets
			// pydicom and the library read alike.
			continue
		}
		for tg, v := range map[uint32]string{tImageComments: listComments, tStudyDesc: listDesc} {
			vr := "LT"
			if tg == tStudyDesc {
				vr = "LO"
			}
			if err := ds.Add(dataelem.NewDataElement(tag.Tag(tg), dataelem.VR(vr), []byte(v))); err != nil {
				t.Fatal(err)
			}
		}
		base := filepath.Join(dir, fmt.Sprintf("%03d-%s", i, filepath.Base(path)))
		if err := os.WriteFile(base+".in.dcm", writeFile(t, ds), 0o600); err != nil {
			t.Fatal(err)
		}
		rec, err := en.Apply(context.Background(), ds, Reception{})
		var f *Failure
		if errors.As(err, &f) {
			t.Logf("%s: refused, %s", filepath.Base(path), f.Error())
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if err := os.WriteFile(base+".out.dcm", writeFile(t, ds), 0o600); err != nil {
			t.Fatal(err)
		}
		p := pair{Original: base + ".in.dcm", Result: base + ".out.dcm", Cleaned: map[string]string{
			formatTag(tImageComments): listCommentsWant, formatTag(tStudyDesc): listDescWant,
		}, Kept: []string{}}
		for _, e := range rec.Entries {
			// Pixel Data is the pixel work's (its JPEG comment segments are
			// removed whatever the profile says), judged by its own tests.
			if !strings.Contains(e.Path, "/") && e.Action == "keep" && !e.Changed && e.Rule < 0 && e.Tag>>16 != 0x7FE0 {
				p.Kept = append(p.Kept, e.Path)
			}
		}
		pairs = append(pairs, p)
	}
	if len(pairs) < 10 {
		t.Fatalf("only %d files de-identified", len(pairs))
	}
	spec, err := json.Marshal(map[string]any{"terms": listTerms, "files": pairs})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifest, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	script, err := filepath.Abs(filepath.Join("..", "..", "..", "scripts", "check", "deid-lists-pydicom.py"))
	if err != nil {
		t.Fatal(err)
	}
	report, err := exec.CommandContext(t.Context(), py, "-I", script, manifest).CombinedOutput() //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	t.Logf("pydicom: %s", report)
	if err != nil {
		t.Fatalf("pydicom: %v", err)
	}

	// A control: the check must find the terms in files not cleaned, each
	// file given as its own result.
	for i := range pairs {
		pairs[i].Result = pairs[i].Original
	}
	spec, err = json.Marshal(map[string]any{"terms": listTerms, "files": pairs})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifest, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err = exec.CommandContext(t.Context(), py, "-I", script, manifest).CombinedOutput() //nolint:gosec // G204: as above.
	if err == nil || !strings.Contains(string(report), "term 1 of the lists stands in 00204000") {
		t.Fatalf("the check found nothing in files not cleaned: %s", report)
	}
}
