package search

import "strings"

// The statements that write the row a delete moved to the hidden tables
// (HiddenPrefix): the row of a scan whose delete has not been purged, which
// an undo moves back, so a column filled for the scans held now is filled
// for these too. Like the live statements they hold only names that are
// constants of this package and placeholders.

// UpdateHiddenSQL returns the statement that gives the hidden row of a scan
// the values Args gives (after its first, the scan's id), taking them, then
// the correction's id and the scan's, and, last, the source of the row
// written: the row changes only when it was read from a file with the same
// or a higher SOP Instance UID than that source, as LowestOrSame does.
func UpdateHiddenSQL() string {
	cols := rowColumns()
	set := make([]string, len(cols))
	for i, c := range cols {
		set[i] = c + " = ?"
	}
	return "UPDATE " + HiddenPrefix + ScanTable + " SET " + strings.Join(set, ", ") +
		" WHERE correction_id = ? AND scan_id = ? AND source_uid >= ?"
}

// DeleteHiddenValuesSQL returns one statement for each hidden values table,
// each removing the rows of one hidden scan, taking the correction's id and
// the scan's.
func DeleteHiddenValuesSQL() []string {
	var out []string
	for _, a := range attributes {
		if a.Multi {
			out = append(out, "DELETE FROM "+HiddenPrefix+a.Values()+" WHERE correction_id = ? AND scan_id = ?")
		}
	}
	return out
}

// InsertHiddenValueSQL returns the statement that writes one row of a hidden
// values table, taking the correction's id, the scan's, the value and its
// folded form.
func InsertHiddenValueSQL(v Value) string {
	return "INSERT INTO " + HiddenPrefix + v.Table + " (correction_id, scan_id, " + v.Column + ", " + v.Folded + ") VALUES (?, ?, ?, ?)"
}
