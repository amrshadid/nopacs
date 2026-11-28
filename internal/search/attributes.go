// Package search is the search tables of a tenant: the registry of the
// attributes a scan's row holds a column for, the row built from a file's
// header, and the SQL and DDL that write and read it.
package search

import (
	"strings"
	"sync"

	"github.com/amrshadid/nopacs/internal/query"
)

// Attribute is one DICOM attribute the search table holds a column for. The
// table describes a scan by one of its files, the one with the lowest SOP
// Instance UID, so an attribute here is one a series keeps the same across
// its files (PS3.3 C.7.3.1 Series, C.7.5 Equipment, C.7.6.1 General Study
// and the acquisition parameters of its images).
type Attribute struct {
	// Keyword is the attribute's PS3.6 keyword, as a condition names it.
	Keyword string
	// Type is the type of its column.
	Type query.ColumnType
	// Folded says a text attribute also keeps its folded form
	// (query.Fold) in a second column, so a case- and accent-blind
	// comparison is an index probe.
	Folded bool
	// Trigram says the folded column also has a trigram index: contains,
	// words and patterns. Only a folded attribute can.
	Trigram bool
	// Multi says the attribute may hold several values (VM greater than
	// one, or a person's name with component groups): each value is a row
	// of the attribute's values table, not a column of the scan's row.
	Multi bool
}

// attributes are the attributes the search table holds, in column order.
// The order is part of the table: a column added is added at the end.
var attributes = []Attribute{
	{Keyword: "Modality", Type: query.TypeText, Folded: true},
	{Keyword: "SeriesDescription", Type: query.TypeText, Folded: true, Trigram: true},
	{Keyword: "SeriesNumber", Type: query.TypeNumber},
	{Keyword: "ProtocolName", Type: query.TypeText, Folded: true, Trigram: true},
	{Keyword: "StudyDescription", Type: query.TypeText, Folded: true, Trigram: true},
	{Keyword: "BodyPartExamined", Type: query.TypeText, Folded: true},
	{Keyword: "Manufacturer", Type: query.TypeText, Folded: true},
	{Keyword: "ManufacturerModelName", Type: query.TypeText, Folded: true},
	{Keyword: "StationName", Type: query.TypeText, Folded: true},
	{Keyword: "SequenceName", Type: query.TypeText, Folded: true},
	{Keyword: "PatientSex", Type: query.TypeText},
	{Keyword: "PatientAge", Type: query.TypeText},
	{Keyword: "MagneticFieldStrength", Type: query.TypeNumber},
	{Keyword: "SliceThickness", Type: query.TypeNumber},
	{Keyword: "RepetitionTime", Type: query.TypeNumber},
	{Keyword: "EchoTime", Type: query.TypeNumber},
	{Keyword: "FlipAngle", Type: query.TypeNumber},
	{Keyword: "AcquisitionDate", Type: query.TypeDate},
	{Keyword: "AcquisitionDateTime", Type: query.TypeDateTime},
	{Keyword: "ImageType", Type: query.TypeText, Folded: true, Multi: true},
	{Keyword: "ScanningSequence", Type: query.TypeText, Folded: true, Multi: true},
	{Keyword: "OperatorsName", Type: query.TypeText, Folded: true, Multi: true},
}

// Attributes returns the attributes the table holds, in column order.
func Attributes() []Attribute { return append([]Attribute(nil), attributes...) }

// Field returns the attribute as a condition's field, the form the
// registry keys it by.
func (a Attribute) Field() query.Field {
	f, err := query.ParseField(a.Keyword)
	if err != nil {
		// The keywords are constants of this package; one the grammar does
		// not read is a mistake caught by the registry's test.
		panic("search: " + a.Keyword + " is not a field: " + err.Error())
	}
	return f
}

// Tag returns the attribute's tag, from the data dictionary.
func (a Attribute) Tag() uint32 { return a.Field().Path[0].Tag }

// Column is the name of the attribute's exact column: its keyword in
// lower case with an underscore before each capital that follows a lower
// case letter or a digit ("SliceThickness" is "slice_thickness"). The names
// come from the keywords here, never from a condition's text.
func (a Attribute) Column() string { return snake(a.Keyword) }

