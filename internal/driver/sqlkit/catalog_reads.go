package sqlkit

import (
	"context"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Row is a row or rows being read, as both drivers' result types are.
type Row interface {
	// Scan copies the row's columns into dest, in order.
	Scan(dest ...any) error
}

// CatalogRunner runs the catalog's statements on one tenant's database:
// what differs between the drivers besides the text CatalogSQL writes.
type CatalogRunner interface {
	// Rows runs q and calls scan with each row it returns; a failure is
	// returned as the driver's error, with message.
	Rows(ctx context.Context, message string, q CatalogRead, scan func(Row) error) error
	// One runs q and scans its one row; no row is NotFound, with what,
	// and another failure the driver's error, with message. scan returns
	// the row's own error unchanged, so no row is told apart.
	One(ctx context.Context, what, message string, q CatalogRead, scan func(Row) error) error
	// Totals returns the tenant's running totals.
	Totals(ctx context.Context) (tenant.CatalogCounts, error)
	// Binding returns what a list's cursor is bound to: the tenant and
	// parts.
	Binding(parts ...string) string
}

// CatalogReader is the catalog's reads (tenant.CatalogReads) on a SQL
// database, which both drivers answer with: which statement a list sends,
// which running total answers its count, that a subject or session the
// filter names exists, its cursor, and a session's modalities. T is the
// type a time column is read into (a time, or microseconds).
type CatalogReader[T any] struct {
	// SQL writes the statements in the driver's terms.
	SQL CatalogSQL
	// Run runs them.
	Run CatalogRunner
	// Time returns the time a column read into a T holds, in UTC.
	Time func(T) time.Time
}

// subjectRow is a subject read with its key.
type subjectRow struct {
	key CatalogKey
	s   tenant.CatalogSubject
}

// sessionRow is a session read with its key.
type sessionRow struct {
	key CatalogKey
	s   tenant.CatalogSession
}

// scanRow is a scan read with its key.
type scanRow struct {
	key CatalogKey
	s   tenant.CatalogScan
}

// scanSubject reads a row of SubjectColumns.
func (c CatalogReader[T]) scanSubject(row Row, r *subjectRow) error {
	var created, updated T
	err := row.Scan(&r.key.ID, &created, &r.key.Folded, &r.s.Label, &r.s.Sessions, &r.s.Instances, &r.s.Bytes, &updated)
	r.key.Created, r.s.Updated = c.Time(created), c.Time(updated)
	r.s.Created = r.key.Created
	return err
}

// scanSession reads a row of SessionColumns.
func (c CatalogReader[T]) scanSession(row Row, r *sessionRow) error {
	var created, updated T
	err := row.Scan(&r.key.ID, &created, &r.key.SubjectFolded, &r.key.Folded, &r.s.StudyDate, &r.s.SubjectLabel, &r.s.Label,
		&r.s.StudyUID, &r.s.Scans, &r.s.Instances, &r.s.Bytes, &r.s.Storage, &r.s.Receiver, &updated)
	r.key.StudyDate = r.s.StudyDate
	r.key.Created, r.s.Updated = c.Time(created), c.Time(updated)
	r.s.Created = r.key.Created
	return err
}

// scanScan reads a row of ScanColumns.
func (c CatalogReader[T]) scanScan(row Row, r *scanRow) error {
	var created, updated T
	var number *int32
	var manifest []byte
	err := row.Scan(&r.key.ID, &created, &r.key.SubjectFolded, &r.key.SessionFolded, &r.key.Folded, &r.key.DescriptionFolded,
		&r.s.SubjectLabel, &r.s.SessionLabel, &r.s.Label, &r.s.SeriesUID, &r.s.Values.Modality, &r.s.Values.SeriesDescription,
		&number, &r.s.Instances, &r.s.Bytes, &r.s.Storage, &manifest, &r.s.Receiver, &updated)
	if number != nil {
		r.s.Values.SeriesNumber, r.s.Values.HasSeriesNumber = *number, true
	}
	r.s.ManifestSHA256 = ToSHA256(manifest)
	r.key.Created, r.s.Updated = c.Time(created), c.Time(updated)
	r.s.Created = r.key.Created
	return err
}

// scanHeld reads a row of HeldInstanceColumns, its key as the row keeps it.
func (c CatalogReader[T]) scanHeld(row Row, in *tenant.ScanInstance) error {
	var sum, archived []byte
	var held T
	var mark string
	err := row.Scan(&in.SOPInstanceUID, &in.ReceptionID, &in.Key, &in.Offset, &in.Length, &in.Size, &sum, &archived, &held,
		&in.Profile.Name, &in.Profile.Version, &mark, &in.PixelRiskAccepted)
	in.SHA256, in.ArchivedSHA256, in.Held, in.Damaged = ToSHA256(sum), ToSHA256(archived), c.Time(held), mark != ""
	return err
}

// placeHeld gives instances read by scanHeld from a scan their scan's
// storage and their key (tenant.InstanceKey), made from the labels the scan
// holds, which are those the instances were archived under.
func placeHeld(scan tenant.CatalogScan, ins []tenant.ScanInstance) error {
	p := label.Path{Subject: scan.SubjectLabel, Session: scan.SessionLabel, Scan: scan.Label}
	storage := scan.Storage
	for i := range ins {
		key, err := tenant.InstanceKey(p, ins[i].SOPInstanceUID, ins[i].Key)
		if err != nil {
			return err
		}
		ins[i].Storage, ins[i].Key = storage, key
	}
	return nil
}

// rows runs q and reads each row with scan into a new element of the
// result.
func rows[E any](ctx context.Context, run CatalogRunner, message string, q CatalogRead, scan func(Row, *E) error) ([]E, error) {
	var out []E
	err := run.Rows(ctx, message, q, func(row Row) error {
		var e E
		if err := scan(row, &e); err != nil {
			return err
		}
		out = append(out, e)
		return nil
	})
	return out, err
}

// one runs q and reads its one row with scan, or returns NotFound with
// what.
func one[E any](ctx context.Context, run CatalogRunner, what, message string, q CatalogRead, scan func(Row, *E) error) (E, error) {
	var e E
	err := run.One(ctx, what, message, q, func(row Row) error { return scan(row, &e) })
	return e, err
}

// subjectByLabel reads the subject with this label, or NotFound.
func (c CatalogReader[T]) subjectByLabel(ctx context.Context, subjectLabel string) (subjectRow, error) {
	return one(ctx, c.Run, "the tenant has no subject with this label", "the subject cannot be read",
		c.SQL.SubjectByLabel(subjectLabel), c.scanSubject)
}

// sessionByLabels reads the session with these labels, or NotFound.
func (c CatalogReader[T]) sessionByLabels(ctx context.Context, subjectLabel, sessionLabel string) (sessionRow, error) {
	return one(ctx, c.Run, "the tenant has no session with these labels", "the session cannot be read",
		c.SQL.SessionByLabels(subjectLabel, sessionLabel), c.scanSession)
}

// scanByLabels reads the scan with these labels, or NotFound.
func (c CatalogReader[T]) scanByLabels(ctx context.Context, p label.Path) (scanRow, error) {
	return one(ctx, c.Run, "the tenant has no scan with these labels", "the scan cannot be read", c.SQL.ScanByLabels(p), c.scanOneScan)
}

// scanOneScan reads a row of ScanColumns and scanSidecarColumn.
func (c CatalogReader[T]) scanOneScan(row Row, r *scanRow) error {
	var created, updated T
	var number *int32
	var manifest, sidecar []byte
	err := row.Scan(&r.key.ID, &created, &r.key.SubjectFolded, &r.key.SessionFolded, &r.key.Folded, &r.key.DescriptionFolded,
		&r.s.SubjectLabel, &r.s.SessionLabel, &r.s.Label, &r.s.SeriesUID, &r.s.Values.Modality, &r.s.Values.SeriesDescription,
		&number, &r.s.Instances, &r.s.Bytes, &r.s.Storage, &manifest, &r.s.Receiver, &updated, &sidecar)
	if number != nil {
		r.s.Values.SeriesNumber, r.s.Values.HasSeriesNumber = *number, true
	}
	r.s.ManifestSHA256, r.s.SidecarSHA256 = ToSHA256(manifest), ToSHA256(sidecar)
	r.key.Created, r.s.Updated = c.Time(created), c.Time(updated)
	r.s.Created = r.key.Created
	return err
}

// tenantTotal returns the tenant's running total that pick chooses.
func (c CatalogReader[T]) tenantTotal(ctx context.Context, pick func(tenant.CatalogCounts) int64) (*int64, error) {
	totals, err := c.Run.Totals(ctx)
	if err != nil {
		return nil, err
	}
	n := pick(totals)
	return &n, nil
}

// CatalogSubject implements tenant.SubjectReads.
func (c CatalogReader[T]) CatalogSubject(ctx context.Context, subjectLabel string) (tenant.CatalogSubject, error) {
	if err := tenant.ValidateLabel("subject", subjectLabel); err != nil {
		return tenant.CatalogSubject{}, err
	}
	r, err := c.subjectByLabel(ctx, subjectLabel)
	return r.s, err
}

// ListSubjects implements tenant.SubjectReads.
func (c CatalogReader[T]) ListSubjects(ctx context.Context, f tenant.SubjectFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogSubject], error) {
	var out tenant.CatalogPage[tenant.CatalogSubject]
	if err := f.Validate(); err != nil {
		return out, err
	}
	binding := c.Run.Binding(subjectFilterParts(f)...)
	after, err := catalogAfter(page, ListCatalogSubjects, binding, f.Order, levelSubjects,
		func(k CatalogKey) bool { return subjectKeyFits(f, k) })
	if err != nil {
		return out, err
	}
	if f.Total() == tenant.TotalTenant {
		if out.Total, err = c.tenantTotal(ctx, func(t tenant.CatalogCounts) int64 { return t.Subjects }); err != nil {
			return out, err
		}
	}
	got, err := rows(ctx, c.Run, "the subjects cannot be listed", c.SQL.Subjects(f, after, page.Limit+1), c.scanSubject)
	if err != nil {
		return out, err
	}
	got, last, more := Cut(got, page.Limit)
	for _, r := range got {
		out.Items = append(out.Items, r.s)
	}
	if more {
		out.Next = catalogCursor(ListCatalogSubjects, binding, f.Order, levelSubjects, last.key)
	}
	return out, nil
}

