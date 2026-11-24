package header

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/core/naming"
)

// BulkThreshold is the longest value of a binary representation (OB, OW, UN
// and the others PS3.5 6.2 lists as bytes or words) that is read with the
// header; a longer one is left in the file and absent from the sidecar's
// data set. Pixel Data is never read, whatever its length.
const BulkThreshold = 1024

// The attributes the pipeline places a file by (PS3.6 Table 6-1).
var (
	tagSOPClassUID    = tag.New(0x0008, 0x0016)
	tagSOPInstanceUID = tag.New(0x0008, 0x0018)
	tagStudyUID       = tag.New(0x0020, 0x000D)
	tagSeriesUID      = tag.New(0x0020, 0x000E)
)

// pixelTags are the elements that hold an image's pixels (PS3.3 C.7.6.3:
// Pixel Data, Float Pixel Data, Double Float Pixel Data); the first present
// is the one a sidecar points at.
var pixelTags = []tag.Tag{tag.New(0x7FE0, 0x0010), tag.New(0x7FE0, 0x0008), tag.New(0x7FE0, 0x0009)}

// Attribute names one of the four attributes that place a file, as the
// quarantine's bad_uid detail spells it.
type Attribute string

// The attributes, by their PS3.6 keyword.
const (
	SOPClassUID    Attribute = "SOPClassUID"
	SOPInstanceUID Attribute = "SOPInstanceUID"
	StudyUID       Attribute = "StudyInstanceUID"
	SeriesUID      Attribute = "SeriesInstanceUID"
)

// Span is where an element is in the file: Offset is the first byte of its
// tag, and Length counts every byte up to the first byte after it, the
// delimiter of encapsulated pixel data included.
type Span struct {
	Offset, Length int64
}

// Header is what was read from one file's header.
type Header struct {
	// TransferSyntaxUID is the transfer syntax the file meta information
	// states (PS3.10 7.1), with its padding removed.
	TransferSyntaxUID string
	// PixelData is where the pixel data element is, when HasPixelData.
	PixelData Span
	// HasPixelData says whether the data set holds a pixel data element.
	HasPixelData bool

	file      *filereader.DICOMFile
	byTag     map[tag.Tag]*filereader.DataElementValue
	encodings []string
}

// NotDICOMError is a file that is not a whole DICOM file: Offset is the byte
// where reading stopped. Its text holds the offset and a fixed phrase only.
type NotDICOMError struct {
	// Offset is the byte offset where the file stopped being a DICOM file.
	Offset int64
	// Why is a fixed phrase of this package.
	Why string
}

// Error returns the phrase and the offset.
func (e *NotDICOMError) Error() string {
	return "header: not a DICOM file: " + e.Why + " at byte " + strconv.FormatInt(e.Offset, 10)
}

// MaxHeader is the default bound on the bytes of a file read before its
// pixel data, the built-in default of the setting receiver.header-limit,
// which an installation may raise (ReadWithin): every
// value the header holds, and every bulk value before the pixel data, must
// lie within it. go-dicom allocates a value whole once its declared length
// fits in what the stream has left, so the stream it is given for the header
// is cut at this bound, and no single value of a hostile file can make the
// read allocate more; every value held is also refused past the bound
// (filereader.ReadOptions.MaxValueLength), which bounds an element after the
// pixel data too. The largest legitimate headers are those of enhanced
// multi-frame images, whose per-frame functional groups (PS3.3 C.7.6.16) take
// a few kilobytes a frame: ten thousand frames at four kilobytes is 40 MiB.
// 64 MiB holds that with room, and a file whose header is larger is refused
// (*TooLargeError), not read.
const MaxHeader = 64 << 20

// TooLargeError is a file whose elements before its pixel data do not
// fit in the bound it was read within, or whose first element past the
// bound is not pixel data: its header is not read.
type TooLargeError struct {
	// Limit is the bound in bytes.
	Limit int64
}

// Error returns the bound.
func (e *TooLargeError) Error() string {
	return "header: the elements before the pixel data are longer than " + strconv.FormatInt(e.Limit, 10) + " bytes"
}

// discard is the logger every read is given. What go-dicom logs while
// reading a damaged file names tags, lengths and offsets of a file that,
// before de-identification, is a patient's; nothing of it reaches the
// server's log, and every refusal this package needs is in the errors it
// returns. A read given its own logger sends nothing to the library's
// global one, so no other user of the library in the process is affected.
var discard = slog.New(slog.DiscardHandler)

// Read is ReadWithin the default bound, MaxHeader.
func Read(r io.ReaderAt, size int64) (*Header, error) { return ReadWithin(r, size, MaxHeader) }

