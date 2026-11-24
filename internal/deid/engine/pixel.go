package engine

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/anonymize"
	"github.com/amrshadid/go-dicom/compress"
	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// The elements the work on pixels reads and writes (PS3.3 C.7.6.3, C.7.6.1,
// PS3.5 A.4).
const (
	tagPixelData            = 0x7FE00010
	tagNumberOfFrames       = 0x00280008
	tagFloatPixelData       = 0x7FE00008
	tagDoubleFloatPixelData = 0x7FE00009
	tagExtendedOffsetTable  = 0x7FE00001
	tagExtendedOffsetLength = 0x7FE00002
	tagPhotometric          = 0x00280004
	tagPlanarConfiguration  = 0x00280006
	tagBurnedInAnnotation   = 0x00280301
	tagLossyCompression     = 0x00282110
	tagLossyMethod          = 0x00282114
	groupImagePixel         = 0x0028
	groupPixelData          = 0x7FE0
)

// Image is what Apply is given, besides the data set, to work on the file's
// pixels.
type Image struct {
	// Source is where the values the data set left in the file are read
	// from (filereader.DICOMFile.GetDatasetWithValuesIn's source): the
	// compressed pixels of a JPEG image whose segments are removed are read
	// from it, and nothing else. Nil when the data set holds its values.
	Source io.ReaderAt
	// Frames reads the stored frames from the file, for a detect rule
	// (filereader.DICOMFile.FrameReader); nil renders them from the data
	// set's own Pixel Data.
	Frames dataset.FrameReader
	// Limit is the most bytes of an image decoded that work on pixels
	// takes (the setting deid.pixel-limit): an image a redact or detect
	// rule matches, or whose JPEG segments are removed, larger than it is
	// refused (CausePixelsTooLarge). Zero or less is no bound.
	Limit int64
}

// ErrPixelsUndecodable is wrapped by the error a writer returns when it
// writes a data set Apply filled and a frame of the image does not decode,
// or cannot be read: the filling is done as the pixels are written, a
// frame at a time, so this is found only then. The file is held
// (CausePixelsUndecodable), never written in part.
var ErrPixelsUndecodable = errors.New("engine: a frame of the image does not decode")

// PixelWork is what was done to a file's pixels, and why. It holds no value
// of the file: the rectangles are positions, a rule's or the detector's.
type PixelWork struct {
	// AtRisk reports that the image may carry burned-in text, by its
	// header as received (anonymize.BurnedInRisk).
	AtRisk bool
	// RiskCause is why: "burned_in_annotation", "secondary_capture" or
	// "modality"; empty when not at risk.
	RiskCause string
	// Rule is the pixel rule that decided, by name; empty when none
	// matched. Origin is the base it came from.
	Rule, Origin string
	// Action is the rule's: "redact", "detect" or "accept"; empty when
	// none matched.
	Action string
	// Regions are the rectangles filled in every frame, in the image's
	// pixels from its top left corner.
	Regions []dataset.PixelRegion
	// Frames is the image's number of frames.
	Frames int
	// DecodedBytes is the size of the image decoded, which Image.Limit
	// bounds.
	DecodedBytes int64
	// Decoded reports that compressed pixels are written native.
	Decoded bool
	// SegmentsRemoved are the names of the JPEG segments removed
	// (anonymize.JPEGSegment.Name), and "COM", "CPF" or "CRG" for a
	// JPEG 2000 segment, once for each segment.
	SegmentsRemoved []string

	// pixel is the Pixel Data element as the work leaves it, with the
	// elements of group 0028 it rewrote and the transfer syntax it
	// leaves; nil when the pixels stay as they are.
	pixel     *dataelem.DataElement
	rewritten []*dataelem.DataElement
	syntax    string
}

// The pixel rule actions, as PixelWork.Action names them.
const (
	pixelRedact = "redact"
	pixelDetect = "detect"
	pixelAccept = "accept"
)

// pixelRules decides which pixel rule of a profile applies to an image.
type pixelRules interface {
	// Pixels returns the pixel rule that decides for an image of rows and
	// columns given the file's facts, and whether one does.
	Pixels(rows, columns int, f facts) (*v1.ExpandedPixelRule, bool)
	// HoldsUnsearchedSegments reports whether an image whose compressed
	// segments are not searched is held.
	HoldsUnsearchedSegments() bool
}

