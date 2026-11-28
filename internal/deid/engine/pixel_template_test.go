package engine

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// fakeTemplates is a tenant's pixel templates for a test: the rectangles of
// one device and size, and the queries it was asked.
type fakeTemplates struct {
	manufacturer, model string
	rows, columns       int
	rects               []*v1.PixelRectangle
	err                 error
	asked               []TemplateQuery
}

// Template answers the rectangles when the query is its device and size,
// comparing text with case folded as the tenant's store does.
func (f *fakeTemplates) Template(_ context.Context, q TemplateQuery) ([]*v1.PixelRectangle, bool, error) {
	f.asked = append(f.asked, q)
	if f.err != nil {
		return nil, false, f.err
	}
	if strings.EqualFold(strings.TrimSpace(q.Manufacturer), f.manufacturer) && strings.EqualFold(strings.TrimSpace(q.Model), f.model) &&
		q.Rows == f.rows && q.Columns == f.columns {
		return f.rects, true, nil
	}
	return nil, false, nil
}

// templateRule is a pixel rule that fills by the tenant's template, in
// images of any kind.
func templateRule(name string) *v1.PixelRule {
	return &v1.PixelRule{Name: name, Action: &v1.PixelRule_RedactByTemplate{RedactByTemplate: &v1.RedactByTemplate{}}}
}

// templateEngine is an engine over the basic profile with a rule that fills
// by template, and the templates given.
func templateEngine(t testing.TB, templates PixelTemplates) *Engine {
	t.Helper()
	return newDocEngine(t, testKeys, Options{PixelTemplates: templates}, &v1.ProfileDocument{
		Bases:      []string{profile.AnnexE("basic")},
		Rules:      []*v1.ProfileRule{rule("pid", kw("PatientID"), aHash)},
		PixelRules: []*v1.PixelRule{templateRule("by-device")},
	})
}

// deviceImage is image with the device's Manufacturer and Model Name, as
// received.
func deviceImage(t testing.TB, modality, class, burnedIn, manufacturer, model string, rows, cols uint16) *dataset.Dataset {
	t.Helper()
	ds := image(t, modality, class, burnedIn, rows, cols, 1)
	put(t, ds, 0x00080070, "LO", manufacturer)
	put(t, ds, 0x00081090, "LO", model)
	return ds
}

// wantFilled checks that after holds zero inside the regions of an 8-bit
// image of one sample per pixel and, outside them, before's bytes.
func wantFilled(t *testing.T, before, after []byte, rows, cols int, regions []dataset.PixelRegion) {
	t.Helper()
	if len(after) != len(before) {
		t.Fatalf("the image is %d bytes after, %d before", len(after), len(before))
	}
	for y := range rows {
		for x := range cols {
			in := false
			for _, r := range regions {
				if x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height {
					in = true
				}
			}
			i := y*cols + x
			switch {
			case in && after[i] != 0:
				t.Fatalf("(%d,%d) inside a rectangle is %d, want 0", x, y, after[i])
			case !in && after[i] != before[i]:
				t.Fatalf("(%d,%d) outside every rectangle changed from %d to %d", x, y, before[i], after[i])
			}
		}
	}
}

// TestTemplateRuleFillsExactlyItsRectangles guards the template action: the
// template is looked up by the device and size the file gave, as received,
// with the text as written; exactly its rectangles are blanked (one from
// the top left, one measured from the bottom right) and every other sample
// is as received; and the file is recorded as cleaned (Burned In Annotation
// NO and the Clean Pixel Data code) as a redact rule's is.
func TestTemplateRuleFillsExactlyItsRectangles(t *testing.T) {
	tpl := &fakeTemplates{manufacturer: "acme imaging", model: "scan one", rows: 24, columns: 32,
		rects: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 32, Height: 3}, {X: 2, Y: 1, Width: 5, Height: 4, FromRight: true, FromBottom: true}}}
	en := templateEngine(t, tpl)
	ds := deviceImage(t, "US", usImageStorage, "", "ACME Imaging  ", "Scan ONE", 24, 32)
	before := append([]byte(nil), pixels(t, ds)...)
	rec := apply(t, en, ds)
	if len(tpl.asked) != 1 || tpl.asked[0] != (TemplateQuery{Manufacturer: "ACME Imaging", Model: "Scan ONE", Rows: 24, Columns: 32, SOPClass: usImageStorage}) {
		t.Fatalf("the template was asked for %+v, want the device, size and class as received", tpl.asked)
	}
	want := []dataset.PixelRegion{{X: 0, Y: 0, Width: 32, Height: 3}, {X: 25, Y: 19, Width: 5, Height: 4}}
	if w := rec.Pixels; w == nil || w.Rule != "by-device" || w.Action != "redact" || !w.AtRisk || len(w.Regions) != 2 || w.Regions[0] != want[0] || w.Regions[1] != want[1] {
		t.Fatalf("pixel work %+v, want the rule by-device and regions %v", rec.Pixels, want)
	}
	wantFilled(t, before, pixels(t, ds), 24, 32, want)
	if v, _ := text(ds, tagBurnedInAnnotation); v != "NO" {
		t.Errorf("Burned In Annotation %q, want NO", v)
	}
	if got := strings.Join(codes(t, ds), " "); got != "113100 113101" {
		t.Errorf("codes %s, want the basic profile's then Clean Pixel Data's", got)
	}
}