// ReadWithin reads the header of the file r holds, size bytes long, within
// bound bytes (MaxHeader when bound is not positive). A file that is not a
// whole DICOM file is a *NotDICOMError, one whose header does not fit in
// bound, or that holds a value longer than bound, a *TooLargeError; any
// other error is a failure to read r.
//
// The header is read first from the file's first bound bytes only. When
// the file is longer and its data set runs past the bound, the first element
// past it must be pixel data (PS3.3 C.7.6.3), which go-dicom skips whatever
// its length; the file is then read whole, every element before the pixel
// data already shown to fit, and an element after it held to the bound as a
// value.
func ReadWithin(r io.ReaderAt, size, bound int64) (*Header, error) {
	if bound <= 0 {
		bound = MaxHeader
	}
	limit := min(size, bound)
	df, err := parse(io.NewSectionReader(r, 0, limit), bound)
	if err != nil {
		return nil, err
	}
	if size > limit {
		end := df.DataSetOffset
		if n := len(df.DataElements); n > 0 {
			end = df.DataElements[n-1].Position.End
		}
		var b [4]byte
		if df.Deflated || !df.HasPreamble {
			return nil, &TooLargeError{Limit: bound}
		}
		if _, err := r.ReadAt(b[:], end); err != nil {
			return nil, fmt.Errorf("header: reading the file: %w", err)
		}
		t, err := tag.FromBytes(b[:], df.IsLittleEndian)
		if err != nil || !slices.Contains(pixelTags, t) {
			return nil, &TooLargeError{Limit: bound}
		}
		if df, err = parse(io.NewSectionReader(r, 0, size), bound); err != nil {
			return nil, err
		}
	}
	return build(df, size)
}

// parse reads a data set from r with bulk values left in the file, holding no
// value longer than bound.
func parse(r io.ReadSeeker, bound int64) (*filereader.DICOMFile, error) {
	cr := &countingReader{r: r}
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(cr), filereader.ReadOptions{
		SkipBulkData: true, BulkDataThreshold: BulkThreshold,
		MaxValueLength: valueLimit(bound),
		Logger:         discard,
	})
	if err != nil {
		if errors.Is(err, filereader.ErrValueTooLong) {
			return nil, &TooLargeError{Limit: bound}
		}
		if cr.err != nil {
			// The reader itself failed, which says nothing about the file.
			return nil, fmt.Errorf("header: reading the file: %w", cr.err)
		}
		// go-dicom's own text may quote bytes of the file, so it is not
		// kept: the offset says where.
		return nil, &NotDICOMError{Offset: cr.pos, Why: "its encoding could not be read"}
	}
	return df, nil
}

// valueLimit returns bound as the library's limit on a value held: bound
// itself, or the most a uint32 holds when bound is larger. A bound that is
// not positive is never passed here (ReadWithin replaces it with
// MaxHeader); it is treated as the smallest limit there is, which refuses
// every value, rather than as zero, which the library reads as no limit.
func valueLimit(bound int64) uint32 {
	switch {
	case bound <= 0:
		return 1
	case bound >= math.MaxUint32:
		return math.MaxUint32
	default:
		return uint32(bound) // #nosec G115 -- 0 < bound < 2^32 here.
	}
}

// build checks a file read whole and makes its Header.
func build(df *filereader.DICOMFile, size int64) (*Header, error) {
	if !df.HasPreamble || df.FileMetaInfo == nil {
		return nil, &NotDICOMError{Offset: 0, Why: "it has no preamble and DICM prefix (PS3.10 7.1)"}
	}
	h := &Header{
		TransferSyntaxUID: trimUID(df.FileMetaInfo.TransferSyntaxUID),
		file:              df,
		byTag:             make(map[tag.Tag]*filereader.DataElementValue, len(df.DataElements)),
	}
	if h.TransferSyntaxUID == "" {
		return nil, &NotDICOMError{Offset: df.DataSetOffset, Why: "its file meta information states no transfer syntax"}
	}
	if len(df.DataElements) == 0 {
		return nil, &NotDICOMError{Offset: df.DataSetOffset, Why: "its data set is empty"}
	}
	for _, e := range df.DataElements {
		h.byTag[e.Tag] = e
	}
	if !df.Deflated {
		// A deflated data set's positions count in the inflated bytes
		// (PS3.5 A.5), so its end cannot be compared with the file's.
		if end := df.DataElements[len(df.DataElements)-1].Position.End; end != size {
			return nil, &NotDICOMError{Offset: end, Why: "its data set does not end where the file ends"}
		}
	}
	for _, t := range pixelTags {
		if e, ok := h.byTag[t]; ok {
			h.HasPixelData = true
			h.PixelData = Span{Offset: e.Position.Offset, Length: e.Position.End - e.Position.Offset}
			break
		}
	}
	h.encodings = filereader.TextEncodings(df.DataElements, nil)
	return h, nil
}

// UID returns the value of one of the attributes that place a file, with its
// padding removed (PS3.5 6.2, UI: a trailing NUL), and whether the data set
// holds the attribute.
func (h *Header) UID(a Attribute) (string, bool) {
	var t tag.Tag
	switch a {
	case SOPClassUID:
		t = tagSOPClassUID
	case SOPInstanceUID:
		t = tagSOPInstanceUID
	case StudyUID:
		t = tagStudyUID
	case SeriesUID:
		t = tagSeriesUID
	default:
		return "", false
	}
	e, ok := h.byTag[t]
	if !ok {
		return "", false
	}
	return trimUID(string(e.Value)), true
}

