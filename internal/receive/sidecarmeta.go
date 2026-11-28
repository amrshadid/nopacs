package receive

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/version"
)

// sidecarMeta is what the scan's sidecar says beyond each file's header:
// what a catalog made again from the storage needs and the tenant's
// database alone holds. The same reading of the clock, at, is the time the
// scan's commit gives every row it makes (tenant.ScanCommit.At), so a
// line's held_at and the scan's created and updated times are the
// database's own.
type sidecarMeta struct {
	// scan is the scan's header as the catalog will hold the scan and its
	// session once the commit is made, but for its values, which the first
	// file written gives (withValues).
	scan sidecar.ScanHeader
	// reception is the id of the reception that archives the files.
	reception string
	// at is when the commit holds them.
	at time.Time
	// form says how the files arrived, from the receiver's type; empty for
	// a released file, whose arrival another reception recorded.
	form string
	// version is this build's version, as a line may hold it.
	version string
}

// scanCatalog is what the catalog holds, read under the scan's lock, of the
// scan at a path and of its session: the one source of the sidecar's
// header, since only what was committed is there. A header an earlier
// attempt wrote to the storage and never committed says nothing.
type scanCatalog struct {
	// scan is the catalog's scan at the path, when hasScan.
	scan    tenant.CatalogScan
	hasScan bool
	// session is the catalog's session at the path, when hasSession; route
	// is its routing.
	session    tenant.CatalogSession
	hasSession bool
	route      tenant.ReceptionRoute
}

// catalogAt reads what the catalog holds at path: the scan (one read) and
// its session (one read). known is the study's session as the archive step
// read it before its first scan; its routing is taken when it is the
// session at path, so the catalog's routing is read again only for a
// session made since by another reception of the study.
func (p *Pipeline) catalogAt(ctx context.Context, path label.Path, seriesUID string, known *tenant.SessionLabels) (scanCatalog, error) {
	var c scanCatalog
	sc, err := p.store.CatalogScan(ctx, path)
	switch {
	case fault.KindOf(err) == fault.NotFound:
	case err != nil:
		return c, err
	default:
		c.scan, c.hasScan = sc, true
	}
	se, err := p.store.CatalogSession(ctx, path.Subject, path.Session)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return c, nil
	case err != nil:
		return c, err
	}
	c.session, c.hasSession = se, true
	if known != nil && known.SubjectLabel == path.Subject && known.SessionLabel == path.Session {
		c.route = known.Route
		return c, nil
	}
	labels, err := p.store.CatalogLabels(ctx, se.StudyUID, []string{seriesUID})
	if err != nil {
		return c, err
	}
	if labels.Session != nil {
		c.route = labels.Session.Route
	}
	return c, nil
}

// clash returns the details of a label conflict when the labels at the path
// are another study's session or another series' scan, nil otherwise. The
// commit would refuse the same files for the same reason; found before any
// put, they are held without a byte written under another scan's keys, and
// its sidecar and manifest are left as they are.
func (c scanCatalog) clash(studyUID, seriesUID string) map[string]string {
	switch {
	case c.hasSession && c.session.StudyUID != studyUID:
		return map[string]string{"label": "session", "fault": tenant.FaultHeldByAnotherSession}
	case c.hasScan && c.scan.SeriesUID != seriesUID:
		return map[string]string{"label": "scan", "fault": tenant.FaultHeldByAnotherScan}
	}
	return nil
}

