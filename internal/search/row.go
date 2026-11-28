package search

import (
	"slices"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/query"
)

// Cell is what a scan's row holds of one attribute.
type Cell struct {
	// Texts are the attribute's values written as its column holds them
	// (query.ColumnValue): at most one for an attribute held in the scan's
	// row, every distinct value for a multi-valued one. Empty when the
	// scan's file has none, or none of the attribute's type.
	Texts []string `json:"t,omitempty"`
	// Number is a number attribute's value, nil for none.
	Number *float64 `json:"n,omitempty"`
}

// Row is what the search table holds of one scan: one Cell for each
// attribute, in the order of Attributes.
type Row struct {
	// Source is the SOP Instance UID of the file the values were read
	// from: the scan's file with the lowest UID, so the row is the same
	// whichever order the files arrived in and a rebuild from the metadata
	// file makes the row an archive step made. Empty for a row with no
	// header (the zero Row).
	Source string `json:"s,omitempty"`
	// Cells holds the attributes' values, as many as Attributes.
	Cells []Cell `json:"c,omitempty"`
}

// IsZero reports whether the row was made from no file.
func (r Row) IsZero() bool { return r.Source == "" }

// Cell returns the cell of the attribute with the given keyword, and
// whether there is one.
func (r Row) Cell(keyword string) (Cell, bool) {
	for i, a := range attributes {
		if a.Keyword == keyword && i < len(r.Cells) {
			return r.Cells[i], true
		}
	}
	return Cell{}, false
}

// Extract reads the row of a scan from the data set of its file source (the
// file's SOP Instance UID): each attribute's values as routing reads them
// (query.Values) in the form its column holds (query.ColumnValue). An
// attribute the file does not hold, holds only empty values, or holds
// values that are not of its type has an empty cell; a value that cannot be
// read never fails the archive of the file, since a search is a way to find
// what is held, not a condition of holding it. A single-valued attribute
// with several values keeps the first.
func Extract(ds *dataset.Dataset, source string) Row {
	r := Row{Source: source, Cells: make([]Cell, len(attributes))}
	for i, a := range attributes {
		vals, err := query.Values(ds, a.Field(), false)
		if err != nil {
			continue
		}
		c := &r.Cells[i]
		for _, v := range vals {
			text, num, ok := query.ColumnValue(a.Type, v)
			if !ok {
				continue
			}
			if a.Type == query.TypeNumber {
				if c.Number == nil {
					c.Number = &num
				}
				continue
			}
			if !a.Multi {
				if len(c.Texts) == 0 {
					c.Texts = []string{text}
				}
				continue
			}
			if !slices.Contains(c.Texts, text) {
				c.Texts = append(c.Texts, text)
			}
		}
		slices.Sort(c.Texts)
	}
	return r
}

// ExtractJSON is Extract for a file's data set as its metadata file keeps
// it, in the DICOM JSON Model (PS3.18 Annex F), which holds text decoded
// under the file's character set, so the archive step and a rebuild or
// backfill read the same values. A header that cannot be read gives an
// empty row from source.
func ExtractJSON(js []byte, source string) Row {
	ds := dataset.NewDataset()
	if len(js) == 0 || ds.FromDICOMJSONString(string(js)) != nil {
		return Row{Source: source, Cells: make([]Cell, len(attributes))}
	}
	return Extract(ds, source)
}

// Empty returns the row of a scan whose header gave no value: every cell
// empty, from the source NoSource.
func Empty() Row { return Row{Source: NoSource, Cells: make([]Cell, len(attributes))} }

// Normal returns the row with a cell for every attribute: a row read from
// a version with fewer attributes is extended with empty cells.
func (r Row) Normal() Row {
	if len(r.Cells) < len(attributes) {
		r.Cells = append(slices.Clone(r.Cells), make([]Cell, len(attributes)-len(r.Cells))...)
	}
	return r
}
