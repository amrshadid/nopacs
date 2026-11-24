package header

import (
	"errors"
	"io"
	"strings"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"
)

// tagSpecificCharacterSet is Specific Character Set (0008,0005), PS3.3
// C.12.1.1.2.
var tagSpecificCharacterSet = tag.New(0x0008, 0x0005)

// CharacterSet returns the file's Specific Character Set as received: its
// values joined by a backslash as the file wrote them, with the padding
// PS3.5 6.2 says is not part of a CS value removed, "" when the data set
// declares none. It is read from the element's bytes, not from the data set
// the library decodes, which declares the UTF-8 it decoded into (ISO_IR
// 192) whatever the file was written in: a value the library keeps as
// bytes (a sequence held as UN, a private value of VR UN) is still in the
// file's own character set, and a de-identifier decoding it needs to know
// which.
func (h *Header) CharacterSet() *string {
	e, ok := h.byTag[tagSpecificCharacterSet]
	if !ok {
		s := ""
		return &s
	}
	s := strings.TrimRight(string(e.Value), " \x00")
	return &s
}

// FrameReader returns a reader of the file's stored frames, a frame at a time,
// from file, the file the header was read from, whole (or the inflated
// data set of a deflated file, which it reads through the same source
// Dataset gives): what a detector reads the pixels from without holding
// the image. Nothing of a value reaches the error's text.
func (h *Header) FrameReader(file io.ReaderAt) (dataset.FrameReader, error) {
	fr, err := h.file.FrameReader(h.file.ValueSource(file))
	if err != nil {
		return nil, errors.New("header: the frames of the pixel data cannot be found")
	}
	return fr, nil
}

// Dataset returns the file's data set for a writer to change and write
// back, with its bulk values (the pixel data among them) left in the file
// as references, and the source to give filewriter's SetValueSource, with
// the byte order of its data set. file is the file the header was read
// from, whole: the references are its offsets, or for a deflated file the
// offsets of its inflated data set, which the source reads by inflating
// the stream as values are asked for (PS3.5 A.5). Nothing of a value
// reaches the error's text.
func (h *Header) Dataset(file io.ReaderAt) (ds *dataset.Dataset, source io.ReaderAt, littleEndian bool, err error) {
	ds, source, err = h.file.GetDatasetWithValuesIn(file)
	if err != nil {
		return nil, nil, false, errors.New("header: the data set cannot be read with its values left in the file")
	}
	return ds, source, h.file.IsLittleEndian, nil
}
