package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/sequence"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// The SOP classes of the synthetic images (PS3.4 B.5).
const (
	usImageStorage = "1.2.840.10008.5.1.4.1.1.6.1"
	scImageStorage = "1.2.840.10008.5.1.4.1.1.7"
)

// us is a US value (PS3.5 6.2), little endian.
func us(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }

// image returns a native 8-bit MONOCHROME2 image of rows by cols and
// frames, its samples 1 to 250 and never 0 (the fill value), with the
// modality, SOP class and Burned In Annotation given ("" for none).
func image(t testing.TB, modality, class, burnedIn string, rows, cols, frames uint16) *dataset.Dataset {
	t.Helper()
	px := make([]byte, int(rows)*int(cols)*int(frames))
	for i := range px {
		px[i] = byte(1 + i%250)
	}
	els := []el{
		{0x00080016, "UI", class}, {0x00080018, "UI", "1.2.3.4.5.6"}, {0x00080060, "CS", modality},
		{0x00100020, "LO", "PID-4711"}, {0x0020000D, "UI", "1.2.3.4.5"}, {0x0020000E, "UI", "1.2.3.4.5.1"},
		{0x00280002, "US", us(1)}, {0x00280004, "CS", "MONOCHROME2"},
		{0x00280010, "US", us(rows)}, {0x00280011, "US", us(cols)},
		{0x00280100, "US", us(8)}, {0x00280101, "US", us(8)}, {0x00280102, "US", us(7)}, {0x00280103, "US", us(0)},
		{0x7FE00010, "OB", px},
	}
	if frames > 1 {
		els = append(els, el{0x00280008, "IS", itoa(int(frames))})
	}
	if burnedIn != "" {
		els = append(els, el{0x00280301, "CS", burnedIn})
	}
	ds := build(t, els...)
	ds.SetTransferSyntaxUID("1.2.840.10008.1.2.1")
	return ds
}

// itoa writes a small number.
func itoa(n int) string { return strconv.Itoa(n) }

// pixels returns an image's Pixel Data held in memory.
func pixels(t testing.TB, ds *dataset.Dataset) []byte {
	t.Helper()
	e, ok := ds.Get(tag.Tag(tagPixelData))
	if !ok {
		t.Fatal("no Pixel Data")
	}
	b, ok := e.Bytes()
	if !ok {
		t.Fatal("Pixel Data is not in memory")
	}
	return b
}

// redactRule is a pixel rule filling rects in images of the modality.
func redactRule(name, modality string, rects ...*v1.PixelRectangle) *v1.PixelRule {
	r := &v1.PixelRule{Name: name, Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: rects}}}
	if modality != "" {
		r.When = []*v1.ProfileCondition{{Subject: v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, Test: &v1.ProfileCondition_Equals{Equals: modality}}}
	}
	return r
}

// pixelEngine is an engine over the basic profile, the bases given, and
// pixel rules.
func pixelEngine(t testing.TB, bases []string, rules ...*v1.PixelRule) *Engine {
	t.Helper()
	return newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{
		Bases:      append([]string{profile.AnnexE("basic")}, bases...),
		Rules:      []*v1.ProfileRule{rule("pid", kw("PatientID"), aHash)},
		PixelRules: rules,
	})
}

// codes returns the code values of De-identification Method Code Sequence.
func codes(t testing.TB, ds *dataset.Dataset) []string {
	t.Helper()
	e, ok := ds.Get(tag.Tag(tagDeidMethodCodes))
	if !ok {
		return nil
	}
	seq, _ := e.GetValue().(*sequence.Sequence)
	var out []string
	for _, x := range seq.Items() {
		it, _ := x.(*dataset.Dataset)
		v, _ := text(it, 0x00080100)
		out = append(out, v)
	}
	return out
}

