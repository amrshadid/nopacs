package receive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"sync/atomic"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// addRawReceiver adds a receiver that archives as received, "raw", beside
// the env's.
func (e *env) addRawReceiver() {
	e.t.Helper()
	if _, err := e.store.AddReceiver(e.ctx, tenant.Receiver{
		Name: "raw", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11114, AETitle: "RAW", AnyCaller: true},
	}, t0, e.audit(), nil); err != nil {
		e.t.Fatal(err)
	}
}

// heldTooLargeHeader sends the CT file to a de-identifying receiver whose
// header bound is 256 bytes, waits for its header_too_large hold, raises
// the bound and returns the record.
func heldTooLargeHeader(t *testing.T, opts envOptions) (*env, tenant.QuarantinedFile) {
	t.Helper()
	var limit atomic.Int64
	limit.Store(256)
	opts.deid = &deidOptions{}
	opts.headerLimit = func(context.Context) (int64, error) { return limit.Load(), nil }
	e := newEnv(t, opts)
	e.addRawReceiver()
	e.clock.Advance(2 * time.Second)
	e.send(studyFiles[0])
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineHeaderTooLarge || !q.Deidentified || q.Receivable() {
		t.Fatalf("held %s, de-identified %v, receivable %v; want a header_too_large record made de-identified that joins nothing",
			q.Reason, q.Deidentified, q.Receivable())
	}
	limit.Store(64 << 20)
	e.clock.Advance(2 * HeaderLimitInterval)
	return e, q
}

// noIdentifiedObject fails the test when any object on the storage holds
// the CT patient's name or identifier.
func noIdentifiedObject(t *testing.T, e *env) {
	t.Helper()
	objs, err := e.storage.List(e.ctx, blob.ListOptions{Prefix: "data/"})
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range objs.Keys {
		b := readObject(t, e.storage, k)
		if bytes.Contains(b, []byte("Doe^Peter")) || bytes.Contains(b, []byte("98890234")) {
			t.Errorf("%s holds the patient's name or identifier", k)
		}
	}
}

// TestADeidentifiedRecordIsNotReleasedAsReceived guards a file held
// de-identified at landing that joins no reception (its header past the
// bound): released through a receiver that archives as received, it would
// land again under that receiver and be archived identified. The store
// refuses such a release (a precondition failure), the record stays held
// with its bytes, nothing is archived; released through its own receiver,
// which de-identifies, it is archived de-identified.
func TestADeidentifiedRecordIsNotReleasedAsReceived(t *testing.T) {
	e, q := heldTooLargeHeader(t, envOptions{})
	_, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs, Receiver: "raw"}, e.audit(), nil)
	if fault.KindOf(err) != fault.Precondition {
		t.Fatalf("a release through an as-received receiver answered %v, want a precondition failure", err)
	}
	after, err := e.store.QuarantinedFile(e.ctx, q.ID)
	if err != nil || after.State != tenant.QuarantineHeld || !after.ContentKept {
		t.Fatalf("after the refused release the record is %s, kept %v (%v)", after.State, after.ContentKept, err)
	}
	if ok, _ := e.p.area.Exists(q.Incoming); !ok {
		t.Fatal("the held bytes are gone after a refused release")
	}
	time.Sleep(50 * time.Millisecond)
	if rs := e.receptions(); len(rs) != 0 {
		t.Fatalf("a refused release made receptions %+v", rs)
	}
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r := e.settle(1)[0]
	if !done(r) || r.Receiver != "dimse" {
		t.Fatalf("reception %s at %s, want done at dimse", r.State, r.Receiver)
	}
	noOriginal(t, e, r)
	noIdentifiedObject(t, e)
}

// TestALandingOfADeidentifiedReleaseAtAnAsReceivedReceiverIsHeld guards
// the check of a file that lands again from a release, when the receiver
// it was released through archives as received by then (the receiver
// changed between the release and the landing, here across a restart):
// the released record was made de-identified, so the file is held
// (deid_failed, no_profile), its record protected (keyed checksum, no
// original UID), and never archived.
func TestALandingOfADeidentifiedReleaseAtAnAsReceivedReceiverIsHeld(t *testing.T) {
	e, q := heldTooLargeHeader(t, envOptions{})
	if err := e.p.Stop(e.ctx); err != nil {
		t.Fatal(err)
	}
	// What Release does before the intake, the process then stopping: the
	// landed name, then the store's release through dimse, which
	// de-identifies.
	e.manual = true
	e.start()
	if err := e.p.area.Unrecord(q.Incoming); err != nil {
		t.Fatal(err)
	}
	out, err := e.store.ReleaseFile(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs,
		ReceptionID: "R000000000000000000000001", Now: t0}, e.audit(), nil)
	if err != nil || out.ReleaseReception != "" || out.ReleaseReceiver != "dimse" {
		t.Fatalf("the release joined %q through %q: %v", out.ReleaseReception, out.ReleaseReceiver, err)
	}
	_ = e.p.area.Close()
	e.manual = false
	if _, err := e.store.SetReceiver(e.ctx, tenant.Receiver{Name: "dimse"}, []tenant.ReceiverField{tenant.FieldDeidentification}, t0, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.start()
	e.waitFor("the second hold", 3*time.Second, func() bool { return len(e.held()) == 2 })
	var again tenant.QuarantinedFile
	for _, x := range e.held() {
		if x.ReleasedFrom == q.ID {
			again = x
		}
	}
	if again.Reason != tenant.QuarantineDeidFailed || again.Details["cause"] != "no_profile" {
		t.Fatalf("the landing was held %s %v, want deid_failed no_profile", again.Reason, again.Details)
	}
	if !again.Deidentified || !again.ContentKept {
		t.Errorf("the new record is de-identified %v, kept %v", again.Deidentified, again.ContentKept)
	}
	sum := tenant.SHA256(sha256.Sum256(corpus(t, studyFiles[0])))
	if again.SHA256 == sum || again.SHA256 != keyedSum(testDeidKeys, sum) {
		t.Error("the new record's checksum is not the keyed checksum of the bytes")
	}
	_, _, inst := fileUIDs(t, corpus(t, studyFiles[0]))
	if again.SOPInstanceUID == inst {
		t.Error("the new record holds the original SOP Instance UID")
	}
	time.Sleep(50 * time.Millisecond)
	if rs := e.receptions(); len(rs) != 0 {
		t.Fatalf("the landing made receptions %+v", rs)
	}
	noIdentifiedObject(t, e)
}
