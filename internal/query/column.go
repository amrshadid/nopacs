package query

import "strings"

// ColumnValue returns an attribute's value as a column of type t holds it,
// by the same reading routing compares with: a number parsed as a decimal,
// a date as YYYYMMDD (PS3.5 6.2, DA, and the date of a DT), a time as
// HHMMSS and a date-time as YYYYMMDDHHMMSS, text as it is. A search writer
// keeps what this returns, so a search compares what routing compares. ok
// is false for a value that is not of the type (a malformed DS, a date that
// is not eight digits): such a value matches nothing in routing and is held
// as absent in a search table. A number is returned in num, every other
// type in text.
func ColumnValue(t ColumnType, v string) (text string, num float64, ok bool) {
	switch t {
	case TypeText:
		// A NUL cannot be stored in a Postgres text column (nor is one a
		// character of a text value, PS3.5 6.1.2.3: it is padding): every one
		// is left out, so a header that holds one fails no archive.
		return strings.ReplaceAll(v, "\x00", ""), 0, true
	case TypeNumber:
		num, ok = parseNumber(v)
		return "", num, ok
	case TypeDate:
		text, ok = normalDate(v)
	case TypeTime:
		text, ok = normalTime(v)
	case TypeDateTime:
		text, ok = normalDateTime(v)
	}
	return text, 0, ok
}
