package receive

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// pixelsPythonEnv names the Python, with pydicom, that reads every image
// the pixel tests archive and compares its decoded pixels with the
// original's (scripts/check/deid-pixels.py); unset, the check is not run.
const pixelsPythonEnv = "NOPACS_CHECK_DEID_PYTHON"

// pixelsScript is the outside check of filled images.
const pixelsScript = "../../scripts/check/deid-pixels.py"

// pixelPair is a file as received and as archived, for the outside check:
// paths, the rectangles filled, and whether the original lost in its
// compression.
type pixelPair struct {
	Original string   `json:"original"`
	Archived string   `json:"archived"`
	Regions  [][4]int `json:"regions"`
	Lossy    bool     `json:"lossy"`
}

// decoded reads a file whole and returns its data set with its pixels
// decoded, by the DICOM library.
func decoded(t *testing.T, b []byte) *dataset.Dataset {
	t.Helper()
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(b)), filereader.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ds := df.GetDataset()
	if err := ds.DecompressInPlace(); err != nil {
		t.Fatal(err)
	}
	return ds
}

// placed places rects in an image of cols by rows, measured from the edges
// each names, clipped to the image as the fill clips them.
func placed(rects []*v1.PixelRectangle, cols, rows int) []dataset.PixelRegion {
	var out []dataset.PixelRegion
	for _, r := range rects {
		x, y := int(r.X), int(r.Y)
		if r.FromRight {
			x = cols - x - int(r.Width)
		}
		if r.FromBottom {
			y = rows - y - int(r.Height)
		}
		x0, y0 := max(x, 0), max(y, 0)
		x1, y1 := min(x+int(r.Width), cols), min(y+int(r.Height), rows)
		out = append(out, dataset.PixelRegion{X: x0, Y: y0, Width: x1 - x0, Height: y1 - y0})
	}
	return out
}

// regionsIn places testRects in an image of cols by rows.
func regionsIn(cols, rows int) []dataset.PixelRegion { return placed(testRects, cols, rows) }

// blackOf is the stored value of black for each sample of an image, as
// the DICOM library fills: the lowest value (sign extended when signed),
// the highest under MONOCHROME1, for YBR zero luminance and chroma at its
// midpoint, for PALETTE COLOR the index of the darkest entry.
func blackOf(ds *dataset.Dataset, info *dataset.PixelDataInfo) []uint64 {
	stored := info.BitsStored
	lowest, highest := uint64(0), uint64(1)<<stored-1
	if info.PixelRepresentation == 1 {
		lowest, highest = ^uint64(0)<<(stored-1), uint64(1)<<(stored-1)-1
	}
	switch info.PhotometricInterpretation {
	case "RGB":
		return []uint64{0, 0, 0}
	case "YBR_FULL":
		mid := uint64(1) << (stored - 1)
		return []uint64{0, mid, mid}
	case "MONOCHROME1":
		return []uint64{highest}
	case "PALETTE COLOR":
		return []uint64{darkestEntry(ds)}
	}
	return []uint64{lowest}
}

// darkestEntry is the index of the darkest entry of a palette of 16-bit
// entries starting at 0 (PS3.3 C.7.6.3.1.5), by luminance (C.7.6.3.1.2).
func darkestEntry(ds *dataset.Dataset) uint64 {
	var tables [3][]byte
	for c := range tables {
		e, _ := ds.Get(tag.New(0x0028, uint16(0x1201+c)))
		tables[c], _ = e.Bytes()
	}
	best, least := 0, -1
	for i := 0; 2*i+1 < len(tables[0]); i++ {
		l := 299*int(binary.LittleEndian.Uint16(tables[0][2*i:])) + 587*int(binary.LittleEndian.Uint16(tables[1][2*i:])) +
			114*int(binary.LittleEndian.Uint16(tables[2][2*i:]))
		if least < 0 || l < least {
			best, least = i, l
		}
	}
	return uint64(best)
}

// comparePixels fails the test unless every sample of after inside the
// regions is black and every other equals before's, frame by frame, by
// pixel or, with Planar Configuration 1, by plane.
func comparePixels(t *testing.T, before, after *dataset.Dataset, regions []dataset.PixelRegion) {
	t.Helper()
	bi, err := before.GetPixelDataInfo()
	if err != nil {
		t.Fatal(err)
	}
	ai, err := after.GetPixelDataInfo()
	if err != nil {
		t.Fatal(err)
	}
	if ai.Rows != bi.Rows || ai.Columns != bi.Columns || ai.NumberOfFrames != bi.NumberOfFrames || ai.SamplesPerPixel != bi.SamplesPerPixel {
		t.Fatalf("the image is %+v after, %+v before", ai, bi)
	}
	get := func(ds *dataset.Dataset) []byte {
		e, _ := ds.Get(tag.New(0x7FE0, 0x0010))
		b, ok := e.Bytes()
		if !ok {
			t.Fatal("the pixels are not in memory")
		}
		return b
	}
	bp, ap := get(before), get(after)
	width := ai.BitsAllocated / 8
	black := blackOf(after, ai)
	inside := func(x, y int) bool {
		for _, r := range regions {
			if x >= r.X && x < r.X+r.Width && y >= r.Y && y < r.Y+r.Height {
				return true
			}
		}
		return false
	}
	byPlane := ai.SamplesPerPixel > 1 && ai.PlanarConfiguration == 1
	filled, kept := 0, 0
	for f := range ai.NumberOfFrames {
		for y := range ai.Rows {
			for x := range ai.Columns {
				for s := range ai.SamplesPerPixel {
					at := (((f*ai.Rows+y)*ai.Columns+x)*ai.SamplesPerPixel + s) * width
					if byPlane {
						at = (((f*ai.SamplesPerPixel+s)*ai.Rows+y)*ai.Columns + x) * width
					}
					var got uint64
					for k := range width {
						got |= uint64(ap[at+k]) << (8 * k)
					}
					if inside(x, y) {
						want := black[s] & (uint64(1)<<(8*width) - 1)
						if got != want {
							t.Fatalf("frame %d (%d,%d) sample %d inside is %d, want %d", f, x, y, s, got, want)
						}
						filled++
						continue
					}
					if !bytes.Equal(ap[at:at+width], bp[at:at+width]) {
						t.Fatalf("frame %d (%d,%d) sample %d outside changed", f, x, y, s)
					}
					kept++
				}
			}
		}
	}
	if filled == 0 && len(regions) > 0 {
		t.Fatal("nothing was filled")
	}
	t.Logf("%d samples filled, %d as they were, in %d frames", filled, kept, ai.NumberOfFrames)
}

