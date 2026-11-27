package correct

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// provenanceDoc is the basic profile, the elements the public files carry
// that it does not name removed, with the pixel rules given.
func provenanceDoc(rules ...*v1.PixelRule) *v1.ProfileDocument {
	remove := &v1.ProfileAction{Kind: &v1.ProfileAction_Remove{Remove: &v1.RemoveAction{}}}
	var named []*v1.ProfileRule
	for i, k := range []string{"ContentLabel", "ScanOptions", "Manufacturer", "ManufacturerModelName"} {
		named = append(named, &v1.ProfileRule{Name: []string{"content-label", "scan-options", "maker", "model"}[i], Selector: &v1.ProfileSelector{Kind: &v1.ProfileSelector_Keyword{Keyword: k}}, Action: remove})
	}
	return &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, Rules: named, PixelRules: rules}
}

// storeVersion stores doc as the profile version ref.
func (r *rig) storeVersion(ref tenant.ProfileRef, doc *v1.ProfileDocument) {
	r.t.Helper()
	v, err := profile.New(ref.Name, ref.Version, doc, profile.Shipped())
	if err != nil {
		r.t.Fatal(err)
	}
	if _, _, err := r.p.(tenant.Profiles).CreateProfileVersion(r.ctx, tenant.NewProfileVersion{Name: ref.Name, Source: v.Source(),
		Expanded: v.ExpandedText(), Checksum: v.Checksum()}, time.Now(), r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
}

// deidentifyWith makes the rig's receiver de-identify by ref.
func (r *rig) deidentifyWith(ref tenant.ProfileRef) {
	r.t.Helper()
	if _, err := r.p.SetReceiver(r.ctx, tenant.Receiver{Name: "dimse", Profile: ref},
		[]tenant.ReceiverField{tenant.FieldDeidentification}, time.Now(), r.audit(), nil); err != nil {
		r.t.Fatal(err)
	}
}

// deidPipeline starts the reception pipeline with the keys and the check
// de-identification needs; the caller stops it.
func (r *rig) deidPipeline() *receive.Pipeline {
	r.t.Helper()
	hasher, err := naming.NewHasher(bytes.Repeat([]byte{7}, naming.MinKeyLength))
	if err != nil {
		r.t.Fatal(err)
	}
	keys := engine.Keys{UID: bytes.Repeat([]byte{'k'}, 32), Date: bytes.Repeat([]byte{'s'}, 32), Hash: bytes.Repeat([]byte{7}, naming.MinKeyLength)}
	p, err := receive.New(receive.Config{Tenant: "lab", Store: r.p, Hasher: hasher, Clock: r.clock,
		OpenStorage:  func(context.Context, string) (blob.Store, error) { return r.storage, nil },
		IncomingRoot: filepath.Join(r.dataDir, "incoming"), Workers: 2, Lease: 30 * time.Second, PollInterval: time.Second,
		BatchInterval: time.Second, RetryInterval: 5 * time.Second, ScanLocks: &r.scanLocks,
		DeidKeys: func(context.Context) (engine.Keys, error) { return keys, nil },
		Check:    func(n int64) receive.Checker { return check.New(check.Options{MaxFileBytes: n}) }})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := p.Start(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	return p
}

// replaceFrom hands body to the running pipeline at receiver, waits for it
// to be held as a different file of an archived instance, and replaces
// the instance with it as a person would; it returns the instance's row
// after the replace.
func (r *rig) replaceFrom(t *testing.T, p *receive.Pipeline, receiver string, body []byte) tenant.ScanInstance {
	t.Helper()
	if out := p.Accept(r.ctx, receive.Arrival{Receiver: receiver, Body: bytes.NewReader(body),
		Caller: tenant.Caller{CallingAETitle: "SITE", Address: "192.0.2.8:40000"}}); out.Status != receive.Received {
		t.Fatalf("outcome %v (%s)", out.Status, out.Reason)
	}
	var q tenant.QuarantinedFile
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if held := r.quarantined(tenant.QuarantineDifferentFile); len(held) == 1 {
			q = held[0]
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no different file held: %+v", r.quarantined(""))
		}
	}
	src, err := p.ReplaceSource(r.ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	c, err := r.engine().Replace(r.ctx, ReplaceRequest{Quarantine: q.ID, Storage: src.Storage, Path: src.Path, UID: q.SOPInstanceUID,
		Open: src.Open, Line: src.Line, Reason: "corrected re-send", Audit: r.audit()})
	src.Done()
	if err != nil || c.NewRevision != 2 {
		t.Fatalf("replace %+v, %v", c, err)
	}
	now, err := r.p.HeldInstance(r.ctx, src.Path, q.SOPInstanceUID)
	if err != nil {
		t.Fatal(err)
	}
	if line := r.lineFor(src.Path, q.SOPInstanceUID); line.Provenance() != now.Provenance() {
		t.Errorf("the metadata line says %+v of revision 2, the row %+v", line.Provenance(), now.Provenance())
	}
	return now
}

// changed returns the corpus file name with one byte of its pixels
// changed: a different file of the same instance.
func changed(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDir, name)) // #nosec G304 -- a test's own file name.
	if err != nil {
		t.Fatal(err)
	}
	b[len(b)-2000] ^= 0x5a
	return b
}

