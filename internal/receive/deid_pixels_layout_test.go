package receive

import (
	"encoding/binary"
	"strconv"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// imageLayout is a synthetic native image's description.
type imageLayout struct {
	photometric           string
	samples, planar       int
	bits, stored, signedv int
	palette               bool
}

// synthImage writes a synthetic native ultrasound image of 24 by 32 and
// two frames in the layout given, its samples a pattern and no patient's
// data, and returns the file.
func synthImage(t *testing.T, l imageLayout) []byte {
	t.Helper()
	const rows, cols, frames = 24, 32, 2
	width := l.bits / 8
	px := make([]byte, rows*cols*frames*l.samples*width)
	for i := 0; i < len(px); i += width {
		px[i] = []byte{3, 77, 140, 201, 250}[(i/width)%5] // the low byte; a wider sample's others stay 0
	}
	us := func(v int) []byte { return binary.LittleEndian.AppendUint16(nil, uint16(v&0xFFFF)) }
	els := []struct {
		t  uint32
		vr dataelem.VR
		v  any
	}{
		{0x00080016, dataelem.UI, "1.2.840.10008.5.1.4.1.1.3.1"}, {0x00080018, dataelem.UI, "2.25.8811"},
		{0x00080060, dataelem.CS, "US"}, {0x00100020, dataelem.LO, "SYNTHETIC-2"},
		{0x0020000D, dataelem.UI, "2.25.8812"}, {0x0020000E, dataelem.UI, "2.25.8813"},
		{0x00280002, dataelem.US, us(l.samples)}, {0x00280004, dataelem.CS, l.photometric},
		{0x00280008, dataelem.IS, strconv.Itoa(frames)},
		{0x00280010, dataelem.US, us(rows)}, {0x00280011, dataelem.US, us(cols)},
		{0x00280100, dataelem.US, us(l.bits)}, {0x00280101, dataelem.US, us(l.stored)}, {0x00280102, dataelem.US, us(l.stored - 1)},
		{0x00280103, dataelem.US, us(l.signedv)},
	}
	if l.samples > 1 {
		els = append(els, struct {
			t  uint32
			vr dataelem.VR
			v  any
		}{0x00280006, dataelem.US, us(l.planar)})
	}
	ds := dataset.NewDataset()
	ds.SetTransferSyntaxUID("1.2.840.10008.1.2.1")
	for _, e := range els {
		v := e.v
		if s, ok := v.(string); ok {
			v = []byte(s)
		}
		if err := ds.Add(dataelem.NewDataElement(tag.Tag(e.t), e.vr, v)); err != nil {
			t.Fatal(err)
		}
	}
	if l.palette {
		// 256 entries of 16 bits from 0; every entry bright but entry 37,
		// which is black: the fill is 37, not 0.
		for c := range 3 {
			desc := append(append(us(256), us(0)...), us(16)...)
			if err := ds.Add(dataelem.NewDataElement(tag.New(0x0028, uint16(0x1101+c)), dataelem.US, desc)); err != nil {
				t.Fatal(err)
			}
			table := make([]byte, 512)
			for i := range 256 {
				v := 0xF000 - i*16
				if i == 37 {
					v = 0
				}
				binary.LittleEndian.PutUint16(table[2*i:], uint16(v&0xFFFF))
			}
			if err := ds.Add(dataelem.NewDataElement(tag.New(0x0028, uint16(0x1201+c)), dataelem.OW, table)); err != nil {
				t.Fatal(err)
			}
		}
	}
	vr := dataelem.OB
	if l.bits > 8 {
		vr = dataelem.OW
	}
	if err := ds.Add(dataelem.NewDataElement(tag.New(0x7FE0, 0x0010), vr, px)); err != nil {
		t.Fatal(err)
	}
	return rewriteDataset(t, ds)
}

// rewriteDataset writes ds as Explicit VR Little Endian and returns the
// file.
func rewriteDataset(t *testing.T, ds *dataset.Dataset) []byte {
	t.Helper()
	mem := &memFile{}
	if err := writeDataset(mem, ds, "1.2.840.10008.1.2.1", nil, true); err != nil {
		t.Fatal(err)
	}
	return mem.b
}

// TestPixelLayoutsFillAtReception guards a redact rule over native
// layouts the public images lack: RGB by plane (Planar Configuration 1),
// PALETTE COLOR (filled with the darkest entry, not 0), signed 12-bit
// MONOCHROME2 (the lowest value, sign extended), MONOCHROME1 (the highest),
// and a rectangle partly past the image's edge (clipped). Each is checked
// sample by sample in both frames, and by pydicom when asked.
func TestPixelLayoutsFillAtReception(t *testing.T) {
	clipped := []*v1.PixelRectangle{{X: 26, Y: 20, Width: 10, Height: 8}, {X: 0, Y: 0, Width: 4, Height: 4}}
	var pairs []pixelPair
	dir := t.TempDir()
	for _, c := range []struct {
		name  string
		l     imageLayout
		rects []*v1.PixelRectangle
	}{
		{"RGB by plane", imageLayout{"RGB", 3, 1, 8, 8, 0, false}, testRects},
		{"palette", imageLayout{"PALETTE COLOR", 1, 0, 8, 8, 0, true}, testRects},
		{"signed 12 bits", imageLayout{"MONOCHROME2", 1, 0, 16, 12, 1, false}, testRects},
		{"MONOCHROME1", imageLayout{"MONOCHROME1", 1, 0, 8, 8, 0, false}, testRects},
		{"clipped", imageLayout{"MONOCHROME2", 1, 0, 8, 8, 0, false}, clipped},
	} {
		t.Run(c.name, func(t *testing.T) {
			pairs = append(pairs, fillAtReception(t, dir, synthImage(t, c.l), "1.2.840.10008.1.2.1", "", c.rects))
		})
	}
	outsideCheck(t, pairs)
}