// CatalogSession implements tenant.SessionReads.
func (c CatalogReader[T]) CatalogSession(ctx context.Context, subjectLabel, sessionLabel string) (tenant.CatalogSession, error) {
	for _, l := range [][2]string{{"subject", subjectLabel}, {"session", sessionLabel}} {
		if err := tenant.ValidateLabel(l[0], l[1]); err != nil {
			return tenant.CatalogSession{}, err
		}
	}
	r, err := c.sessionByLabels(ctx, subjectLabel, sessionLabel)
	if err != nil {
		return tenant.CatalogSession{}, err
	}
	got := []sessionRow{r}
	if err := c.addModalities(ctx, got); err != nil {
		return tenant.CatalogSession{}, err
	}
	return got[0].s, nil
}

// ListSessions implements tenant.SessionReads.
func (c CatalogReader[T]) ListSessions(ctx context.Context, f tenant.SessionFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogSession], error) {
	var out tenant.CatalogPage[tenant.CatalogSession]
	if err := f.Validate(); err != nil {
		return out, err
	}
	binding := c.Run.Binding(sessionFilterParts(f)...)
	after, err := catalogAfter(page, ListCatalogSessions, binding, f.Order, levelSessions,
		func(k CatalogKey) bool { return sessionKeyFits(f, k) })
	if err != nil {
		return out, err
	}
	var subjectID int64
	if f.SubjectLabel != "" {
		sub, err := c.subjectByLabel(ctx, f.SubjectLabel)
		if err != nil {
			return out, err
		}
		subjectID = sub.key.ID
		if f.Total() == tenant.TotalParent {
			out.Total = &sub.s.Sessions
		}
	} else if f.Total() == tenant.TotalTenant {
		if out.Total, err = c.tenantTotal(ctx, func(t tenant.CatalogCounts) int64 { return t.Sessions }); err != nil {
			return out, err
		}
	}
	q := c.SQL.Sessions(f, subjectID, after, page.Limit+1)
	var got []sessionRow
	if SessionsOfModality(f) {
		got, err = c.sessionsOfModality(ctx, q)
	} else {
		got, err = rows(ctx, c.Run, "the sessions cannot be listed", q, c.scanSession)
	}
	if err != nil {
		return out, err
	}
	got, last, more := Cut(got, page.Limit)
	if err := c.addModalities(ctx, got); err != nil {
		return out, err
	}
	for _, r := range got {
		out.Items = append(out.Items, r.s)
	}
	if more {
		out.Next = catalogCursor(ListCatalogSessions, binding, f.Order, levelSessions, last.key)
	}
	return out, nil
}

