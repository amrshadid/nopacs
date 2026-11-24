package tenant

import (
	"context"
	"time"
	"unicode/utf8"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/master"
)

// The catalog's reads: its subjects, sessions and scans listed and got by
// their labels, and the instances a scan holds. Each list answers only the
// combinations of filters and order that an index of the tenant's database
// serves in that order, which each filter's Validate states; any other is
// Invalid, with a message naming the ones served, never answered by
// reading a table. So a page costs about the same at ten thousand rows as
// at a hundred million.
//
// Every list is paged by an opaque cursor that holds the position of the
// last row answered (its order's key, then the row's own id, so the order
// is total) and is bound to the list, its filter, its order and the
// tenant. A page resumes exactly after that row however many rows are
// archived meanwhile: a row archived after the first page appears on a
// later page only if it sorts after the rows already answered.

// CatalogOrder is the order a catalog list answers in.
type CatalogOrder int

// The orders.
const (
	// OrderNewest is newest first: by the time the row was first archived,
	// latest first, then by the row's id, highest first. The zero value,
	// which a list takes when none is asked for.
	OrderNewest CatalogOrder = iota
	// OrderLabel is parent first: by the folded label of the subject, then
	// of the session, then of the scan, down to the level listed, each in
	// byte order. Folded labels are unique at each level (label.Fold), so
	// the order is total.
	OrderLabel
	// OrderStudyDate lists sessions by study date, latest first, then by
	// id, highest first. Sessions with no study date are not listed in it.
	OrderStudyDate
	// OrderSeriesDescription lists scans by their folded series
	// description (FoldDescription), in byte order, then by id, lowest
	// first. Scans with no series description are not listed in it.
	OrderSeriesDescription
)

// String names the order, as a list's binding and an audit entry's
// details write it.
func (o CatalogOrder) String() string {
	switch o {
	case OrderNewest:
		return "newest"
	case OrderLabel:
		return "label"
	case OrderStudyDate:
		return "study_date"
	case OrderSeriesDescription:
		return "series_description"
	}
	return "unknown"
}

// TimeRange keeps the rows first archived at or after Start and before
// End; a zero bound is no bound.
type TimeRange struct {
	Start, End time.Time
}

// IsZero reports whether the range bounds nothing.
func (r TimeRange) IsZero() bool { return r.Start.IsZero() && r.End.IsZero() }

// validate returns an Invalid error unless each bound given is a time the
// store keeps and End is after Start when both are given.
func (r TimeRange) validate() error {
	if !r.Start.IsZero() {
		if err := master.ValidateTime("start of the time range", r.Start); err != nil {
			return err
		}
	}
	if !r.End.IsZero() {
		if err := master.ValidateTime("end of the time range", r.End); err != nil {
			return err
		}
	}
	if !r.Start.IsZero() && !r.End.IsZero() && !r.End.After(r.Start) {
		return invalid("the end of a time range is after its start")
	}
	return nil
}

// TotalSource says where a list's count of the rows that match comes
// from, when a running total gives it without counting rows.
type TotalSource int

// The sources of a count.
const (
	// TotalUnknown is a list whose count only a count of its rows could
	// tell: it is not answered.
	TotalUnknown TotalSource = iota
	// TotalTenant is the tenant's running total of the rows listed.
	TotalTenant
	// TotalParent is the running total of the subject's sessions, or of
	// the session's scans, the filter names.
	TotalParent
)

// CatalogPage is one page of a catalog list.
type CatalogPage[T any] struct {
	// Items are the rows of the page, in the order asked for.
	Items []T
	// Next is the cursor of the next page, empty after the last.
	Next string
	// Total is how many rows match, the same on every page, when a running
	// total gives it (the filter's Total); nil means not known, never zero.
	Total *int64
}

