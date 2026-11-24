package header

import (
	"encoding/json"
	"errors"

	"github.com/amrshadid/go-dicom/filereader"
)

// ShortValueLimit is the longest value, in bytes, DICOMJSONShort keeps.
const ShortValueLimit = 1 << 10

// DICOMJSONShort returns the data set in the DICOM JSON Model (PS3.18
// Annex F) for a person to read: Pixel Data and every attribute whose value
// is longer than ShortValueLimit bytes, or was not read, are left out
// entirely, with no BulkDataURI and no InlineBinary, inside sequences too.
// The members are in the order of their tags, so the same header always
// gives the same bytes.
func (h *Header) DICOMJSONShort() ([]byte, error) {
	m, err := h.file.GetDataset().ToDICOMJSON()
	if err != nil {
		// As DICOMJSON: go-dicom's text is not passed on.
		return nil, errors.New("header: the data set cannot be written as DICOM JSON")
	}
	delete(m, keyTrailingPadding)
	refer(h.file.DataElements, m, nil, true, func(e *filereader.DataElementValue, _ string, length int64) bool {
		return isPixelTag(e.Tag) || e.ValueSkipped || length > ShortValueLimit
	})
	return json.Marshal(m)
}