// planPixels decides what is done to the pixels of the data set, by its
// header as received, and does the work on a scratch data set that holds
// only the image's description and its Pixel Data, so the data set itself
// is changed only when every other change is (applyPixels). In order:
//
//  1. An image at risk of burned-in text (anonymize.BurnedInRisk: Burned
//     In Annotation (0028,0301) YES, or absent on a secondary capture or a
//     modality commonly drawn on) that no pixel rule matches is refused,
//     CauseBurnedInRisk.
//  2. A redact rule's rectangles, measured from the edges it names, are
//     filled; a detect rule's detector is run and what it finds filled
//     (PS3.15 E.3.1). Each is bounded by Image.Limit first.
//  3. Pixels left compressed have the comment and application segments of
//     the JPEG family removed (anonymize.RemoveJPEGText), or the comment
//     segments of JPEG 2000 and High-Throughput JPEG 2000 (removeJ2KText);
//     a syntax whose segments are not searched is refused when the profile
//     says so.
func (r *run) planPixels(img Image) error {
	ds := r.top
	elem, ok := ds.Get(tag.Tag(tagPixelData))
	if !ok {
		return nil
	}
	w := &PixelWork{}
	risk, cause := burnedInRisk(ds)
	if risk {
		w.AtRisk, w.RiskCause = true, cause
	}
	info, infoErr := ds.GetPixelDataInfo()
	rows, cols := 0, 0
	if infoErr == nil {
		rows, cols, w.Frames = info.Rows, info.Columns, info.NumberOfFrames
		w.DecodedBytes = decodedBytes(info)
	}
	r.record.Pixels = w
	var rule *v1.ExpandedPixelRule
	if r.en.pixels != nil {
		rule, _ = r.en.pixels.Pixels(rows, cols, r.facts)
	}
	if rule == nil {
		if risk {
			return &Failure{Cause: CauseBurnedInRisk, RiskCause: w.RiskCause}
		}
	} else {
		w.Rule, w.Origin = rule.GetRule().GetName(), rule.GetOrigin()
		if err := r.planRule(w, rule.GetRule(), img, infoErr); err != nil {
			return err
		}
	}
	if len(w.Regions) > 0 {
		return r.fill(w, elem)
	}
	return r.planSegments(w, elem, img)
}

// planRule works out what a matching pixel rule does: the rectangles a
// redact rule names, or those the detector finds for a detect rule.
func (r *run) planRule(w *PixelWork, rule *v1.PixelRule, img Image, infoErr error) error {
	name := rule.GetName()
	switch a := rule.GetAction().(type) {
	case *v1.PixelRule_Accept:
		w.Action = pixelAccept
		return nil
	case *v1.PixelRule_Redact:
		w.Action = pixelRedact
		if infoErr != nil {
			return &Failure{Cause: CausePixelsUnsupported, Path: formatTag(tagPixelData), Rule: name}
		}
		if err := r.withinLimit(w, img, name); err != nil {
			return err
		}
		if err := r.framesMatch(name); err != nil {
			return err
		}
		info, _ := r.top.GetPixelDataInfo()
		for _, rect := range a.Redact.GetRectangles() {
			reg := placeRectangle(rect, info.Columns, info.Rows)
			if reg.X >= info.Columns || reg.Y >= info.Rows || reg.X+reg.Width <= 0 || reg.Y+reg.Height <= 0 {
				return &Failure{Cause: CausePixelRegionOutside, Path: formatTag(tagPixelData), Rule: name}
			}
			w.Regions = append(w.Regions, reg)
		}
		return nil
	case *v1.PixelRule_Detect:
		w.Action = pixelDetect
		if infoErr != nil {
			return &Failure{Cause: CausePixelsUnsupported, Path: formatTag(tagPixelData), Rule: name}
		}
		if err := r.withinLimit(w, img, name); err != nil {
			return err
		}
		if err := r.framesMatch(name); err != nil {
			return err
		}
		return r.detect(w, img, name)
	}
	return fmt.Errorf("engine: pixel rule %s has no action", name)
}

// detect runs the DICOM library's text detector over every frame, a frame
// at a time, and keeps what it finds for filling.
func (r *run) detect(w *PixelWork, img Image, name string) error {
	var regions []dataset.PixelRegion
	var err error
	if img.Frames != nil {
		regions, err = anonymize.DetectBurnedInTextFrom(r.top, img.Frames, nil)
	} else {
		regions, err = anonymize.DetectBurnedInText(r.top, nil)
	}
	if err != nil {
		return &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData), Rule: name}
	}
	info, _ := r.top.GetPixelDataInfo()
	if len(regions) > 0 {
		if err := anonymize.CheckDetectedShare(regions, info.Columns, info.Rows); err != nil {
			return &Failure{Cause: CauseDetectImplausible, Path: formatTag(tagPixelData), Rule: name}
		}
	}
	w.Regions = regions
	return nil
}

