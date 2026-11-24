package engine

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/compress"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/encaps"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// cause returns the cause of a Failure err is, or "" for none.
func cause(err error) Cause {
	var f *Failure
	if errors.As(err, &f) {
		return f.Cause
	}
	return ""
}

// fillAll is a redact rule covering a whole 8 by 8 image.
func fillAll() *v1.PixelRule {
	return redactRule("all", "", &v1.PixelRectangle{X: 0, Y: 0, Width: 8, Height: 8})
}

// TestFramesMismatchIsRefused guards native pixels holding more frames'
// bytes than Number of Frames (0028,0008) states (absent, empty, 0 or
// negative counting as one): refused as frames_mismatch before any fill,
// the pixels and Burned In Annotation as received; one frame's bytes with
// the element absent, and an odd length's pad byte, pass.
func TestFramesMismatchIsRefused(t *testing.T) {
	en := pixelEngine(t, nil, fillAll())
	for _, nf := range []string{"absent", "", "0", "-2", "2"} {
		ds := image(t, "US", usImageStorage, "", 8, 8, 3)
		ds.Remove(tag.Tag(tagNumberOfFrames))
		if nf != "absent" {
			if err := ds.Add(dataelem.NewDataElement(tag.Tag(tagNumberOfFrames), dataelem.IS, []byte(nf))); err != nil {
				t.Fatal(err)
			}
		}
		before := string(pixels(t, ds))
		_, err := en.Apply(context.Background(), ds, Reception{})
		if cause(err) != CauseFramesMismatch {
			t.Errorf("Number of Frames %q over 3 frames' bytes: %v", nf, err)
		}
		if string(pixels(t, ds)) != before {
			t.Errorf("Number of Frames %q: pixels changed", nf)
		}
		if _, ok := text(ds, tagBurnedInAnnotation); ok {
			t.Errorf("Number of Frames %q: Burned In Annotation written", nf)
		}
	}
	one := image(t, "US", usImageStorage, "", 8, 8, 1)
	e, _ := one.Get(tag.Tag(tagPixelData))
	e.SetValue(append(pixels(t, one), 0))
	if _, err := en.Apply(context.Background(), one, Reception{}); err != nil {
		t.Errorf("one frame and a pad byte: %v", err)
	}
}

// TestNestedPixelsAreRemoved guards pixel data below the top level: under
// the basic profile, an item of the Anatomic Region Sequence holding
// (7FE0,0010) has it removed, for the reason the pipeline gives; and no
// profile can keep the Icon Image Sequence, refused at validation.
func TestNestedPixelsAreRemoved(t *testing.T) {
	en := newDocEngine(t, testKeys, Options{}, &v1.ProfileDocument{
		Bases: []string{profile.AnnexE("basic")},
		Rules: []*v1.ProfileRule{rule("keep-region", kw("AnatomicRegionSequence"), aKeep)},
	})
	region := build(t, el{0x00080100, "SH", "T-D3000"}, el{0x7FE00010, "OB", "DOE^JOHN 1970"})
	ds := build(t, el{tSOPClass, "UI", ctImageStorage}, el{tSOPInstance, "UI", "1.2.3.4"}, el{0x00100020, "LO", "PID-1"},
		el{0x00082218, "SQ", []*dataset.Dataset{region}})
	rec := apply(t, en, ds)
	got := item(t, ds, 0x00082218, 0)
	if _, ok := got.Get(tag.Tag(tagPixelData)); ok {
		t.Fatal("pixel data in an item was kept")
	}
	found := false
	for _, e := range rec.Entries {
		if e.Path == "00082218[0]/7FE00010" {
			found = e.Action == "remove" && e.Reason == profile.NestedPixelReason
		}
	}
	if !found {
		t.Error("no entry removing the nested pixel data for its reason")
	}
	_, problems := profile.Expand(&v1.ProfileDocument{Rules: []*v1.ProfileRule{rule("icon", kw("IconImageSequence"), aKeep)}}, profile.Shipped())
	if len(problems) == 0 {
		t.Error("a rule keeping the Icon Image Sequence was taken")
	}
}

