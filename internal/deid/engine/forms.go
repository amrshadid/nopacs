package engine

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// The written forms of removed values. Clean removes them from free text
// and the check after de-identification searches the result for them,
// from this one list, so what one removes and the other looks for cannot
// drift apart.

// Edge is where a written form is taken for its value: what may stand
// beside it.
type Edge int

// The edges of a form.
const (
	// EdgeAny: anywhere. (The check after holds a form that begins or ends
	// with a digit to digit edges there, so an identifier is not found
	// inside a longer number.)
	EdgeAny Edge = iota
	// EdgeDate: a form ambiguous out of context (a year, a year and
	// month, a two-digit year, a day or month without its leading zero, a
	// month's name) is taken only where it stands alone as a date: no
	// ASCII letter or digit beside it, and no date's separator ("-", "/",
	// ".") with a digit beyond it, so "2024" is not the year of 2024-03-15
	// and "5 March 2024" is not inside "15 March 2024". A letter of another
	// script beside it does not count: "1957年" is the year 1957. A run that
	// reads as the date, such as "5.3.24" in "version 5.3.24", is taken for
	// it all the same: removing what may be a date is the safe side.
	EdgeDate
	// EdgeWord: a name's form that holds a part of one or two letters
	// ("Li", "Al", "Ng") is taken only as a whole word, with no letter or
	// digit of any script beside it: "LE AN" is not in "SCAN LEFT".
	EdgeWord
	// EdgeDigits: a run of digits (a date written without separators, such
	// as 05032024) is taken only with no digit beside it.
	EdgeDigits
	// EdgeIdentifier: an identifier of words (IdentifierEdge): letters and
	// no digit, in a script written with spaces, of an attribute that
	// does not identify the patient (an institution's or a station's
	// name). On a side where it ends in a letter it is taken only at a
	// word's edge: no letter of any script and no "_" beside it, since "_"
	// joins words in the names devices write ("TOSHIBA_MEC" is a maker,
	// not the institution "TOSHIBA"); a digit beside it does not join it.
	// Any other side anywhere. A name of a person is not one: its own
	// forms are taken as NameForms says.
	EdgeIdentifier
)

// IdentifierEdge returns the edge an identifier's value is taken at by
// its text alone: EdgeIdentifier for words (a letter at an end, no digit,
// no letter of a script written without spaces), so an institution's or
// a station's name is not found inside another word; else EdgeAny. A code
// that holds a digit is not words: a copy of it glued to other text
// ("MRNAB778812", "sub_AB778812") is how it leaks, so it is taken
// anywhere. In a script written without spaces (Unspaced) no word has an
// edge to find, so a name in it is taken anywhere too ("東京中央病院" in
// "東京中央病院放射線科"). The engine gives no word edge to an attribute
// that identifies the patient whatever its text (needles.go,
// patientIdentifierTags).
func IdentifierEdge(v string) Edge {
	first, _ := utf8.DecodeRuneInString(v)
	last, _ := utf8.DecodeLastRuneInString(v)
	if !unicode.IsLetter(first) && !unicode.IsLetter(last) {
		return EdgeAny
	}
	if Unspaced(v) || strings.ContainsFunc(v, unicode.IsDigit) {
		return EdgeAny
	}
	return EdgeIdentifier
}

// IdentifierWordRune reports whether r continues a word beside an
// identifier's letter (EdgeIdentifier): a letter of any script, or "_".
func IdentifierWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r)
}

// WrittenForm is one way people write a value in free text.
type WrittenForm struct {
	// Text is the form.
	Text string
	// Edge is where it is taken for the value.
	Edge Edge
}

// minPart is the length under which a Latin name part matches inside
// ordinary words ("LE", "AN"), so it, and every form holding it, is taken
// only as a whole word. A part of a script written without spaces is a
// name at any length (Unspaced).
const minPart = 3