// jpegTextFactor is how many times the compressed pixels' length the
// removal of JPEG segments allocates: the value read, the frames it is
// divided into, each frame cleaned, the frames encapsulated again, the
// division again for the check of what is left, with the slack of growing
// slices (measured: a frame of 32 MiB took 253 MiB, 7.9 times).
const jpegTextFactor = 8

// segmentsWithinLimit refuses compressed pixels whose segment removal would
// hold more than Image.Limit (jpegTextFactor times their length), or whose
// image is not described: its decoded size, which the limit is first held
// to, is then not known.
func (r *run) segmentsWithinLimit(elem *dataelem.DataElement, img Image) error {
	if _, err := r.top.GetPixelDataInfo(); err != nil || r.record.Pixels.DecodedBytes <= 0 {
		return &Failure{Cause: CausePixelsUnsupported, Path: formatTag(tagPixelData)}
	}
	var length int64
	switch v := elem.GetValue().(type) {
	case dataelem.ValueInFile:
		length = v.Length
	default:
		b, _ := elem.Bytes()
		length = int64(len(b))
	}
	if img.Limit > 0 && length > img.Limit/jpegTextFactor {
		return &Failure{Cause: CausePixelsTooLarge, Path: formatTag(tagPixelData)}
	}
	return nil
}

// framesMatch refuses native pixels that hold more bytes than the frames
// the data set states, before any is filled: a fill reaches only the frames
// stated, so the rest would be archived unfilled under a record that says
// the image was cleaned. Number of Frames (0028,0008) present and not a
// whole number from 1 counts as one frame (PS3.3 C.7.6.6); an image of
// more bytes than that is refused too. One more byte, the pad of an odd
// length (PS3.5 7.1.1), is allowed. Compressed pixels are divided into
// their frames by the DICOM library, which refuses a division that does
// not match.
func (r *run) framesMatch(rule string) error {
	ds := r.top
	if compress.IsEncapsulated(ds.TransferSyntaxUID()) {
		return nil
	}
	elem, _ := ds.Get(tag.Tag(tagPixelData))
	info, err := ds.GetPixelDataInfo()
	if err != nil || info.BytesPerFrame <= 0 {
		return &Failure{Cause: CausePixelsUnsupported, Path: formatTag(tagPixelData), Rule: rule}
	}
	var stored int64
	switch v := elem.GetValue().(type) {
	case dataelem.ValueInFile:
		stored = v.Length
	default:
		b, _ := elem.Bytes()
		stored = int64(len(b))
	}
	frames := int64(info.NumberOfFrames)
	if v, ok := topText(ds, tagNumberOfFrames); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err != nil || n < 1 {
			frames = 1
		}
	}
	if frames < 1 || stored > int64(info.BytesPerFrame)*frames+1 {
		return &Failure{Cause: CauseFramesMismatch, Path: formatTag(tagPixelData), Rule: rule}
	}
	return nil
}

// withinLimit refuses an image whose decoded size passes the limit.
func (r *run) withinLimit(w *PixelWork, img Image, rule string) error {
	if img.Limit > 0 && w.DecodedBytes > img.Limit {
		return &Failure{Cause: CausePixelsTooLarge, Path: formatTag(tagPixelData), Rule: rule}
	}
	return nil
}

// fill fills w's regions in every frame of a scratch copy of the image
// (dataset.Dataset.DecompressRedactingRegions): Pixel Data left in the file
// becomes a value that decodes and fills each frame as it is written, and
// Pixel Data in memory is filled now. A compressed image comes out native;
// its Extended Offset Table, which only encapsulated Pixel Data has (PS3.5
// A.4), is removed, and Lossy Image Compression written when the syntax
// says the image was compressed with loss and the file did not.
func (r *run) fill(w *PixelWork, elem *dataelem.DataElement) error {
	scratch := imageScratch(r.top)
	before := scratch.TransferSyntaxUID()
	_, inFile := elem.GetValue().(dataelem.ValueInFile)
	if err := scratch.DecompressRedactingRegions(w.Regions...); err != nil {
		cause := CausePixelsUnsupported
		if !inFile && compress.IsEncapsulated(before) {
			// Pixel Data in memory is decoded here, so a frame that does
			// not decode is found here too.
			cause = CausePixelsUndecodable
		}
		return &Failure{Cause: cause, Path: formatTag(tagPixelData), Rule: w.Rule}
	}
	filled, _ := scratch.Get(tag.Tag(tagPixelData))
	if sv, ok := filled.GetValue().(*dataelem.StreamedValue); ok {
		filled.SetValue(undecodable(sv))
	}
	w.pixel, w.syntax = filled, scratch.TransferSyntaxUID()
	w.Decoded = w.syntax != before
	if !w.Decoded {
		return nil
	}
	for _, t := range []uint32{tagPhotometric, tagPlanarConfiguration} {
		if e, ok := scratch.Get(tag.Tag(t)); ok {
			w.rewritten = append(w.rewritten, e)
		}
	}
	if lossy := lossyMethod(before); lossy != "" {
		if v, _ := topText(r.top, tagLossyCompression); v != "01" {
			r.put(tagLossyCompression, "CS", "01", "the image was compressed with loss, as its transfer syntax says, and is written decoded, so this records the loss (PS3.3 C.7.6.1.1.5)")
			if _, ok := r.top.Get(tag.Tag(tagLossyMethod)); !ok {
				r.put(tagLossyMethod, "CS", lossy, "the method of the loss, by its transfer syntax (PS3.3 C.7.6.1.1.5)")
			}
		}
	}
	return nil
}

