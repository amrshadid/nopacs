package receive

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/testkit"
)

// cine writes a synthetic ultrasound cine of frames 8-bit frames of rows
// by cols, native or, with rle, RLE compressed by the DICOM library, to a
// file, and returns its path: no patient's data, a pattern of samples.
func cine(t testing.TB, rows, cols, frames int, rle bool) string {
	t.Helper()
	ds := dataset.NewDataset()
	ds.SetTransferSyntaxUID("1.2.840.10008.1.2.1")
	us := func(v int) []byte { return []byte{byte(v & 0xFF), byte(v >> 8 & 0xFF)} }
	px := make([]byte, rows*cols*frames)
	for i := range px {
		px[i] = byte(1 + (i/7)%250)
	}
	for _, e := range []struct {
		t  uint32
		vr dataelem.VR
		v  any
	}{
		{0x00080016, dataelem.UI, "1.2.840.10008.5.1.4.1.1.3.1"}, {0x00080018, dataelem.UI, "2.25.4711"},
		{0x00080060, dataelem.CS, "US"}, {0x00100020, dataelem.LO, "SYNTHETIC-1"},
		{0x0020000D, dataelem.UI, "2.25.4712"}, {0x0020000E, dataelem.UI, "2.25.4713"},
		{0x00280002, dataelem.US, us(1)}, {0x00280004, dataelem.CS, "MONOCHROME2"},
		{0x00280008, dataelem.IS, strconv.Itoa(frames)},
		{0x00280010, dataelem.US, us(rows)}, {0x00280011, dataelem.US, us(cols)},
		{0x00280100, dataelem.US, us(8)}, {0x00280101, dataelem.US, us(8)}, {0x00280102, dataelem.US, us(7)},
		{0x00280103, dataelem.US, us(0)}, {0x7FE00010, dataelem.OB, px},
	} {
		v := e.v
		if s, ok := v.(string); ok {
			v = []byte(s)
		}
		if err := ds.Add(dataelem.NewDataElement(tag.Tag(e.t), e.vr, v)); err != nil {
			t.Fatal(err)
		}
	}
	ts := "1.2.840.10008.1.2.1"
	if rle {
		if err := ds.CompressRLELosslessInPlace(); err != nil {
			t.Fatal(err)
		}
		ts = ds.TransferSyntaxUID()
	}
	path := filepath.Join(t.TempDir(), "cine.dcm")
	f, err := os.Create(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	if err := writeDataset(f, ds, ts, nil, true); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

// pixelTestEngine is an engine over the basic profile and a pixel rule.
func pixelTestEngine(t testing.TB, rule *v1.PixelRule) *engine.Engine {
	t.Helper()
	e, problems := profile.Expand(&v1.ProfileDocument{Bases: []string{profile.AnnexE("basic")}, PixelRules: []*v1.PixelRule{rule}}, profile.Shipped())
	if problems != nil {
		t.Fatal(problems)
	}
	en, err := engine.New(e, testDeidKeys, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return en
}

// deidentifyFile runs the receiver's de-identification of the file at
// path, as the archive step does (its data set's values left in the file,
// its pixels filled as they are written), into a file of dir, and returns
// the pixel work.
func deidentifyFile(t testing.TB, en *engine.Engine, path, dir string) *engine.PixelWork {
	t.Helper()
	in, err := os.Open(path) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = in.Close() }()
	st, err := in.Stat()
	if err != nil {
		t.Fatal(err)
	}
	h, err := header.ReadWithin(in, st.Size(), header.MaxHeader)
	if err != nil {
		t.Fatal(err)
	}
	ds, source, little, err := h.Dataset(in)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := en.ApplyImage(context.Background(), ds, engine.Reception{}, image(h, in, source, 0))
	if err != nil {
		t.Fatal(err)
	}
	out, err := os.Create(filepath.Join(dir, "out.dcm")) // #nosec G304 -- the test's own temporary file.
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = out.Close() }()
	if err := writeDataset(out, ds, writtenSyntax(h.TransferSyntaxUID, ds, rec), source, little); err != nil {
		t.Fatal(err)
	}
	return rec.Pixels
}

// TestPixelFillIsFrameByFrame guards the memory of filling a large image:
// a 25 MiB cine of 100 frames, native and RLE compressed, is filled and
// written (decoded, for RLE) while the live heap grows by a few MiB, a
// frame and the writer's chunks, never by the image.
func TestPixelFillIsFrameByFrame(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector's own memory hides the heap's")
	}
	const rows, cols, frames = 512, 512, 100
	rule := &v1.PixelRule{Name: "bar", Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 512, Height: 40}}}}}
	en := pixelTestEngine(t, rule)
	for _, rle := range []bool{false, true} {
		path := cine(t, rows, cols, frames, rle)
		runtime.GC()
		var base runtime.MemStats
		runtime.ReadMemStats(&base)
		stop := testkit.HeapPeak()
		dir := t.TempDir()
		w := deidentifyFile(t, en, path, dir)
		peak := stop()
		grew := float64(peak) - float64(base.HeapAlloc)
		t.Logf("rle %v: %d frames, %d MiB decoded, decoded %v; the live heap grew by %.1f MiB", rle, w.Frames, w.DecodedBytes>>20, w.Decoded, grew/(1<<20))
		if w.Frames != frames || w.Decoded != rle {
			t.Errorf("pixel work %+v", w)
		}
		out, err := os.ReadFile(filepath.Join(dir, "out.dcm")) // #nosec G304 -- the test's own temporary file.
		if err != nil {
			t.Fatal(err)
		}
		px := pixelBytes(t, decoded(t, out))
		for _, f := range []int{0, frames - 1} {
			frame := px[f*rows*cols : (f+1)*rows*cols]
			for i, v := range frame {
				if inBar := i < 40*cols; inBar != (v == 0) {
					t.Fatalf("rle %v: frame %d byte %d is %d: the bar is not filled, or the rest is", rle, f, i, v)
				}
			}
		}
		if grew > 16<<20 {
			t.Errorf("the heap grew by %.1f MiB for an image of %d MiB", grew/(1<<20), w.DecodedBytes>>20)
		}
	}
}

