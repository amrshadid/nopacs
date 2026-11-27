package receive

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
)

// What a correction that edits archived header values needs of the
// reception's own reading and writing of files, so an edited revision is
// read and written by the same code as a received one: the data set read
// with its bulk values left in the file, written back by the DICOM
// library's writer in the transfer syntax the file had, and its line of
// the scan's metadata made from the bytes written.

// ErrUnreadable is a file RewriteFile or EditedLine could not read as
// DICOM, wrapped; nothing of a value of the file is in it.
var ErrUnreadable = errors.New("receive: the file cannot be read as DICOM")

// ErrRawText is a file CheckRewritable refuses: written again, it would
// declare UTF-8 over bytes in another character set.
var ErrRawText = errors.New("receive: the file declares a character set other than the default or UTF-8 and holds an element " +
	"of VR UN, whose bytes a rewrite keeps as they are under a UTF-8 declaration")

// CheckRewritable returns ErrRawText, wrapped as a precondition failure,
// for a file RewriteFile would write wrongly. The DICOM library decodes
// every text value into UTF-8 and writes the file back declaring ISO_IR
// 192, but keeps an element of VR UN (a private element of an implicit-VR
// file among them) as the bytes it read: in a file whose Specific
// Character Set is neither the default repertoire nor UTF-8 those bytes
// would then be read as UTF-8 (PS3.5 6.1). The same holds inside a
// sequence item that declares its own character set (rawUnder). A file in
// the default repertoire or in UTF-8 with no such item, or one with no
// element of VR UN, is rewritable. Nothing of a value of the file is in
// the error.
func CheckRewritable(file []byte, headerLimit int64) error {
	h, err := header.ReadWithin(bytes.NewReader(file), int64(len(file)), headerLimit)
	if err != nil {
		return errors.Join(ErrUnreadable, err)
	}
	other := true
	switch cs := h.CharacterSet(); {
	case cs == nil, *cs == "", *cs == "ISO_IR 6", *cs == "ISO_IR 192":
		other = false
	}
	js, err := h.DICOMJSON()
	if err != nil {
		return errors.Join(ErrUnreadable, err)
	}
	var ds map[string]any
	if err := json.Unmarshal(js, &ds); err != nil {
		return errors.Join(ErrUnreadable, err)
	}
	if rawUnder(ds, other) {
		return fault.Wrap(fault.Precondition, ErrRawText, "the file's character set is not kept by a rewrite: it holds an element of VR UN "+
			"under a character set other than the default or UTF-8")
	}
	return nil
}

// charsetKey is Specific Character Set (0008,0005) as the DICOM JSON model
// names it (PS3.18 F.2.1.1).
const charsetKey = "00080005"

// rawUnder reports whether a data set in the DICOM JSON model (PS3.18 F.2)
// holds an element of VR UN under a character set other than the default
// or UTF-8, at any depth: other says the data set's own is (its file's, or
// an enclosing item's). An item may declare its own (PS3.5 7.5.3); the
// library reads that declaration as UTF-8 too, so the one it had is not
// known, and any item that declares one is taken as other, the safe side.
func rawUnder(ds map[string]any, other bool) bool {
	for _, a := range ds {
		attr, ok := a.(map[string]any)
		if !ok {
			continue
		}
		if other && attr["vr"] == "UN" {
			return true
		}
		items, _ := attr["Value"].([]any)
		for _, it := range items {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			_, declares := m[charsetKey]
			if rawUnder(m, other || declares) {
				return true
			}
		}
	}
	return false
}

// RewriteFile reads file's data set within headerLimit bytes of header
// (header.MaxHeader when not positive), lets change change it, and writes
// it back as a DICOM file in the transfer syntax the file had, its bulk
// values (the pixel data among them) copied from file as they are. The
// file meta information is made again from the data set's SOP Class and
// SOP Instance UIDs, as a de-identified file's is. An error of change is
// returned as it is.
func RewriteFile(file []byte, headerLimit int64, change func(ds *dataset.Dataset) error) ([]byte, error) {
	h, err := header.ReadWithin(bytes.NewReader(file), int64(len(file)), headerLimit)
	if err != nil {
		return nil, errors.Join(ErrUnreadable, err)
	}
	ds, source, littleEndian, err := h.Dataset(bytes.NewReader(file))
	if err != nil {
		return nil, errors.Join(ErrUnreadable, err)
	}
	if err := change(ds); err != nil {
		return nil, err
	}
	var w memFile
	if err := writeDataset(&w, ds, h.TransferSyntaxUID, source, littleEndian); err != nil {
		return nil, fault.Wrap(fault.Invalid, err, "the edited file cannot be written")
	}
	return w.b, nil
}

// EditedLine returns the line of the scan's metadata of an edited file
// held whole in memory, at the key and revision given: what the file says
// of itself (transfer syntax, pixel data and frame table with each frame's
// checksum, data set) read from its bytes as a received file's is
// (sidecarLine), and what only the database and the reception know taken
// from old, the line of the revision it replaces.
func EditedLine(file []byte, old sidecar.Instance, sum string, headerLimit int64) (sidecar.Instance, error) {
	src := bytes.NewReader(file)
	h, err := header.ReadWithin(src, int64(len(file)), headerLimit)
	if err != nil {
		return sidecar.Instance{}, errors.Join(ErrUnreadable, err)
	}
	in := old
	in.Size, in.SHA256 = int64(len(file)), sum
	in.TransferSyntax = h.TransferSyntaxUID
	in.PixelData, in.Frames, in.FrameSHA256, in.Dataset, in.DatasetOmitted = nil, nil, nil, nil, true
	// A deflated data set's positions count inflated bytes (PS3.5 A.5): such
	// a file has no place for its pixels in the line, as at reception.
	if h.HasPixelData && h.TransferSyntaxUID != header.TransferSyntaxDeflated {
		table, err := frames.Table(h, src)
		if err != nil {
			return sidecar.Instance{}, errors.Join(ErrUnreadable, err)
		}
		in.PixelData = &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}
		in.Frames = table
		if len(table) > 0 {
			sums, _, err := frames.Checksums(io.NewSectionReader(src, 0, int64(len(file))), table)
			if err != nil {
				return sidecar.Instance{}, errors.Join(ErrUnreadable, err)
			}
			in.FrameSHA256 = sums
		}
	}
	if js, err := h.DICOMJSON(); err == nil && len(js) <= sidecar.MaxDataset {
		in.Dataset, in.DatasetOmitted = js, false
	}
	return in, nil
}