// SubjectFilter is what ListSubjects keeps and its order. The
// combinations served:
//
//   - OrderNewest, with Range or without it;
//   - OrderLabel, with LabelPrefix or without it.
type SubjectFilter struct {
	// LabelPrefix keeps the subjects whose folded label starts with its
	// folded form: a label or the start of one. OrderLabel only.
	LabelPrefix string
	// Range keeps the subjects first archived in it. OrderNewest only.
	Range TimeRange
	// Order is the order of the answer: OrderNewest or OrderLabel.
	Order CatalogOrder
}

// Validate returns an Invalid error unless the filter is one an index
// serves, each value well formed.
func (f SubjectFilter) Validate() error {
	const served = "subjects are listed newest first with a time range or without one, or by label with a label prefix or without one"
	switch f.Order {
	case OrderNewest:
		if f.LabelPrefix != "" {
			return invalid(served)
		}
		return f.Range.validate()
	case OrderLabel:
		if !f.Range.IsZero() {
			return invalid(served)
		}
		return validateLabelPrefix(f.LabelPrefix)
	}
	return invalid(served)
}

// Total says where the list's count comes from: the tenant's total of its
// subjects with no filter, and none otherwise.
func (f SubjectFilter) Total() TotalSource {
	if f.LabelPrefix == "" && f.Range.IsZero() {
		return TotalTenant
	}
	return TotalUnknown
}

// SessionFilter is what ListSessions keeps and its order. The
// combinations served:
//
//   - OrderNewest, with at most one of SubjectLabel, Modality and
//     Receiver, and with Range or without it;
//   - OrderLabel, with SubjectLabel or without it, and LabelPrefix only
//     with SubjectLabel;
//   - OrderStudyDate, with SubjectLabel or without it, and with
//     StudyDateStart and StudyDateEnd or without them.
type SessionFilter struct {
	// SubjectLabel keeps the sessions of the subject with this label,
	// which must exist (NotFound otherwise).
	SubjectLabel string
	// LabelPrefix keeps the sessions whose folded label starts with its
	// folded form. OrderLabel with SubjectLabel only.
	LabelPrefix string
	// StudyDateStart and StudyDateEnd keep the sessions whose study date
	// is on or after the start and on or before the end, each "YYYYMMDD"
	// or empty for no bound; the end is not before the start.
	// OrderStudyDate only.
	StudyDateStart, StudyDateEnd string
	// Modality keeps the sessions with a scan of this Modality, compared
	// exactly (ValidateModality). OrderNewest only.
	Modality string
	// Receiver keeps the sessions first archived through the receiver of
	// this name, whether or not it still exists. OrderNewest only.
	Receiver string
	// Range keeps the sessions first archived in it. OrderNewest only.
	Range TimeRange
	// Order is the order of the answer: OrderNewest, OrderLabel or
	// OrderStudyDate.
	Order CatalogOrder
}

// Validate returns an Invalid error unless the filter is one an index
// serves, each value well formed.
func (f SessionFilter) Validate() error {
	const served = "sessions are listed newest first with at most one of a subject, a modality and a receiver, and a time range or none; " +
		"by label, of one subject (with a label prefix or without one) or of all; " +
		"or by study date, of one subject or of all, with a range of study dates or without one"
	if f.SubjectLabel != "" {
		if err := ValidateLabel("subject", f.SubjectLabel); err != nil {
			return err
		}
	}
	switch f.Order {
	case OrderNewest:
		filters := 0
		for _, s := range []string{f.SubjectLabel, f.Modality, f.Receiver} {
			if s != "" {
				filters++
			}
		}
		if filters > 1 || f.LabelPrefix != "" || f.StudyDateStart != "" || f.StudyDateEnd != "" {
			return invalid(served)
		}
		if f.Modality != "" {
			if err := ValidateModality(f.Modality); err != nil {
				return err
			}
		}
		if f.Receiver != "" {
			if err := ValidateName("receiver", f.Receiver); err != nil {
				return err
			}
		}
		return f.Range.validate()
	case OrderLabel:
		if f.Modality != "" || f.Receiver != "" || !f.Range.IsZero() || f.StudyDateStart != "" || f.StudyDateEnd != "" ||
			(f.LabelPrefix != "" && f.SubjectLabel == "") {
			return invalid(served)
		}
		return validateLabelPrefix(f.LabelPrefix)
	case OrderStudyDate:
		if f.Modality != "" || f.Receiver != "" || !f.Range.IsZero() || f.LabelPrefix != "" {
			return invalid(served)
		}
		for _, d := range []string{f.StudyDateStart, f.StudyDateEnd} {
			if d != "" && !ValidStudyDate(d) {
				return invalid("a study date is a date that exists, written YYYYMMDD")
			}
		}
		if f.StudyDateStart != "" && f.StudyDateEnd != "" && f.StudyDateEnd < f.StudyDateStart {
			return invalid("the end of a range of study dates is not before its start")
		}
		return nil
	}
	return invalid(served)
}