// TestAReplaceGivesTheRowItsNewRevisionsProvenance guards that a replace
// leaves the catalog judging the bytes it points to: an image at risk of
// burned-in text, archived with its corners filled under version 1 and
// replaced by a file version 2 de-identifies by accepting its pixels as
// they are, is refused to an unmarked sender as pixel_risk_accepted, its
// row naming version 2; the metadata line and the row say the same.
func TestAReplaceGivesTheRowItsNewRevisionsProvenance(t *testing.T) {
	r := newRig(t, false)
	one, two := tenant.ProfileRef{Name: "research", Version: 1}, tenant.ProfileRef{Name: "research", Version: 2}
	r.storeVersion(one, provenanceDoc(&v1.PixelRule{Name: "fill",
		Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: []*v1.PixelRectangle{{X: 2, Y: 3, Width: 10, Height: 5}}}}}))
	r.storeVersion(two, provenanceDoc(&v1.PixelRule{Name: "accept", Reason: "reviewed by the site",
		Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}}))
	r.deidentifyWith(one)
	p := r.deidPipeline()
	r.accept(p, "jpeg_3frame_nobot.dcm")
	r.waitEnded()
	_ = p.Stop(context.Background())
	first := r.held(pathOf(r.scans()[0]))[0]
	if first.Profile != one || first.PixelRiskAccepted {
		t.Fatalf("revision 1 records %v, risk %v", first.Profile, first.PixelRiskAccepted)
	}
	r.deidentifyWith(two)
	p = r.deidPipeline()
	defer func() { _ = p.Stop(context.Background()) }()
	now := r.replaceFrom(t, p, "dimse", changed(t, "jpeg_3frame_nobot.dcm"))
	if now.Profile != two || !now.PixelRiskAccepted {
		t.Errorf("revision 2's row records %v, risk %v; want %v, true", now.Profile, now.PixelRiskAccepted, two)
	}
	if code, _ := tenant.SendRefusal(now.Provenance(), false); code != tenant.FailurePixelRiskAccepted {
		t.Errorf("an unmarked sender judges revision 2 %q, want pixel_risk_accepted", code)
	}
}

// TestAReplaceFromAnAsReceivedReceiverIsRefused guards a replace made from
// a file held at a receiver of the tenant that archives as received: the
// instance's row then records no profile version, as its bytes were never
// de-identified, and an unmarked sender refuses it as as_received, though
// the revision it replaced was de-identified.
func TestAReplaceFromAnAsReceivedReceiverIsRefused(t *testing.T) {
	r := newRig(t, false)
	ref := tenant.ProfileRef{Name: "research", Version: 1}
	r.storeVersion(ref, provenanceDoc())
	r.deidentifyWith(ref)
	if _, err := r.p.AddReceiver(r.ctx, tenant.Receiver{Name: "raw", Type: tenant.ReceiverDIMSE, Enabled: true, Storage: "main",
		QuietPeriod: 5 * time.Second, MaxOpenTime: time.Minute, MaxFileBytes: 1 << 30,
		DIMSE: tenant.DIMSEReceiver{Port: 11113, AETitle: "RAW", AnyCaller: true}}, time.Now(), r.audit(), nil); err != nil {
		t.Fatal(err)
	}
	p := r.deidPipeline()
	r.accept(p, "MR_small.dcm")
	r.waitEnded()
	_ = p.Stop(context.Background())
	path := pathOf(r.scans()[0])
	first := r.held(path)[0]
	if first.Profile != ref {
		t.Fatalf("revision 1 records %v", first.Profile)
	}
	// The archived (de-identified) file, changed, sent back to the
	// receiver that keeps what it gets: it carries the archived UIDs.
	archived, err := readAll(r.ctx, r.storage, first.Key, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	archived[len(archived)-2000] ^= 0x5a
	p = r.deidPipeline()
	defer func() { _ = p.Stop(context.Background()) }()
	now := r.replaceFrom(t, p, "raw", archived)
	if !now.Profile.IsZero() || now.PixelRiskAccepted {
		t.Errorf("revision 2's row records %v, risk %v; want none", now.Profile, now.PixelRiskAccepted)
	}
	if code, _ := tenant.SendRefusal(now.Provenance(), false); code != tenant.FailureAsReceived {
		t.Errorf("an unmarked sender judges revision 2 %q, want as_received", code)
	}
}