// sessionsOfModality reads a page of a modality's sessions: their ids in
// order from session_modalities, then the sessions by id.
func (c CatalogReader[T]) sessionsOfModality(ctx context.Context, q CatalogRead) ([]sessionRow, error) {
	ids, err := rows(ctx, c.Run, "the sessions cannot be listed", q, func(row Row, id *int64) error {
		var created T
		return row.Scan(id, &created)
	})
	if err != nil || len(ids) == 0 {
		return nil, err
	}
	byIDs, err := c.SQL.SessionsByID(ids)
	if err != nil {
		return nil, err
	}
	found, err := rows(ctx, c.Run, "the sessions cannot be listed", byIDs, c.scanSession)
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]sessionRow, len(found))
	for _, r := range found {
		byID[r.key.ID] = r
	}
	out := make([]sessionRow, 0, len(ids))
	for _, id := range ids {
		// A session is never removed, so each id has its row.
		if r, ok := byID[id]; ok {
			out = append(out, r)
		}
	}
	return out, nil
}

// addModalities sets the modalities of each session read, each once, in
// byte order.
func (c CatalogReader[T]) addModalities(ctx context.Context, got []sessionRow) error {
	if len(got) == 0 {
		return nil
	}
	ids := make([]int64, len(got))
	at := make(map[int64]int, len(got))
	for i, r := range got {
		ids[i], at[r.key.ID] = r.key.ID, i
	}
	q, err := c.SQL.ModalitiesOf(ids)
	if err != nil {
		return err
	}
	type pair struct {
		id       int64
		modality string
	}
	pairs, err := rows(ctx, c.Run, "the sessions' modalities cannot be read", q, func(row Row, p *pair) error {
		return row.Scan(&p.id, &p.modality)
	})
	if err != nil {
		return err
	}
	for _, p := range pairs {
		i := at[p.id]
		got[i].s.Modalities = append(got[i].s.Modalities, p.modality)
	}
	for i := range got {
		slices.Sort(got[i].s.Modalities)
	}
	return nil
}

