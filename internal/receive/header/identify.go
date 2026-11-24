package header

import (
	"bytes"
	"errors"

	"github.com/amrshadid/go-dicom/filebase"
	"github.com/amrshadid/go-dicom/filereader"
)

// Identity is what a file's first bytes say it is: its transfer syntax and
// the four UIDs that place it, each with its padding removed, empty when
// the file does not hold it.
type Identity struct {
	// TransferSyntaxUID is the transfer syntax its file meta information
	// states (PS3.10 7.1).
	TransferSyntaxUID string
	// SOPClassUID, SOPInstanceUID, StudyUID and SeriesUID are the values
	// of the data set's top level (PS3.6 Table 6-1).
	SOPClassUID, SOPInstanceUID, StudyUID, SeriesUID string
	// Complete says each UID left empty is absent from the file, not only
	// from the bytes read: the prefix was the whole file, or its data set
	// was read past (0020,000E), where the last of them would be, since a
	// data set's elements are in the order of their tags (PS3.5 7.1).
	Complete bool
}

// ErrNotDICOM is a prefix that does not start as a DICOM file (PS3.10 7.1):
// no preamble and DICM prefix, no transfer syntax, or an encoding go-dicom
// cannot read.
var ErrNotDICOM = errors.New("header: the bytes do not start as a DICOM file")

// Identify reads the identity of a file from its first bytes, prefix, which
// may end anywhere in the file: go-dicom reads a data set cut short and
// drops the element the cut runs through, so every value returned is whole.
// whole says prefix is the entire file. A source that hands a file over a
// stream reads this much before it decides where the file goes, and keeps
// the bytes it read, so the file is still kept exactly as it was sent.
//
// Bulk values are left unread, as in Read, and no value longer than prefix
// is held, so the memory a call takes follows len(prefix). Nothing of a
// value reaches an error's text.
func Identify(prefix []byte, whole bool) (Identity, error) {
	df, err := filereader.ReadDICOMFileWithOptions(filebase.NewFileReader(bytes.NewReader(prefix)), filereader.ReadOptions{
		SkipBulkData: true, BulkDataThreshold: BulkThreshold,
		MaxValueLength: valueLimit(int64(len(prefix))),
		Logger:         discard,
	})
	if err != nil || df == nil || !df.HasPreamble || df.FileMetaInfo == nil {
		return Identity{}, ErrNotDICOM
	}
	id := Identity{TransferSyntaxUID: trimUID(df.FileMetaInfo.TransferSyntaxUID), Complete: whole}
	if id.TransferSyntaxUID == "" {
		return Identity{}, ErrNotDICOM
	}
	for _, e := range df.DataElements {
		switch e.Tag {
		case tagSOPClassUID:
			id.SOPClassUID = trimUID(string(e.Value))
		case tagSOPInstanceUID:
			id.SOPInstanceUID = trimUID(string(e.Value))
		case tagStudyUID:
			id.StudyUID = trimUID(string(e.Value))
		case tagSeriesUID:
			id.SeriesUID = trimUID(string(e.Value))
		}
		if e.Tag > tagSeriesUID {
			id.Complete = true
		}
	}
	return id, nil
}
