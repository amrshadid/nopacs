package check

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/filereader/attributes"
	"github.com/amrshadid/go-dicom/filewriter"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// The variables of the outside check, the same the engine's corpus test
// reads (docs/develop/testing.md).
const (
	// pythonEnv names the Python, with pydicom, that runs
	// scripts/check/deid-pydicom.py.
	pythonEnv = "NOPACS_CHECK_DEID_PYTHON"
	// corpusEnv names pydicom's directory of public test files.
	corpusEnv = "NOPACS_PYDICOM_CORPUS"
	// pydicomScript is the outside check, from this package's directory.
	pydicomScript = "../../../scripts/check/deid-pydicom.py"
	// adversarialScript writes synthetic files that hide values where they
	// slip past a de-identifier.
	adversarialScript = "../../../scripts/check/deid-adversarial.py"
	// receiveTestdata is the receiver's directory of public test files.
	receiveTestdata = "../../receive/testdata"
)

// removedKeywords are the attributes the profile below removes or
// replaces, which the outside check also reads from each original.
var removedKeywords = []string{
	"PatientName", "PatientID", "OtherPatientIDs", "OtherPatientNames", "IssuerOfPatientID",
	"PatientBirthDate", "PatientAddress", "AccessionNumber", "StudyID",
	"InstitutionName", "InstitutionAddress", "InstitutionalDepartmentName", "StationName",
	"ReferringPhysicianName", "PerformingPhysicianName", "OperatorsName",
	"NameOfPhysiciansReadingStudy", "RequestingPhysician", "DeviceSerialNumber",
	"StudyInstanceUID", "SeriesInstanceUID", "SOPInstanceUID", "FrameOfReferenceUID",
	"StudyDate", "SeriesDate", "AcquisitionDate", "ContentDate",
}

func kwSel(k string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}
}

func vrSelector(v string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Vr{Vr: v}}
}

var (
	actRemove = &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	actHash   = &v1.ProfileAction{Kind: &v1.ProfileAction_Hash{Hash: &v1.HashAction{}}}
	actUID    = &v1.ProfileAction{Kind: &v1.ProfileAction_Uid{Uid: &v1.UidAction{}}}
	actShift  = &v1.ProfileAction{Kind: &v1.ProfileAction_Shift{Shift: &v1.ShiftAction{}}}
	actClean  = &v1.ProfileAction{Kind: &v1.ProfileAction_Clean{Clean: &v1.CleanAction{}}}
)

func actReplace(c string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Replace{Replace: &v1.ReplaceAction{Value: &v1.ReplaceAction_Constant{Constant: c}}}}
}

func profileRule(name string, sel *v1.ProfileSelector, act *v1.ProfileAction) *v1.ProfileRule {
	return &v1.ProfileRule{Name: name, Selector: sel, Action: act}
}

// corpusProfile is a profile that removes or replaces every value
// identifying a person or a place in the public files: names removed, the
// patient's identifier hashed, UIDs remapped, dates shifted, identifying
// descriptions cleaned; with clean false, the free text is left as it is,
// so the names written in it remain for both checks to find.
func corpusProfile(clean bool) []*v1.ProfileRule {
	rules := []*v1.ProfileRule{
		profileRule("names", vrSelector("PN"), actRemove),
		profileRule("patient-name", kwSel("PatientName"), actReplace("SUBJECT")),
		profileRule("uids", vrSelector("UI"), actUID),
		profileRule("dates", vrSelector("DA"), actShift),
		profileRule("datetimes", vrSelector("DT"), actShift),
		profileRule("content-label", kwSel("ContentLabel"), actReplace("SEGMENT")),
	}
	for _, k := range []string{"PatientID", "AccessionNumber", "StudyID", "RequestedProcedureID",
		"ScheduledProcedureStepID", "PerformedProcedureStepID"} {
		rules = append(rules, profileRule("hash-"+strings.ToLower(k), kwSel(k), actHash))
	}
	for _, k := range []string{"OtherPatientIDs", "OtherPatientIDsSequence", "IssuerOfPatientID", "PatientAddress",
		"InstitutionName", "InstitutionAddress", "InstitutionalDepartmentName", "StationName", "DeviceSerialNumber",
		"Manufacturer", "ManufacturerModelName"} {
		rules = append(rules, profileRule("remove-"+strings.ToLower(k), kwSel(k), actRemove))
	}
	if clean {
		for _, k := range []string{"StudyDescription", "SeriesDescription", "ImageComments", "PatientComments",
			"StructureSetLabel", "StructureSetName", "SoftwareVersions"} {
			rules = append(rules, profileRule("clean-"+strings.ToLower(k), kwSel(k), actClean))
		}
		rules = append(rules, profileRule("clean-overlay-description",
			&v1.ProfileSelector{Kind: &v1.ProfileSelector_Tag{Tag: "60000022"}}, actClean))
	}
	rules = append(rules, profileRule("remove-acme", &v1.ProfileSelector{Kind: &v1.ProfileSelector_PrivateElement{
		PrivateElement: &v1.PrivateElement{Creator: "ACME 1.0", Group: "0009", Element: "xx"}}}, actRemove))
	return rules
}