// NameForms returns the forms of a person's name (PS3.5 6.2.1): the value;
// each component group (split by "="); each component of a group (split by
// "^") and each word of a component, since a component often holds more
// than one ("van der Berg", "山田 太郎"); and the family and given names as
// people write them: "JOHN DOE", "DOE JOHN", "DOE, JOHN", "DOE,JOHN",
// "JOHN PAUL DOE" with the middle name, and every component in order with
// spaces. A form that holds a Latin part shorter than three letters is
// taken only as a whole word (EdgeWord).
func NameForms(pn string) []WrittenForm {
	var out []WrittenForm
	add := func(text string, parts ...string) {
		edge := EdgeAny
		for _, p := range parts {
			if shortPart(p) {
				edge = EdgeWord
			}
		}
		out = append(out, WrittenForm{Text: text, Edge: edge})
	}
	var all []string
	for _, group := range strings.Split(pn, "=") {
		var parts, words []string
		for _, c := range strings.Split(group, "^") {
			if c = strings.TrimSpace(c); c != "" {
				parts = append(parts, c)
				words = append(words, strings.Fields(c)...)
			}
		}
		all = append(all, words...)
		add(group, words...)
		for _, p := range parts {
			add(p, strings.Fields(p)...)
			if ws := strings.Fields(p); len(ws) > 1 {
				for _, w := range ws {
					add(w, w)
				}
			}
		}
		if len(parts) >= 2 {
			fam, given := parts[0], parts[1]
			for _, t := range []string{given + " " + fam, fam + " " + given, fam + ", " + given, fam + "," + given} {
				add(t, append(strings.Fields(fam), strings.Fields(given)...)...)
			}
			add(strings.Join(parts, " "), words...)
			if len(parts) >= 3 {
				add(given+" "+parts[2]+" "+fam, append(append(strings.Fields(given), strings.Fields(parts[2])...), strings.Fields(fam)...)...)
			}
		}
	}
	add(pn, all...)
	return dedupe(out)
}

// shortPart reports whether a name's word is a Latin one of fewer than
// minPart letters.
func shortPart(w string) bool {
	return !Unspaced(w) && utf8.RuneCountInString(w) < minPart
}

// months are the English names of the months, by number from 1.
var months = [...]string{"January", "February", "March", "April", "May", "June", "July",
	"August", "September", "October", "November", "December"}

// DateForms returns the forms the date of a DA or DT value (PS3.5 6.2) is
// written in by people.
//
// For a value of eight digits or more, from its first eight: 20240305,
// 2024-03-05, 2024/03/05, 2024.03.05, 05/03/2024, 03/05/2024, 05.03.2024
// and 05-03-2024 (taken anywhere); 05032024 and 03052024 with no digit
// beside them; and standing alone as a date: without leading zeros
// (2024-3-5, 3/5/2024, 5.3.2024), with a two-digit year (05/03/24, 3/5/24,
// "5 Mar 24"), with the month's name, its three letters and those with a
// dot, the day with and without its zero and as an ordinal ("5 March 2024",
// "March 5th, 2024", "Mar. 5, 2024", "05-Mar-2024", "5MAR2024"), in Chinese
// and Japanese (2024年3月5日, 2024年03月05日) and Korean (2024년 3월 5일,
// "2024. 3. 5."). A month of 00, or one past 12, is unknown: the forms of
// the year are given instead of the named ones; a day of 00, or past 31,
// likewise gives the forms of the year and month.
//
// For a value of only a year, or a year and month, partial is true and the
// forms are of that precision only, standing alone (2024; 202403, 2024-03,
// 03/2024, 3/2024, "March 2024", 2024年3月, 2024년 3월): the value itself is
// then no form of its own. Nil for a value whose leading digits are not
// four, six or eight.
func DateForms(v string) (forms []WrittenForm, partial bool) {
	n := 0
	for n < len(v) && n < 8 && v[n] >= '0' && v[n] <= '9' {
		n++
	}
	switch {
	case n == 4 && (len(v) == 4 || !isDigitByte(v[4])):
		return yearForms(v[:4]), true
	case n == 6 && (len(v) == 6 || !isDigitByte(v[6])):
		return monthForms(v[:4], v[4:6]), true
	case n == 8:
		return dayForms(v[:4], v[4:6], v[6:8]), false
	}
	return nil, false
}

// yearForms returns the forms of a year alone.
func yearForms(y string) []WrittenForm {
	return []WrittenForm{{Text: y, Edge: EdgeDate}, {Text: y + "年", Edge: EdgeDate}, {Text: y + "년", Edge: EdgeDate}}
}

// monthForms returns the forms of a year and month; those of the year when
// the month is not one.
func monthForms(y, m string) []WrittenForm {
	mi, _ := strconv.Atoi(m)
	if mi < 1 || mi > 12 {
		return yearForms(y)
	}
	mm := strconv.Itoa(mi)
	var out []WrittenForm
	for _, t := range []string{y + m, y + "-" + m, y + "/" + m, y + "." + m, m + "/" + y, m + "." + y, m + "-" + y,
		y + "-" + mm, mm + "/" + y, months[mi-1] + " " + y, months[mi-1][:3] + " " + y, months[mi-1][:3] + ". " + y,
		y + "年" + mm + "月", y + "年" + m + "月", y + "년 " + mm + "월", y + "년" + mm + "월"} {
		out = append(out, WrittenForm{Text: t, Edge: EdgeDate})
	}
	return dedupe(out)
}

