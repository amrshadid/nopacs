package tenant

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The bounds of the values a scan carries from its files' headers.
const (
	// MaxModalityLength is a Modality's bound: the CS value representation
	// holds at most 16 characters (PS3.5 6.2).
	MaxModalityLength = 16
	// MaxSeriesDescriptionLength is a Series Description's bound in
	// characters: the LO value representation holds at most 64 (PS3.5
	// 6.2).
	MaxSeriesDescriptionLength = 64
)

// ScanValues are what a scan's files say of it, and of its session, that
// the catalog keeps to list and filter by: read from the header of one of
// the scan's files by the reception that commits it. Each is empty when the
// file did not hold it, or held a value that is not one of its kind; the
// catalog never refuses a file for them.
//
// A scan or session keeps the first value it was given: a later commit
// fills a value still empty, and never changes one held, so a listing's
// order does not move under a reader because a later file says otherwise.
type ScanValues struct {
	// StudyDate is the session's Study Date (0008,0020), in the form of
	// the DA value representation, "YYYYMMDD" (PS3.5 6.2), a date that
	// exists.
	StudyDate string
	// Modality is the scan's Modality (0008,0060): 1 to 16 characters of
	// A-Z, 0-9, "_" and space (CS, PS3.5 6.2), with no space at either end.
	Modality string
	// SeriesDescription is the scan's Series Description (0008,103E), at
	// most 64 characters of valid UTF-8 with no control character and no
	// space at either end.
	SeriesDescription string
	// SeriesNumber is the scan's Series Number (0020,0011), when
	// HasSeriesNumber.
	SeriesNumber int32
	// HasSeriesNumber says whether the files held a Series Number that is
	// an integer of 32 bits (IS, PS3.5 6.2).
	HasSeriesNumber bool
}

// ScanValuesOf returns the values of a scan from the text of its file's
// attributes, as a header reader decodes them: each one that is not of its
// kind is left empty, a description too long is cut to its first 64
// characters, and bytes that are not UTF-8 or are control characters are
// removed from it. The result passes ScanValues.Validate.
func ScanValuesOf(studyDate, modality, seriesDescription, seriesNumber string) ScanValues {
	v := ScanValues{SeriesDescription: cleanDescription(seriesDescription)}
	if d := strings.TrimSpace(studyDate); ValidStudyDate(d) {
		v.StudyDate = d
	}
	if m := strings.TrimSpace(modality); ValidateModality(m) == nil {
		v.Modality = m
	}
	if n, err := strconv.ParseInt(strings.TrimSpace(seriesNumber), 10, 32); err == nil {
		v.SeriesNumber, v.HasSeriesNumber = int32(n), true
	}
	return v
}

// Validate returns an Invalid error unless each value is of its kind, as
// ScanValuesOf leaves it.
func (v ScanValues) Validate() error {
	if v.StudyDate != "" && !ValidStudyDate(v.StudyDate) {
		return invalid("a study date is a date that exists, written YYYYMMDD")
	}
	if v.Modality != "" {
		if err := ValidateModality(v.Modality); err != nil {
			return err
		}
	}
	if cleanDescription(v.SeriesDescription) != v.SeriesDescription {
		return invalid("a series description is at most 64 characters of UTF-8 text, with no control character and no space at either end")
	}
	if !v.HasSeriesNumber && v.SeriesNumber != 0 {
		return invalid("a series number is given with HasSeriesNumber")
	}
	return nil
}

// ValidStudyDate reports whether s is "YYYYMMDD", a date that exists: a
// study date as the catalog keeps it.
func ValidStudyDate(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	_, err := time.Parse("20060102", s)
	return err == nil
}

// ValidateModality returns an Invalid error unless m is a Modality: 1 to 16
// characters of A-Z, 0-9, "_" and space (CS, PS3.5 6.2), with no space at
// either end, which the value representation's padding would be.
func ValidateModality(m string) error {
	if m == "" || len(m) > MaxModalityLength || m[0] == ' ' || m[len(m)-1] == ' ' {
		return invalid("a modality is 1 to 16 characters of A-Z, 0-9, \"_\" and space, with no space at either end")
	}
	if !allBytes(m, modalityByte) {
		return invalid("a modality is 1 to 16 characters of A-Z, 0-9, \"_\" and space, with no space at either end")
	}
	return nil
}

// modalityByte reports whether b may be in a Modality: A-Z, 0-9, "_" and
// space, the CS value representation's characters (PS3.5 6.2).
func modalityByte(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == ' '
}

// cleanDescription returns s as a scan keeps it: bytes that are not UTF-8
// and control characters removed, spaces at either end trimmed (the LO
// value representation's padding and leading spaces are not part of its
// value, PS3.5 6.2), and cut to its first 64 characters.
func cleanDescription(s string) string {
	s = strings.ToValidUTF8(s, "")
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > MaxSeriesDescriptionLength {
		n := 0
		for i := range s {
			if n == MaxSeriesDescriptionLength {
				s = s[:i]
				break
			}
			n++
		}
		s = strings.TrimSpace(s)
	}
	return s
}

// FoldDescription returns the form of a series description that the
// catalog orders and searches by: spaces at either end trimmed, and its
// ASCII letters in lower case. Every other byte is kept as it is, so the
// order of folded descriptions is the order of their bytes, the same on
// every database, and a description in another script is found by the
// prefix of its own characters. The description a scan keeps is folded
// with it; a prefix a reader asks for with FoldDescriptionPrefix.
func FoldDescription(s string) string {
	return FoldDescriptionPrefix(strings.TrimSpace(s))
}

// FoldDescriptionPrefix returns the form of a series description prefix
// that the catalog compares with folded descriptions: its ASCII letters in
// lower case, every other byte kept. It is not trimmed: the contract folds
// a prefix and says nothing of its spaces, so "T1 " keeps the descriptions
// that start with "t1 " and not "t10", and a prefix of spaces alone keeps
// those that start with them, which none does, since a description is kept
// trimmed.
func FoldDescriptionPrefix(p string) string {
	b := []byte(p)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// IsFoldedDescription reports whether s is a series description as the
// catalog keeps it folded (FoldDescription of a description
// ScanValues.Validate accepts): not empty, at most 64 characters of UTF-8
// with no control character, no space at either end and no ASCII capital
// letter.
func IsFoldedDescription(s string) bool {
	return s != "" && cleanDescription(s) == s && FoldDescriptionPrefix(s) == s
}

// PrefixEnd returns the least text greater than every text that starts
// with prefix, and true; or false when there is none (an empty prefix, or
// one made only of the greatest character, or one that is not UTF-8,
// which no validated prefix is). Texts are compared by their bytes, which
// for UTF-8 is the order of their characters, so the texts that start with
// prefix are exactly those at or after prefix and before its end: a range
// one index answers, on any database that orders text by its bytes. The
// end is valid UTF-8, as a database that checks its text needs.
func PrefixEnd(prefix string) (string, bool) {
	if !utf8.ValidString(prefix) {
		return "", false
	}
	for prefix != "" {
		r, size := utf8.DecodeLastRuneInString(prefix)
		prefix = prefix[:len(prefix)-size]
		next := r + 1
		if next >= 0xd800 && next <= 0xdfff {
			next = 0xe000
		}
		if next > utf8.MaxRune {
			continue
		}
		return prefix + string(next), true
	}
	return "", false
}