// Total says where the list's count comes from: in the newest or label
// order with no filter, the tenant's total of its sessions; with only a
// subject, the subject's count of its sessions; none otherwise.
func (f SessionFilter) Total() TotalSource {
	if f.Order != OrderNewest && f.Order != OrderLabel {
		return TotalUnknown
	}
	if f.LabelPrefix != "" || f.Modality != "" || f.Receiver != "" || !f.Range.IsZero() {
		return TotalUnknown
	}
	if f.SubjectLabel != "" {
		return TotalParent
	}
	return TotalTenant
}

// ScanFilter is what ListScans keeps and its order. The combinations
// served:
//
//   - OrderNewest, with at most one of a session (SubjectLabel and
//     SessionLabel), Modality and Receiver, and with Range or without it;
//   - OrderLabel, with a subject, a session or neither, and LabelPrefix
//     only with a session;
//   - OrderSeriesDescription, with DescriptionPrefix or without it, and
//     no other filter.
type ScanFilter struct {
	// SubjectLabel keeps the scans of the subject with this label, which
	// must exist (NotFound otherwise).
	SubjectLabel string
	// SessionLabel keeps the scans of the session with this label in the
	// subject SubjectLabel names, which it needs; the session must exist.
	SessionLabel string
	// LabelPrefix keeps the scans whose folded label starts with its
	// folded form. OrderLabel with a session only.
	LabelPrefix string
	// Modality keeps the scans of this Modality, compared exactly.
	// OrderNewest only.
	Modality string
	// DescriptionPrefix keeps the scans whose folded series description
	// starts with its folded form (FoldDescriptionPrefix: ASCII letters in
	// lower case, spaces kept), at most 64 characters of UTF-8.
	// OrderSeriesDescription only.
	DescriptionPrefix string
	// Receiver keeps the scans first archived through the receiver of
	// this name. OrderNewest only.
	Receiver string
	// Range keeps the scans first archived in it. OrderNewest only.
	Range TimeRange
	// Order is the order of the answer: OrderNewest, OrderLabel or
	// OrderSeriesDescription.
	Order CatalogOrder
}

