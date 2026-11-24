package tenant

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestScanValuesOf guards how a header's text becomes the values a scan
// keeps: padding trimmed, a date that does not exist or is not YYYYMMDD
// left empty, a modality outside the CS rule left empty, a description cut
// to 64 characters with bytes that are not UTF-8 and control characters
// removed, a series number kept only as an integer of 32 bits; and that
// what it returns passes Validate.
func TestScanValuesOf(t *testing.T) {
	long := strings.Repeat("é", 70)
	for _, c := range []struct {
		date, modality, description, number string
		want                                ScanValues
	}{
		{"20200105", "MR", "T1 MPRAGE", "3", ScanValues{StudyDate: "20200105", Modality: "MR", SeriesDescription: "T1 MPRAGE", SeriesNumber: 3, HasSeriesNumber: true}},
		{" 20200105 ", " CT ", "  Scout  ", " +4 ", ScanValues{StudyDate: "20200105", Modality: "CT", SeriesDescription: "Scout", SeriesNumber: 4, HasSeriesNumber: true}},
		{"20200230", "mr", "", "", ScanValues{}},
		{"2020.01.05", "MR\\CT", "", "3.5", ScanValues{}},
		{"", "TOOLONGMODALITY17", "", "2147483648", ScanValues{}},
		{"", "OT", "a\x00b\x1bc\xffd", "-7", ScanValues{Modality: "OT", SeriesDescription: "abcd", SeriesNumber: -7, HasSeriesNumber: true}},
		{"", "", long, "", ScanValues{SeriesDescription: strings.Repeat("é", 64)}},
	} {
		got := ScanValuesOf(c.date, c.modality, c.description, c.number)
		if got != c.want {
			t.Errorf("ScanValuesOf(%q, %q, %q, %q) = %+v, want %+v", c.date, c.modality, c.description, c.number, got, c.want)
		}
		if err := got.Validate(); err != nil {
			t.Errorf("ScanValuesOf's result %+v does not validate: %v", got, err)
		}
	}
	for _, bad := range []ScanValues{{StudyDate: "2020-01-05"}, {Modality: "mr"}, {SeriesDescription: " padded"},
		{SeriesDescription: strings.Repeat("x", 65)}, {SeriesNumber: 3}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("Validate let %+v through", bad)
		}
	}
}

// TestFoldDescription guards the folded form of a series description: its
// ASCII letters in lower case and its ends trimmed, every other byte kept,
// so its order is that of its bytes on every database.
func TestFoldDescription(t *testing.T) {
	for in, want := range map[string]string{"T1 MPRAGE": "t1 mprage", "  DWI b1000 ": "dwi b1000", "PET Äbrain": "pet Äbrain", "": ""} {
		if got := FoldDescription(in); got != want {
			t.Errorf("FoldDescription(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestFoldDescriptionPrefix guards the folded form of a prefix a reader
// asks for: its ASCII letters in lower case, its spaces kept at either
// end, every other byte kept.
func TestFoldDescriptionPrefix(t *testing.T) {
	for in, want := range map[string]string{"T1 ": "t1 ", "   ": "   ", " DWI": " dwi", "PET Ä": "pet Ä", "": ""} {
		if got := FoldDescriptionPrefix(in); got != want {
			t.Errorf("FoldDescriptionPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestIsFoldedDescription guards which texts are a description as the
// catalog keeps it folded, which a cursor's key must be: not empty, no
// capital ASCII letter, no space at either end, valid UTF-8 with no
// control character, at most 64 characters.
func TestIsFoldedDescription(t *testing.T) {
	for s, want := range map[string]bool{"t1 mprage": true, "pet Äbrain": true, strings.Repeat("é", 64): true,
		"": false, "T1": false, "t1 ": false, " t1": false, "t1\x00": false, "t1\xff": false, strings.Repeat("é", 65): false} {
		if got := IsFoldedDescription(s); got != want {
			t.Errorf("IsFoldedDescription(%q) = %v, want %v", s, got, want)
		}
	}
}

// TestPrefixEnd guards the end of a prefix's range: every text that starts
// with the prefix is before it and at or after the prefix, every other
// text of the examples is outside, it is valid UTF-8, and a prefix that
// has no end says so.
func TestPrefixEnd(t *testing.T) {
	texts := []string{"", "a", "ab", "abz", "ac", "b", "ä", "äx", "å", "퟿", "", "\U0010ffff", "\U0010ffffa"}
	for _, p := range []string{"a", "ab", "ä", "퟿", "z\U0010ffff"} {
		end, ok := PrefixEnd(p)
		if !ok || !utf8.ValidString(end) {
			t.Fatalf("PrefixEnd(%q) = %q, %v", p, end, ok)
		}
		for _, s := range texts {
			if in := s >= p && s < end; in != strings.HasPrefix(s, p) {
				t.Errorf("prefix %q, end %q: %q is in the range %v, starts with the prefix %v", p, end, s, in, strings.HasPrefix(s, p))
			}
		}
	}
	for _, p := range []string{"", "\U0010ffff", "\xff"} {
		if end, ok := PrefixEnd(p); ok {
			t.Errorf("PrefixEnd(%q) = %q, want none", p, end)
		}
	}
}
