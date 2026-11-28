package api

import (
	"context"
	"slices"

	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/dicomweb"
	"github.com/amrshadid/nopacs/internal/master"
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

// scanAsReceived reports whether any instance the scan holds was archived
// as received (no profile version de-identified it). A scan whose
// instances are mixed counts: its values come from whichever file the
// catalog read them from first, which may be the unprotected one.
func scanAsReceived(ctx context.Context, p tenant.Pipeline, path label.Path) (bool, error) {
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		held, next, err := p.ListHeldInstances(ctx, path, page)
		if err != nil {
			return false, err
		}
		for _, in := range held {
			if in.Profile.IsZero() {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		page.Cursor = next
	}
}

// sessionAsReceived reports whether any scan of the session holds an
// instance archived as received.
func sessionAsReceived(ctx context.Context, p tenant.Pipeline, subject, session string) (bool, error) {
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		got, err := p.ListScans(ctx, tenant.ScanFilter{SubjectLabel: subject, SessionLabel: session, Order: tenant.OrderLabel}, page)
		if err != nil {
			return false, err
		}
		for _, sc := range got.Items {
			as, err := scanAsReceived(ctx, p, label.Path{Subject: subject, Session: session, Scan: sc.Label})
			if err != nil || as {
				return as, err
			}
		}
		if got.Next == "" {
			return false, nil
		}
		page.Cursor = got.Next
	}
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
// masked key, without the values of a scan archived as received.
func scanMessageFor(ctx context.Context, masked bool, p tenant.Pipeline, s tenant.CatalogScan) (*v1.Scan, error) {
	m := scanMessage(s)
	if !masked {
		return m, nil
	}
	as, err := scanAsReceived(ctx, p, label.Path{Subject: s.SubjectLabel, Session: s.SessionLabel, Scan: s.Label})
	if err != nil {
		return nil, err
	}
	if as {
		withholdScan(m)
	}
	return m, nil
}

// sessionMessageFor is scanMessageFor for a session.
func sessionMessageFor(ctx context.Context, masked bool, p tenant.Pipeline, s tenant.CatalogSession) (*v1.Session, error) {
	m := sessionMessage(s)
	if !masked {
		return m, nil
	}
	as, err := sessionAsReceived(ctx, p, s.SubjectLabel, s.Label)
	if err != nil {
		return nil, err
	}
	if as {
		withholdSession(m)
	}
	return m, nil
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
