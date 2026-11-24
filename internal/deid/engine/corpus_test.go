package engine

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
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// The variables of the outside check.
const (
	// pythonEnv names the Python, with pydicom, that reads every file the
	// corpus test de-identifies (scripts/check/deid-pydicom.py).
	pythonEnv = "NOPACS_CHECK_DEID_PYTHON"
	// corpusEnv names pydicom's directory of public test files, every file
	// of which the corpus test de-identifies besides the receiver's.
	corpusEnv = "NOPACS_PYDICOM_CORPUS"
	// deidScript is the outside check, from this package's directory.
	deidScript = "../../../scripts/check/deid-pydicom.py"
	// adversarialScript writes the synthetic files that hide values where
	// they slip past a de-identifier, from this package's directory.
	adversarialScript = "../../../scripts/check/deid-adversarial.py"
)

// removedKeywords are the attributes the corpus profile removes or
// replaces, whose values must be nowhere in a de-identified file.
var removedKeywords = []string{
	"PatientName", "PatientID", "OtherPatientIDs", "OtherPatientNames", "IssuerOfPatientID",
	"PatientBirthDate", "PatientAddress", "AccessionNumber", "StudyID",
	"InstitutionName", "InstitutionAddress", "InstitutionalDepartmentName", "StationName",
	"ReferringPhysicianName", "PerformingPhysicianName", "OperatorsName",
	"NameOfPhysiciansReadingStudy", "RequestingPhysician", "DeviceSerialNumber",
	"StudyInstanceUID", "SeriesInstanceUID", "SOPInstanceUID", "FrameOfReferenceUID",
	"StudyDate", "SeriesDate", "AcquisitionDate", "ContentDate",
}

// corpusRules is the profile of the corpus test: names removed, the
// patient's identifier hashed, every UID remapped, every date shifted,
// identifying descriptions cleaned.
func corpusRules() []*v1.ProfileRule {
	rules := []*v1.ProfileRule{
		rule("names", vrSel("PN"), aRemove),
		rule("patient-name", kw("PatientName"), aReplace("SUBJECT")),
		rule("patient-id", kw("PatientID"), aHash),
		rule("accession", kw("AccessionNumber"), aHash),
		rule("study-id", kw("StudyID"), aHash),
		// Scanners copy the accession number into the procedure's
		// identifiers (examples_overlay).
		rule("requested-procedure", kw("RequestedProcedureID"), aHash),
		rule("scheduled-step", kw("ScheduledProcedureStepID"), aHash),
		rule("performed-step", kw("PerformedProcedureStepID"), aHash),
		rule("uids", vrSel("UI"), aUID),
		rule("dates", vrSel("DA"), aShift),
		rule("datetimes", vrSel("DT"), aShift),
		// A segmentation's label names its maker (liver_1frame: QIICR QIN
		// IOWA), as Manufacturer, removed below, does.
		rule("content-label", kw("ContentLabel"), aReplace("SEGMENT")),
	}
	for _, k := range []string{"OtherPatientIDs", "OtherPatientIDsSequence", "IssuerOfPatientID", "PatientAddress",
		"InstitutionName", "InstitutionAddress", "InstitutionalDepartmentName", "StationName", "DeviceSerialNumber",
		// A vendor's name can hold the institution's (MR_small: TOSHIBA in
		// TOSHIBA_MEC), and a model's name too (rtplan), which a search for the
		// removed name would find; they are removed at every depth.
		"Manufacturer", "ManufacturerModelName"} {
		rules = append(rules, rule("remove-"+strings.ToLower(k), kw(k), aRemove))
	}
	// A structure set's label can hold the study's ID (rtstruct: sep30); an
	// overlay's description the vendor's name (examples_overlay), and the
	// software versions the model's (examples_palette), which the profile
	// removes.
	for _, k := range []string{"StudyDescription", "SeriesDescription", "ImageComments", "PatientComments", "StructureSetLabel", "StructureSetName",
		"SoftwareVersions"} {
		rules = append(rules, rule("clean-"+strings.ToLower(k), kw(k), aClean))
	}
	rules = append(rules, rule("clean-overlay-description", &v1.ProfileSelector{Kind: &v1.ProfileSelector_Tag{Tag: "60000022"}}, aClean))
	// A vendor's block that holds a name and a record number the
	// description repeats (the synthetic implicit-private.dcm): removed by
	// a rule. Its name is a needle, which clean takes out of the
	// description; its record number, a private value of no known kind,
	// is not, and stays in the description.
	rules = append(rules, rule("remove-acme", privSel("ACME 1.0", "0009", "xx"), aRemove))
	return rules
}

