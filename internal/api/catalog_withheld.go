package api

import (
	"slices"

	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The catalog holds, beside a scan's labels, counts and times, the values
// read from its files: the Study and Series Instance UIDs, the study date,
// the modality, the series description and number. For a scan a receiver
// that keeps files as received archived they are the sender's originals,
// and a masked key is refused the same values in the scan's metadata and
// downloads (refuseAsReceived). So a masked key reading the catalog is
// answered such a scan or session with those values left out and
// values_withheld set; every other caller, and a masked key reading a scan
// archived de-identified, gets them.

// originalsFor returns how the catalog's lists treat the rows archived as
// received for the caller of d: for a masked key they never match a filter
// on a value read from the files and sort after the rest in an order by one
// (tenant.OriginalsHidden), so asking which rows match is no way to learn
// the values withheld from their answers.
func originalsFor(d access.Decision) tenant.Originals {
	if d.Masked() {
		return tenant.OriginalsHidden
	}
	return tenant.OriginalsAll
}

// withholdScan removes from m the values read from the scan's files and
// says so.
func withholdScan(m *v1.Scan) {
	m.SeriesInstanceUid, m.Modality, m.SeriesDescription, m.SeriesNumber = "", "", "", nil
	m.ValuesWithheld = true
}

// withholdSession removes from m the values read from the session's files
// and says so.
func withholdSession(m *v1.Session) {
	m.StudyInstanceUid, m.StudyDate, m.Modalities = "", "", nil
	m.ValuesWithheld = true
}

// scanMessageFor returns s as the contract writes it for the caller: for a
// masked key, without the values of a scan archived as received. A scan
// whose instances are mixed counts: its values come from whichever file the
// catalog read them from first, which may be the unprotected one. The row
// says whether it was (CatalogScan.AsReceived), so no instance is read.
func scanMessageFor(masked bool, s tenant.CatalogScan) *v1.Scan {
	m := scanMessage(s)
	if masked && s.AsReceived {
		withholdScan(m)
	}
	return m
}

// sessionMessageFor is scanMessageFor for a session: one with a scan archived
// as received counts.
func sessionMessageFor(masked bool, s tenant.CatalogSession) *v1.Session {
	m := sessionMessage(s)
	if masked && s.AsReceived {
		withholdSession(m)
	}
	return m
}

// namesTag reports whether any of the search's keys is on one of tags.
func namesTag(keys []dicomweb.Key, tags ...tag.Tag) bool {
	for _, k := range keys {
		if slices.Contains(tags, k.Tag) {
			return true
		}
	}
	return false
}