// CatalogScan implements tenant.ScanReads.
func (c CatalogReader[T]) CatalogScan(ctx context.Context, p label.Path) (tenant.CatalogScan, error) {
	if err := tenant.ValidateScanPath(p); err != nil {
		return tenant.CatalogScan{}, err
	}
	r, err := c.scanByLabels(ctx, p)
	return r.s, err
}

// ListScans implements tenant.ScanReads.
func (c CatalogReader[T]) ListScans(ctx context.Context, f tenant.ScanFilter, page master.Page) (tenant.CatalogPage[tenant.CatalogScan], error) {
	var out tenant.CatalogPage[tenant.CatalogScan]
	if err := f.Validate(); err != nil {
		return out, err
	}
	binding := c.Run.Binding(scanFilterParts(f)...)
	after, err := catalogAfter(page, ListCatalogScans, binding, f.Order, levelScans,
		func(k CatalogKey) bool { return scanKeyFits(f, k) })
	if err != nil {
		return out, err
	}
	var sessionID int64
	switch {
	case f.SessionLabel != "":
		se, err := c.sessionByLabels(ctx, f.SubjectLabel, f.SessionLabel)
		if err != nil {
			return out, err
		}
		sessionID = se.key.ID
		if f.Total() == tenant.TotalParent {
			out.Total = &se.s.Scans
		}
	case f.SubjectLabel != "":
		if _, err := c.subjectByLabel(ctx, f.SubjectLabel); err != nil {
			return out, err
		}
	case f.Total() == tenant.TotalTenant:
		if out.Total, err = c.tenantTotal(ctx, func(t tenant.CatalogCounts) int64 { return t.Scans }); err != nil {
			return out, err
		}
	}
	got, err := rows(ctx, c.Run, "the scans cannot be listed", c.SQL.Scans(f, sessionID, after, page.Limit+1), c.scanScan)
	if err != nil {
		return out, err
	}
	got, last, more := Cut(got, page.Limit)
	for _, r := range got {
		out.Items = append(out.Items, r.s)
	}
	if more {
		out.Next = catalogCursor(ListCatalogScans, binding, f.Order, levelScans, last.key)
	}
	return out, nil
}