// TestTemplateRuleHoldsWhatNoTemplateFits guards the failure to close: an
// image at risk of burned-in text with no template for its device or size
// (another size, another model, no templates reachable, or none given at
// all) is held as no_pixel_template with its pixels as received, and a
// template read that fails is the read's error, not an archived file.
func TestTemplateRuleHoldsWhatNoTemplateFits(t *testing.T) {
	rects := []*v1.PixelRectangle{{X: 0, Y: 0, Width: 8, Height: 2}}
	for _, c := range []struct {
		name     string
		tpl      PixelTemplates
		model    string
		rows     uint16
		wantFail bool
	}{
		{"another size", &fakeTemplates{manufacturer: "acme", model: "q1", rows: 16, columns: 16, rects: rects}, "Q1", 17, true},
		{"another model", &fakeTemplates{manufacturer: "acme", model: "q1", rows: 16, columns: 16, rects: rects}, "Q2", 16, true},
		{"no templates given", nil, "Q1", 16, true},
		{"a template that fits", &fakeTemplates{manufacturer: "acme", model: "q1", rows: 16, columns: 16, rects: rects}, "Q1", 16, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			en := templateEngine(t, c.tpl)
			ds := deviceImage(t, "US", usImageStorage, "", "ACME", c.model, c.rows, 16)
			before := append([]byte(nil), pixels(t, ds)...)
			_, err := en.Apply(context.Background(), ds, Reception{})
			var f *Failure
			switch {
			case c.wantFail && (!errors.As(err, &f) || f.Cause != CausePixelNoTemplate || f.Rule != "by-device"):
				t.Fatalf("got %v, want a Failure no_pixel_template naming the rule", err)
			case c.wantFail && string(pixels(t, ds)) != string(before):
				t.Fatal("an image held for want of a template was changed")
			case !c.wantFail && err != nil:
				t.Fatalf("an image a template fits was refused: %v", err)
			}
			if f != nil && (f.Reason() != ReasonFailed || f.Detail() != "no_pixel_template") {
				t.Errorf("the quarantine says %q, %q, want a failed de-identification with the cause no_pixel_template", f.Reason(), f.Detail())
			}
		})
	}
	boom := errors.New("the tenant's database does not answer")
	en := templateEngine(t, &fakeTemplates{err: boom})
	if _, err := en.Apply(context.Background(), deviceImage(t, "US", usImageStorage, "", "ACME", "Q1", 16, 16), Reception{}); !errors.Is(err, boom) {
		t.Errorf("a failed template read gave %v, want its error", err)
	}
}

// TestTemplateRuleLeavesAnImageNotAtRisk guards the other side of the
// closed gate: a template is for a device known to draw text, so an image
// that may carry none (a CT that does not say it does) with no template is
// archived as received, not held, and one with a template is filled.
func TestTemplateRuleLeavesAnImageNotAtRisk(t *testing.T) {
	tpl := &fakeTemplates{manufacturer: "acme", model: "ct", rows: 16, columns: 16, rects: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 4, Height: 4}}}
	en := templateEngine(t, tpl)
	other := deviceImage(t, "CT", ctImageStorage, "", "ACME", "OTHER", 16, 16)
	before := append([]byte(nil), pixels(t, other)...)
	rec := apply(t, en, other)
	if rec.Pixels.AtRisk || len(rec.Pixels.Regions) != 0 || string(pixels(t, other)) != string(before) {
		t.Errorf("an image not at risk with no template: pixel work %+v, pixels changed %v", rec.Pixels, string(pixels(t, other)) != string(before))
	}
	fit := deviceImage(t, "CT", ctImageStorage, "", "ACME", "CT", 16, 16)
	before = append([]byte(nil), pixels(t, fit)...)
	apply(t, en, fit)
	wantFilled(t, before, pixels(t, fit), 16, 16, []dataset.PixelRegion{{X: 0, Y: 0, Width: 4, Height: 4}})
}