// archivedOne returns the one file a reception archived.
func archivedOne(t *testing.T, e *env, r tenant.Reception) []byte {
	t.Helper()
	var out [][]byte
	for k, b := range archivedObjects(t, e, r) {
		if !strings.HasPrefix(k, "sidecar:") && !strings.HasPrefix(k, "manifest:") {
			out = append(out, b)
		}
	}
	if len(out) != 1 {
		t.Fatalf("%d files archived", len(out))
	}
	return out[0]
}

// text is a top-level text value of a data set, its padding removed.
func text(ds *dataset.Dataset, t uint32) string {
	e, ok := ds.Get(tag.Tag(t))
	if !ok {
		return ""
	}
	b, _ := e.Bytes()
	return strings.TrimRight(string(b), " \x00")
}

// fillAtReception sends body to a receiver whose profile fills rects in
// every image, and checks what it archives: in syntax, every sample of
// the rectangles black in every frame and every other what the DICOM
// library decodes from body; Burned In Annotation NO, the Clean Pixel
// Data code, Lossy Image Compression as lossy says, no Extended Offset
// Table. It returns the pair for the outside check, its files written
// under dir, which outlives the subtest t.
func fillAtReception(t *testing.T, dir string, body []byte, syntax, lossy string, rects []*v1.PixelRectangle) pixelPair {
	t.Helper()
	rule := &v1.PixelRule{Name: "fill", Action: &v1.PixelRule_Redact{Redact: &v1.RedactPixels{Rectangles: rects}}}
	e := newEnv(t, envOptions{deid: &deidOptions{doc: pixelDoc(rule)}})
	e.sendBytes(body, Received)
	r := e.settle(1)[0]
	if !done(r) || r.Counts.FilesArchived != 1 {
		t.Fatalf("reception %s %+v, held %v", r.State, r.Counts, e.held())
	}
	out := archivedOne(t, e, r)
	after, before := decoded(t, out), decoded(t, body)
	if got := after.TransferSyntaxUID(); got != syntax {
		t.Errorf("archived in %s, want %s", got, syntax)
	}
	info, _ := after.GetPixelDataInfo()
	regions := placed(rects, info.Columns, info.Rows)
	comparePixels(t, before, after, regions)
	if v := text(after, 0x00280301); v != "NO" {
		t.Errorf("Burned In Annotation %q", v)
	}
	if v := text(after, 0x00282110); v != lossy {
		t.Errorf("Lossy Image Compression %q, want %q", v, lossy)
	}
	if !strings.Contains(text(after, 0x00120063), "Clean Pixel Data") {
		t.Errorf("De-identification Method %q names no Clean Pixel Data", text(after, 0x00120063))
	}
	if _, ok := after.Get(tag.New(0x7FE0, 0x0001)); ok {
		t.Error("an Extended Offset Table beside native pixels")
	}
	dir, err := os.MkdirTemp(dir, "pair")
	if err != nil {
		t.Fatal(err)
	}
	orig, arch := filepath.Join(dir, "orig.dcm"), filepath.Join(dir, "out.dcm")
	if err := os.WriteFile(orig, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(arch, out, 0o600); err != nil {
		t.Fatal(err)
	}
	p := pixelPair{Original: orig, Archived: arch, Lossy: lossy == "01"}
	for _, g := range regions {
		p.Regions = append(p.Regions, [4]int{g.X, g.Y, g.Width, g.Height})
	}
	return p
}

// outsideCheck runs pydicom over the pairs (scripts/check/deid-pixels.py)
// when NOPACS_CHECK_DEID_PYTHON names it and the test has not failed.
func outsideCheck(t *testing.T, pairs []pixelPair) {
	t.Helper()
	py := os.Getenv(pixelsPythonEnv)
	if py == "" || t.Failed() {
		return
	}
	manifest := filepath.Join(t.TempDir(), "pairs.json")
	b, _ := json.Marshal(pairs)
	if err := os.WriteFile(manifest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	report, err := exec.CommandContext(t.Context(), py, "-I", pixelsScript, manifest).CombinedOutput() //nolint:gosec // G204: the interpreter is the test runner's own and the script the repository's.
	t.Logf("pydicom:\n%s", report)
	if err != nil {
		t.Fatalf("the outside check: %v", err)
	}
}
