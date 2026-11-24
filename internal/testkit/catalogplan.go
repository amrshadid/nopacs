package testkit

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/driver/sqlkit"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// CatalogSample is what the catalog's plan checks draw their arguments
// from: rows of a tenant's catalog, read by the database driver's test.
type CatalogSample struct {
	// Subject is a subject's label and SubjectID its id.
	Subject   string
	SubjectID int64
	// Session is a session's labels and SessionID its id.
	Session   [2]string
	SessionID int64
	// Scan is a scan's labels and ScanID its id; UID one of its instances.
	Scan   label.Path
	ScanID int64
	UID    string
	// Modality, Receiver and StudyDate are values rows hold.
	Modality, Receiver, StudyDate string
	// SubjectKey, SessionKey and ScanKey are the keys of rows near the
	// middle of their tables, where a page's cursor starts.
	SubjectKey, SessionKey, ScanKey sqlkit.CatalogKey
}

// CatalogIndexes names the indexes a database gives the catalog's unique
// constraints, which each database names its own way.
type CatalogIndexes struct {
	// SubjectsFolded is subjects (folded); SessionsFolded sessions
	// (subject_id, folded); ScansFolded scans (session_id, folded);
	// InstancesKey the index that finds an instance by its SOP Instance
	// UID; InstancesByScan the one that holds a scan's instances in order
	// of UID.
	SubjectsFolded, SessionsFolded, ScansFolded, InstancesKey, InstancesByScan string
}

// The values of a list's filter that the plan checks try beside a common
// one. A database driver's test catalog gives the rare ones to ten rows
// (ten scans the modality, ten sessions and their scans the receiver) and
// the absent ones to none. A plan that walks another index and tests each
// row reads nearly all of it for a rare value and all of it for an absent
// one; the plan of either must still be a range of the index that begins
// with the filtered column.
const (
	RareModality   = "XA"
	RareReceiver   = "rare"
	AbsentModality = "NM"
	AbsentReceiver = "absent"
)

// CatalogPlanCase is one statement of the catalog's reads, the index it
// must read, whether it is a page after a cursor, which reads a range of
// that index rather than walking it from one end, and the column whose
// value the list keeps, if any, which the index must find rather than
// test row by row.
type CatalogPlanCase struct {
	// Name says what the statement reads.
	Name string
	// Read is the statement and its arguments.
	Read sqlkit.CatalogRead
	// Index is the index the plan must read.
	Index string
	// Also is another index the plan may read instead, one that answers
	// the statement as well, or "".
	Also string
	// Paged is a page after a cursor.
	Paged bool
	// Column is the column whose one value the list keeps, or "".
	Column string
}

