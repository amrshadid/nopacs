package engine

import "testing"

// TestDateFormsAreTheWrittenOnes guards the written forms of a date that
// clean removes and the check after searches for: without leading zeros,
// with a two-digit year, the month's name with a dot and the day as an
// ordinal, Chinese, Japanese and Korean, compact without separators; with
// the edges each is held to; a year and month only at their precision;
// and a month or day of 00 taken as unknown, its separated forms kept and
// the forms of the precision it has given.
func TestDateFormsAreTheWrittenOnes(t *testing.T) {
	edges := func(v string) (map[string]Edge, bool) {
		forms, partial := DateForms(v)
		got := map[string]Edge{}
		for _, f := range forms {
			got[f.Text] = f.Edge
		}
		return got, partial
	}
	got, partial := edges("20240305101500")
	if partial {
		t.Fatal("a date and time taken as partial")
	}
	for text, edge := range map[string]Edge{
		"20240305": EdgeAny, "2024-03-05": EdgeAny, "05/03/2024": EdgeAny,
		"3/5/2024": EdgeDate, "5.3.2024": EdgeDate, "2024-3-5": EdgeDate, "03/05/24": EdgeDate, "Mar. 5, 2024": EdgeDate,
		"5th March 2024": EdgeDate, "March 5th, 2024": EdgeDate, "2024年3月5日": EdgeDate, "5Mar2024": EdgeDate,
		"2024年03月05日": EdgeDate, "2024년 3월 5일": EdgeDate, "2024. 3. 5.": EdgeDate, "5 Mar 24": EdgeDate,
		"05032024": EdgeDigits, "03052024": EdgeDigits,
	} {
		if e, ok := got[text]; !ok || e != edge {
			t.Errorf("%s: present %v, edge %v, want %v", text, ok, e, edge)
		}
	}
	ym, partial := edges("195703")
	if e, ok := ym["1957年3月"]; !partial || !ok || e != EdgeDate {
		t.Errorf("year and month: %v %v", partial, ym)
	}
	for v, want := range map[string][]string{
		"19570000": {"1957-00-00", "00/00/1957", "1957"},
		"19570300": {"1957-03-00", "March 1957"},
		"195700":   {"1957"},
		"20241305": {"2024-13-05", "2024"},
	} {
		got, _ := edges(v)
		for _, w := range want {
			if _, ok := got[w]; !ok {
				t.Errorf("%s: no form %q in %v", v, w, got)
			}
		}
	}
}

// TestNameFormsOfShortPartsAreWords guards that a name's form holding a
// Latin part of one or two letters is taken only as a whole word, and one
// of longer parts, or of parts of a script written without spaces,
// anywhere.
func TestNameFormsOfShortPartsAreWords(t *testing.T) {
	edges := func(pn string) map[string]Edge {
		got := map[string]Edge{}
		for _, f := range NameForms(pn) {
			got[f.Text] = f.Edge
		}
		return got
	}
	for pn, want := range map[string]map[string]Edge{
		"LE^AN":      {"LE^AN": EdgeWord, "AN LE": EdgeWord, "LE AN": EdgeWord, "LE, AN": EdgeWord, "LE": EdgeWord},
		"DOE^JOHN":   {"DOE^JOHN": EdgeAny, "JOHN DOE": EdgeAny, "DOE": EdgeAny},
		"DOE^JO":     {"JO DOE": EdgeWord, "DOE": EdgeAny},
		"山田^太郎":      {"太郎 山田": EdgeAny, "山田": EdgeAny},
		"Al^Bo^Cy":   {"Bo Cy Al": EdgeWord},
		"DOE^JOHN^P": {"JOHN P DOE": EdgeWord, "JOHN DOE": EdgeAny},
	} {
		got := edges(pn)
		for text, edge := range want {
			if e, ok := got[text]; !ok || e != edge {
				t.Errorf("%s: %q present %v, edge %v, want %v", pn, text, ok, e, edge)
			}
		}
	}
}

// TestFoldTakesFullWidthAsASCII guards that full-width letters and digits,
// written in East Asian text, fold to their ASCII ones, and case is
// folded beyond ASCII.
func TestFoldTakesFullWidthAsASCII(t *testing.T) {
	for in, want := range map[rune]rune{'ｄ': 'D', 'Ｄ': 'D', '１': '1', 'é': 'É', 'K': 'K', '王': '王'} {
		if got := Fold(in); got != want {
			t.Errorf("Fold(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCleanRemovesTheNewWrittenForms guards clean on the written forms
// people use: a birth date without leading zeros, with the month's name
// and a dot, in Japanese and Korean, compact; a year beside a Japanese
// letter; full-width letters of a name; and that kept text which only
// holds a short name's letters inside words is left as it was.
func TestCleanRemovesTheNewWrittenForms(t *testing.T) {
	terms, ok := cleanTerms([]Needle{{Kind: NeedleDate, Value: "20240305"}, {Kind: NeedleDate, Value: "1957"},
		{Kind: NeedleName, Value: "DOE^JOHN"}, {Kind: NeedleName, Value: "LE^AN"}, {Kind: NeedleName, Value: "LI^AL"},
		{Kind: NeedleName, Value: "NG^AN"}, {Kind: NeedleDate, Value: "19620000"}})
	if !ok {
		t.Fatal("too many terms")
	}
	c := newCleaner(terms, maxCleanWork)
	for in, want := range map[string]string{
		"on 3/5/2024.":           "on .",
		"seen Mar. 5, 2024":      "seen ",
		"2024年3月5日に":             "に",
		"2024年03月05日に":           "に",
		"2024년 3월 5일 검사":         " 검사",
		"on 2024. 3. 5. ok":      "on  ok",
		"born 5 Mar 24":          "born ",
		"id 05032024 x":          "id  x",
		"id 105032024 stays":     "id 105032024 stays",
		"1957年生まれ":               "生まれ",
		"ＤＯＥ ＪＯＨＮ came":          " came",
		"13/5/2024 stays":        "13/5/2024 stays",
		"born 1957-03 stays":     "born 1957-03 stays",
		"SCAN LEFT KNEE":         "SCAN LEFT KNEE",
		"DELI ALSO":              "DELI ALSO",
		"SCAN NG TUBE":           "SCAN  TUBE",
		"seen by An Le today":    "seen by  today",
		"born 1962 in":           "born  in",
		"on 1962-00-00 recorded": "on  recorded",
	} {
		got, err := c.clean(t.Context(), in)
		if err != nil || got != want {
			t.Errorf("clean(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
}