// FoldedColumn is the name of the folded column, "" for an attribute that
// has none.
func (a Attribute) FoldedColumn() string {
	if !a.Folded {
		return ""
	}
	return a.Column() + "_folded"
}

// Values is the name of the attribute's values table, "" for an attribute
// held in the scan's row.
func (a Attribute) Values() string {
	if !a.Multi {
		return ""
	}
	return ScanTable + "_" + a.Column()
}

// TrigramName is the name of the attribute's trigram structure on SQLite
// (an FTS5 table), "" for an attribute without one.
func (a Attribute) TrigramName() string {
	if !a.Trigram {
		return ""
	}
	return ScanTable + "_" + a.Column() + "_fts"
}

// index returns the indexes the attribute's columns have: its exact column
// always, its folded one and its trigram structure when it has them.
func (a Attribute) index() query.Index {
	i := query.IndexExact
	if a.Folded {
		i |= query.IndexFolded
	}
	if a.Trigram {
		i |= query.IndexTrigram
	}
	return i
}

// snake returns a keyword in lower snake case.
func snake(k string) string {
	var b strings.Builder
	for i := 0; i < len(k); i++ {
		c := k[i]
		if 'A' <= c && c <= 'Z' {
			if i > 0 && (k[i-1] < 'A' || k[i-1] > 'Z') {
				b.WriteByte('_')
			}
			c += 'a' - 'A'
		}
		b.WriteByte(c)
	}
	return b.String()
}

// The names of the search tables. A scan's row is ScanTable; a search over
// scans reads ScanView, which adds the scan's session and subject from the
// catalog, so a scan moved to another session is found under it without
// the search table being written; the session and subject levels are the
// catalog's own tables.
const (
	// ScanTable holds one row per scan.
	ScanTable = "scan_search"
	// ScanView is ScanTable with the scan's session and subject ids.
	ScanView = "scan_search_view"
	// NoSource is the source of a row written without a header: it sorts
	// after every SOP Instance UID, so the first header given replaces it.
	NoSource = "~"
)

var (
	registryOnce sync.Once
	registry     *query.Registry
)

// Registry returns the registry of every attribute and catalog field a
// search can name: the only source of the identifiers a compiled query
// holds. It is built once and shared.
func Registry() *query.Registry {
	registryOnce.Do(func() {
		cols := []query.Column{
			{Field: "session.date", Level: query.LevelSession, Type: query.TypeDate, Exact: "study_date", Index: query.IndexExact},
			{Field: "session.receiver", Level: query.LevelSession, Type: query.TypeText, Exact: "receiver", Index: query.IndexExact},
			{Field: "calling_ae", Level: query.LevelSession, Type: query.TypeText, Exact: "calling_ae", Index: query.IndexExact},
			{Field: "called_ae", Level: query.LevelSession, Type: query.TypeText, Exact: "called_ae", Index: query.IndexExact},
			{Field: "subject.label", Level: query.LevelSubject, Type: query.TypeText, Exact: "label", Folded: "folded", Index: query.IndexFolded},
		}
		for _, a := range attributes {
			cols = append(cols, query.Column{
				Field: a.Keyword, Level: query.LevelScan, Type: a.Type, Exact: a.Column(), Folded: a.FoldedColumn(),
				Index: a.index(), Trigram: a.TrigramName(), Values: a.Values(),
			})
		}
		r, err := query.NewRegistry([]query.Table{
			{Level: query.LevelSubject, Name: "subjects", Key: "id"},
			{Level: query.LevelSession, Name: "sessions", Key: "id", Parents: map[query.Level]string{query.LevelSubject: "subject_id"}},
			{Level: query.LevelScan, Name: ScanView, Key: "scan_id", Parents: map[query.Level]string{query.LevelSubject: "subject_id", query.LevelSession: "session_id"}},
		}, cols)
		if err != nil {
			// The attributes are constants of this package; a registry
			// that does not build is a mistake caught by its test.
			panic("search: the registry does not build: " + err.Error())
		}
		registry = r
	})
	return registry
}
