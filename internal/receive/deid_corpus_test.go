package receive

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/receive/header"
)

// The variables of the outside check over pydicom's corpus, the names the
// engine's corpus test reads too.
const (
	// deidPythonEnv names the Python, with pydicom, that reads every
	// archived file (scripts/check/deid-pydicom.py).
	deidPythonEnv = "NOPACS_CHECK_DEID_PYTHON"
	// deidCorpusEnv names pydicom's directory of public test files.
	deidCorpusEnv = "NOPACS_PYDICOM_CORPUS"
	// deidCheckScript is the outside check, from this package's directory.
	deidCheckScript = "../../scripts/check/deid-pydicom.py"
)

// deidRemovedKeywords are attributes PS3.15 Annex E's basic profile removes
// or replaces, whose values pydicom must find in no archived file.
var deidRemovedKeywords = []string{"PatientName", "PatientID", "PatientBirthDate", "StudyInstanceUID", "SeriesInstanceUID",
	"SOPInstanceUID", "StudyDate", "SeriesDate", "AcquisitionDate", "ContentDate", "AccessionNumber", "StudyID",
	"InstitutionName", "ReferringPhysicianName", "OperatorsName", "StationName",
	// Sequences the profile removes whose every value identifies: pydicom
	// searches for all they held.
	"InstitutionCodeSequence", "ReferringPhysicianIdentificationSequence", "PerformingPhysicianIdentificationSequence",
	"OperatorIdentificationSequence", "PhysiciansReadingStudyIdentificationSequence", "ContentCreatorIdentificationCodeSequence",
	"VerifyingObserverIdentificationCodeSequence", "IssuerOfAdmissionIDSequence", "IssuerOfServiceEpisodeIDSequence"}

// TestCorpusThroughADeidentifyingReceiver is the outside check of
// de-identification at reception, run when deidCorpusEnv and deidPythonEnv
// are set: every file of pydicom's public corpus is handed to a receiver
// that de-identifies, every reception ends, and pydicom reads each archived
// file beside its original and finds none of the values the profile
// removed (the engine's needles for the file, given to the check after,
// and what pydicom reads of the original itself), in any element or in the
// raw bytes. What was held is counted by reason; nothing is lost: each
// file is archived, held, or refused before it was kept.
func TestCorpusThroughADeidentifyingReceiver(t *testing.T) {
	dir, py := os.Getenv(deidCorpusEnv), os.Getenv(deidPythonEnv)
	if dir == "" || py == "" {
		t.Skip("the outside check over pydicom's corpus; set " + deidCorpusEnv + " and " + deidPythonEnv)
	}
	e := newEnv(t, envOptions{deid: &deidOptions{}, workers: 4})
	originals := map[string]string{} // remapped SOP Instance UID: original's path
	sent := 0
	for _, p := range corpusPaths(t, dir) {
		b, err := os.ReadFile(p) // #nosec G304 -- a public corpus the test is pointed at.
		if err != nil {
			t.Fatal(err)
		}
		if out := e.p.Accept(e.ctx, Arrival{Receiver: "dimse", Body: bytes.NewReader(b)}); out.Status != Received {
			continue
		}
		sent++
		if h, err := header.Read(bytes.NewReader(b), int64(len(b))); err == nil {
			if u, _ := h.UID(header.SOPInstanceUID); u != "" {
				originals[remapped(u)] = p
			}
		}
	}
	e.waitFor("every reception to end", 3*time.Second, func() bool {
		rs := e.receptions()
		for _, r := range rs {
			if !r.State.Ended() {
				return false
			}
		}
		return len(rs) > 0 && e.p.batch.idle()
	})
	reasons := map[string]int{}
	for _, q := range e.held() {
		k := string(q.Reason)
		if c := q.Details["cause"]; c != "" {
			k += "/" + c
		}
		reasons[k]++
	}
	out := t.TempDir()
	type pair struct {
		Original     string `json:"original"`
		Deidentified string `json:"deidentified"`
		Needles      string `json:"needles"`
	}
	var pairs []pair
	for _, r := range e.receptions() {
		if r.State != "done" {
			continue
		}
		for u, obj := range archivedObjects(t, e, r) {
			orig, ok := originals[u]
			if !ok {
				continue
			}
			p := filepath.Join(out, u+".dcm")
			np := filepath.Join(out, u+".needles.json")
			if err := os.WriteFile(p, obj, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(np, needlesJSON(t, e.check.needles[u]), 0o600); err != nil {
				t.Fatal(err)
			}
			pairs = append(pairs, pair{Original: orig, Deidentified: p, Needles: np})
		}
	}
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var held []string
	for _, k := range keys {
		held = append(held, k+"="+itoa(int64(reasons[k])))
	}
	t.Logf("%d files kept, %d archived and checked, held: %s", sent, len(pairs), strings.Join(held, " "))
	if len(pairs) < 20 {
		t.Fatalf("only %d files archived", len(pairs))
	}
	manifest, err := json.Marshal(map[string]any{"keywords": deidRemovedKeywords, "files": pairs})
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(out, "check.json")
	if err := os.WriteFile(mp, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := exec.CommandContext(t.Context(), py, "-I", deidCheckScript, mp).CombinedOutput() //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	t.Logf("pydicom: %s", report)
	if err != nil {
		t.Fatalf("pydicom found a removed value in an archived file: %v", err)
	}
}

// corpusPaths lists the files of dir that are not directories or notes.
func corpusPaths(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, en := range entries {
		if en.IsDir() || strings.HasSuffix(en.Name(), ".md") || strings.HasSuffix(en.Name(), ".txt") || strings.HasSuffix(en.Name(), ".py") {
			continue
		}
		out = append(out, filepath.Join(dir, en.Name()))
	}
	return out
}

// needlesJSON writes the engine's needles of a file as the outside check
// reads them.
func needlesJSON(t *testing.T, ns engine.Needles) []byte {
	t.Helper()
	type needle struct {
		Kind  string `json:"kind"`
		Value string `json:"value"`
	}
	kinds := map[engine.NeedleKind]string{engine.NeedleName: "name", engine.NeedleDate: "date", engine.NeedleUID: "uid", engine.NeedleText: "text"}
	out := make([]needle, 0, len(ns))
	for _, n := range ns {
		out = append(out, needle{Kind: kinds[n.Kind], Value: n.Value})
	}
	b, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