// corpusEngine makes the engine of corpusProfile under fixed test keys.
func corpusEngine(t *testing.T, clean bool) *engine.Engine {
	t.Helper()
	e, problems := profile.Expand(&v1.ProfileDocument{Rules: corpusProfile(clean)}, profile.Shipped())
	if problems != nil {
		t.Fatalf("expanding the profile: %v", problems)
	}
	k := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	en, err := engine.New(e, engine.Keys{UID: k('u'), Date: k('d'), Hash: k('h')}, engine.Options{Method: "check@1"})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// pair is a file, its result and its needles, as the outside check reads
// them.
type pair struct {
	Original     string `json:"original"`
	Deidentified string `json:"deidentified"`
	Needles      string `json:"needles"`
	charset      *string
	needles      engine.Needles
	written      engine.Written
}

// listFiles returns the files of dir that are not directories or notes.
func listFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || strings.HasSuffix(e.Name(), ".md") || strings.HasSuffix(e.Name(), ".txt") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// deidentify runs en over each file the DICOM library reads and the
// engine accepts, and writes each result and its needles into dir.
func deidentify(t *testing.T, en *engine.Engine, files []string, dir string) []pair {
	t.Helper()
	var out []pair
	for i, path := range files {
		b, err := os.ReadFile(path) //nolint:gosec // G304: a public test file the test names.
		if err != nil {
			t.Fatal(err)
		}
		df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(b)), filereader.ReadOptions{})
		if err != nil {
			continue
		}
		ds := df.GetDataset()
		if ts := ds.TransferSyntaxUID(); ts == "1.2.840.10008.1.2.2" || ts == "1.2.840.10008.1.2.1.99" {
			// Neither is written back here: a big endian data set's binary
			// values would need swapping, and the pipeline refuses deflate.
			continue
		}
		rec, err := en.Apply(context.Background(), ds, engine.Reception{})
		var f *engine.Failure
		if errors.As(err, &f) {
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		name := filepath.Join(dir, fmt.Sprintf("%03d-%s", i, filepath.Base(path)))
		writeResult(t, ds, name)
		out = append(out, pair{Original: path, Deidentified: name, Needles: writeNeedles(t, name, rec.Needles),
			charset: receivedCharset(b), needles: rec.Needles, written: rec.Written})
	}
	return out
}

// receivedCharset returns a file's Specific Character Set as the file
// wrote it, nil when it declares none.
func receivedCharset(b []byte) *string {
	f, err := attributes.Read(filebase.NewFileReader(bytes.NewReader(b)), attributes.ReadOptions{SkipBulkData: true})
	if err != nil {
		return nil
	}
	for _, e := range f.DataElements {
		if e.Tag == 0x00080005 {
			s := strings.TrimRight(string(e.Value), " \x00")
			return &s
		}
	}
	return nil
}

// writeResult writes a de-identified data set as a Part 10 file.
func writeResult(t *testing.T, ds *dataset.Dataset, name string) {
	t.Helper()
	text := func(tg uint32) string { return strings.TrimRight(string(ds.GetValue(tag.Tag(tg))), " \x00") }
	f, err := os.Create(name) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	w := filewriter.NewDICOMFileWriter(filebase.NewFileWriter(f))
	w.SetFileMetaInfo(&filewriter.FileMetaInfo{
		MediaStorageSOPClassUID:    text(0x00080016),
		MediaStorageSOPInstanceUID: text(0x00080018),
		TransferSyntaxUID:          filewriter.StorageTransferSyntax(ds),
	})
	if err := w.AddDataElements(filewriter.ElementsFromDataset(ds)); err != nil {
		t.Fatal(err)
	}
	if err := w.Write(); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
}

// writeNeedles writes a result's needles beside it, never in it, as the
// outside check reads them, and returns the file's path.
func writeNeedles(t *testing.T, name string, needles engine.Needles) string {
	t.Helper()
	kinds := map[engine.NeedleKind]string{engine.NeedleName: "name", engine.NeedleDate: "date",
		engine.NeedleUID: "uid", engine.NeedleText: "text"}
	type item struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Edge  string `json:"edge,omitempty"`
	}
	list := make([]item, 0, len(needles))
	for _, n := range needles {
		it := item{Kind: kinds[n.Kind], Value: n.Value}
		switch n.Edge {
		case engine.EdgeWord:
			it.Edge = "word"
		case engine.EdgeIdentifier:
			it.Edge = "ident"
		}
		list = append(list, it)
	}
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	side := name + ".needles.json"
	if err := os.WriteFile(side, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return side
}