// TestTemplateRuleOnCorpusFiles guards the template action on public files:
// a corpus MR image (TOSHIBA_MEC MRT50H1, 64 by 64) is filled in exactly the
// template's rectangles and every other sample is as the file has it,
// compared with the file read again from disk; and the secondary capture of
// the receiver's corpus, whose header names no device, is held. With the
// variable of pydicom's corpus set, an ultrasound of it (a GE LOGIQ 700)
// is filled the same way, as an image at risk.
func TestTemplateRuleOnCorpusFiles(t *testing.T) {
	rects := []*v1.PixelRectangle{{X: 0, Y: 0, Width: 64, Height: 6}, {X: 3, Y: 2, Width: 10, Height: 8, FromRight: true, FromBottom: true}}
	want := []dataset.PixelRegion{{X: 0, Y: 0, Width: 64, Height: 6}, {X: 51, Y: 54, Width: 10, Height: 8}}
	tpl := &fakeTemplates{manufacturer: "toshiba_mec", model: "mrt50h1", rows: 64, columns: 64, rects: rects}
	en := templateEngine(t, tpl)
	ds, err := readPath(t, filepath.Join(receiveTestdata, "MR_small.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	rec := apply(t, en, ds)
	if len(rec.Pixels.Regions) != 2 || rec.Pixels.Regions[0] != want[0] || rec.Pixels.Regions[1] != want[1] {
		t.Fatalf("regions %v, want %v", rec.Pixels.Regions, want)
	}
	orig, err := readPath(t, filepath.Join(receiveTestdata, "MR_small.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	got, before := pixels(t, readFile(t, writeFile(t, ds))), pixels(t, orig)
	for y := range 64 {
		for x := range 64 {
			in := false
			for _, r := range want {
				in = in || (x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height)
			}
			// The file's samples are signed 16 bits, whose lowest value, the
			// fill, is 0x8000: bytes 00 80, little endian.
			for b, fill := range []byte{0x00, 0x80} {
				i := (y*64+x)*2 + b
				switch {
				case in && got[i] != fill:
					t.Fatalf("(%d,%d) inside is byte %d, want the lowest sample's %d", x, y, got[i], fill)
				case !in && got[i] != before[i]:
					t.Fatalf("(%d,%d) outside changed", x, y)
				}
			}
		}
	}

	sc, err := readPath(t, filepath.Join(receiveTestdata, "SC_rgb_rle_2frame.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	var f *Failure
	if _, err := en.Apply(context.Background(), sc, Reception{}); !errors.As(err, &f) || f.Cause != CausePixelNoTemplate {
		t.Errorf("a secondary capture with no device named: %v, want no_pixel_template", err)
	}

	dir := os.Getenv(corpusEnv)
	if dir == "" {
		t.Skipf("%s is not set: the ultrasound of pydicom's corpus is not checked", corpusEnv)
	}
	us, err := readPath(t, filepath.Join(dir, "examples_rgb_color.dcm"))
	if err != nil {
		t.Skipf("the corpus has no examples_rgb_color.dcm: %v", err)
	}
	orig, err = readPath(t, filepath.Join(dir, "examples_rgb_color.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	usRects := []*v1.PixelRectangle{{X: 0, Y: 0, Width: 320, Height: 30}}
	usEngine := templateEngine(t, &fakeTemplates{manufacturer: "g.e. medical systems", model: "logiq 700", rows: 240, columns: 320, rects: usRects})
	apply(t, usEngine, us)
	got, before = pixels(t, readFile(t, writeFile(t, us))), pixels(t, orig)
	if len(got) != len(before) || len(got) != 240*320*3 {
		t.Skipf("the ultrasound is %d bytes, not interleaved 8-bit RGB of 240 by 320", len(got))
	}
	for i := range got {
		if y := i / (320 * 3); (y < 30 && got[i] != 0) || (y >= 30 && got[i] != before[i]) {
			t.Fatalf("byte %d (row %d) is %d, file has %d", i, y, got[i], before[i])
		}
	}
}

// TestTemplateRuleIsEditionFour guards the language edition: a version that
// fills by template is of edition 4, so a build that does not know the
// action refuses to run it rather than archive an image a template should
// have filled; a version that does not use it stays of the edition it was.
func TestTemplateRuleIsEditionFour(t *testing.T) {
	doc := &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, PixelRules: []*v1.PixelRule{templateRule("by-device")}}
	e, problems := profile.Expand(doc, profile.Shipped())
	if problems != nil {
		t.Fatalf("expanding: %v", problems)
	}
	if e.GetLanguageEdition() != 4 {
		t.Errorf("a version with a template rule is of edition %d, want 4", e.GetLanguageEdition())
	}
	e.LanguageEdition = 3
	if _, err := profile.Compile(e); err == nil {
		t.Error("an edition 3 version that fills by template compiled")
	}
	plain, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}}, profile.Shipped())
	if problems != nil {
		t.Fatalf("expanding: %v", problems)
	}
	if plain.GetLanguageEdition() != 1 {
		t.Errorf("a version without the action is of edition %d, want 1", plain.GetLanguageEdition())
	}
}

// put adds a text element to a test data set.
func put(t testing.TB, ds *dataset.Dataset, tg uint32, vr, value string) {
	t.Helper()
	if err := ds.Add(dataelem.NewDataElement(tag.Tag(tg), dataelem.VR(vr), []byte(value))); err != nil {
		t.Fatal(err)
	}
}