// sidecarMetaFor returns what a scan's sidecar says for the reception r
// archiving series seriesUID at the time at, given what the catalog holds
// at the scan's path (c). The header is the catalog's once the commit is
// made: a scan the catalog holds keeps its created time and receiver, and
// its updated time moves to at (the commit takes the later); a scan it does
// not hold is created at at by r's receiver. A session the catalog holds
// keeps its receiver and routing; a new one takes r's.
func (p *Pipeline) sidecarMetaFor(ctx context.Context, r tenant.Reception, c scanCatalog, seriesUID string, at time.Time) sidecarMeta {
	m := sidecarMeta{
		scan:      sidecar.ScanHeader{StudyUID: r.StudyUID, SeriesUID: seriesUID, SessionReceiver: r.Receiver, Receiver: r.Receiver, Route: sidecar.RouteOf(r.Route), Created: at, Updated: at},
		reception: r.ID,
		at:        at,
		version:   sidecar.SafeVersion(version.Get().Version),
	}
	if rc, err := p.receivers.get(ctx, r.Receiver); err == nil {
		switch rc.Type {
		case tenant.ReceiverDIMSE:
			m.form = sidecar.FormDIMSE
		case tenant.ReceiverSTOW, tenant.ReceiverImport, tenant.ReceiverJob:
			m.form = sidecar.FormPart10
		}
	}
	if c.hasSession {
		m.scan.SessionReceiver, m.scan.Route = c.session.Receiver, sidecar.RouteOf(c.route)
	}
	if c.hasScan {
		m.scan.Receiver = c.scan.Receiver
		m.scan.Created = c.scan.Created.UTC()
		m.scan.Updated = laterOf(at, c.scan.Updated.UTC())
		if m.scan.Updated.Before(m.scan.Created) {
			m.scan.Updated = m.scan.Created
		}
	}
	return m
}

// withValues sets the header's values to what the catalog will hold once
// the commit gives it v: a value the catalog holds is kept, and one it
// does not is v's, as every driver's commit fills only what is empty
// (tenant.ScanValues).
func (m *sidecarMeta) withValues(c scanCatalog, v tenant.ScanValues) {
	var held tenant.ScanValues
	if c.hasScan {
		held = c.scan.Values
	}
	if c.hasSession {
		held.StudyDate = c.session.StudyDate
	}
	if held.StudyDate == "" {
		held.StudyDate = v.StudyDate
	}
	if held.Modality == "" {
		held.Modality = v.Modality
	}
	if held.SeriesDescription == "" {
		held.SeriesDescription = v.SeriesDescription
	}
	if !held.HasSeriesNumber {
		held.SeriesNumber, held.HasSeriesNumber = v.SeriesNumber, v.HasSeriesNumber
	}
	m.scan.Values = sidecar.ValuesOf(held)
}

// laterOf returns the later of two times.
func laterOf(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// lineOf returns the sidecar line of the written file wr, with what its file
// says of itself (transfer syntax, pixel data, data set) left to the
// caller, and the fields only the database and the reception know.
func (p *Pipeline) lineOf(m sidecarMeta, wr written) sidecar.Instance {
	in := sidecar.Instance{
		SOPInstanceUID: wr.f.SOPInstanceUID, Key: wr.key, Size: wr.size, SHA256: wr.sum.String(),
		Reception: m.reception, HeldAt: m.at, NopacsVersion: m.version,
	}
	if !wr.f.Released {
		in.ReceivedForm = m.form
	}
	if wr.f.SHA256 != wr.sum {
		in.ReceivedSHA256 = wr.f.SHA256.String()
	}
	if uid.Check(wr.f.TransferSyntaxUID) == nil {
		in.ReceivedSyntax = wr.f.TransferSyntaxUID
	}
	if !wr.profile.IsZero() {
		in.Profile = wr.profile.String()
		in.ProfileSHA256 = p.profileSum(wr.profile)
		if !wr.base.IsZero() {
			in.BaseProfile = wr.base.String()
		}
		in.PixelRiskAccepted = wr.pixelRiskAccepted
		in.Unnamed, in.UnnamedOver = wr.unnamed, wr.unnamedOver
	}
	return in
}

// unnamedOf returns the elements no rule named that a de-identification
// recorded, as a line lists them: never nil, so a line says it was recorded
// even when none was found. A private creator the engine withheld stays
// withheld, since the line outlives the run and a report is made from it
// alone.
func unnamedOf(rec *engine.Record) ([]sidecar.UnnamedElement, int) {
	out := make([]sidecar.UnnamedElement, 0, len(rec.Unnamed))
	for _, u := range rec.Unnamed {
		out = append(out, sidecar.UnnamedElement{Tag: u.Tag, VR: u.VR, Count: u.Count, Creator: u.Creator, Withheld: u.CreatorUnreadable})
	}
	return out, rec.UnnamedOver
}
