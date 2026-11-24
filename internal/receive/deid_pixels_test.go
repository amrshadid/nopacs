package receive

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/compress"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/encaps"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testRects are the rectangles the pixel rule of these tests fills: one
// from the top left, one measured from the bottom right.
var testRects = []*v1.PixelRectangle{
	{X: 2, Y: 3, Width: 10, Height: 5},
	{X: 1, Y: 2, Width: 6, Height: 4, FromRight: true, FromBottom: true},
}

// pixelDoc is the basic profile of these tests with pixel rules.
func pixelDoc(rules ...*v1.PixelRule) *v1.ProfileDocument {
	doc := basicProfile()
	doc.PixelRules = rules
	return doc
}

// fillRule fills testRects in every image.
func fillRule() *v1.PixelRule {
	return &v1.PixelRule{Name: "fill", Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: testRects}}}
}

// pixelCase is one public image a pixel rule fills at reception.
type pixelCase struct {
	name, file string
	// syntax is the transfer syntax the image is archived in.
	syntax string
	// lossy is what Lossy Image Compression (0028,2110) says after.
	lossy string
}

// pixelCases are the images, one of each encoding the receiver fills.
var pixelCases = []pixelCase{
	{"native", "MR_small.dcm", "1.2.840.10008.1.2.1", ""},
	{"RLE", "MR_small_RLE.dcm", "1.2.840.10008.1.2.1", ""},
	{"RLE color, two frames", "SC_rgb_rle_2frame.dcm", "1.2.840.10008.1.2.1", ""},
	{"JPEG-LS", "MR_small_jpeg_ls_lossless.dcm", "1.2.840.10008.1.2.1", ""},
	{"JPEG baseline, three frames", "jpeg_3frame_nobot.dcm", "1.2.840.10008.1.2.1", "01"},
}

// TestPixelRulesFillAtReception guards a redact rule at a receiver that
// de-identifies, over a native, two RLE, a JPEG-LS and a JPEG baseline
// public image: each is archived, native, with every sample of the rule's
// rectangles black in every frame and every other sample what the DICOM
// library decodes from the original; Burned In Annotation is NO, the
// Clean Pixel Data code recorded, Lossy Image Compression 01 for the JPEG
// baseline image that did not say, and no Extended Offset Table. With
// NOPACS_CHECK_DEID_PYTHON set, pydicom reads every archived image and
// compares it with the original the same way (scripts/check/deid-pixels.py).
func TestPixelRulesFillAtReception(t *testing.T) {
	var pairs []pixelPair
	dir := t.TempDir()
	for _, c := range pixelCases {
		t.Run(c.name, func(t *testing.T) {
			pairs = append(pairs, fillAtReception(t, dir, corpus(t, c.file), c.syntax, c.lossy, testRects))
		})
	}
	outsideCheck(t, pairs)
}

// TestBurnedInRiskIsHeld guards the gate at reception: an ultrasound
// image (a modality commonly drawn on, Burned In Annotation absent) under
// a profile with no pixel rule for it is held as burned_in_risk, cause
// modality, its original bytes kept, and nothing archived; a pixel rule
// that accepts it then lets a release archive it.
func TestBurnedInRiskIsHeld(t *testing.T) {
	ct := &v1.ProfileCondition{Subject: v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, Test: &v1.ProfileCondition_Equals{Equals: "CT"}}
	onlyCT := fillRule()
	onlyCT.When = []*v1.ProfileCondition{ct}
	e := newEnv(t, envOptions{deid: &deidOptions{doc: pixelDoc(onlyCT)}})
	e.send("jpeg_3frame_nobot.dcm")
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineBurnedInRisk || q.Details["cause"] != "modality" || !q.ContentKept {
		t.Fatalf("held %s %v, kept %v", q.Reason, q.Details, q.ContentKept)
	}
	if err := q.Validate(); err != nil {
		t.Errorf("the record: %v", err)
	}
	if n := len(e.receptions()); n != 0 {
		for _, r := range e.receptions() {
			if r.Counts.FilesArchived != 0 {
				t.Errorf("a reception archived %d files", r.Counts.FilesArchived)
			}
		}
	}
}