// BenchmarkPixelWork measures the work on pixels at reception, per frame
// of a 512 by 512 8-bit cine of 20 frames: filling native pixels as they
// are copied, decoding RLE and filling, and running the text detector.
func BenchmarkPixelWork(b *testing.B) {
	const frames = 20
	redact := &v1.PixelRule{Name: "bar", Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 512, Height: 40}}}}}
	detect := &v1.PixelRule{Name: "find", Action: &v1.PixelRule_Detect{Detect: &v1.DetectText{}}}
	for _, c := range []struct {
		name string
		rle  bool
		rule *v1.PixelRule
	}{{"fill-native", false, redact}, {"fill-rle", true, redact}, {"detect-native", false, detect}} {
		b.Run(c.name, func(b *testing.B) {
			en := pixelTestEngine(b, c.rule)
			path := cine(b, 512, 512, frames, c.rle)
			dir := b.TempDir()
			b.ResetTimer()
			for range b.N {
				deidentifyFile(b, en, path, dir)
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*frames), "ns/frame")
		})
	}
}

// TestTrialShowsPixelWork guards what a trial answers of a file's pixels:
// a rule that fills names itself, its rectangles in the image's pixels,
// the frames, the size decoded and that the pixels are written decoded,
// and the file passes; the same image under no pixel rule is held as
// burned_in_risk with its cause; past the pixel bound it is held as
// pixels_too_large.
func TestTrialShowsPixelWork(t *testing.T) {
	b := corpus(t, "jpeg_3frame_nobot.dcm")
	o := TrialOptions{Keys: testDeidKeys, HeaderLimit: header.MaxHeader, Check: func(int64) Checker { return &recordingCheck{} }}
	rule := &v1.PixelRule{Name: "bar", Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: []*v1.PixelRectangle{{X: 0, Y: 0, Width: 320, Height: 20}}}}}
	tr, err := TryFile(t.Context(), pixelTestEngine(t, rule), b, engine.Reception{}, o)
	if err != nil || tr.Hold != nil {
		t.Fatalf("held %+v: %v", tr.Hold, err)
	}
	w := tr.Pixels
	if w == nil || w.Rule != "bar" || w.Action != "redact" || !w.AtRisk || w.RiskCause != "modality" || w.Frames != 3 ||
		w.DecodedBytes != 240*320*3*3 || !w.Decoded || len(w.Regions) != 1 || w.Regions[0] != (dataset.PixelRegion{Width: 320, Height: 20}) {
		t.Errorf("pixel work %+v", w)
	}
	accept := &v1.PixelRule{Name: "ct-only", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "reviewed",
		When: []*v1.ProfileCondition{{Subject: v1.ConditionSubject_CONDITION_SUBJECT_MODALITY, Test: &v1.ProfileCondition_Equals{Equals: "CT"}}}}
	tr, err = TryFile(t.Context(), pixelTestEngine(t, accept), b, engine.Reception{}, o)
	if err != nil || tr.Hold == nil || tr.Hold.Reason != tenant.QuarantineBurnedInRisk || tr.Hold.Details["cause"] != "modality" {
		t.Errorf("no rule for it: %+v, %v", tr.Hold, err)
	}
	o.PixelLimit = 1 << 10
	tr, err = TryFile(t.Context(), pixelTestEngine(t, rule), b, engine.Reception{}, o)
	if err != nil || tr.Hold == nil || tr.Hold.Details["cause"] != "pixels_too_large" {
		t.Errorf("past the bound: %+v, %v", tr.Hold, err)
	}
}

// pixelBytes returns a data set's Pixel Data held in memory.
func pixelBytes(t *testing.T, ds *dataset.Dataset) []byte {
	t.Helper()
	e, ok := ds.Get(tag.New(0x7FE0, 0x0010))
	if !ok {
		t.Fatal("no Pixel Data")
	}
	b, ok := e.Bytes()
	if !ok {
		t.Fatal("the pixels are not in memory")
	}
	return b
}
