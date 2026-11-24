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
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// valuePatternScript is the outside check of value patterns and redact,
// from this package's directory.
const valuePatternScript = "../../../scripts/check/deid-valuepattern.py"

// The patterns of the value pattern corpus test, as the outside check
// reads them too.
const (
	mrnPattern = `MRN\d{6}`
	accPattern = `ACC-\d{4}`
)

// plantedValues are what the value pattern corpus test writes into each
// public file: an MRN inside a protocol's name (LO) and twice inside a
// patient's history (LT), and an accession-like number as the whole of a
// requested procedure's description (LO). No rule names these attributes,
// so the value patterns decide them.
var plantedValues = []struct {
	tag   uint32
	vr    string
	value string
}{
	{0x00181030, "LO", "Head CT MRN123456 follow-up"},
	{0x001021B0, "LT", "MRN123456 seen; again MRN654321."},
	{0x00321060, "LO", "ACC-4321"},
}

// plant sets an element of ds, replacing the one there.
func plant(t *testing.T, ds *dataset.Dataset, tg uint32, vr, value string) {
	t.Helper()
	ds.Remove(tag.Tag(tg))
	if err := ds.Add(dataelem.NewDataElement(tag.Tag(tg), dataelem.VR(vr), []byte(value))); err != nil {
		t.Fatal(err)
	}
}

// TestValuePatternsAgreeWithPydicom guards value patterns and redact over
// public files read by an implementation that is not this one: the
// receiver's test files and, with corpusEnv set, pydicom's corpus, each
// with values planted (plantedValues), de-identified by the corpus profile
// and two rules: a search for an MRN that redacts it, and a whole match of
// an accession-like number that removes it. pydicom then confirms each
// redacted value and each removal, and finds none of the matched texts in
// any result (scripts/check/deid-valuepattern.py); the outside check of
// the corpus (scripts/check/deid-pydicom.py) finds nothing either, with
// the engine's needles, which hold what the rules took out. A control
// that keeps a copy of an MRN in Body Part Examined (CS, which no value
// pattern reads) is found by both.
func TestValuePatternsAgreeWithPydicom(t *testing.T) {
	py, asked := os.LookupEnv(pythonEnv)
	if !asked {
		t.Skipf("%s is not set", pythonEnv)
	}
	if py == "" {
		t.Fatalf("%s is set to nothing: the check was asked for without an interpreter", pythonEnv)
	}
	files := corpusFiles(t, receiveTestdata)
	if dir := os.Getenv(corpusEnv); dir != "" {
		files = append(files, corpusFiles(t, dir)...)
	}
	rules := append(corpusRules(),
		rule("mrn", valueSel(mrnPattern, true, ""), aRedact(mrnPattern, "")),
		rule("accession-like", valueSel(accPattern, false, ""), aRemove))
	en := newDocEngine(t, testKeys, Options{Method: "corpus@1"}, &v1.ProfileDocument{
		Rules:              rules,
		PixelRules:         []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the attributes are judged here"}},
		UnsearchedSegments: v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP,
	})
	run := func(dir string, copyInCS bool) ([]checkPair, []map[string]string) {
		var pairs []checkPair
		var mine []map[string]string
		for i, path := range files {
			ds, err := readPath(t, path)
			if err != nil {
				continue
			}
			if ts := ds.TransferSyntaxUID(); ts == "1.2.840.10008.1.2.2" || ts == "1.2.840.10008.1.2.1.99" {
				continue
			}
			for _, p := range plantedValues {
				plant(t, ds, p.tag, p.vr, p.value)
			}
			if copyInCS {
				plant(t, ds, 0x00180015, "CS", "MRN123456")
			}
			input := filepath.Join(dir, fmt.Sprintf("%03d-in-%s", i, filepath.Base(path)))
			if err := os.WriteFile(input, writeFile(t, ds), 0o600); err != nil {
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
			p := writePair(t, dir, fmt.Sprintf("%03d", i), input, writeFile(t, ds), rec)
			pairs = append(pairs, p)
			mine = append(mine, map[string]string{"input": input, "output": p.Deidentified})
		}
		return pairs, mine
	}
	checkMine := func(dir string, mine []map[string]string) ([]byte, error) {
		spec, err := json.Marshal(map[string]any{"redact": mrnPattern, "remove": accPattern, "files": mine})
		if err != nil {
			t.Fatal(err)
		}
		manifest := filepath.Join(dir, "valuepattern.json")
		if err := os.WriteFile(manifest, spec, 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), py, "-I", valuePatternScript, manifest) //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
		report, err := cmd.CombinedOutput()
		t.Logf("pydicom, value patterns: %s", report)
		return report, err
	}

	out := t.TempDir()
	pairs, mine := run(out, false)
	if len(pairs) < 5 {
		t.Fatalf("only %d files de-identified", len(pairs))
	}
	if report, err := checkMine(out, mine); err != nil {
		t.Errorf("pydicom finds the value patterns not applied: %v: %s", err, report)
	}
	if report, err := runCheck(t, py, out, pairs); err != nil {
		t.Errorf("pydicom found removed values: %v: %s", err, report)
	}

	ctl := t.TempDir()
	leaks, mineLeaks := run(ctl, true)
	if report, err := checkMine(ctl, mineLeaks); err == nil || !bytes.Contains(report, []byte("is in 00180015 of the output")) {
		t.Errorf("the value pattern check did not find the copy kept in Body Part Examined: %v: %s", err, report)
	}
	if report, err := runCheck(t, py, ctl, leaks); err == nil || !bytes.Contains(report, []byte("MRN123456")) {
		t.Errorf("the corpus check did not find the copy kept in Body Part Examined: %v: %s", err, report)
	}
}
