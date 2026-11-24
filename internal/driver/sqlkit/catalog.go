package sqlkit

import (
	"strconv"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The catalog's reads (tenant.CatalogReads) as SQL both drivers run, so the
// statements, their indexes and their cursors are one implementation. Each
// list reads one table through the one index its filter and order name,
// with no join: a page is a range of that index, read in its order, cut to
// its limit, so neither database sorts or hashes rows, whatever the size of
// the tables. That is why a session and a scan keep the labels of their
// parents beside their own (both are never changed once given), and a scan
// its session's storage. A session's modalities are read by a second
// statement, for the page's sessions only.
//
// Every statement is built from the constant fragments below; values are
// always parameters.

// The catalog lists' names, the first field of their cursors.
const (
	ListCatalogSubjects = "tenant-catalog-subjects"
	ListCatalogSessions = "tenant-catalog-sessions"
	ListCatalogScans    = "tenant-catalog-scans"
	ListHeldInstances   = "tenant-catalog-instances"
)

// The columns each catalog row is read with, in this order: its key (id,
// created and the texts its orders sort by), then what the reads answer.
const (
	SubjectColumns = `id, created, folded, label, session_count, instance_count, bytes, updated`
	SessionColumns = `id, created, subject_folded, folded, study_date, subject_label, label, study_uid, scan_count,
		instance_count, bytes, storage, receiver, updated`
	ScanColumns = `id, created, subject_folded, session_folded, folded, description_folded, subject_label, session_label, label,
		series_uid, modality, series_description, series_number, instances, bytes, storage, manifest_sha256, receiver, updated`
	// HeldInstanceColumns are what an instance row keeps of
	// tenant.ScanInstance, as the pipeline's ScanInstances reads them; the
	// storage and the labels its key is made from are its scan's
	// (tenant.InstanceKey), and its archived checksum is the received one
	// when the row keeps none. The profile version that de-identified it
	// is read by the primary key of profile_versions, '' and 0 for one
	// archived as received; the finding that marked it, '' for none, by the
	// primary key of instance_damage, a table that is empty while the
	// archive is whole. A scalar subquery rather than EXISTS: Postgres may
	// answer an EXISTS by hashing the whole table once, where this is one
	// probe of its key per row read.
	HeldInstanceColumns = `sop_instance_uid, reception_id, storage_key, byte_offset, byte_length, size, sha256,
		coalesce(archived_sha256, sha256), held_at,
		coalesce((SELECT pv.name FROM profile_versions AS pv WHERE pv.id = instances.profile_version_id), ''),
		coalesce((SELECT pv.version FROM profile_versions AS pv WHERE pv.id = instances.profile_version_id), 0),
		coalesce((SELECT d.finding_id FROM instance_damage AS d WHERE d.sop_instance_uid = instances.sop_instance_uid), '')`
)

// The bounds of a newest-first range of keyRange with no time given. Every
// row is created in the years 1970 to 9999
// (master.ValidateTime), so these bound nothing; both databases hold them.
var (
	catalogTimeStart = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)
	catalogTimeEnd   = time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
)

// CatalogSQL writes the catalog's reads in one driver's terms.
type CatalogSQL struct {
	// Param returns the placeholder of the nth parameter, from 1: "?" or
	// "$n".
	Param func(n int) string
	// Time returns a time as the driver's columns hold it.
	Time func(time.Time) any
	// InIDs returns the condition that column is one of the ids of the
	// one parameter whose placeholder is p.
	InIDs func(column, p string) string
	// IDs returns a list of ids as InIDs's parameter takes it.
	IDs func(ids []int64) (any, error)
}

// CatalogRead is one statement and its arguments.
type CatalogRead struct {
	// SQL is the statement.
	SQL string
	// Args are its parameters' values, in order.
	Args []any
}

// builder gathers a statement's conditions and their arguments.
type builder struct {
	c    CatalogSQL
	args []any
	cond []string
}

// arg adds a value and returns its placeholder.
func (b *builder) arg(v any) string {
	b.args = append(b.args, v)
	return b.c.Param(len(b.args))
}

// where adds a condition; each %s in it is replaced by the next value's
// placeholder, in order.
func (b *builder) where(cond string, values ...any) {
	for _, v := range values {
		cond = strings.Replace(cond, "%s", b.arg(v), 1)
	}
	b.cond = append(b.cond, cond)
}

