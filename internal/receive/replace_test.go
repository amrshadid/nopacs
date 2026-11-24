package receive

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// heldDifferent archives MR_small.dcm, then sends MR_small_RLE.dcm (the
// same instance in another transfer syntax) and returns the file held as
// a different file: what a replace is made from.
func heldDifferent(t *testing.T, e *env) tenant.QuarantinedFile {
	t.Helper()
	e.send("MR_small.dcm")
	e.settle(1)
	e.clock.Advance(time.Minute)
	e.send("MR_small_RLE.dcm")
	e.settle(2)
	var out []tenant.QuarantinedFile
	for _, q := range e.held() {
		if q.Reason == tenant.QuarantineDifferentFile && q.State == tenant.QuarantineHeld {
			out = append(out, q)
		}
	}
	if len(out) != 1 || !out[0].ContentKept {
		t.Fatalf("held %+v, want one different file with its content kept", e.held())
	}
	return out[0]
}

// stillHeld fails the test unless the quarantined file q is still held.
func stillHeld(t *testing.T, e *env, q tenant.QuarantinedFile) {
	t.Helper()
	for _, x := range e.held() {
		if x.ID == q.ID {
			if x.State != tenant.QuarantineHeld {
				t.Errorf("the quarantined file is %s, want held", x.State)
			}
			return
		}
	}
	t.Error("the quarantined file's record is gone")
}

// storedKeys returns every key of the env's storage.
func storedKeys(t *testing.T, e *env) []string {
	t.Helper()
	l, err := e.storage.List(e.ctx, blob.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Keys
}

// refusal asks for a replace's file from q and answers the error; a file
// made after all is given back at once, so the pipeline can stop.
func refusal(e *env, q tenant.QuarantinedFile) error {
	src, err := e.p.ReplaceSource(e.ctx, q)
	if err == nil {
		src.Done()
	}
	return err
}

// setDeidentification points the env's receiver at profile, or makes it
// archive as received for the zero profile, and lets the pipeline's view of
// its receivers catch up.
func (e *env) setDeidentification(profile tenant.ProfileRef) {
	e.t.Helper()
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse", Profile: profile},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		e.t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
}

// TestReplaceFromADeidentifyingReceiverIsDeidentified guards the file a
// replace puts when the held file arrived at a receiver that
// de-identifies: it is the file de-identified by the receiver's profile
// version, under the archived instance's remapped UID and with the
// metadata line of those bytes, never the identified bytes held; pydicom
// reads it beside the original and finds none of the values removed, when
// the outside check's Python is set.
func TestReplaceFromADeidentifyingReceiverIsDeidentified(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	q := heldDifferent(t, e)
	if !q.Deidentified {
		t.Fatal("a file held at a receiver that de-identifies is not recorded de-identified")
	}
	original := corpus(t, "MR_small_RLE.dcm")
	uid := uidOf(t, original)
	if q.SOPInstanceUID != remapped(uid) {
		t.Fatalf("the held record names %s, want the remapped UID", q.SOPInstanceUID)
	}
	calls := e.check.calls
	src, err := e.p.ReplaceSource(e.ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	// Registered after the env's, so run before its pipeline stops, which
	// waits for the decision this file holds.
	t.Cleanup(src.Done)
	rc, err := src.Open()
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(got)) != src.Size || tenant.SHA256(sha256.Sum256(got)) != src.SHA256 {
		t.Fatalf("the bytes given are %d bytes, not the %d the replace was told", len(got), src.Size)
	}
	if bytes.Equal(got, original) {
		t.Fatal("the replace would put the identified file as it was held")
	}
	if e.check.calls != calls+1 {
		t.Errorf("the check after ran %d times for the replace, want once", e.check.calls-calls)
	}
	if u := uidOf(t, got); u != remapped(uid) {
		t.Errorf("the replacement names %s, want the remapped UID", u)
	}
	if bytes.Contains(got, []byte(uid)) {
		t.Error("the replacement holds the original SOP Instance UID")
	}
	if src.Line.SOPInstanceUID != "" && src.Line.SOPInstanceUID != remapped(uid) {
		t.Errorf("the metadata line names %s", src.Line.SOPInstanceUID)
	}
	if src.Line.Profile == "" {
		t.Error("the metadata line names no profile version")
	}
	pydicomFindsNothing(t, e, original, got, remapped(uid))
	stillHeld(t, e, q)
}

// pydicomFindsNothing runs the outside check of de-identification over one
// file and its original, when deidPythonEnv names the Python: pydicom
// finds in it none of the values the profile removed.
func pydicomFindsNothing(t *testing.T, e *env, original, deidentified []byte, uid string) {
	t.Helper()
	py := os.Getenv(deidPythonEnv)
	if py == "" {
		t.Log("the outside check is skipped; set " + deidPythonEnv)
		return
	}
	dir := t.TempDir()
	op, dp, np := filepath.Join(dir, "original.dcm"), filepath.Join(dir, "replacement.dcm"), filepath.Join(dir, "needles.json")
	for p, b := range map[string][]byte{op: original, dp: deidentified, np: needlesJSON(t, e.check.needles[uid])} {
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	type pair struct {
		Original     string `json:"original"`
		Deidentified string `json:"deidentified"`
		Needles      string `json:"needles"`
	}
	m, err := json.Marshal(map[string]any{"keywords": deidRemovedKeywords, "files": []pair{{op, dp, np}}})
	if err != nil {
		t.Fatal(err)
	}
	mp := filepath.Join(dir, "check.json")
	if err := os.WriteFile(mp, m, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := exec.CommandContext(t.Context(), py, "-I", deidCheckScript, mp).CombinedOutput() //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	t.Logf("pydicom: %s", report)
	if err != nil {
		t.Fatalf("pydicom found a removed value in the replacement: %v", err)
	}
}

// TestReplaceRefusesAFileHeldAsReceivedAtAReceiverThatDeidentifiesNow
// guards a file held while its receiver archived as received, its receiver
// then made to de-identify: a replace from it is refused as a precondition,
// since the file is identified and recorded so; the storage is unchanged
// and the file stays held.
func TestReplaceRefusesAFileHeldAsReceivedAtAReceiverThatDeidentifiesNow(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{asReceived: true}})
	q := heldDifferent(t, e)
	if q.Deidentified {
		t.Fatal("a file held at a receiver that archives as received is recorded de-identified")
	}
	before := storedKeys(t, e)
	e.setDeidentification(testProfile)
	err := refusal(e, q)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "held as received") {
		t.Fatalf("a replace from a file held as received: %v, want the precondition", err)
	}
	if after := storedKeys(t, e); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the storage changed: %v, was %v", after, before)
	}
	stillHeld(t, e, q)
}