// Validate returns an Invalid error unless the filter is one an index
// serves, each value well formed.
func (f ScanFilter) Validate() error {
	const served = "scans are listed newest first with at most one of a session, a modality and a receiver, and a time range or none; " +
		"by label, of one session (with a label prefix or without one), of one subject or of all; " +
		"or by series description, with a description prefix or without one and no other filter"
	if f.SessionLabel != "" && f.SubjectLabel == "" {
		return invalid("a session is named with its subject's label")
	}
	if f.SubjectLabel != "" {
		if err := ValidateLabel("subject", f.SubjectLabel); err != nil {
			return err
		}
	}
	if f.SessionLabel != "" {
		if err := ValidateLabel("session", f.SessionLabel); err != nil {
			return err
		}
	}
	switch f.Order {
	case OrderNewest:
		filters := 0
		if f.SubjectLabel != "" {
			if f.SessionLabel == "" {
				return invalid(served)
			}
			filters++
		}
		for _, s := range []string{f.Modality, f.Receiver} {
			if s != "" {
				filters++
			}
		}
		if filters > 1 || f.LabelPrefix != "" || f.DescriptionPrefix != "" {
			return invalid(served)
		}
		if f.Modality != "" {
			if err := ValidateModality(f.Modality); err != nil {
				return err
			}
		}
		if f.Receiver != "" {
			if err := ValidateName("receiver", f.Receiver); err != nil {
				return err
			}
		}
		return f.Range.validate()
	case OrderLabel:
		if f.Modality != "" || f.Receiver != "" || !f.Range.IsZero() || f.DescriptionPrefix != "" ||
			(f.LabelPrefix != "" && f.SessionLabel == "") {
			return invalid(served)
		}
		return validateLabelPrefix(f.LabelPrefix)
	case OrderSeriesDescription:
		if f.SubjectLabel != "" || f.Modality != "" || f.Receiver != "" || !f.Range.IsZero() || f.LabelPrefix != "" {
			return invalid(served)
		}
		if !utf8.ValidString(f.DescriptionPrefix) || utf8.RuneCountInString(f.DescriptionPrefix) > MaxSeriesDescriptionLength {
			return invalid("a series description prefix is at most 64 characters of UTF-8")
		}
		return nil
	}
	return invalid(served)
}

// Total says where the list's count comes from: in the newest or label
// order with no filter, the tenant's total of its scans; with only a
// session, the session's count of its scans; none otherwise, a subject
// alone included, since a subject keeps no count of its scans.
func (f ScanFilter) Total() TotalSource {
	if f.Order != OrderNewest && f.Order != OrderLabel {
		return TotalUnknown
	}
	if f.LabelPrefix != "" || f.Modality != "" || f.Receiver != "" || !f.Range.IsZero() {
		return TotalUnknown
	}
	switch {
	case f.SessionLabel != "":
		return TotalParent
	case f.SubjectLabel != "":
		return TotalUnknown
	}
	return TotalTenant
}

// ValidateLabel returns an Invalid error unless s is a label (label.Check)
// of the level named ("subject", "session" or "scan").
func ValidateLabel(level, s string) error {
	if !label.Valid(s) {
		return invalid("a " + level + "'s label is 1 to 128 of A-Z, a-z, 0-9, \".\", \"_\" and \"-\"")
	}
	return nil
}

// validateLabelPrefix returns an Invalid error unless p is empty, or a
// label or the start of one: at most 128 of the bytes a label holds.
func validateLabelPrefix(p string) error {
	if len(p) > label.MaxLength {
		return invalid("a label prefix is at most 128 bytes")
	}
	for i := range len(p) {
		if !label.Allowed(p[i]) {
			return invalid("a label prefix holds only what a label does: A-Z, a-z, 0-9, \".\", \"_\" and \"-\"")
		}
	}
	return nil
}

// ValidateScanPath returns an Invalid error unless each of the three labels
// is a label.
func ValidateScanPath(p label.Path) error {
	if err := p.Check(); err != nil {
		return invalid("a scan is named by three labels, each 1 to 128 of A-Z, a-z, 0-9, \".\", \"_\" and \"-\"")
	}
	return nil
}

// CatalogSubject is a subject as the catalog's reads answer it.
type CatalogSubject struct {
	// Label is its label, unique in the tenant.
	Label string
	// Sessions, Instances and Bytes are its running totals: its sessions,
	// the instances they hold and the bytes of those files.
	Sessions, Instances, Bytes int64
	// Created is when its first file was archived: the earliest held time
	// (ScanInstance.Held) of the instances beneath it, whatever the order
	// their commits landed in, so a rebuild from the storage gives the
	// same; Updated when a file was last archived into it.
	Created, Updated time.Time
}

