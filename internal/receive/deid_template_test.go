package receive

import (
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// templateRule fills by the tenant's template in every image.
func templateRule() *v1.PixelRule {
	return &v1.PixelRule{Name: "by-device", Action: &v1.PixelRule_RedactByTemplate{RedactByTemplate: &v1.RedactByTemplate{}}}
}

// sonoSiteTemplate is a template for the public ultrasound image the tests
// send (SonoSite Turbo, 240 rows by 320 columns), with the manufacturer and
// model written in another case than the file's, filling testRects.
func sonoSiteTemplate() tenant.PixelTemplate {
	t := tenant.PixelTemplate{Manufacturer: "sonosite, INC.", Model: "TURBO", Rows: 240, Columns: 320}
	for _, r := range testRects {
		t.Rectangles = append(t.Rectangles, tenant.PixelRect{X: r.GetX(), Y: r.GetY(), Width: r.GetWidth(), Height: r.GetHeight(),
			FromRight: r.GetFromRight(), FromBottom: r.GetFromBottom()})
	}
	return t
}

// TestTemplateRuleAtReception guards the template action through a
// receiver that de-identifies, over a public ultrasound whose header names
// its device: with no template the image is held as deid_failed with the
// cause no_pixel_template and its bytes kept, nothing archived; once the
// tenant has a template for the device (written in another case) and the
// held file is released, it is archived native with exactly the template's
// rectangles black in every frame and every other sample what the DICOM
// library decodes from the original, Burned In Annotation NO and the Clean
// Pixel Data code recorded; and a template of another size does not fit.
func TestTemplateRuleAtReception(t *testing.T) {
	e := newEnv(t, envOptions{deid: &deidOptions{doc: pixelDoc(templateRule())}})
	templates, ok := e.store.(tenant.PixelTemplates)
	if !ok {
		t.Fatal("the store keeps no pixel templates")
	}
	other := sonoSiteTemplate()
	other.Rows = 241
	if err := templates.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{other}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	body := corpus(t, "jpeg_3frame_nobot.dcm")
	e.sendBytes(body, Received)
	e.waitFor("the hold", 2*time.Second, func() bool { return len(e.held()) == 1 })
	q := e.held()[0]
	if q.Reason != tenant.QuarantineDeidFailed || q.Details["cause"] != "no_pixel_template" || !q.ContentKept {
		t.Fatalf("held %s %v, kept %v", q.Reason, q.Details, q.ContentKept)
	}
	if err := q.Validate(); err != nil {
		t.Errorf("the record: %v", err)
	}
	for _, r := range e.receptions() {
		if r.Counts.FilesArchived != 0 {
			t.Fatalf("a reception archived %d files before a template fit", r.Counts.FilesArchived)
		}
	}
	if err := templates.AddPixelTemplates(e.ctx, []tenant.PixelTemplate{sonoSiteTemplate()}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * time.Second)
	if _, err := e.p.Release(e.ctx, tenant.Release{ID: q.ID, Mode: tenant.ReleaseAsIs}, e.audit(), nil); err != nil {
		t.Fatal(err)
	}
	r := e.settle(1)[0]
	if !done(r) || r.Counts.FilesArchived != 1 {
		t.Fatalf("reception %s %+v, held %v", r.State, r.Counts, e.held())
	}
	after, before := decoded(t, archivedOne(t, e, r)), decoded(t, body)
	info, _ := after.GetPixelDataInfo()
	comparePixels(t, before, after, placed(testRects, info.Columns, info.Rows))
	if v := text(after, 0x00280301); v != "NO" {
		t.Errorf("Burned In Annotation %q", v)
	}
}
