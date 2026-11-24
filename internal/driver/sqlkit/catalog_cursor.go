package sqlkit

import (
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The catalog lists' cursors: the key of the last row a page answered,
// bound to the list, its filter, its order and the tenant (Binding). A
// cursor carries a checksum, not a secret, so anyone who knows what it is
// bound to can write one; its key is therefore checked against the filter
// before it reaches a statement (subjectKeyFits, sessionKeyFits,
// scanKeyFits), and one that does not fit is refused as any other bad
// page token is.

// CatalogKey is the key of one catalog row, read with it, from which the
// cursor after it is made.
type CatalogKey struct {
	// ID is the row's id in its table.
	ID int64
	// Created is when the row was first archived.
	Created time.Time
	// SubjectFolded, SessionFolded and Folded are the folded labels of its
	// subject, its session and its own, down to its level.
	SubjectFolded, SessionFolded, Folded string
	// StudyDate is a session's study date; DescriptionFolded a scan's
	// folded series description.
	StudyDate, DescriptionFolded string
}

// The levels of the catalog a list answers, which say how many folded
// labels its key holds.
const (
	levelSubjects = 1
	levelSessions = 2
	levelScans    = 3
)

// subjectFilterParts returns what a subject list's cursor is bound to: its
// order and every field of its filter.
func subjectFilterParts(f tenant.SubjectFilter) []string {
	return []string{ListCatalogSubjects, f.Order.String(), f.LabelPrefix, MicrosText(f.Range.Start), MicrosText(f.Range.End)}
}

// sessionFilterParts returns what a session list's cursor is bound to.
func sessionFilterParts(f tenant.SessionFilter) []string {
	return []string{ListCatalogSessions, f.Order.String(), f.SubjectLabel, f.LabelPrefix, f.StudyDateStart, f.StudyDateEnd,
		f.Modality, f.Receiver, MicrosText(f.Range.Start), MicrosText(f.Range.End)}
}

// scanFilterParts returns what a scan list's cursor is bound to.
func scanFilterParts(f tenant.ScanFilter) []string {
	return []string{ListCatalogScans, f.Order.String(), f.SubjectLabel, f.SessionLabel, f.LabelPrefix, f.Modality,
		f.DescriptionPrefix, f.Receiver, MicrosText(f.Range.Start), MicrosText(f.Range.End)}
}

// keyFields returns the fields of a cursor after k in a list of level and
// order: the order's key, then the id where the key is not unique.
func keyFields(order tenant.CatalogOrder, level int, k CatalogKey) []string {
	id := strconv.FormatInt(k.ID, 10)
	switch order {
	case tenant.OrderLabel:
		switch level {
		case levelSubjects:
			return []string{k.Folded}
		case levelSessions:
			return []string{k.SubjectFolded, k.Folded}
		}
		return []string{k.SubjectFolded, k.SessionFolded, k.Folded}
	case tenant.OrderStudyDate:
		return []string{k.StudyDate, id}
	case tenant.OrderSeriesDescription:
		return []string{k.DescriptionFolded, id}
	}
	return []string{strconv.FormatInt(k.Created.UnixMicro(), 10), id}
}

// catalogCursor returns the cursor after the row of key k of a catalog
// list of level, bound to binding.
func catalogCursor(list, binding string, order tenant.CatalogOrder, level int, k CatalogKey) string {
	return EncodeCursor(list, binding, keyFields(order, level, k)...)
}

// catalogAfter checks page and decodes its cursor, made by catalogCursor
// for the same list, binding, order and level, or returns nil for the
// first page. A cursor whose key fits returns false is refused as a bad
// page token.
func catalogAfter(page master.Page, list, binding string, order tenant.CatalogOrder, level int, fits func(CatalogKey) bool) (*CatalogKey, error) {
	if err := page.Validate(); err != nil {
		return nil, err
	}
	if page.Cursor == "" {
		return nil, nil
	}
	n := len(keyFields(order, level, CatalogKey{}))
	f, err := DecodeCursor(page.Cursor, list, binding, n)
	if err != nil {
		return nil, err
	}
	var k CatalogKey
	switch order {
	case tenant.OrderLabel:
		switch level {
		case levelSubjects:
			k.Folded = f[0]
		case levelSessions:
			k.SubjectFolded, k.Folded = f[0], f[1]
		default:
			k.SubjectFolded, k.SessionFolded, k.Folded = f[0], f[1], f[2]
		}
	case tenant.OrderStudyDate:
		k.StudyDate = f[0]
	case tenant.OrderSeriesDescription:
		k.DescriptionFolded = f[0]
	default:
		at, err := DecodeCursorInt(f[0])
		if err != nil {
			return nil, err
		}
		k.Created = time.UnixMicro(at).UTC()
	}
	if order != tenant.OrderLabel {
		if k.ID, err = DecodeCursorInt(f[1]); err != nil {
			return nil, err
		}
	}
	if !fits(k) {
		return nil, errBadCursor()
	}
	return &k, nil
}

// The checks of a decoded key against its list's filter. A cursor a list
// made holds the key of a row it answered, which the filter kept, so it
// always fits; one that does not was written by someone else. Its key
// would otherwise reach the statement: a condition after it could keep
// rows outside the filter where the statement leaves a bound to the
// cursor (newest, prefix), and a text that is not UTF-8 or holds a NUL
// makes Postgres fail the statement, an error of the server rather than
// the caller's.

// foldedLabel reports whether s is a label as the catalog keeps it folded.
func foldedLabel(s string) bool { return label.Valid(s) && label.Fold(s) == s }

// foldedWithPrefix reports whether s is a folded label that starts with
// the folded form of prefix.
func foldedWithPrefix(s, prefix string) bool {
	return foldedLabel(s) && strings.HasPrefix(s, label.Fold(prefix))
}

// timeFits reports whether a newest-first key fits the time range: a
// positive id and a time inside the range, compared to the microsecond as
// the database holds it.
func timeFits(k CatalogKey, r tenant.TimeRange) bool {
	at := k.Created.UnixMicro()
	if k.ID <= 0 || at < catalogTimeStart.UnixMicro() || at >= catalogTimeEnd.UnixMicro() {
		return false
	}
	if !r.Start.IsZero() && at < r.Start.UnixMicro() {
		return false
	}
	return r.End.IsZero() || at < r.End.UnixMicro()
}

// subjectKeyFits reports whether k is the key of a subject f keeps.
func subjectKeyFits(f tenant.SubjectFilter, k CatalogKey) bool {
	if f.Order == tenant.OrderLabel {
		return foldedWithPrefix(k.Folded, f.LabelPrefix)
	}
	return timeFits(k, f.Range)
}

// sessionKeyFits reports whether k is the key of a session f keeps.
func sessionKeyFits(f tenant.SessionFilter, k CatalogKey) bool {
	switch f.Order {
	case tenant.OrderLabel:
		if f.SubjectLabel != "" {
			return k.SubjectFolded == label.Fold(f.SubjectLabel) && foldedWithPrefix(k.Folded, f.LabelPrefix)
		}
		return foldedLabel(k.SubjectFolded) && foldedLabel(k.Folded)
	case tenant.OrderStudyDate:
		return k.ID > 0 && tenant.ValidStudyDate(k.StudyDate) && k.StudyDate >= f.StudyDateStart &&
			(f.StudyDateEnd == "" || k.StudyDate <= f.StudyDateEnd)
	}
	return timeFits(k, f.Range)
}

// scanKeyFits reports whether k is the key of a scan f keeps.
func scanKeyFits(f tenant.ScanFilter, k CatalogKey) bool {
	switch f.Order {
	case tenant.OrderLabel:
		switch {
		case f.SessionLabel != "":
			return k.SubjectFolded == label.Fold(f.SubjectLabel) && k.SessionFolded == label.Fold(f.SessionLabel) &&
				foldedWithPrefix(k.Folded, f.LabelPrefix)
		case f.SubjectLabel != "":
			return k.SubjectFolded == label.Fold(f.SubjectLabel) && foldedLabel(k.SessionFolded) && foldedLabel(k.Folded)
		}
		return foldedLabel(k.SubjectFolded) && foldedLabel(k.SessionFolded) && foldedLabel(k.Folded)
	case tenant.OrderSeriesDescription:
		return k.ID > 0 && tenant.IsFoldedDescription(k.DescriptionFolded) &&
			strings.HasPrefix(k.DescriptionFolded, tenant.FoldDescriptionPrefix(f.DescriptionPrefix))
	}
	return timeFits(k, f.Range)
}

// heldAfter checks page and decodes the cursor of a scan's held instances,
// made for the same binding: the SOP Instance UID of the last instance
// answered, which must be a UID; or "" for the first page.
func heldAfter(page master.Page, binding string) (string, error) {
	if err := page.Validate(); err != nil {
		return "", err
	}
	if page.Cursor == "" {
		return "", nil
	}
	f, err := DecodeCursor(page.Cursor, ListHeldInstances, binding, 1)
	if err != nil {
		return "", err
	}
	if tenant.ValidateInstanceUID(f[0]) != nil {
		return "", errBadCursor()
	}
	return f[0], nil
}