// CatalogPlanCases returns every statement the catalog's reads send, built
// by c with arguments from v: each list in each combination of filter and
// order it serves, as its first page and as a page after a cursor, the
// gets by labels, and the reads of a scan's instances. limit is the rows a
// page reads.
func CatalogPlanCases(c sqlkit.CatalogSQL, v CatalogSample, ix CatalogIndexes, limit int) []CatalogPlanCase {
	var out []CatalogPlanCase
	column := ""
	add := func(name, index string, paged bool, read sqlkit.CatalogRead) {
		out = append(out, CatalogPlanCase{Name: name, Read: read, Index: index, Paged: paged, Column: column})
	}
	subjects := func(name, index string, f tenant.SubjectFilter) {
		add(name, index, false, c.Subjects(f, nil, limit))
		k := v.SubjectKey
		add(name+", after a cursor", index, true, c.Subjects(f, &k, limit))
	}
	start := tenant.TimeRange{Start: v.SubjectKey.Created.Add(-1e12)}
	subjects("subjects newest first", "subjects_by_created", tenant.SubjectFilter{})
	subjects("subjects newest first in a time range", "subjects_by_created", tenant.SubjectFilter{Range: start})
	subjects("subjects by label", ix.SubjectsFolded, tenant.SubjectFilter{Order: tenant.OrderLabel})
	subjects("subjects by a label prefix", ix.SubjectsFolded, tenant.SubjectFilter{Order: tenant.OrderLabel, LabelPrefix: "sub-0"})

	sessions := func(name, index string, f tenant.SessionFilter) {
		add(name, index, false, c.Sessions(f, v.SubjectID, nil, limit))
		k := v.SessionKey
		add(name+", after a cursor", index, true, c.Sessions(f, v.SubjectID, &k, limit))
	}
	sessions("sessions newest first", "sessions_by_created", tenant.SessionFilter{})
	sessions("sessions by label", "sessions_by_label", tenant.SessionFilter{Order: tenant.OrderLabel})
	sessions("sessions by study date", "sessions_by_study_date", tenant.SessionFilter{Order: tenant.OrderStudyDate})
	sessions("sessions by study date in a range", "sessions_by_study_date",
		tenant.SessionFilter{Order: tenant.OrderStudyDate, StudyDateStart: "20150101", StudyDateEnd: v.StudyDate})
	column = "subject_id"
	sessions("a subject's sessions newest first", "sessions_by_subject_created", tenant.SessionFilter{SubjectLabel: v.Subject})
	sessions("a subject's sessions by label", ix.SessionsFolded, tenant.SessionFilter{Order: tenant.OrderLabel, SubjectLabel: v.Subject})
	sessions("a subject's sessions by a label prefix", ix.SessionsFolded,
		tenant.SessionFilter{Order: tenant.OrderLabel, SubjectLabel: v.Subject, LabelPrefix: "ses"})
	sessions("a subject's sessions by study date", "sessions_by_subject_study_date",
		tenant.SessionFilter{Order: tenant.OrderStudyDate, SubjectLabel: v.Subject})
	sessions("a subject's sessions by study date in a range", "sessions_by_subject_study_date",
		tenant.SessionFilter{Order: tenant.OrderStudyDate, SubjectLabel: v.Subject, StudyDateStart: "20150101", StudyDateEnd: v.StudyDate})
	column = "receiver"
	for _, r := range []string{v.Receiver, RareReceiver, AbsentReceiver} {
		sessions("the sessions of receiver "+r, "sessions_by_receiver", tenant.SessionFilter{Receiver: r})
		sessions("the sessions of receiver "+r+" in a time range", "sessions_by_receiver",
			tenant.SessionFilter{Receiver: r, Range: start})
	}
	column = "modality"
	for _, m := range []string{v.Modality, RareModality, AbsentModality} {
		sessions("the sessions of modality "+m, "session_modalities_by_modality", tenant.SessionFilter{Modality: m})
	}
	column = ""

	scans := func(name, index string, f tenant.ScanFilter) {
		add(name, index, false, c.Scans(f, v.SessionID, nil, limit))
		k := v.ScanKey
		add(name+", after a cursor", index, true, c.Scans(f, v.SessionID, &k, limit))
	}
	scans("scans newest first", "scans_by_created", tenant.ScanFilter{})
	scans("scans by label", "scans_by_label", tenant.ScanFilter{Order: tenant.OrderLabel})
	scans("scans by series description", "scans_by_description", tenant.ScanFilter{Order: tenant.OrderSeriesDescription})
	scans("scans by a description prefix", "scans_by_description",
		tenant.ScanFilter{Order: tenant.OrderSeriesDescription, DescriptionPrefix: "T"})
	column = "subject_folded"
	scans("a subject's scans by label", "scans_by_label", tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: v.Subject})
	column = "session_id"
	scans("a session's scans newest first", "scans_by_session_created",
		tenant.ScanFilter{SubjectLabel: v.Session[0], SessionLabel: v.Session[1]})
	scans("a session's scans by label", ix.ScansFolded,
		tenant.ScanFilter{Order: tenant.OrderLabel, SubjectLabel: v.Session[0], SessionLabel: v.Session[1]})
	column = "modality"
	for _, m := range []string{v.Modality, RareModality, AbsentModality} {
		scans("the scans of modality "+m, "scans_by_modality", tenant.ScanFilter{Modality: m})
		scans("the scans of modality "+m+" in a time range", "scans_by_modality", tenant.ScanFilter{Modality: m, Range: start})
	}
	column = "receiver"
	for _, r := range []string{v.Receiver, RareReceiver, AbsentReceiver} {
		scans("the scans of receiver "+r, "scans_by_receiver", tenant.ScanFilter{Receiver: r})
	}
	column = ""

	add("a subject by its label", ix.SubjectsFolded, true, c.SubjectByLabel(v.Subject))
	add("a session by its labels", "sessions_by_label", true, c.SessionByLabels(v.Session[0], v.Session[1]))
	add("a scan by its labels", "scans_by_label", true, c.ScanByLabels(v.Scan))
	add("an instance of a scan", ix.InstancesKey, true, c.HeldInstance(v.ScanID, v.UID))
	// Found by its UID, or by its scan and UID in the scan's index: a
	// lookup of one row either way.
	out[len(out)-1].Also = ix.InstancesByScan
	column = "scan_id"
	add("a scan's instances", ix.InstancesByScan, false, c.HeldInstances(v.ScanID, "", limit))
	add("a scan's instances, after a cursor", ix.InstancesByScan, true, c.HeldInstances(v.ScanID, v.UID, limit))
	return out
}