// corpusFiles lists the files of dir that are not directories or notes.
func corpusFiles(t *testing.T, dir string) []string {
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

// TestCorpusFilesAreDeidentified guards the engine over public files: the
// receiver's test files and, with corpusEnv set, every file of pydicom's
// corpus. Each file the library reads is de-identified by the corpus
// profile or refused with a Failure, never left half done; each result is
// written, read back by the library, and holds none of the values the
// profile removed; doing it again gives the same bytes. With pythonEnv
// set, pydicom first writes synthetic files that hide values where they
// slip past a de-identifier (scripts/check/deid-adversarial.py: names in
// ISO 2022 and Latin-1, CJK and two-letter names, a private sequence kept
// as UN bytes, crafted UTF-8, private values of an implicit VR file, names
// written with another VR, directory elements and an Original Attributes
// Sequence), which join the corpus; and then reads every
// pair of original and result and finds none of the removed values in the
// result, searching for the engine's needles, given in a file beside each
// result and never in it, and for what it reads itself
// (scripts/check/deid-pydicom.py).
func TestCorpusFilesAreDeidentified(t *testing.T) {
	files := corpusFiles(t, receiveTestdata)
	if dir := os.Getenv(corpusEnv); dir != "" {
		files = append(files, corpusFiles(t, dir)...)
	}
	py, asked := os.LookupEnv(pythonEnv)
	if asked && py == "" {
		t.Fatalf("%s is set to nothing: the check was asked for without an interpreter", pythonEnv)
	}
	var adversarial []string
	if asked {
		dir := t.TempDir()
		cmd := exec.CommandContext(t.Context(), py, "-I", adversarialScript, dir) //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
		if report, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("writing the synthetic files: %v: %s", err, report)
		}
		adversarial = corpusFiles(t, dir)
		if len(adversarial) < 10 {
			t.Fatalf("%d synthetic files", len(adversarial))
		}
		files = append(files, adversarial...)
	}
	// The attributes are what this test judges: every image is accepted
	// as it is, so no file is held for burned-in text (pixel_test.go
	// judges pixels). The corpus holds no syntax whose segments are not
	// searched; its JPEG and JPEG 2000 frames are walked, and only the two
	// JPEG 2000 files of heldJ2KCorpus are held for theirs.
	en := newDocEngine(t, testKeys, Options{Method: "corpus@1"}, &v1.ProfileDocument{
		Rules:      corpusRules(),
		PixelRules: []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the attributes are judged here"}},
	})
	out := t.TempDir()
	var pairs []checkPair
	// The needles searched for here are those of removedKeywords, as the
	// outside check's: a vendor's or model's name, removed by the profile
	// too, identifies no patient and recurs in what a file keeps.
	checked := map[string]bool{}
	for _, k := range removedKeywords {
		checked[formatTag(uint32(tag.GlobalDictionary().GetByKeyword(k)))] = true
	}
	for t := range floorTags {
		checked[formatTag(t)] = true
	}
	causes := map[Cause]int{}
	unread := 0
	for i, path := range files {
		ds, err := readPath(t, path)
		if err != nil {
			unread++
			continue
		}
		if ts := ds.TransferSyntaxUID(); ts == "1.2.840.10008.1.2.2" || ts == "1.2.840.10008.1.2.1.99" {
			// Written back here as Explicit VR Little Endian, a big
			// endian data set's binary values would need swapping, and a
			// deflated one is refused by the pipeline: neither is this
			// engine's to change.
			unread++
			continue
		}
		before := writeFile(t, ds)
		rec, err := en.Apply(context.Background(), ds, Reception{})
		var f *Failure
		if errors.As(err, &f) {
			if _, held := heldJ2KCorpus[filepath.Base(path)]; !held || f.Cause != CausePixelTextUnsearched {
				causes[f.Cause]++
			}
			t.Logf("%s: refused, %s", filepath.Base(path), f.Error())
			if after := writeFile(t, ds); !bytes.Equal(before, after) {
				t.Errorf("%s: a refused file was changed", path)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		b := writeFile(t, ds)
		again, err := readPath(t, path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := en.Apply(context.Background(), again, Reception{}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, writeFile(t, again)) {
			t.Errorf("%s: two runs gave different bytes", path)
		}
		back := readFile(t, b)
		hay := strings.ToLower(strings.Join(allText(back), "\x00"))
		for _, n := range rec.Needles {
			if !checked[n.Path[strings.LastIndexByte(n.Path, '/')+1:]] {
				continue
			}
			// A date of only a year or a year and month is matched as a
			// date of that precision by the outside check, not here.
			if n.Kind == NeedleDate && len(n.Value) < 8 {
				continue
			}
			if len(n.Value) >= 4 && strings.Contains(hay, strings.ToLower(n.Value)) {
				t.Errorf("%s: a removed value of %s is in the result", path, n.Path)
			}
		}
		pairs = append(pairs, writePair(t, out, fmt.Sprintf("%03d", i), path, b, rec))
	}
	t.Logf("%d files: %d de-identified, %d refused %v, %d not read or not written here", len(files), len(pairs), len(files)-len(pairs)-unread, causes, unread)
	if len(pairs) < 10 {
		t.Fatalf("only %d files de-identified", len(pairs))
	}
	for c := range causes {
		if c != CauseNoSubject && c != CauseBadValue {
			t.Errorf("a refusal of cause %s", c)
		}
	}
	if !asked {
		return
	}
	if report, err := runCheck(t, py, out, pairs); err != nil {
		t.Fatalf("pydicom found removed values: %v: %s", err, report)
	}

	// A control: the check must find what a profile leaves. The synthetic
	// files de-identified without the rules that clean free text keep the
	// names, birth dates and addresses written in it, in each script.
	var noClean []*v1.ProfileRule
	for _, r := range corpusRules() {
		if !strings.HasPrefix(r.GetName(), "clean-") {
			noClean = append(noClean, r)
		}
	}
	leaky := newTestEngine(t, testKeys, Options{}, noClean...)
	ctl := t.TempDir()
	var leaks []checkPair
	for i, path := range adversarial {
		ds, err := readPath(t, path)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := leaky.Apply(context.Background(), ds, Reception{})
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		leaks = append(leaks, writePair(t, ctl, fmt.Sprintf("%03d", i), path, writeFile(t, ds), rec))
	}
	report, err := runCheck(t, py, ctl, leaks)
	if err == nil {
		t.Fatalf("the check found nothing in files whose free text was not cleaned: %s", report)
	}
	for _, f := range []string{"cjk-iso2022", "cjk-utf8", "latin1", "short-names", "crafted-utf8", "implicit-private", "wrong-vr-name"} {
		if !bytes.Contains(report, []byte(f+".dcm: ")) {
			t.Errorf("the check did not find the names left in %s: %s", f, report)
		}
	}
}

// checkPair is a file and its result, for the outside check, with the
// file of the engine's needles: written beside the result, never in it.
type checkPair struct {
	Original     string `json:"original"`
	Deidentified string `json:"deidentified"`
	Needles      string `json:"needles"`
}

// writePair writes a result and its needles into dir, under a name made
// of prefix and the original's.
func writePair(t *testing.T, dir, prefix, original string, result []byte, rec *Record) checkPair {
	t.Helper()
	name := filepath.Join(dir, prefix+"-"+filepath.Base(original))
	if err := os.WriteFile(name, result, 0o600); err != nil {
		t.Fatal(err)
	}
	kinds := map[NeedleKind]string{NeedleName: "name", NeedleDate: "date", NeedleUID: "uid", NeedleText: "text"}
	type needle struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
		Edge  string `json:"edge,omitempty"`
	}
	list := make([]needle, 0, len(rec.Needles))
	for _, n := range rec.Needles {
		nd := needle{Kind: kinds[n.Kind], Value: n.Value}
		switch n.Edge {
		case EdgeWord:
			nd.Edge = "word"
		case EdgeIdentifier:
			nd.Edge = "ident"
		}
		list = append(list, nd)
	}
	side, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name+".needles.json", side, 0o600); err != nil {
		t.Fatal(err)
	}
	return checkPair{original, name, name + ".needles.json"}
}

// runCheck runs the outside check over pairs, its manifest written into
// dir, and returns what it printed.
func runCheck(t *testing.T, py, dir string, pairs []checkPair) ([]byte, error) {
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
	cmd := exec.CommandContext(t.Context(), py, "-I", deidScript, manifest) //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	report, err := cmd.CombinedOutput()
	t.Logf("pydicom: %s", report)
	return report, err
}

// TestStudyKeepsItsLinksAcrossFiles guards that the files of one study,
// de-identified apart, still form one study: one Study Instance UID, one
// Series Instance UID per series, distinct SOP Instance UIDs, one date
// shift for the patient; and that another tenant's keys give other UIDs.
func TestStudyKeepsItsLinksAcrossFiles(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{}, corpusRules()...)
	other := newTestEngine(t, otherKeys, Options{}, corpusRules()...)
	matches, err := filepath.Glob(filepath.Join(receiveTestdata, "ct*n-*.dcm"))
	if err != nil || len(matches) < 4 {
		t.Fatalf("the study's files: %v %v", matches, err)
	}
	studies, dates, instances := map[string]bool{}, map[string]bool{}, map[string]bool{}
	series := map[string]map[string]bool{}
	for _, path := range matches {
		ds, err := readPath(t, path)
		if err != nil {
			t.Fatal(err)
		}
		origSeries := mustText(t, ds, 0x0020000E)
		apply(t, en, ds)
		studies[mustText(t, ds, tStudyUID)] = true
		dates[mustText(t, ds, tStudyDate)] = true
		instances[mustText(t, ds, tSOPInstance)] = true
		if series[origSeries] == nil {
			series[origSeries] = map[string]bool{}
		}
		series[origSeries][mustText(t, ds, 0x0020000E)] = true

		ds2, _ := readPath(t, path)
		apply(t, other, ds2)
		if mustText(t, ds2, tStudyUID) == mustText(t, ds, tStudyUID) {
			t.Errorf("%s: two tenants share a study UID", path)
		}
	}
	if len(studies) != 1 || len(dates) != 1 || len(instances) != len(matches) {
		t.Errorf("studies %d, study dates %d, instances %d of %d", len(studies), len(dates), len(instances), len(matches))
	}
	for orig, got := range series {
		if len(got) != 1 {
			t.Errorf("series %s became %d series", orig, len(got))
		}
	}
	if len(series) < 2 {
		t.Errorf("the study has %d series", len(series))
	}
}