// read returns the statement over table with the conditions, order and
// limit. The limit is written into the text, not passed as a parameter:
// a plan made for any value of the parameters (the generic plan Postgres
// settles on for a prepared statement) then still knows a page is a few
// rows, and reads them from the index in its order rather than sorting
// what a filter keeps. It is an int a page has validated, so the text
// holds digits only.
func (b *builder) read(columns, table, order string, limit int) CatalogRead {
	q := "SELECT " + columns + " FROM " + table
	if len(b.cond) > 0 {
		q += " WHERE " + strings.Join(b.cond, " AND ")
	}
	q += " ORDER BY " + order + " LIMIT " + strconv.Itoa(limit)
	return CatalogRead{SQL: q, Args: b.args} // #nosec G202 -- every fragment is a constant of this file; values are parameters.
}

// one returns the statement of one row over table with the conditions.
func (b *builder) one(columns, table string) CatalogRead {
	return CatalogRead{SQL: "SELECT " + columns + " FROM " + table + " WHERE " + strings.Join(b.cond, " AND "), Args: b.args}
}

// newest adds a newest-first list's time range and cursor on the columns
// given. After a cursor the range's end is left out: the cursor's row was
// before it (timeFits refuses a cursor that is not), so the cursor's
// condition is the tighter upper bound, and a planner given both estimates
// fewer rows than there are and may choose to sort them.
func (b *builder) newest(created, id string, r tenant.TimeRange, after *CatalogKey) {
	if !r.Start.IsZero() {
		b.where(created+" >= %s", b.c.Time(r.Start))
	}
	if !r.End.IsZero() && after == nil {
		b.where(created+" < %s", b.c.Time(r.End))
	}
	if after != nil {
		b.where("("+created+", "+id+") < (%s, %s)", b.c.Time(after.Created), after.ID)
	}
}

// keyRange adds the rows whose column equals value as a range of the index
// on (column, key, id): from (value, low, lowID) on, to before (value,
// high, highID). A list that keeps one value of a column many rows share
// (a receiver, a modality) states it this way rather than as column =
// value, and orders by all three columns. With an equality, Postgres takes
// the column as constant, so the order (key, id) is one an index on (key,
// id) gives too, and it may walk that index and test each row: the plan it
// keeps for a statement run often, made for any value, does so for a value
// that a third of the rows hold, and for a value a few rows hold, or none,
// it then reads the whole index (tens of milliseconds at fifty thousand
// rows, growing with the table); so does a plan for a value whose rows are
// all old. As a range of the composite key, only the index that begins
// with the column yields the order, whatever the value; no other index
// begins with these columns.
func (b *builder) keyRange(column, key, id string, value, low any, lowID int64, high any, highID int64) {
	cols := "(" + column + ", " + key + ", " + id + ")"
	b.where(cols+" >= (%s, %s, %s)", value, low, lowID)
	b.where(cols+" < (%s, %s, %s)", value, high, highID)
}

// newestOf adds a newest-first list of the rows whose column equals value
// (keyRange), with its time range and cursor: ids are positive, so id 0
// places a bound before every row of its time. After a cursor the upper
// bound is the cursor, which is inside the range (timeFits).
func (b *builder) newestOf(column string, value any, created, id string, r tenant.TimeRange, after *CatalogKey) {
	low, high, highID := catalogTimeStart, catalogTimeEnd, int64(0)
	if !r.Start.IsZero() {
		low = r.Start
	}
	if !r.End.IsZero() {
		high = r.End
	}
	if after != nil {
		high, highID = after.Created, after.ID
	}
	b.keyRange(column, created, id, value, b.c.Time(low), 0, b.c.Time(high), highID)
}

// newestOrder is the order of newestOf over column.
func newestOrder(column, created, id string) string {
	return column + " DESC, " + created + " DESC, " + id + " DESC"
}

// prefix adds the range of the texts of column that start with p, folded
// already; nothing for an empty prefix. After a cursor (resumed) the
// cursor's own condition is the lower bound, past the prefix's start since
// the cursor's text starts with the prefix (its key check), so only
// the end is added: a planner given two lower bounds on one column reads its
// index from one of them, and could choose the prefix's start, walking
// every row before the cursor again.
func (b *builder) prefix(column, p string, resumed bool) {
	if p == "" {
		return
	}
	if !resumed {
		b.where(column+" >= %s", p)
	}
	if end, ok := tenant.PrefixEnd(p); ok {
		b.where(column+" < %s", end)
	}
}

// Subjects returns the page of ListSubjects after the row after, nil for
// the first page, read with limit rows.
func (c CatalogSQL) Subjects(f tenant.SubjectFilter, after *CatalogKey, limit int) CatalogRead {
	b := &builder{c: c}
	if f.Order == tenant.OrderLabel {
		b.prefix("folded", label.Fold(f.LabelPrefix), after != nil)
		if after != nil {
			b.where("folded > %s", after.Folded)
		}
		return b.read(SubjectColumns, "subjects", "folded", limit)
	}
	b.newest("created", "id", f.Range, after)
	return b.read(SubjectColumns, "subjects", "created DESC, id DESC", limit)
}

