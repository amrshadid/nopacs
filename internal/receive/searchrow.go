package receive

import (
	"context"

	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/search"
)

// searchRow returns what the search table holds of the scan the files in
// puts belong to: the values of the file with the lowest SOP Instance UID,
// read from its header as the archive keeps it (de-identified, never the
// original a profile changed), in the DICOM JSON form the metadata file
// holds, so a rebuild or a backfill from that file reads the same values.
// first is the header of puts[0], already read; another file's header is
// read only when it has the lower UID. A header that cannot be read gives
// the zero row: the scan is archived without values to search by, never
// refused for it.
func (p *Pipeline) searchRow(ctx context.Context, puts []written, first *header.Header) search.Row {
	low := 0
	for i, wr := range puts {
		if wr.f.SOPInstanceUID < puts[low].f.SOPInstanceUID {
			low = i
		}
	}
	h := first
	if low != 0 {
		f, err := p.openPut(puts[low])
		if err != nil {
			return search.Row{}
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return search.Row{}
		}
		if h, err = header.ReadWithin(f, info.Size(), p.headerLimit(ctx)); err != nil {
			return search.Row{}
		}
	}
	js, err := h.DICOMJSON()
	if err != nil {
		return search.Row{}
	}
	return search.ExtractJSON(js, puts[low].f.SOPInstanceUID)
}
