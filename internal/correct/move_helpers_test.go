package correct

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"
	"github.com/amrshadid/go-dicom/tag"

	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The UIDs a second study of MR_small.dcm's patient takes (secondStudy):
// UUID-derived OIDs (PS3.5 B.2), none a corpus file holds.
const (
	secondStudyUID  = "2.25.100100100100100100"
	secondSeriesUID = "2.25.100100100100100101"
	secondSOPUID    = "2.25.100100100100100102"
)

// secondStudy returns MR_small.dcm made a second study of its patient: its
// Study, Series and SOP Instance UIDs changed through the DICOM library's
// writer (receive.RewriteFile), the rest of its data set and its pixel data
// as they are. The corpus holds no patient with two studies.
func secondStudy(t testing.TB) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(corpusDir, "MR_small.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	out, err := rewriteUIDs(b, map[uint32]string{0x0020000D: secondStudyUID, 0x0020000E: secondSeriesUID, 0x00080018: secondSOPUID})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// rewriteUIDs returns b with UID attributes it holds given other values,
// through the DICOM library's data set and writer: an edit never changes a
// UID, so a test that needs another study or instance of a corpus file
// makes it here.
func rewriteUIDs(b []byte, values map[uint32]string) ([]byte, error) {
	return receive.RewriteFile(b, 0, func(ds *dataset.Dataset) error {
		for t, v := range values {
			elem, ok := ds.Get(tag.Tag(t))
			if !ok {
				return fmt.Errorf("the file holds no %08X", t)
			}
			elem.SetValue([]byte(v))
		}
		return nil
	})
}

// acceptBytes hands one file's bytes to a running pipeline.
func (r *rig) acceptBytes(p *receive.Pipeline, b []byte) {
	r.t.Helper()
	out := p.Accept(r.ctx, receive.Arrival{Receiver: "dimse", Body: bytes.NewReader(b),
		Caller: tenant.Caller{CallingAETitle: "SCANNER", Address: "192.0.2.7:40000"}})
	if out.Status != receive.Received {
		r.t.Fatalf("a synthetic file: outcome %v (%s)", out.Status, out.Reason)
	}
}

// moveRig names what archiveForMove archived: the MR subject (two sessions)
// and its second session, and the CT subject (one session).
type moveRig struct {
	from, session, to string
}

// archiveForMove archives MR_small.dcm and its second study (secondStudy),
// one subject of two sessions, and the two files of the CT study of
// ct2n-*.dcm, another subject of one session.
func (r *rig) archiveForMove() moveRig {
	r.t.Helper()
	p := r.pipeline()
	r.accept(p, "MR_small.dcm", "ct2n-6293.dcm", "ct2n-6924.dcm")
	r.acceptBytes(p, secondStudy(r.t))
	r.waitEnded()
	if err := p.Stop(r.ctx); err != nil {
		r.t.Fatal(err)
	}
	var m moveRig
	for _, s := range r.scans() {
		if s.SeriesUID == secondSeriesUID {
			m.from, m.session = s.SubjectLabel, s.SessionLabel
		}
	}
	for _, s := range r.scans() {
		if s.SubjectLabel != m.from {
			m.to = s.SubjectLabel
		}
	}
	if m.from == "" || m.to == "" {
		r.t.Fatalf("the archive gives no two subjects to move between: %+v", r.scans())
	}
	return m
}

// moveNow previews a move of target under to, makes it with the preview's
// token, and copies and publishes it (ContinueRelabel), as the server's
// request and its background do.
func (r *rig) moveNow(e *Engine, target tenant.CorrectionTarget, to string, rewrite bool) (tenant.Correction, error) {
	r.t.Helper()
	p, err := e.PreviewMove(r.ctx, target, to)
	if err != nil {
		return tenant.Correction{}, err
	}
	c, err := e.Move(r.ctx, MoveRequest{Target: target, To: to, Reason: "filed under the wrong patient", Confirm: p.Confirm,
		RewriteIdentity: rewrite, Audit: r.audit()})
	if err != nil {
		return c, err
	}
	return e.ContinueRelabel(r.ctx, c.ID)
}

// scanOf returns the catalog's scan of a series, failing when there is none.
func (r *rig) scanOf(series string) tenant.CatalogScan {
	r.t.Helper()
	for _, s := range r.scans() {
		if s.SeriesUID == series {
			return s
		}
	}
	r.t.Fatalf("the catalog holds no scan of %s", series)
	return tenant.CatalogScan{}
}