// TestReplaceRefusesAFileHeldDeidentifiedAtAReceiverThatNoLongerDoes
// guards a file held de-identified, its receiver then made to archive as
// received: a replace from it is refused as a precondition, since no
// profile version names what to remove from it now; the file stays held.
func TestReplaceRefusesAFileHeldDeidentifiedAtAReceiverThatNoLongerDoes(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	q := heldDifferent(t, e)
	before := storedKeys(t, e)
	e.setDeidentification(tenant.ProfileRef{})
	err := refusal(e, q)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "no profile version") {
		t.Fatalf("a replace from a file held de-identified: %v, want the precondition", err)
	}
	if after := storedKeys(t, e); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the storage changed: %v, was %v", after, before)
	}
	stillHeld(t, e, q)
}

// TestReplaceWhoseDeidentificationHoldsKeepsTheFileHeld guards a replace
// whose de-identification would hold the file (the check after finds a
// value removed): it is refused as a precondition naming the reason, the
// file stays held, its de-identified copy is not left in the incoming
// area, and the storage is unchanged.
func TestReplaceWhoseDeidentificationHoldsKeepsTheFileHeld(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	q := heldDifferent(t, e)
	before, files := storedKeys(t, e), e.incomingFiles()
	e.check.setFind(func(uid string) *check.Residual {
		if uid == q.SOPInstanceUID {
			return &check.Residual{Total: 1}
		}
		return nil
	})
	err := refusal(e, q)
	if fault.KindOf(err) != fault.Precondition || !strings.Contains(fault.Message(err), "stays held") {
		t.Fatalf("a replace whose de-identification holds: %v, want the precondition", err)
	}
	if msg := fault.Message(err); !strings.Contains(msg, "fails (") || strings.Contains(msg, "fails ()") {
		t.Errorf("the refusal names no reason: %s", msg)
	}
	if after := storedKeys(t, e); strings.Join(after, "\n") != strings.Join(before, "\n") {
		t.Errorf("the storage changed: %v, was %v", after, before)
	}
	if n := e.incomingFiles(); n != files {
		t.Errorf("the incoming area holds %d files, was %d", n, files)
	}
	stillHeld(t, e, q)
}