// CatalogSession is a session as the catalog's reads answer it.
type CatalogSession struct {
	// SubjectLabel and Label place it.
	SubjectLabel, Label string
	// StudyUID is its Study Instance UID, as received.
	StudyUID string
	// StudyDate is its study date (ScanValues.StudyDate), or empty.
	StudyDate string
	// Modalities are its scans' modalities, each once, in byte order.
	Modalities []string
	// Scans, Instances and Bytes are its running totals.
	Scans, Instances, Bytes int64
	// Storage is the storage its files are on.
	Storage string
	// Receiver is the receiver whose reception first archived into it.
	Receiver string
	// Created is when its first file was archived, the earliest held time
	// beneath it as CatalogSubject's; Updated when a file was last
	// archived into it.
	Created, Updated time.Time
}

// CatalogScan is a scan as the catalog's reads answer it.
type CatalogScan struct {
	// SubjectLabel, SessionLabel and Label place it.
	SubjectLabel, SessionLabel, Label string
	// SeriesUID is its Series Instance UID, as received.
	SeriesUID string
	// Values are its modality, series description and series number;
	// StudyDate is left empty (it is its session's).
	Values ScanValues
	// Instances and Bytes are its running totals.
	Instances, Bytes int64
	// Storage is its session's storage.
	Storage string
	// ManifestSHA256 is the checksum of its manifest as last written.
	ManifestSHA256 SHA256
	// Receiver is the receiver whose reception first archived into it.
	Receiver string
	// Created is when its first file was archived, the earliest held time
	// of its instances as CatalogSubject's; Updated when a file was last
	// archived into it.
	Created, Updated time.Time
}

// SubjectReads reads the catalog's subjects.
type SubjectReads interface {
	// CatalogSubject returns the subject with this label, compared
	// exactly, or NotFound: a label asked for in another case is not
	// found.
	CatalogSubject(ctx context.Context, subjectLabel string) (CatalogSubject, error)
	// ListSubjects returns a page of the subjects the filter keeps, in its
	// order.
	ListSubjects(ctx context.Context, filter SubjectFilter, page master.Page) (CatalogPage[CatalogSubject], error)
}

// SessionReads reads the catalog's sessions.
type SessionReads interface {
	// CatalogSession returns the session with these labels, or NotFound.
	CatalogSession(ctx context.Context, subjectLabel, sessionLabel string) (CatalogSession, error)
	// ListSessions returns a page of the sessions the filter keeps, in its
	// order. A subject the filter names that the tenant does not have is
	// NotFound.
	ListSessions(ctx context.Context, filter SessionFilter, page master.Page) (CatalogPage[CatalogSession], error)
}

// ScanReads reads the catalog's scans.
type ScanReads interface {
	// CatalogScan returns the scan with these labels, or NotFound.
	CatalogScan(ctx context.Context, path label.Path) (CatalogScan, error)
	// ListScans returns a page of the scans the filter keeps, in its
	// order. A subject or session the filter names that the tenant does
	// not have is NotFound.
	ListScans(ctx context.Context, filter ScanFilter, page master.Page) (CatalogPage[CatalogScan], error)
}

// InstanceReads reads the instances a scan holds, by the scan's labels.
type InstanceReads interface {
	// ListHeldInstances returns a page of the instances held in the scan
	// with these labels, in order of SOP Instance UID, whichever reception
	// confirmed each; a claim not confirmed is never listed. A scan the
	// tenant does not have is NotFound. The cursor is bound to the scan's
	// labels and the tenant. Each instance carries the profile version
	// that de-identified it (ScanInstance.Profile).
	ListHeldInstances(ctx context.Context, path label.Path, page master.Page) ([]ScanInstance, string, error)
	// HeldInstance returns the instance held under the SOP Instance UID in
	// the scan with these labels, or NotFound: a UID the tenant holds in
	// another scan, or only claims, is not found.
	HeldInstance(ctx context.Context, path label.Path, sopInstanceUID string) (ScanInstance, error)
}

// CatalogReads is every read of the catalog, which PipelineParts embeds.
type CatalogReads interface {
	SubjectReads
	SessionReads
	ScanReads
	InstanceReads
}

// ValidateInstanceUID returns an Invalid error unless s is a SOP Instance
// UID (PS3.5 9.1).
func ValidateInstanceUID(s string) error { return validateUID("SOP Instance UID", s) }