// Sessions returns the page of ListSessions; subjectID is the id of the
// subject the filter names, zero for none. The modality filter reads the
// session_modalities index only: it returns the page's session ids and
// creation times (SessionsOfModality), and the sessions are read by id.
func (c CatalogSQL) Sessions(f tenant.SessionFilter, subjectID int64, after *CatalogKey, limit int) CatalogRead {
	b := &builder{c: c}
	switch f.Order {
	case tenant.OrderLabel:
		if f.SubjectLabel != "" {
			b.where("subject_id = %s", subjectID)
			b.prefix("folded", label.Fold(f.LabelPrefix), after != nil)
			if after != nil {
				b.where("folded > %s", after.Folded)
			}
			return b.read(SessionColumns, "sessions", "folded", limit)
		}
		if after != nil {
			b.where("(subject_folded, folded) > (%s, %s)", after.SubjectFolded, after.Folded)
		}
		return b.read(SessionColumns, "sessions", "subject_folded, folded", limit)
	case tenant.OrderStudyDate:
		// A session with no study date holds '', before every date, so
		// a lower bound of "0" leaves it out when no start is given.
		start := f.StudyDateStart
		if start == "" {
			start = "0"
		}
		if f.SubjectLabel != "" {
			b.where("subject_id = %s", subjectID)
		}
		b.where("study_date >= %s", start)
		if f.StudyDateEnd != "" && after == nil {
			// After a cursor its own condition is the upper bound (newest
			// says why the end is then left out).
			b.where("study_date <= %s", f.StudyDateEnd)
		}
		if after != nil {
			b.where("(study_date, id) < (%s, %s)", after.StudyDate, after.ID)
		}
		return b.read(SessionColumns, "sessions", "study_date DESC, id DESC", limit)
	}
	switch {
	case f.Modality != "":
		b.newestOf("modality", f.Modality, "created", "session_id", f.Range, after)
		return b.read("session_id, created", "session_modalities", newestOrder("modality", "created", "session_id"), limit)
	case f.SubjectLabel != "":
		// A subject's id, unlike a receiver or a modality, is held by a few
		// rows each, so the plan made for any value is the one for a few
		// rows; and other indexes begin with it (by label, by study date),
		// so a range of its key would let a planner read one of them and
		// sort. It stays an equality, as a session's id does in Scans.
		b.where("subject_id = %s", subjectID)
		b.newest("created", "id", f.Range, after)
		return b.read(SessionColumns, "sessions", "created DESC, id DESC", limit)
	case f.Receiver != "":
		b.newestOf("receiver", f.Receiver, "created", "id", f.Range, after)
		return b.read(SessionColumns, "sessions", newestOrder("receiver", "created", "id"), limit)
	}
	b.newest("created", "id", f.Range, after)
	return b.read(SessionColumns, "sessions", "created DESC, id DESC", limit)
}

// SessionsOfModality reports whether Sessions reads session_modalities for
// the filter, whose rows are a session's id and creation time.
func SessionsOfModality(f tenant.SessionFilter) bool {
	return f.Order == tenant.OrderNewest && f.Modality != ""
}

// Scans returns the page of ListScans; sessionID is the id of the session
// the filter names, zero for none.
func (c CatalogSQL) Scans(f tenant.ScanFilter, sessionID int64, after *CatalogKey, limit int) CatalogRead {
	b := &builder{c: c}
	switch f.Order {
	case tenant.OrderLabel:
		switch {
		case f.SessionLabel != "":
			b.where("session_id = %s", sessionID)
			b.prefix("folded", label.Fold(f.LabelPrefix), after != nil)
			if after != nil {
				b.where("folded > %s", after.Folded)
			}
			return b.read(ScanColumns, "scans", "folded", limit)
		case f.SubjectLabel != "":
			b.where("subject_folded = %s", label.Fold(f.SubjectLabel))
			if after != nil {
				b.where("(session_folded, folded) > (%s, %s)", after.SessionFolded, after.Folded)
			}
			return b.read(ScanColumns, "scans", "session_folded, folded", limit)
		}
		if after != nil {
			b.where("(subject_folded, session_folded, folded) > (%s, %s, %s)", after.SubjectFolded, after.SessionFolded, after.Folded)
		}
		return b.read(ScanColumns, "scans", "subject_folded, session_folded, folded", limit)
	case tenant.OrderSeriesDescription:
		// A scan with no description holds '', before every other text,
		// and a cursor's description is never empty (scanKeyFits), so
		// the cursor is the lower bound once there is one (prefix says why
		// only one).
		if p := tenant.FoldDescriptionPrefix(f.DescriptionPrefix); p != "" {
			b.prefix("description_folded", p, after != nil)
		} else if after == nil {
			b.where("description_folded > %s", "")
		}
		if after != nil {
			b.where("(description_folded, id) > (%s, %s)", after.DescriptionFolded, after.ID)
		}
		return b.read(ScanColumns, "scans", "description_folded, id", limit)
	}
	switch {
	case f.SessionLabel != "":
		b.where("session_id = %s", sessionID)
		b.newest("created", "id", f.Range, after)
		return b.read(ScanColumns, "scans", "created DESC, id DESC", limit)
	case f.Modality != "":
		b.newestOf("modality", f.Modality, "created", "id", f.Range, after)
		return b.read(ScanColumns, "scans", newestOrder("modality", "created", "id"), limit)
	case f.Receiver != "":
		b.newestOf("receiver", f.Receiver, "created", "id", f.Range, after)
		return b.read(ScanColumns, "scans", newestOrder("receiver", "created", "id"), limit)
	}
	b.newest("created", "id", f.Range, after)
	return b.read(ScanColumns, "scans", "created DESC, id DESC", limit)
}

