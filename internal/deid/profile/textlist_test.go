package profile_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/profile"
	"github.com/amrshadid/nopacs/internal/deid/textmatch"
)

// seededTerm is a term no other text of the tests holds, searched for in
// every printed form a list has.
const seededTerm = "Zebulonquist"

// mustLists compiles lists, failing the test when they are refused.
func mustLists(t testing.TB, lists ...profile.TextList) *profile.TextLists {
	t.Helper()
	l, problems := profile.CompileTextLists(lists)
	if problems != nil {
		t.Fatalf("compiling the lists: %v", problems)
	}
	return l
}

// cut returns text with every span Find gives for mask taken out.
func cut(t *testing.T, l *profile.TextLists, mask uint32, text string) string {
	t.Helper()
	runes := []rune(profile.NFC(text))
	budget := int64(1 << 20)
	spans, err := l.Find(context.Background(), runes, mask, &budget)
	if err != nil {
		t.Fatal(err)
	}
	drop := make([]bool, len(runes))
	for _, s := range spans {
		for i := s[0]; i < s[1]; i++ {
			drop[i] = true
		}
	}
	var b strings.Builder
	for i, r := range runes {
		if !drop[i] {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// TestListTermsStandAsWords guards how a list's term is matched: only as a
// whole word where it ends in a letter or digit ("Ann" not in "Annual",
// "Lee" not in "Leeds"), at any edge where it ends in punctuation
// ("Dr." before a name); in any case, width and Unicode form (a name
// written with a combining mark, full-width letters, a run of white
// space for the term's one space); and anywhere for a term of a script
// written without spaces.
func TestListTermsStandAsWords(t *testing.T) {
	l := mustLists(t,
		profile.TextList{Name: "staff", Version: 1, Terms: []string{"Ann", "Lee", "Müller", "John  Smith", "山田"}},
		profile.TextList{Name: "titles", Version: 2, Terms: []string{"Dr."}},
	)
	all := l.All()
	for _, c := range []struct{ in, want string }{
		{"Annual review by Ann, Leeds site", "Annual review by , Leeds site"},
		{"seen by ANN and lee", "seen by  and "},
		{"Dr.Ann", ""},
		{"Dr.X", "X"},
		{"read by Müller", "read by "},
		{"read by ＭÜＬＬＥＲ", "read by "},
		{"john\tsmith reported", " reported"},
		{"Johnsmith", "Johnsmith"},
		{"山田太郎の頭部", "太郎の頭部"},
		{"Ann2 and 2Ann", "Ann2 and 2Ann"},
		{"Ann_x", "_x"},
	} {
		if got := cut(t, l, all, c.in); got != c.want {
			t.Errorf("%q: %q, want %q", c.in, got, c.want)
		}
	}
	staff, _ := l.Mask([]string{"staff"})
	if got := cut(t, l, staff, "Dr. Lee"); got != "Dr. " {
		t.Errorf("only the staff list: %q", got)
	}
	if _, ok := l.Mask([]string{"nurses"}); ok {
		t.Error("a list the profile does not have has a mask")
	}
}

// TestListChecksumIsOfFoldedTerms guards a list's summary: its name,
// version, count and the SHA-256 of its folded terms, the same for terms
// written in another case, width, Unicode form, white space or order, and
// different for other terms.
func TestListChecksumIsOfFoldedTerms(t *testing.T) {
	a := mustLists(t, profile.TextList{Name: "staff", Version: 3, Terms: []string{"Müller", "John Smith"}})
	b := mustLists(t, profile.TextList{Name: "staff", Version: 3, Terms: []string{"  JOHN smith ", "MÜLLER"}})
	c := mustLists(t, profile.TextList{Name: "staff", Version: 3, Terms: []string{"Müller", "John Smyth"}})
	sa, sb, sc := a.Summaries()[0], b.Summaries()[0], c.Summaries()[0]
	if sa.SHA256 != sb.SHA256 || len(sa.SHA256) != 64 {
		t.Errorf("one list written two ways: %s and %s", sa.SHA256, sb.SHA256)
	}
	if sa.SHA256 == sc.SHA256 {
		t.Error("two lists of other terms share a checksum")
	}
	if sa.Name != "staff" || sa.Version != 3 || sa.Terms != 2 {
		t.Errorf("summary %+v", sa)
	}
}

// TestListBoundsRefuse guards that a list past a bound, or with a term it
// cannot hold, is refused whole with a fixed code at the term's position,
// and that no problem's text holds the term.
func TestListBoundsRefuse(t *testing.T) {
	many := func(n int, prefix string) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("%s%05d", prefix, i)
		}
		return out
	}
	lists := func(n int) []profile.TextList {
		out := make([]profile.TextList, n)
		for i := range out {
			out[i] = profile.TextList{Name: fmt.Sprintf("l%d", i), Version: 1, Terms: []string{"term"}}
		}
		return out
	}
	for _, c := range []struct {
		name  string
		lists []profile.TextList
		code  profile.Code
		field string
	}{
		{"too many terms", []profile.TextList{{Name: "a", Version: 1, Terms: many(profile.MaxListTerms+1, "t")}}, profile.CodeTooMany, "textLists[1].terms"},
		{"too many lists", lists(profile.MaxTextLists + 1), profile.CodeTooMany, "textLists"},
		{"too many terms together", []profile.TextList{
			{Name: "a", Version: 1, Terms: many(profile.MaxListTerms, "a")},
			{Name: "b", Version: 1, Terms: many(profile.MaxListTerms, "b")},
			{Name: "c", Version: 1, Terms: []string{"one more"}},
		}, profile.CodeTooMany, "textLists[3].terms"},
		{"no term", []profile.TextList{{Name: "a", Version: 1}}, profile.CodeRequired, "textLists[1].terms"},
		{"bad name", []profile.TextList{{Name: "Staff", Version: 1, Terms: []string{"term"}}}, profile.CodeBadName, "textLists[1].name"},
		{"same name", []profile.TextList{{Name: "a", Version: 1, Terms: []string{"term"}}, {Name: "a", Version: 1, Terms: []string{"term"}}}, profile.CodeDuplicateName, "textLists[2].name"},
		{"no version", []profile.TextList{{Name: "a", Terms: []string{"term"}}}, profile.CodeOutOfRange, "textLists[1].version"},
		{"long term", []profile.TextList{{Name: "a", Version: 1, Terms: []string{"xy", strings.Repeat(seededTerm, 30)}}}, profile.CodeTooLong, "textLists[1].terms[2]"},
		{"backslash", []profile.TextList{{Name: "a", Version: 1, Terms: []string{seededTerm + `\x`}}}, profile.CodeBadTerm, "textLists[1].terms[1]"},
		{"control", []profile.TextList{{Name: "a", Version: 1, Terms: []string{seededTerm + "\n"}}}, profile.CodeBadTerm, "textLists[1].terms[1]"},
		{"blank", []profile.TextList{{Name: "a", Version: 1, Terms: []string{" 　 "}}}, profile.CodeBadTerm, "textLists[1].terms[1]"},
		{"short", []profile.TextList{{Name: "a", Version: 1, Terms: []string{"Z"}}}, profile.CodeBadTerm, "textLists[1].terms[1]"},
		{"not UTF-8", []profile.TextList{{Name: "a", Version: 1, Terms: []string{seededTerm + "\xff"}}}, profile.CodeBadTerm, "textLists[1].terms[1]"},
		{"twice", []profile.TextList{{Name: "a", Version: 1, Terms: []string{seededTerm, strings.ToUpper(seededTerm)}}}, profile.CodeDuplicateTerm, "textLists[1].terms[2]"},
	} {
		t.Run(c.name, func(t *testing.T) {
			l, problems := profile.CompileTextLists(c.lists)
			if l != nil || len(problems) == 0 {
				t.Fatal("refused nothing")
			}
			p := problems[0]
			if p.Code != c.code || p.Field != c.field {
				t.Errorf("problem %s at %s, want %s at %s", p.Code, p.Field, c.code, c.field)
			}
			if strings.Contains(strings.ToLower(problems.Error()+fmt.Sprint(problems)), strings.ToLower(seededTerm)) {
				t.Errorf("a problem prints the term: %v", problems)
			}
		})
	}
	// A unspaced term of one character is a name.
	mustLists(t, profile.TextList{Name: "a", Version: 1, Terms: []string{"林"}})
	// The full bound fits.
	mustLists(t, profile.TextList{Name: "a", Version: 1, Terms: many(profile.MaxListTerms, "term")})
}

// TestListsPrintNoTerm guards every printed form of a list and of the
// compiled lists, with every verb: its name, version and count only.
func TestListsPrintNoTerm(t *testing.T) {
	list := profile.TextList{Name: "staff", Version: 1, Terms: []string{seededTerm, "Other Name"}}
	l := mustLists(t, list)
	var out []string
	for _, v := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x"} {
		out = append(out, fmt.Sprintf(v, list), fmt.Sprintf(v, l), fmt.Sprintf(v, []profile.TextList{list}), fmt.Sprintf(v, l.Summaries()))
	}
	out = append(out, list.String(), l.String())
	for _, s := range out {
		if strings.Contains(strings.ToLower(s), strings.ToLower(seededTerm)) || strings.Contains(s, "5A656275") {
			t.Fatalf("printed a term: %s", s)
		}
	}
	if !strings.Contains(l.String(), "staff v1 (2 terms)") {
		t.Errorf("the lists print as %q", l.String())
	}
}

// TestFindIsBounded guards Find's budget: a text whose every position
// ends a term is refused once the budget is spent, never searched without
// bound.
func TestFindIsBounded(t *testing.T) {
	terms := make([]string, 0, 64)
	for n := 2; n < 66; n++ {
		terms = append(terms, strings.Repeat("a", n))
	}
	l := mustLists(t, profile.TextList{Name: "a", Version: 1, Terms: terms})
	budget := int64(1000)
	_, err := l.Find(context.Background(), []rune(strings.Repeat("a", 10000)), l.All(), &budget)
	if !errors.As(err, new(textmatch.ErrBudget)) {
		t.Errorf("an unbounded search: %v", err)
	}
}