// TestBurnedInGateHoldsWhatNoRuleCovers guards the gate: an image whose
// header says it may carry burned-in text (Burned In Annotation YES, a
// secondary capture, a modality commonly drawn on) and that no pixel rule
// matches is refused as burned_in_risk with the cause named, unchanged;
// one whose header says NO, or of a modality not drawn on, passes with
// its pixels as they were; and the risk is read as received, so a rule of
// the profile setting Burned In Annotation NO does not open the gate.
func TestBurnedInGateHoldsWhatNoRuleCovers(t *testing.T) {
	en := pixelEngine(t, nil)
	for _, c := range []struct {
		name, modality, class, burnedIn, cause string
	}{
		{"an ultrasound", "US", usImageStorage, "", "modality"},
		{"a secondary capture", "OT", scImageStorage, "", "secondary_capture"},
		{"a CT that says YES", "CT", ctImageStorage, "YES", "burned_in_annotation"},
		{"an ultrasound that says NO", "US", usImageStorage, "NO", ""},
		{"a CT that says nothing", "CT", ctImageStorage, "", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			ds := image(t, c.modality, c.class, c.burnedIn, 8, 8, 1)
			before := append([]byte(nil), pixels(t, ds)...)
			rec, err := en.Apply(context.Background(), ds, Reception{})
			var f *Failure
			if c.cause == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if rec.Pixels == nil || rec.Pixels.AtRisk || rec.Pixels.Action != "" {
					t.Errorf("pixel work %+v", rec.Pixels)
				}
				if string(pixels(t, ds)) != string(before) {
					t.Error("the pixels changed")
				}
				return
			}
			if !errors.As(err, &f) || f.Cause != CauseBurnedInRisk || f.RiskCause != c.cause || f.Reason() != ReasonBurnedIn || f.Detail() != c.cause {
				t.Fatalf("got %v (%+v), want burned_in_risk %s", err, f, c.cause)
			}
			if v, _ := text(ds, 0x00100020); v != "PID-4711" {
				t.Error("a refused file was changed")
			}
		})
	}
	setsNo := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{
		Bases: []string{profile.AnnexE("basic")},
		Rules: []*v1.ProfileRule{rule("says-no", kw("BurnedInAnnotation"), aReplace("NO"))},
	})
	_, err := setsNo.Apply(context.Background(), image(t, "US", usImageStorage, "YES", 8, 8, 1), Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CauseBurnedInRisk {
		t.Errorf("a rule writing NO opened the gate: %v", err)
	}
}

// TestRedactRuleFillsItsRectangles guards a redact rule on a native image
// held in memory: each rectangle, measured from the edges it names, is
// black (0 for MONOCHROME2) in every frame and every other sample is as it
// was; Burned In Annotation is written NO and the Clean Pixel Data code
// recorded after the basic profile's; the record names the rule, the
// rectangles in the image's pixels, the frames and the size decoded.
func TestRedactRuleFillsItsRectangles(t *testing.T) {
	en := pixelEngine(t, nil, redactRule("top-bar", "US",
		&v1.PixelRectangle{X: 0, Y: 0, Width: 10, Height: 2},
		&v1.PixelRectangle{X: 1, Y: 1, Width: 3, Height: 2, FromRight: true, FromBottom: true}))
	const rows, cols, frames = 8, 10, 3
	ds := image(t, "US", usImageStorage, "", rows, cols, frames)
	before := append([]byte(nil), pixels(t, ds)...)
	rec := apply(t, en, ds)
	w := rec.Pixels
	if w == nil || w.Rule != "top-bar" || w.Action != "redact" || !w.AtRisk || w.RiskCause != "modality" || w.Frames != frames ||
		w.DecodedBytes != rows*cols*frames || w.Decoded {
		t.Fatalf("pixel work %+v", w)
	}
	want := []dataset.PixelRegion{{X: 0, Y: 0, Width: 10, Height: 2}, {X: 6, Y: 5, Width: 3, Height: 2}}
	if len(w.Regions) != 2 || w.Regions[0] != want[0] || w.Regions[1] != want[1] {
		t.Fatalf("regions %v, want %v", w.Regions, want)
	}
	after := pixels(t, ds)
	inside := func(x, y int) bool {
		for _, r := range want {
			if x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height {
				return true
			}
		}
		return false
	}
	for f := range frames {
		for y := range rows {
			for x := range cols {
				i := f*rows*cols + y*cols + x
				switch {
				case inside(x, y) && after[i] != 0:
					t.Fatalf("frame %d (%d,%d) inside is %d, want 0", f, x, y, after[i])
				case !inside(x, y) && after[i] != before[i]:
					t.Fatalf("frame %d (%d,%d) outside changed", f, x, y)
				}
			}
		}
	}
	if v, _ := text(ds, tagBurnedInAnnotation); v != "NO" {
		t.Errorf("Burned In Annotation %q", v)
	}
	if got := strings.Join(codes(t, ds), " "); got != "113100 113101" {
		t.Errorf("codes %s, want the basic profile's then Clean Pixel Data's", got)
	}
}

// TestPixelWorkRecordsOnlyWhatItDid guards the method record's truth: an
// accept rule leaves the pixels and records no Clean Pixel Data code, even
// on a profile built on the Clean Pixel Data base; a CT no rule matches
// records none either; and a detect rule that finds nothing changes
// nothing and notes no automatic redaction.
func TestPixelWorkRecordsOnlyWhatItDid(t *testing.T) {
	accept := &v1.PixelRule{Name: "reviewed", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "read by a person after"}
	en := pixelEngine(t, []string{profile.AnnexE("clean-pixel-data")}, accept)
	ds := image(t, "US", usImageStorage, "", 8, 8, 1)
	before := append([]byte(nil), pixels(t, ds)...)
	rec := apply(t, en, ds)
	if rec.Pixels.Action != "accept" || len(rec.Pixels.Regions) != 0 || string(pixels(t, ds)) != string(before) {
		t.Errorf("accept: %+v", rec.Pixels)
	}
	if got := strings.Join(codes(t, ds), " "); got != "113100" {
		t.Errorf("accept on the Clean Pixel Data base recorded %s", got)
	}
	if _, ok := text(ds, tagBurnedInAnnotation); ok {
		t.Error("accept wrote Burned In Annotation")
	}
	detect := pixelEngine(t, nil, &v1.PixelRule{Name: "find", Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}})
	plain := image(t, "US", usImageStorage, "", 64, 64, 1)
	px := pixels(t, plain)
	for i := range px {
		px[i] = 90 // a flat picture holds no lettering
	}
	rec = apply(t, detect, plain)
	if rec.Pixels.Action != "detect" || len(rec.Pixels.Regions) != 0 {
		t.Errorf("detect on a flat picture: %+v", rec.Pixels)
	}
	if v, _ := text(plain, tagDeidMethod); strings.Contains(v, anonymize.AutomaticRedactionNote) {
		t.Error("a detector that found nothing noted a redaction")
	}
}

