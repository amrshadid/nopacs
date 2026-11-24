package receive

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// trialEngine compiles the tests' profile under the tests' keys, as the
// receiver of the tests runs it.
func trialEngine(t *testing.T) *engine.Engine {
	t.Helper()
	v, err := profile.New(testProfile.Name, testProfile.Version, basicProfile(), profile.Shipped())
	if err != nil {
		t.Fatal(err)
	}
	en, err := engine.New(v.Expanded(), testDeidKeys, engine.Options{Method: testProfile.String()})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// TestTrialAgreesWithTheReceiver guards that a trial is what reception
// does: every corpus file of the package is handed to a receiver that
// de-identifies and tried by TryFile with the same keys, profile and
// receiver; each is archived exactly when the trial passes it, held
// exactly when the trial holds it (with the same reason), and an archived
// file is byte for byte the trial's output. Of several files of one SOP
// Instance UID only the first is sent.
func TestTrialAgreesWithTheReceiver(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{}})
	rcv, err := e.store.Receiver(e.ctx, "dimse")
	if err != nil {
		t.Fatal(err)
	}
	en := trialEngine(t)
	paths, err := filepath.Glob("testdata/*.dcm")
	if err != nil {
		t.Fatal(err)
	}
	ct := corpus(t, "ct5n-2062.dcm")
	// A synthetic file has a study, series and instance of its own: a
	// receiver names a series from its first file, and a trial each file.
	newUID := func(ds *dataset.Dataset, last string) {
		for i, t := range []tag.Tag{tag.New(0x0008, 0x0018), tag.New(0x0020, 0x000D), tag.New(0x0020, 0x000E)} {
			_ = ds.SetTextValue(t, "UI", "1.2.826.0.1.3680043.10.999."+last+"."+string(rune('1'+i)))
		}
	}
	files := map[string][]byte{
		// No Patient ID: the default naming rule cannot name its subject.
		"no-patient-id": rewrite(t, ct, "", func(ds *dataset.Dataset) { ds.Remove(tag.New(0x0010, 0x0020)); newUID(ds, "1") }),
		// The Patient ID copied into an element the profile keeps: the
		// check after finds it.
		"id-kept-elsewhere": rewrite(t, ct, "", func(ds *dataset.Dataset) {
			_ = ds.SetTextValue(tag.New(0x0018, 0x0015), "CS", "98890234")
			newUID(ds, "2")
		}),
	}
	for _, p := range paths {
		b, err := os.ReadFile(p) // #nosec G304 -- the package's own test files.
		if err != nil {
			t.Fatal(err)
		}
		files[p] = b
	}
	trials := map[string]Trial{}
	for p, b := range files {
		h, err := header.Read(bytes.NewReader(b), int64(len(b)))
		if err != nil {
			continue // not a file a receiver records; the trial's own tests cover it
		}
		u, _ := h.UID(header.SOPInstanceUID)
		if _, seen := trials[remapped(u)]; seen {
			// The corpus holds one instance in several transfer syntaxes;
			// a second file of a UID is another case of reception.
			continue
		}
		if out := e.p.Accept(e.ctx, Arrival{Receiver: "dimse", Body: bytes.NewReader(b)}); out.Status != Received {
			continue
		}
		tr, err := TryFile(e.ctx, en, b, engine.Reception{Receiver: "dimse"}, TrialOptions{
			Keys: testDeidKeys, HeaderLimit: header.MaxHeader, Receiver: &rcv, Check: func(int64) Checker { return e.check },
		})
		if err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		trials[remapped(u)] = tr
	}
	if len(trials) < 8 {
		t.Fatalf("only %d files tried", len(trials))
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
	archived := map[string][]byte{}
	for _, r := range e.receptions() {
		for u, obj := range archivedObjects(t, e, r) {
			archived[u] = obj
		}
	}
	held := map[string]tenant.QuarantinedFile{}
	for _, q := range e.held() {
		held[q.SOPInstanceUID] = q
	}
	passed := 0
	for _, tr := range trials {
		if tr.Hold == nil {
			passed++
		}
	}
	if passed == 0 || passed == len(trials) {
		t.Fatalf("%d of %d files passed; the comparison needs both kinds", passed, len(trials))
	}
	t.Logf("%d files: %d passed, %d held", len(trials), passed, len(trials)-passed)
	for u, tr := range trials {
		obj, wasArchived := archived[u]
		q, wasHeld := held[u]
		switch {
		case tr.Hold == nil && !wasArchived:
			t.Errorf("%s: the trial passed it; the receiver held it as %s %v", u, q.Reason, q.Details)
		case tr.Hold != nil && !wasHeld:
			t.Errorf("%s: the trial held it as %s %v; the receiver archived it", u, tr.Hold.Reason, tr.Hold.Details)
		case tr.Hold != nil && q.Reason != tr.Hold.Reason:
			t.Errorf("%s: held as %s by the trial, %s by the receiver", u, tr.Hold.Reason, q.Reason)
		case tr.Hold == nil && !bytes.Equal(obj, tr.Written):
			t.Errorf("%s: the archived file (%d bytes) is not the trial's output (%d bytes)", u, len(obj), len(tr.Written))
		}
	}
}