// planSegments removes the comment and application segments of pixels left
// compressed in the JPEG family, and the comment segments of JPEG 2000
// (pixel_j2k.go), or refuses an image whose compressed segments are not
// searched when the profile holds such images: the other encapsulated
// syntaxes but RLE (JPEG 2000 Part 2, the video syntaxes, JPEG XL, Deflated
// Image Frame Compression, any added later). Pixel Data left in the file is
// read into memory for it, within Image.Limit.
func (r *run) planSegments(w *PixelWork, elem *dataelem.DataElement, img Image) error {
	ref, inFile := elem.GetValue().(dataelem.ValueInFile)
	syntax := r.top.TransferSyntaxUID()
	if !compress.IsEncapsulated(syntax) || syntax == compress.RLELossless {
		// Native pixels have no segments; RLE's are packed samples, which
		// hold no text (PS3.5 G.3).
		return nil
	}
	j2k := j2kSyntax(syntax)
	if !j2k {
		_, err := anonymize.JPEGSegmentText(imageScratch(r.top))
		switch {
		case errors.Is(err, anonymize.ErrJPEGSegmentsNotSearched):
			if r.en.pixels == nil || r.en.pixels.HoldsUnsearchedSegments() {
				return &Failure{Cause: CausePixelTextUnsearched, Path: formatTag(tagPixelData)}
			}
			return nil
		case err != nil && !inFile:
			return &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData)}
		}
	}
	if err := r.withinLimit(w, img, ""); err != nil {
		return err
	}
	if err := r.segmentsWithinLimit(elem, img); err != nil {
		return err
	}
	scratch := imageScratch(r.top)
	if inFile {
		if img.Source == nil || ref.Length < 0 {
			return &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData)}
		}
		b := make([]byte, ref.Length)
		if _, err := img.Source.ReadAt(b, ref.Offset); err != nil {
			return fmt.Errorf("engine: reading the compressed pixels: %w", err)
		}
		held, _ := scratch.Get(tag.Tag(tagPixelData))
		held.SetValue(b)
	}
	if j2k {
		removed, err := removeJ2KText(scratch)
		if err != nil || len(removed) == 0 {
			return err
		}
		w.SegmentsRemoved = append(w.SegmentsRemoved, removed...)
		w.pixel, _ = scratch.Get(tag.Tag(tagPixelData))
		w.syntax = syntax
		return nil
	}
	removed, err := anonymize.RemoveJPEGText(scratch)
	if err != nil {
		return &Failure{Cause: CausePixelsUndecodable, Path: formatTag(tagPixelData)}
	}
	for _, s := range removed {
		if !s.Kept {
			w.SegmentsRemoved = append(w.SegmentsRemoved, s.Name())
		}
	}
	if err := onlyDefinedSegments(scratch); err != nil {
		return err
	}
	if len(w.SegmentsRemoved) == 0 {
		return nil
	}
	w.pixel, _ = scratch.Get(tag.Tag(tagPixelData))
	w.syntax = syntax
	return nil
}