// Lookup implements naming.Attributes over the data set's top level: the
// value as text decoded under the data set's character set, with the padding
// PS3.5 6.2 says is not part of a value removed for its representation. A
// value left in the file as bulk data is reported present and empty.
func (h *Header) Lookup(t naming.Tag) (string, bool) {
	e, ok := h.byTag[tag.Tag(t)]
	if !ok {
		return "", false
	}
	if e.ValueSkipped || len(e.Items) > 0 || e.VR == "SQ" {
		return "", true
	}
	return naming.TrimValue(e.VR, filereader.DecodeText(e.VR, e.Value, h.encodings)), true
}

// Item implements naming.ItemAttributes: the value at path, through the
// first item of each sequence it names, decoded and trimmed as Lookup
// does a top-level one. A value inside an item is decoded under the item's
// own Specific Character Set (0008,0005) when it declares one, else the one
// it inherits (PS3.5 6.1.2.3, PS3.3 C.12.1.1.2), as the DICOM library
// decodes the data set the de-identification engine reads, so the two read
// one value alike. A value left in the file, a sequence where text is
// asked, or text longer than naming.MaxValueLength is an error, never an
// empty value.
func (h *Header) Item(path []naming.Tag) (string, bool, error) {
	if len(path) == 0 {
		return "", false, nil
	}
	enc := h.encodings
	e, ok := h.byTag[tag.Tag(path[0])]
	for _, t := range path[1:] {
		if !ok {
			return "", false, nil
		}
		if e.VR != "SQ" && len(e.Items) == 0 {
			return "", true, errNotText
		}
		if len(e.Items) == 0 || e.Items[0] == nil {
			return "", false, nil
		}
		first := e.Items[0].Elements
		enc = filereader.TextEncodings(first, enc)
		e, ok = nil, false
		for _, c := range first {
			if c.Tag == tag.Tag(t) {
				e, ok = c, true
				break
			}
		}
	}
	switch {
	case !ok:
		return "", false, nil
	case e.ValueSkipped || len(e.Items) > 0 || e.VR == "SQ" || len(e.Value) > naming.MaxValueLength:
		return "", true, errNotText
	}
	return naming.TrimValue(e.VR, filereader.DecodeText(e.VR, e.Value, enc)), true, nil
}

// errNotText is a value Item cannot answer as text: left in the file, a
// sequence, or too long.
var errNotText = errors.New("header: the value is not text held in memory")

// The attributes the catalog keeps of a file's scan and session (PS3.6
// Table 6-1): the session's Study Date and the scan's Modality, Series
// Description and Series Number. Each is a study's or a series's
// attribute, the same in every file of the scan by the standard (PS3.3
// C.7.2.1, C.7.3.1), so one file's header says it.
var (
	tagStudyDate         = naming.NewTag(0x0008, 0x0020)
	tagModality          = naming.NewTag(0x0008, 0x0060)
	tagSeriesDescription = naming.NewTag(0x0008, 0x103E)
	tagSeriesNumber      = naming.NewTag(0x0020, 0x0011)
)

// CatalogText returns the text of the attributes the catalog keeps of the
// file's scan and session, each as Lookup decodes it, under the file's
// character set with its padding removed, and empty when absent; the
// catalog's values are made from them (tenant.ScanValuesOf), by the
// reception that archives the file and by the pass that fills in a scan
// archived before the catalog kept them, alike.
func (h *Header) CatalogText() (studyDate, modality, seriesDescription, seriesNumber string) {
	text := func(t naming.Tag) string {
		v, _ := h.Lookup(t)
		return v
	}
	return text(tagStudyDate), text(tagModality), text(tagSeriesDescription), text(tagSeriesNumber)
}

// DICOMJSON returns the data set in the DICOM JSON Model (PS3.18 Annex F),
// without the file meta information and without the values left in the file
// as bulk data, which have no Value member. The members are in the order of
// their tags, so the same header always gives the same bytes.
func (h *Header) DICOMJSON() ([]byte, error) {
	m, err := h.file.GetDataset().ToDICOMJSON()
	if err != nil {
		// go-dicom's text names the tag and the representation, never a
		// value; it is still not passed on, so no future text can leak one.
		return nil, errors.New("header: the data set cannot be written as DICOM JSON")
	}
	return json.Marshal(m)
}

// trimUID removes the padding of a UI value: a trailing NUL (PS3.5 6.2), and
// the spaces some writers pad with instead.
func trimUID(s string) string { return strings.TrimRight(s, "\x00 ") }

// countingReader counts where a read stopped and keeps the error of the
// reader beneath, so Read can tell a file that is not DICOM from a disk that
// failed.
type countingReader struct {
	r   io.ReadSeeker
	pos int64
	err error
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.pos += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && c.err == nil {
		c.err = err
	}
	return n, err
}

func (c *countingReader) Seek(offset int64, whence int) (int64, error) {
	n, err := c.r.Seek(offset, whence)
	if err == nil {
		c.pos = n
	} else if c.err == nil {
		c.err = err
	}
	return n, err
}