// ListHeldInstances implements tenant.InstanceReads.
func (c CatalogReader[T]) ListHeldInstances(ctx context.Context, p label.Path, page master.Page) ([]tenant.ScanInstance, string, error) {
	if err := tenant.ValidateScanPath(p); err != nil {
		return nil, "", err
	}
	binding := c.Run.Binding(ListHeldInstances, p.Subject, p.Session, p.Scan)
	after, err := heldAfter(page, binding)
	if err != nil {
		return nil, "", err
	}
	scan, err := c.scanByLabels(ctx, p)
	if err != nil {
		return nil, "", err
	}
	out, err := rows(ctx, c.Run, "the scan's instances cannot be listed", c.SQL.HeldInstances(scan.key.ID, after, page.Limit+1), c.scanHeld)
	if err == nil {
		err = placeHeld(scan.s, out)
	}
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListHeldInstances, binding, last.SOPInstanceUID), nil
}

// largestInstanceSQL reads the largest instance file and the furthest end
// of an instance's range in its object: an aggregate, so always one row.
const largestInstanceSQL = `SELECT COALESCE(MAX(size), 0), COALESCE(MAX(byte_offset + byte_length), 0) FROM instances`

// LargestInstance implements tenant.InstanceSizes.
func (c CatalogReader[T]) LargestInstance(ctx context.Context) (int64, error) {
	var size, extent int64
	err := c.Run.One(ctx, "the catalog's instances cannot be read", "the largest instance cannot be read", CatalogRead{SQL: largestInstanceSQL},
		func(row Row) error { return row.Scan(&size, &extent) })
	if err != nil {
		return 0, err
	}
	return max(size, extent), nil
}

// HeldInstance implements tenant.InstanceReads.
func (c CatalogReader[T]) HeldInstance(ctx context.Context, p label.Path, uid string) (tenant.ScanInstance, error) {
	if err := tenant.ValidateScanPath(p); err != nil {
		return tenant.ScanInstance{}, err
	}
	if err := tenant.ValidateInstanceUID(uid); err != nil {
		return tenant.ScanInstance{}, err
	}
	scan, err := c.scanByLabels(ctx, p)
	if err != nil {
		return tenant.ScanInstance{}, err
	}
	in, err := one(ctx, c.Run, "the scan holds no instance with this SOP Instance UID", "the instance cannot be read",
		c.SQL.HeldInstance(scan.key.ID, uid), c.scanHeld)
	if err != nil {
		return tenant.ScanInstance{}, err
	}
	got := []tenant.ScanInstance{in}
	if err := placeHeld(scan.s, got); err != nil {
		return tenant.ScanInstance{}, err
	}
	return got[0], nil
}