// applyPixels puts what planPixels made into the data set, once every other
// change is made: the Pixel Data element, the elements of group 0028 the
// decoding rewrote, the transfer syntax, and no Extended Offset Table once
// the frames are laid out anew. It cannot fail; each change is undone with
// the others (rollback).
func (r *run) applyPixels() {
	w := r.record.Pixels
	if w == nil || w.pixel == nil {
		return
	}
	ds := r.top
	for _, e := range append([]*dataelem.DataElement{w.pixel}, w.rewritten...) {
		t, _ := e.Tag()
		r.applyOp(op{kind: opElement, ds: ds, tag: t, elem: e})
	}
	for _, t := range []uint32{tagExtendedOffsetTable, tagExtendedOffsetLength} {
		r.applyOp(op{kind: opRemove, ds: ds, tag: tag.Tag(t)})
	}
	if old := ds.TransferSyntaxUID(); old != w.syntax {
		ds.SetTransferSyntaxUID(w.syntax)
		r.undo = append(r.undo, func() { ds.SetTransferSyntaxUID(old) })
	}
}

// imageScratch returns a data set holding what describes the image (the
// top-level elements of group 0028, the Image Pixel module and its tables)
// and the elements of group 7FE0, with the data set's transfer syntax: the
// DICOM library's pixel functions read nothing else, and changing it
// leaves the data set as it is. The values are shared, never changed in
// place by those functions.
func imageScratch(ds *dataset.Dataset) *dataset.Dataset {
	s := dataset.NewDataset()
	s.SetTransferSyntaxUID(ds.TransferSyntaxUID())
	for _, t := range ds.Tags() {
		if g := uint16(uint32(t) >> 16); g != groupImagePixel && g != groupPixelData {
			continue
		}
		e, _ := ds.Get(t)
		_ = s.Add(dataelem.NewDataElement(e.GetTag(), e.GetVR(), e.GetValue()))
	}
	return s
}

// undecodable wraps a value that fills an image as it is written so that a
// failure to read or decode a frame is ErrPixelsUndecodable, and a failure
// of the output stays what it was: the file is held for the first, and the
// step tried again for the second.
func undecodable(sv *dataelem.StreamedValue) *dataelem.StreamedValue {
	write := sv.WriteValue
	return &dataelem.StreamedValue{Length: sv.Length, Origin: sv.Origin, WriteValue: func(src io.ReaderAt, littleEndian bool, w io.Writer) error {
		out := &outputErr{w: w}
		if err := write(src, littleEndian, out); err != nil {
			if out.err != nil {
				return err
			}
			return fmt.Errorf("%w: %w", ErrPixelsUndecodable, err)
		}
		return nil
	}}
}

// outputErr is a writer that remembers whether a write of it failed.
type outputErr struct {
	w   io.Writer
	err error
}

// Write writes p, and remembers a failure.
func (o *outputErr) Write(p []byte) (int, error) {
	n, err := o.w.Write(p)
	if err != nil {
		o.err = err
	}
	return n, err
}

// placeRectangle returns a pixel rule's rectangle in an image of cols by
// rows, measured from its top left corner: x from the right edge is
// counted back from the right side, y from the bottom likewise.
func placeRectangle(rect *v1.PixelRectangle, cols, rows int) dataset.PixelRegion {
	x, y := int(rect.GetX()), int(rect.GetY())
	wd, ht := int(rect.GetWidth()), int(rect.GetHeight())
	if rect.GetFromRight() {
		x = cols - x - wd
	}
	if rect.GetFromBottom() {
		y = rows - y - ht
	}
	return dataset.PixelRegion{X: x, Y: y, Width: wd, Height: ht}
}

// decodedBytes is the size of an image's samples decoded: rows by columns
// by samples, each in whole bytes, in every frame.
func decodedBytes(info *dataset.PixelDataInfo) int64 {
	bytesPer := int64((info.BitsAllocated + 7) / 8)
	return int64(info.Rows) * int64(info.Columns) * int64(max(info.SamplesPerPixel, 1)) * bytesPer * int64(max(info.NumberOfFrames, 1))
}

// lossyMethod is the Lossy Image Compression Method (0028,2114) of a
// transfer syntax whose process always loses (PS3.5 A.4.1: the JPEG
// Baseline and Extended processes are DCT based), or "" for one that may
// not: JPEG-LS near-lossless and JPEG 2000 may be lossless, and only the
// file says which.
func lossyMethod(syntax string) string {
	if slices.Contains([]string{compress.JPEGBaseline, compress.JPEGExtended}, syntax) {
		return "ISO_10918_1"
	}
	return ""
}

// programPixels is pixelRules over the language's compiled Program.
type programPixels struct{ prog *profile.Program }

// Pixels implements pixelRules.
func (p programPixels) Pixels(rows, columns int, f facts) (*v1.ExpandedPixelRule, bool) {
	return p.prog.Pixels(rows, columns, f)
}

// HoldsUnsearchedSegments implements pixelRules.
func (p programPixels) HoldsUnsearchedSegments() bool { return p.prog.HoldsUnsearchedSegments() }