// TestTryFileWithAndWithoutAReceiver guards TryFile itself: bytes that are
// not DICOM are held as not_dicom; a file of a SOP class the receiver does
// not take is held as at reception, and passes with no receiver, saying
// so; a passed file answers its values before and after, read back from
// what was written.
func TestTryFileWithAndWithoutAReceiver(t *testing.T) {
	en := trialEngine(t)
	b, err := os.ReadFile(filepath.Join("testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	o := TrialOptions{Keys: testDeidKeys, HeaderLimit: header.MaxHeader, Check: func(int64) Checker { return &recordingCheck{} }}
	if tr, err := TryFile(t.Context(), en, []byte("not a DICOM file at all"), engine.Reception{}, o); err != nil || tr.Hold == nil || tr.Hold.Reason != tenant.QuarantineNotDICOM {
		t.Errorf("not DICOM: %+v %v", tr.Hold, err)
	}
	mrOnly := tenant.Receiver{Name: "mr", MaxFileBytes: 1 << 30}
	mrOnly.DIMSE.SOPClasses = []string{"1.2.840.10008.5.1.4.1.1.4"}
	o.Receiver = &mrOnly
	if tr, err := TryFile(t.Context(), en, b, engine.Reception{}, o); err != nil || tr.Hold == nil || tr.Hold.Reason != tenant.QuarantineSOPClassNotAccepted || tr.Receiver != "mr" {
		t.Errorf("a class the receiver does not take: %+v %v", tr.Hold, err)
	}
	o.Receiver = nil
	tr, err := TryFile(t.Context(), en, b, engine.Reception{}, o)
	if err != nil || tr.Hold != nil || tr.Receiver != "" || tr.Record == nil || tr.After == nil || len(tr.Written) == 0 {
		t.Fatalf("with no receiver: %+v %v", tr.Hold, err)
	}
	study, _ := tr.After.Get(dsStudyUID)
	if v, _ := study.Bytes(); !strings.HasPrefix(string(v), "2.25.") {
		t.Errorf("the study UID after: %q", v)
	}
}

// TestExplainFile guards ExplainFile: an explanation for every element of
// a corpus file, each with the deciding rule's tier when a rule decides;
// bytes that are not DICOM are refused with a fixed cause.
func TestExplainFile(t *testing.T) {
	en := trialEngine(t)
	b, err := os.ReadFile(filepath.Join("testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	xs, hold, err := ExplainFile(t.Context(), en, b, engine.Reception{}, header.MaxHeader)
	if err != nil || hold != nil || len(xs) == 0 {
		t.Fatalf("%d explanations, %+v, %v", len(xs), hold, err)
	}
	found := false
	for _, x := range xs {
		if x.Entry.Path == "00100010" {
			found = x.Entry.Rule >= 0 && x.SelectedBy == "tag"
		}
	}
	if !found {
		t.Error("Patient's Name is not explained as chosen by a tag rule")
	}
	if _, hold, err := ExplainFile(t.Context(), en, []byte("nope"), engine.Reception{}, header.MaxHeader); err != nil || hold == nil || hold.Details["cause"] != "unreadable" {
		t.Errorf("not DICOM: %+v %v", hold, err)
	}
}

// TestTryFileWithoutValues guards a trial for a caller that answers no
// value: the same verdict and record, and neither the data set before,
// nor the one after, nor the written bytes kept.
func TestTryFileWithoutValues(t *testing.T) {
	en := trialEngine(t)
	b, err := os.ReadFile(filepath.Join("testdata", "ct5n-2062.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	o := TrialOptions{Keys: testDeidKeys, HeaderLimit: header.MaxHeader, Check: func(int64) Checker { return &recordingCheck{} }, NoValues: true}
	tr, err := TryFile(t.Context(), en, b, engine.Reception{}, o)
	if err != nil || tr.Hold != nil || tr.Record == nil || len(tr.Record.Entries) == 0 {
		t.Fatalf("without values: %+v %v", tr.Hold, err)
	}
	if tr.Before != nil || tr.After != nil || tr.Written != nil {
		t.Errorf("a trial without values kept the data set before %v, after %v, or %d written bytes", tr.Before != nil, tr.After != nil, len(tr.Written))
	}
}