// TestPixelRefusals guards the fixed causes of the work on pixels: an image
// past the limit is pixels_too_large, never filled; a rectangle wholly
// outside the image is pixel_region_outside; a layout the library cannot
// fill (12 bits allocated) is pixels_unsupported; each names the rule and
// leaves the data set as it was.
func TestPixelRefusals(t *testing.T) {
	bar := redactRule("bar", "", &v1.PixelRectangle{X: 0, Y: 0, Width: 4, Height: 1})
	en := pixelEngine(t, nil, bar)
	ds := image(t, "US", usImageStorage, "", 16, 16, 4)
	before := append([]byte(nil), pixels(t, ds)...)
	_, err := en.ApplyImage(context.Background(), ds, Reception{}, Image{Limit: 16*16*4 - 1})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CausePixelsTooLarge || f.Rule != "bar" {
		t.Errorf("past the limit: %v", err)
	}
	if string(pixels(t, ds)) != string(before) {
		t.Error("an image past the limit was changed")
	}
	if _, err := en.ApplyImage(context.Background(), image(t, "US", usImageStorage, "", 16, 16, 4), Reception{}, Image{Limit: 16 * 16 * 4}); err != nil {
		t.Errorf("at the limit: %v", err)
	}
	outside := pixelEngine(t, nil, redactRule("far", "", &v1.PixelRectangle{X: 100, Y: 0, Width: 4, Height: 1}))
	if _, err := outside.Apply(context.Background(), image(t, "US", usImageStorage, "", 16, 16, 1), Reception{}); !errors.As(err, &f) ||
		f.Cause != CausePixelRegionOutside || f.Rule != "far" {
		t.Errorf("outside: %v", err)
	}
	odd := image(t, "US", usImageStorage, "", 4, 4, 1)
	odd.Remove(tag.Tag(0x00280100))
	if err := odd.Add(dataelem.NewDataElement(tag.Tag(0x00280100), dataelem.US, us(12))); err != nil {
		t.Fatal(err)
	}
	if _, err := en.Apply(context.Background(), odd, Reception{}); !errors.As(err, &f) || f.Cause != CausePixelsUnsupported {
		t.Errorf("12 bits allocated: %v", err)
	}
}

// TestUnsearchedSegmentsFollowTheProfile guards compressed pixels whose
// segments are not searched (here JPEG 2000 Part 2, a JPEG 2000 file
// relabeled): held as pixel_text_unsearched by default and by a document
// that says hold, archived by one that says keep; and the two documents
// that hold have one checksum.
func TestUnsearchedSegmentsFollowTheProfile(t *testing.T) {
	path := receiveTestdata + "/MR_small_jp2klossless.dcm"
	const part2 = "1.2.840.10008.1.2.4.92" // JPEG 2000 Part 2 Multi-component Image Compression (Lossless Only)
	for _, c := range []struct {
		name string
		say  v1.UnsearchedSegments
		held bool
	}{
		{"unspecified", v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_UNSPECIFIED, true},
		{"hold", v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_HOLD, true},
		{"keep", v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_KEEP, false},
	} {
		en := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, UnsearchedSegments: c.say})
		ds, err := readPath(t, path)
		if err != nil {
			t.Fatal(err)
		}
		ds.SetTransferSyntaxUID(part2)
		_, err = en.Apply(context.Background(), ds, Reception{})
		var f *Failure
		if got := errors.As(err, &f) && f.Cause == CausePixelTextUnsearched; got != c.held {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	held, _ := profile.Expand(&v1.ProfileDocument{UnsearchedSegments: v1.UnsearchedSegments_UNSEARCHED_SEGMENTS_HOLD}, profile.Shipped())
	plain, _ := profile.Expand(&v1.ProfileDocument{}, profile.Shipped())
	a, _ := profile.Canonical(held)
	b, _ := profile.Canonical(plain)
	if string(a) != string(b) {
		t.Error("a document saying hold expands otherwise than one saying nothing")
	}
}