// SubjectByLabel returns the read of one subject by its label: the
// subjects' unique index on folded, the label then compared exactly.
func (c CatalogSQL) SubjectByLabel(subjectLabel string) CatalogRead {
	b := &builder{c: c}
	b.where("folded = %s", label.Fold(subjectLabel))
	b.where("label = %s", subjectLabel)
	return b.one(SubjectColumns, "subjects")
}

// SessionByLabels returns the read of one session by its labels: the
// sessions' unique index on its subject's folded label and its own, the
// labels then compared exactly.
func (c CatalogSQL) SessionByLabels(subjectLabel, sessionLabel string) CatalogRead {
	b := &builder{c: c}
	b.where("subject_folded = %s", label.Fold(subjectLabel))
	b.where("folded = %s", label.Fold(sessionLabel))
	b.where("subject_label = %s", subjectLabel)
	b.where("label = %s", sessionLabel)
	return b.one(SessionColumns, "sessions")
}

// ScanByLabels returns the read of one scan by its labels: the scans'
// unique index on the folded labels of its subject, its session and its
// own, the labels then compared exactly.
func (c CatalogSQL) ScanByLabels(p label.Path) CatalogRead {
	b := &builder{c: c}
	b.where("subject_folded = %s", label.Fold(p.Subject))
	b.where("session_folded = %s", label.Fold(p.Session))
	b.where("folded = %s", label.Fold(p.Scan))
	b.where("subject_label = %s", p.Subject)
	b.where("session_label = %s", p.Session)
	b.where("label = %s", p.Scan)
	return b.one(ScanColumns, "scans")
}

// SessionsByID returns the read of the sessions of these ids, each a
// lookup of the primary key: the sessions of a page of a modality's.
func (c CatalogSQL) SessionsByID(ids []int64) (CatalogRead, error) {
	list, err := c.IDs(ids)
	if err != nil {
		return CatalogRead{}, err
	}
	b := &builder{c: c}
	b.where(c.InIDs("id", "%s"), list)
	return b.one(SessionColumns, "sessions"), nil
}

// ModalitiesOf returns the read of the modalities of the sessions of these
// ids: session_modalities' primary key. They are put in order by the
// reader, a few a session, rather than sorted by the database.
func (c CatalogSQL) ModalitiesOf(ids []int64) (CatalogRead, error) {
	list, err := c.IDs(ids)
	if err != nil {
		return CatalogRead{}, err
	}
	b := &builder{c: c}
	b.where(c.InIDs("session_id", "%s"), list)
	return b.one("session_id, modality", "session_modalities"), nil
}

// HeldInstances returns the page of a scan's held instances after the UID
// after: a range of the instances' primary key, (scan_id,
// sop_instance_uid), in order of SOP Instance UID.
func (c CatalogSQL) HeldInstances(scanID int64, after string, limit int) CatalogRead {
	b := &builder{c: c}
	b.where("scan_id = %s", scanID)
	b.where("sop_instance_uid > %s", after)
	return b.read(HeldInstanceColumns, "instances", "sop_instance_uid", limit)
}

// HeldInstance returns the read of the instance held under uid in the scan
// of id scanID: the instances' primary key.
func (c CatalogSQL) HeldInstance(scanID int64, uid string) CatalogRead {
	b := &builder{c: c}
	b.where("scan_id = %s", scanID)
	b.where("sop_instance_uid = %s", uid)
	return b.one(HeldInstanceColumns, "instances")
}