// ours returns the originals whose results this package finds a needle
// in, checked as the pipeline checks them (with the run's record of the
// method), and fails on a refusal.
func ours(t *testing.T, pairs []pair) []string {
	t.Helper()
	c := New(Options{})
	var found []string
	for _, p := range pairs {
		b, err := os.ReadFile(p.Deidentified) //nolint:gosec // G304: the test's own temporary file.
		if err != nil {
			t.Fatal(err)
		}
		_, err = c.CheckWritten(context.Background(), bytes.NewReader(b), int64(len(b)), p.needles, p.written, p.charset)
		var res *Residual
		switch {
		case err == nil:
		case errors.As(err, &res):
			t.Logf("check: %s: %s", filepath.Base(p.Original), res.Detail())
			found = append(found, p.Original)
		default:
			t.Fatalf("%s: %v", p.Original, err)
		}
	}
	sort.Strings(found)
	return found
}

// theirs runs the outside check over pairs and returns the originals it
// finds a needle in.
func theirs(t *testing.T, py, dir string, pairs []pair) []string {
	t.Helper()
	keywords := slices.Sorted(slices.Values(removedKeywords))
	spec, err := json.Marshal(map[string]any{"keywords": keywords, "files": pairs})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(manifest, spec, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), py, "-I", pydicomScript, manifest) //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	report, _ := cmd.CombinedOutput()
	t.Logf("pydicom: %s", report)
	seen := map[string]bool{}
	for _, line := range strings.Split(string(report), "\n") {
		for _, p := range pairs {
			if strings.HasPrefix(line, p.Original+": ") {
				seen[p.Original] = true
			}
		}
	}
	return slices.Sorted(func(yield func(string) bool) {
		for k := range seen {
			if !yield(k) {
				return
			}
		}
	})
}

// TestAgreesWithPydicom guards the check against an implementation that is
// not this one: the public files (the receiver's, pydicom's corpus with
// corpusEnv set, and synthetic files that hide values where they slip
// past a de-identifier) de-identified by the engine, then searched by this
// package and by scripts/check/deid-pydicom.py, which reads each original
// itself as well. Both must find nothing in any result; and over the
// synthetic files de-identified without the cleans of free text, both must
// find the names left there, in the same files.
func TestAgreesWithPydicom(t *testing.T) {
	py, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skipf("%s is not set", pythonEnv)
	}
	if py == "" {
		t.Fatalf("%s is set to nothing: the check was asked for without an interpreter", pythonEnv)
	}
	syn := t.TempDir()
	cmd := exec.CommandContext(t.Context(), py, "-I", adversarialScript, syn) //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	if report, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("writing the synthetic files: %v: %s", err, report)
	}
	adversarial := listFiles(t, syn)
	files := listFiles(t, receiveTestdata)
	if dir := os.Getenv(corpusEnv); dir != "" {
		files = append(files, listFiles(t, dir)...)
	}
	files = append(files, adversarial...)

	out := t.TempDir()
	pairs := deidentify(t, corpusEngine(t, true), files, out)
	if len(pairs) < 10 {
		t.Fatalf("only %d files de-identified", len(pairs))
	}
	got, want := ours(t, pairs), theirs(t, py, out, pairs)
	t.Logf("%d files de-identified: this check finds %d, pydicom %d", len(pairs), len(got), len(want))
	if len(got) != 0 || len(want) != 0 {
		t.Errorf("found in the engine's results: this check %v, pydicom %v", got, want)
	}

	ctl := t.TempDir()
	leaks := deidentify(t, corpusEngine(t, false), adversarial, ctl)
	got, want = ours(t, leaks), theirs(t, py, ctl, leaks)
	t.Logf("control: %d files: this check finds %d, pydicom %d", len(leaks), len(got), len(want))
	if !slices.Equal(got, want) {
		t.Errorf("the checks disagree on the control:\nthis check %v\npydicom    %v", got, want)
	}
	for _, f := range []string{"cjk-iso2022", "cjk-utf8", "latin1", "short-names", "crafted-utf8", "implicit-private", "wrong-vr-name"} {
		if !slices.Contains(got, filepath.Join(syn, f+".dcm")) {
			t.Errorf("this check did not find the names left in %s", f)
		}
	}
}