// jpegImage returns an 8 by 8 MR image compressed by the DICOM library in
// syntax (JPEG Baseline, JPEG-LS lossless or JPEG 2000 lossless), with
// change applied to its one frame's codestream.
func jpegImage(t *testing.T, syntax string, change func([]byte) []byte) *dataset.Dataset {
	t.Helper()
	ds := image(t, "MR", "1.2.840.10008.5.1.4.1.1.4", "", 8, 8, 1)
	var err error
	switch syntax {
	case compress.JPEGBaseline:
		err = ds.CompressJPEGBaselineInPlace(90)
	case compress.JPEG2000Lossless:
		err = ds.CompressJPEG2000LosslessInPlace()
	default:
		err = ds.CompressJPEGLSLosslessInPlace()
	}
	if err != nil {
		t.Fatal(err)
	}
	if change == nil {
		return ds
	}
	enc, err := ds.ExtractEncapsulatedFrames()
	if err != nil {
		t.Fatal(err)
	}
	frames, err := encaps.Frames(enc, 1)
	if err != nil {
		t.Fatal(err)
	}
	f := frames[0]
	for len(f) > 2 && f[len(f)-1] == 0 && f[len(f)-2] != 0xFF {
		f = f[:len(f)-1] // the pad of the library's even length
	}
	f = change(append([]byte(nil), f...))
	if len(f)%2 == 1 {
		f = append(f, 0)
	}
	value, err := compress.EncapsulateFrames([][]byte{f}, 1, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := ds.SetValue(tag.Tag(tagPixelData), value); err != nil {
		t.Fatal(err)
	}
	return ds
}

// afterSOI inserts a segment of marker m and payload p after a frame's
// start-of-image marker.
func afterSOI(m byte, p []byte) func([]byte) []byte {
	return func(f []byte) []byte {
		seg := binary.BigEndian.AppendUint16([]byte{0xFF, m}, uint16((len(p)+2)&0xFFFF))
		return append(append(append([]byte(nil), f[:2]...), append(seg, p...)...), f[2:]...)
	}
}

// TestSegmentRemovalIsBounded guards the bound of JPEG segment removal: a
// small JPEG image whose frame carries megabytes after its end-of-image
// marker is refused as pixels_too_large before its pixels are read whole,
// and one whose image is not described (no Rows) as pixels_unsupported.
func TestSegmentRemovalIsBounded(t *testing.T) {
	en := pixelEngine(t, nil)
	big := jpegImage(t, compress.JPEGBaseline, func(f []byte) []byte { return append(f, make([]byte, 2<<20)...) })
	if _, err := en.ApplyImage(context.Background(), big, Reception{}, Image{Limit: 1 << 20}); cause(err) != CausePixelsTooLarge {
		t.Errorf("2 MiB of compressed pixels under a 1 MiB limit: %v", err)
	}
	norows := jpegImage(t, compress.JPEGBaseline, nil)
	norows.Remove(tag.Tag(0x00280010))
	if _, err := en.ApplyImage(context.Background(), norows, Reception{}, Image{Limit: 1 << 20}); cause(err) != CausePixelsUnsupported {
		t.Errorf("no Rows: %v", err)
	}
	if _, err := en.ApplyImage(context.Background(), jpegImage(t, compress.JPEGBaseline, nil), Reception{}, Image{Limit: 1 << 20}); err != nil {
		t.Errorf("a plain small JPEG image: %v", err)
	}
}

// TestSegmentsBeyondTheDefinedAreHeld guards what is left after segment
// removal: a JPEG-LS color transform segment with text after its defined
// bytes, a reserved JPGn marker holding text, and a quantization table
// segment with text after its table are each held as
// pixel_text_unsearched; a JPEG-LS image and a JPEG image as the library
// writes them pass.
func TestSegmentsBeyondTheDefinedAreHeld(t *testing.T) {
	en := pixelEngine(t, nil)
	dqt := append(append([]byte{0x00}, make([]byte, 64)...), "PATIENT DOE"...)
	for _, c := range []struct {
		name   string
		syntax string
		change func([]byte) []byte
	}{
		{"mrfx with a name after it", compress.JPEGLSLossless, afterSOI(0xE8, append([]byte("mrfx\x01"), "DOE^JOHN 19700101"...))},
		{"a JPGn marker with text", compress.JPEGBaseline, afterSOI(0xF1, []byte("DOE^JOHN"))},
		{"a DQT with text after its table", compress.JPEGBaseline, afterSOI(0xDB, dqt)},
	} {
		_, err := en.Apply(context.Background(), jpegImage(t, c.syntax, c.change), Reception{})
		if cause(err) != CausePixelTextUnsearched {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	for _, syntax := range []string{compress.JPEGLSLossless, compress.JPEGBaseline} {
		if _, err := en.Apply(context.Background(), jpegImage(t, syntax, nil), Reception{}); err != nil {
			t.Errorf("%s as the library writes it: %v", syntax, err)
		}
	}
	if _, err := en.Apply(context.Background(), jpegImage(t, compress.JPEGLSLossless, afterSOI(0xE8, []byte("mrfx\x01"))), Reception{}); err != nil {
		t.Errorf("mrfx at its defined length: %v", err)
	}
	// A frame padded after its end-of-image marker with the fill byte 0xFF
	// (ITU-T T.81 B.1.1.2), as pydicom's JPEG-lossy.dcm and JPGExtended.dcm
	// are, passes as it is; text there is removed as trailing data.
	fill := func(f []byte) []byte { return append(f, 0xFF) }
	if _, err := en.Apply(context.Background(), jpegImage(t, compress.JPEGBaseline, fill), Reception{}); err != nil {
		t.Errorf("a 0xFF fill byte after the end of the image: %v", err)
	}
	letter := func(f []byte) []byte { return append(f, 'D', 'O', 'E', 0) }
	rec, err := en.Apply(context.Background(), jpegImage(t, compress.JPEGBaseline, letter), Reception{})
	if err != nil || rec.Pixels == nil || !slices.Contains(rec.Pixels.SegmentsRemoved, "trailing data") {
		t.Errorf("text after the end of the image: %v, %+v", err, rec)
	}
}

// TestRiskByClassModalityAndSlideLabel guards the gate's own list beside
// the library's: a VL endoscopic image of no listed modality is at risk by
// its SOP class, an intra-oral radiograph by its modality, a whole slide's
// label image whatever Burned In Annotation says; a DX image is not, and a
// listed class saying NO is not.
func TestRiskByClassModalityAndSlideLabel(t *testing.T) {
	en := pixelEngine(t, nil)
	for _, c := range []struct {
		name, modality, class, burnedIn, imageType, cause string
	}{
		{"VL endoscopic", "XX", "1.2.840.10008.5.1.4.1.1.77.1.1", "", "", "sop_class"},
		{"intra-oral", "IO", "1.2.840.10008.5.1.4.1.1.1.3", "", "", "modality"},
		{"slide label", "SM", wholeSlide, "NO", `DERIVED\PRIMARY\LABEL\NONE`, "slide_label"},
		{"slide volume", "XX", wholeSlide, "NO", `ORIGINAL\PRIMARY\VOLUME\NONE`, ""},
		{"DX", "DX", "1.2.840.10008.5.1.4.1.1.1.1", "", "", ""},
		{"VL endoscopic saying NO", "XX", "1.2.840.10008.5.1.4.1.1.77.1.1", "NO", "", ""},
	} {
		ds := image(t, c.modality, c.class, c.burnedIn, 8, 8, 1)
		if c.imageType != "" {
			if err := ds.Add(dataelem.NewDataElement(tag.Tag(0x00080008), dataelem.CS, []byte(c.imageType))); err != nil {
				t.Fatal(err)
			}
		}
		_, err := en.Apply(context.Background(), ds, Reception{})
		var f *Failure
		switch {
		case c.cause == "" && err != nil:
			t.Errorf("%s: %v", c.name, err)
		case c.cause != "" && (!errors.As(err, &f) || f.Cause != CauseBurnedInRisk || f.RiskCause != c.cause):
			t.Errorf("%s: %v, want burned_in_risk %s", c.name, err, c.cause)
		}
	}
}

// TestCleanClaimNeedsEveryImageFilled guards the method record: a file
// holding Float Pixel Data beside the image a redact rule fills records
// neither the Clean Pixel Data code nor Burned In Annotation NO.
func TestCleanClaimNeedsEveryImageFilled(t *testing.T) {
	en := pixelEngine(t, nil, fillAll())
	ds := image(t, "US", usImageStorage, "", 8, 8, 1)
	if err := ds.Add(dataelem.NewDataElement(tag.Tag(tagFloatPixelData), dataelem.OF, make([]byte, 8*8*4))); err != nil {
		t.Fatal(err)
	}
	apply(t, en, ds)
	if got := strings.Join(codes(t, ds), " "); strings.Contains(got, "113101") {
		t.Errorf("codes %s with Float Pixel Data unfilled", got)
	}
	if v, _ := text(ds, tagBurnedInAnnotation); v == "NO" {
		t.Error("Burned In Annotation NO with Float Pixel Data unfilled")
	}
}

// TestDetectorFindingNoneSaysSo guards a detect rule that finds nothing:
// De-identification Method records that the detector found none,
// unverified, and Burned In Annotation stays as received.
func TestDetectorFindingNoneSaysSo(t *testing.T) {
	en := pixelEngine(t, nil, &v1.PixelRule{Name: "find", Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}})
	ds := image(t, "US", usImageStorage, "", 64, 64, 1)
	px := pixels(t, ds)
	for i := range px {
		px[i] = 90
	}
	apply(t, en, ds)
	if v, _ := text(ds, tagDeidMethod); !strings.Contains(v, detectorFoundNone) {
		t.Errorf("De-identification Method %q", v)
	}
	if _, ok := text(ds, tagBurnedInAnnotation); ok {
		t.Error("Burned In Annotation written")
	}
}
