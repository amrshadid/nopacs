package correct

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// editPythonEnv names the Python, with pydicom, that runs
// scripts/check/edit-pydicom.py over the edited revisions (listed in
// docs/develop/testing.md; make check-edit). Unset, the reading
// is skipped; set to nothing, it fails.
const editPythonEnv = "NOPACS_CHECK_EDIT_PYTHON"

// editProfile is the profile version that judges the tests' edits.
var editProfile = tenant.ProfileRef{Name: "research", Version: 1}

// editKeys are the judge's keys, each its own.
var editKeys = engine.Keys{UID: bytes.Repeat([]byte{'u'}, 32), Date: bytes.Repeat([]byte{'d'}, 32), Hash: bytes.Repeat([]byte{'h'}, 32)}

// editProfileDoc is PS3.15 Annex E's basic profile with the changes the
// public CT files need to pass its check after (their scan options and
// device's maker removed, as a site's profile would), and the series
// description kept: the attribute the tests edit.
func editProfileDoc() *v1.ProfileDocument {
	remove := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	keep := &v1.ProfileAction{Kind: &v1.ProfileAction_Keep{Keep: &v1.KeepAction{}}}
	kw := func(k string) *v1.ProfileSelector {
		return &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}
	}
	return &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, Rules: []*v1.ProfileRule{
		{Name: "content-label", Selector: kw("ContentLabel"), Action: remove},
		{Name: "scan-options", Selector: kw("ScanOptions"), Action: remove},
		{Name: "maker", Selector: kw("Manufacturer"), Action: remove},
		{Name: "model", Selector: kw("ManufacturerModelName"), Action: remove},
		{Name: "description", Selector: kw("SeriesDescription"), Action: keep},
	}}
}

// editJudge returns the judge of the tests: the profile above, as the
// effective profile of every instance (the rig archives as received).
func editJudge(t testing.TB) ProfileJudge {
	t.Helper()
	v, err := profile.New(editProfile.Name, editProfile.Version, editProfileDoc(), profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	en, err := engine.New(v.Expanded(), editKeys, engine.Options{Method: editProfile.String()})
	if err != nil {
		t.Fatal(err)
	}
	return ProfileJudge{
		Profile: func(context.Context, tenant.EditTarget) (tenant.ProfileRef, error) { return editProfile, nil },
		Engine:  func(context.Context, tenant.ProfileRef) (*engine.Engine, error) { return en, nil },
		Options: receive.TrialOptions{Keys: editKeys, HeaderLimit: header.MaxHeader,
			Check: func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }},
	}
}

// editEngine returns an engine over both storages of the replica rig that
// judges edits by editJudge.
func (r *replicaRig) editEngine() *Engine {
	r.t.Helper()
	j := editJudge(r.t)
	return r.engineWith(func(c *Config) { c.OpenStorage, c.Judge = r.open, j })
}

// editEngine returns an engine over the rig that judges edits by editJudge.
func (r *rig) editEngine() *Engine {
	r.t.Helper()
	j := editJudge(r.t)
	return r.engineWith(func(c *Config) { c.Judge = j })
}

// editNow previews an edit and makes it with the preview's token.
func (r *rig) editNow(e *Engine, target tenant.CorrectionTarget, changes ...EditChange) (tenant.Correction, error) {
	r.t.Helper()
	p, err := e.PreviewEdit(r.ctx, EditPreviewRequest{Target: target, Changes: changes})
	if err != nil {
		return tenant.Correction{}, err
	}
	return e.Edit(r.ctx, EditRequest{Target: target, Changes: changes, Reason: "series named wrongly at the scanner", Confirm: p.Confirm,
		Audit: r.audit()})
}

// scanTarget returns the target of a whole scan.
func scanTarget(s tenant.CatalogScan) tenant.CorrectionTarget {
	return tenant.CorrectionTarget{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label}
}

// setDescription is the change the tests make most.
func setDescription(v string) EditChange { return EditChange{Tag: 0x0008103E, Value: v} }

// editPython returns the Python editPythonEnv names, and false (logged)
// when the variable is not set: the rest of a test runs without pydicom's
// reading, which an outside check asks for by setting it.
func editPython(t *testing.T) (string, bool) {
	t.Helper()
	python, asked := os.LookupEnv(editPythonEnv)
	if !asked {
		t.Log(editPythonEnv + " is not set; pydicom's reading of the edited revisions is skipped")
		return "", false
	}
	if python == "" {
		t.Fatal(editPythonEnv + " is empty; name an interpreter that has pydicom")
	}
	return python, true
}

// pydicomValues reads, with the Python editPythonEnv names, the keyword's
// value of each file, in order: what a tool that is not NoPACs reads of
// the revisions an edit wrote. Nil when the variable is not set.
func pydicomValues(t *testing.T, keyword string, files ...string) []string {
	t.Helper()
	python, ok := editPython(t)
	if !ok {
		return nil
	}
	out := runEditCheck(t, python, append([]string{"values", keyword}, files...)...)
	return strings.Split(strings.TrimSpace(out), "\n")
}

// runEditCheck runs scripts/check/edit-pydicom.py with python, isolated
// (-I), and returns its standard output; a failure fails the test with
// what the script said.
func runEditCheck(t *testing.T, python string, args ...string) string {
	t.Helper()
	script, err := filepath.Abs(filepath.Join("..", "..", "scripts", "check", "edit-pydicom.py"))
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(python, append([]string{"-I", script}, args...)...) // #nosec G204 -- the operator's interpreter, the repository's script and the test's own files.
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("edit-pydicom.py: %v\n%s%s", err, stdout.String(), stderr.String())
	}
	return stdout.String()
}

// pydicomSameBut fails unless pydicom reads the new revision as the old one
// with only the changes made: every element of the data set as pydicom
// decodes it (sequences whole, Pixel Data byte for byte) and the transfer
// syntax equal, but each keyword of set, which holds its value there, and
// each of removed, which is absent. Specific Character Set is the old one
// or ISO_IR 192: the DICOM library writes every file it writes in UTF-8,
// as a de-identified file is, and the text it decodes is the same. It
// does nothing when editPythonEnv is not set.
func pydicomSameBut(t *testing.T, oldFile, newFile string, set map[string]string, removed ...string) {
	t.Helper()
	python, ok := editPython(t)
	if !ok {
		return
	}
	args := []string{"compare", oldFile, newFile}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		args = append(args, "--set", k+"="+set[k])
	}
	for _, k := range removed {
		args = append(args, "--remove", k)
	}
	if got := strings.TrimSpace(runEditCheck(t, python, args...)); got != "same" {
		t.Fatalf("pydicom reads %s as more than the edit changed: %s", newFile, got)
	}
}

// under returns the path of a key on a directory storage's root.
func pathUnder(root, key string) string { return filepath.Join(root, filepath.FromSlash(key)) }
