package header

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/tag"
)

// The bounds of what DICOM JSON answers inline (PS3.18 F.2.2): a value of a
// binary representation longer than InlineBinaryLimit, or any value longer
// than InlineValueLimit, is bulk data and is referred to, never inlined.
const (
	// InlineBinaryLimit is the longest value of the representations OB, OD,
	// OF, OL, OV, OW and UN that is answered inline.
	InlineBinaryLimit = 1 << 10
	// InlineValueLimit is the longest value of any representation that is
	// answered inline.
	InlineValueLimit = 64 << 10
)

// TransferSyntaxDeflated is Deflated Explicit VR Little Endian (PS3.5 A.5):
// the data set is compressed, so a position counted in the data set's bytes
// is not a position in the file.
const TransferSyntaxDeflated = "1.2.840.10008.1.2.1.99"

// MaxMetaPrefix is how many bytes of a file ReadTransferSyntax reads at
// most: the preamble, the prefix and the file meta information, which is a
// few hundred bytes in a file any tool writes.
const MaxMetaPrefix = 64 << 10

// keyTrailingPadding is the member name of Data Set Trailing Padding
// (FFFC,FFFC), which an answer leaves out: it is padding with no meaning
// (PS3.5 7.1.3, PS3.10 7.2).
const keyTrailingPadding = "FFFCFFFC"

// ReadTransferSyntax returns the Transfer Syntax UID (0002,0010) of the
// file r holds, size bytes long, reading at most MaxMetaPrefix bytes of
// its start, never its data set. A file that is not a DICOM file, or whose
// file meta information states no transfer syntax, is a *NotDICOMError; any
// other error is a failure to read r.
func ReadTransferSyntax(r io.ReaderAt, size int64) (string, error) {
	cr := &countingReader{r: io.NewSectionReader(r, 0, min(size, MaxMetaPrefix))}
	dfr := filereader.NewDCMFileReader(filebase.NewFileReader(cr))
	notDICOM := func(why string) error { return &NotDICOMError{Offset: cr.pos, Why: why} }
	fail := func(why string) (string, error) {
		if cr.err != nil {
			return "", fmt.Errorf("header: reading the file: %w", cr.err)
		}
		return "", notDICOM(why)
	}
	if err := dfr.ReadPreamble(); err != nil {
		return fail("it has no preamble (PS3.10 7.1)")
	}
	if err := dfr.ReadDICMPrefix(); err != nil {
		return fail("it has no DICM prefix (PS3.10 7.1)")
	}
	meta, err := dfr.ReadFileMetaInfo()
	if err != nil {
		return fail("its file meta information could not be read")
	}
	ts := trimUID(meta.TransferSyntaxUID)
	if ts == "" {
		return "", notDICOM("its file meta information states no transfer syntax")
	}
	return ts, nil
}

// DICOMJSONBulk returns the data set in the DICOM JSON Model (PS3.18 Annex
// F) as an answer to a reader gives it: without the file meta information
// and without Data Set Trailing Padding, with every value that is bulk data
// (Pixel Data, Float Pixel Data and Double Float Pixel Data whatever their
// length; a value of a binary representation longer than InlineBinaryLimit;
// any value longer than InlineValueLimit) in place of a BulkDataURI that
// uri makes from where the value's bytes are in the file: the offset of
// the first byte of the value field and its length, which for encapsulated
// Pixel Data runs through the Sequence Delimitation Item (PS3.5 A.4). A
// value left in the file that the rule does not make bulk data (a value of
// a representation the dictionary does not know in an implicit file) is
// referred to too, since it is not held and cannot be inlined.
//
// When omit is set no reference is made and each such attribute is left
// out, as for a file whose data set is deflated, where no offset in the
// file names a value (PS3.5 A.5); uri is then not called.
//
// The members are in the order of their tags, so the same header always
// gives the same bytes.
func (h *Header) DICOMJSONBulk(uri func(offset, length int64) string, omit bool) ([]byte, error) {
	m, err := h.file.GetDataset().ToDICOMJSON()
	if err != nil {
		// As DICOMJSON: go-dicom's text is not passed on.
		return nil, errors.New("header: the data set cannot be written as DICOM JSON")
	}
	delete(m, keyTrailingPadding)
	refer(h.file.DataElements, m, uri, omit, isBulk)
	return json.Marshal(m)
}

// refer replaces, in the members m of the data set whose elements are
// elements, each value that is bulk data by its reference, or drops it, and
// does the same inside each item of a sequence.
func refer(elements []*filereader.DataElementValue, m map[string]dataset.DICOMJSONElement, uri func(offset, length int64) string, omit bool, bulk func(*filereader.DataElementValue, string, int64) bool) {
	for _, e := range elements {
		key := fmt.Sprintf("%04X%04X", e.Tag.Group(), e.Tag.Element())
		entry, ok := m[key]
		if !ok {
			continue
		}
		if e.Items != nil || entry.VR == "SQ" {
			for i, it := range e.Items {
				if i >= len(entry.Value) {
					break
				}
				if nested, ok := entry.Value[i].(map[string]dataset.DICOMJSONElement); ok {
					refer(it.Elements, nested, uri, omit, bulk)
				}
			}
			continue
		}
		offset, length := e.Position.ValueOffset, int64(e.Length)
		if e.UndefinedLength {
			length = e.Position.End - e.Position.ValueOffset
		}
		if !bulk(e, entry.VR, length) {
			continue
		}
		if omit {
			delete(m, key)
			continue
		}
		m[key] = dataset.DICOMJSONElement{VR: entry.VR, BulkDataURI: uri(offset, length)}
	}
}

// isBulk reports whether the element e, whose value field is length bytes
// long and whose representation the answer gives as vr, is bulk data.
func isBulk(e *filereader.DataElementValue, vr string, length int64) bool {
	switch {
	case isPixelTag(e.Tag):
		// Whatever its length: an empty Pixel Data still has the reference
		// the metadata file's line gives it (PS3.18 F.2.7).
		return true
	case length == 0:
		return false
	case e.ValueSkipped:
		return true
	case length > InlineValueLimit:
		return true
	}
	switch vr {
	case "OB", "OD", "OF", "OL", "OV", "OW", "UN":
		return length > InlineBinaryLimit
	}
	return false
}

// isPixelTag reports whether t is one of the elements that hold an image's
// pixels.
func isPixelTag(t tag.Tag) bool {
	for _, p := range pixelTags {
		if t == p {
			return true
		}
	}
	return false
}