// TestPixelsPastTheLimitAreHeldThenReleased guards deid.pixel-limit as a
// border that loses nothing: an image whose decoded size passes the limit
// is held as deid_failed, pixels_too_large, its bytes kept; once the
// setting is raised, its release archives it, filled.
func TestPixelsPastTheLimitAreHeldThenReleased(t *testing.T) {
	limit := int64(1 << 20)
	read := func(context.Context) (int64, error) { return limit, nil }
	e := newEnv(t, envOptions{deid: &deidOptions{doc: pixelDoc(fillRule()), pixelLimit: read}})
	b := corpus(t, "jpeg_3frame_nobot.dcm") // 3 frames of 240 by 320 by 3: 691,200 bytes decoded
	limit = 600_000
	e.sendBytes(b, Received)
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineDeidFailed || q.Details["cause"] != "pixels_too_large" || !q.ContentKept {
		t.Fatalf("held %s %v, kept %v", q.Reason, q.Details, q.ContentKept)
	}
	limit = 1 << 20
	e.clock.Advance(2 * time.Second)
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r := e.settle(1)[0]
	if !done(r) || r.Counts.FilesArchived != 1 {
		t.Fatalf("reception %s %+v, held %v", r.State, r.Counts, e.held())
	}
	after := decoded(t, archivedOne(t, e, r))
	info, _ := after.GetPixelDataInfo()
	comparePixels(t, decoded(t, b), after, regionsIn(info.Columns, info.Rows))
}

// withComment returns a JPEG baseline public image whose every frame
// carries a comment segment (COM, 0xFFFE) right after its start-of-image
// marker, holding text, laid out one fragment a frame.
func withComment(t *testing.T, text string) []byte {
	t.Helper()
	return rewrite(t, corpus(t, "jpeg_3frame_nobot.dcm"), "", func(ds *dataset.Dataset) {
		enc, err := ds.ExtractEncapsulatedFrames()
		if err != nil {
			t.Fatal(err)
		}
		frames, err := encaps.Frames(enc, 3)
		if err != nil {
			t.Fatal(err)
		}
		com := append([]byte{0xFF, 0xFE, 0, byte((len(text) + 2) & 0xFF)}, text...)
		for i, f := range frames {
			out := append(append(append([]byte(nil), f[:2]...), com...), f[2:]...)
			if len(out)%2 == 1 {
				out = append(out, 0)
			}
			frames[i] = out
		}
		value, err := compress.EncapsulateFrames(frames, 1, true)
		if err != nil {
			t.Fatal(err)
		}
		if err := ds.SetValue(tag.New(0x7FE0, 0x0010), value); err != nil {
			t.Fatal(err)
		}
	})
}

// TestJPEGCommentIsRemoved guards the JPEG segments of an image whose
// pixels stay compressed (a pixel rule accepts it): the comment segment of
// every frame, and the identifying text in it, is not in the archived
// file, which stays JPEG baseline and decodes to the same samples as the
// original.
func TestJPEGCommentIsRemoved(t *testing.T) {
	const secret = "PATIENT DOE^JOHN 1970"
	b := withComment(t, secret)
	if !bytes.Contains(b, []byte(secret)) {
		t.Fatal("the test file holds no comment")
	}
	accept := &v1.PixelRule{Name: "reviewed", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "reviewed by a person"}
	e := newEnv(t, envOptions{deid: &deidOptions{doc: pixelDoc(accept)}})
	e.sendBytes(b, Received)
	r := e.settle(1)[0]
	if !done(r) || r.Counts.FilesArchived != 1 {
		t.Fatalf("reception %s %+v, held %v", r.State, r.Counts, e.held())
	}
	out := archivedOne(t, e, r)
	if bytes.Contains(out, []byte(secret)) || bytes.Contains(out, []byte{0xFF, 0xFE, 0, byte(len(secret) + 2)}) {
		t.Error("the comment is still in the archived file")
	}
	h, err := header.Read(bytes.NewReader(out), int64(len(out)))
	if err != nil || h.TransferSyntaxUID != "1.2.840.10008.1.2.4.50" {
		t.Fatalf("archived in %s: %v", h.TransferSyntaxUID, err)
	}
	comparePixels(t, decoded(t, b), decoded(t, out), nil)
}