// dayForms returns the forms of a full date: the numeric ones as written
// whatever the digits, and the others of the year and month, or of the
// year, when the day, or the month, is not one.
func dayForms(y, m, d string) []WrittenForm {
	out := []WrittenForm{
		{Text: y + m + d}, {Text: y + "-" + m + "-" + d}, {Text: y + "/" + m + "/" + d}, {Text: y + "." + m + "." + d},
		{Text: d + "/" + m + "/" + y}, {Text: m + "/" + d + "/" + y}, {Text: d + "." + m + "." + y}, {Text: d + "-" + m + "-" + y},
	}
	mi, _ := strconv.Atoi(m)
	di, _ := strconv.Atoi(d)
	if mi < 1 || mi > 12 || di < 1 || di > 31 {
		return dedupe(append(out, monthForms(y, m)...))
	}
	out = append(out, WrittenForm{Text: d + m + y, Edge: EdgeDigits}, WrittenForm{Text: m + d + y, Edge: EdgeDigits})
	mm, dd, yy := strconv.Itoa(mi), strconv.Itoa(di), y[2:]
	bounded := []string{
		y + "-" + mm + "-" + dd, y + "/" + mm + "/" + dd, y + "." + mm + "." + dd,
		dd + "/" + mm + "/" + y, mm + "/" + dd + "/" + y, dd + "." + mm + "." + y, dd + "-" + mm + "-" + y,
		d + "/" + m + "/" + yy, m + "/" + d + "/" + yy, d + "." + m + "." + yy, d + "-" + m + "-" + yy,
		dd + "/" + mm + "/" + yy, mm + "/" + dd + "/" + yy, dd + "." + mm + "." + yy,
		y + "年" + mm + "月" + dd + "日", y + "年" + m + "月" + d + "日",
		y + "년 " + mm + "월 " + dd + "일", y + "년" + mm + "월" + dd + "일",
		y + ". " + mm + ". " + dd + ".", y + ". " + m + ". " + d + ".",
	}
	full, abbr := months[mi-1], months[mi-1][:3]
	for _, day := range []string{d, dd, dd + ordinal(di)} {
		for _, name := range []string{full, abbr, abbr + "."} {
			bounded = append(bounded, day+" "+name+" "+y, name+" "+day+", "+y, name+" "+day+" "+y)
		}
		bounded = append(bounded, day+" "+abbr+" "+yy)
	}
	for _, day := range []string{d, dd} {
		bounded = append(bounded, day+"-"+abbr+"-"+y, day+abbr+y)
	}
	for _, t := range bounded {
		out = append(out, WrittenForm{Text: t, Edge: EdgeDate})
	}
	return dedupe(out)
}

// ordinal returns the English ordinal suffix of a day: st, nd, rd or th.
func ordinal(d int) string {
	switch {
	case d >= 11 && d <= 13:
		return "th"
	case d%10 == 1:
		return "st"
	case d%10 == 2:
		return "nd"
	case d%10 == 3:
		return "rd"
	}
	return "th"
}

// dedupe drops forms whose text an earlier one has, keeping the first.
func dedupe(forms []WrittenForm) []WrittenForm {
	seen := make(map[string]bool, len(forms))
	out := forms[:0]
	for _, f := range forms {
		if !seen[f.Text] {
			seen[f.Text] = true
			out = append(out, f)
		}
	}
	return out
}

func isDigitByte(c byte) bool { return c >= '0' && c <= '9' }

// Unspaced reports whether s holds a letter of a script written without
// spaces between words, or whose names are a syllable or two (Han,
// Hiragana, Katakana, Hangul): a name part of it is a name at any length.
func Unspaced(s string) bool {
	for _, r := range s {
		if unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul) {
			return true
		}
	}
	return false
}

// Fold returns the rune every rune of r's case folding orbit maps to
// (unicode.SimpleFold), its least, after a full-width form of an ASCII
// character (U+FF01 to U+FF5E, written in East Asian text) is taken as
// that character: "K", "k", "ｋ" and the Kelvin sign are one rune, and
// "１２３" is "123". It maps a rune to a rune, so positions in a folded
// text are positions in the text.
func Fold(r rune) rune {
	if r >= 0xFF01 && r <= 0xFF5E {
		r -= 0xFEE0
	}
	if r < utf8.RuneSelf {
		if 'a' <= r && r <= 'z' {
			return r - 'a' + 'A'
		}
		return r
	}
	least := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		least = min(least, f)
	}
	return least
}
