package profile_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// valuePat is a value pattern selector.
func valuePat(p string, search bool, vrName string) *v1.ProfileSelector {
	return &v1.ProfileSelector{Kind: &v1.ProfileSelector_ValuePattern{ValuePattern: &v1.ValuePatternSelector{Pattern: p, Search: search, Vr: vrName}}}
}

// redactAct is a redact action.
func redactAct(p, repl string) *v1.ProfileAction {
	return &v1.ProfileAction{Kind: &v1.ProfileAction_Redact{Redact: &v1.RedactAction{Pattern: p, Replacement: repl}}}
}

// TestValuePatternsAndRedactValidate guards what the language accepts of a
// value pattern and of redact, and that each mistake is refused with a
// fixed code at its field: a pattern past the bounds, a VR a value pattern
// does not read, a search that matches the empty string, an action that
// does not fit the seven VRs, and a replacement that would break a name's
// structure, be cut again or not fit its VR.
func TestValuePatternsAndRedactValidate(t *testing.T) {
	cat := testCatalog(t)
	one := func(r *v1.ProfileRule) *v1.ProfileDocument { return &v1.ProfileDocument{Rules: []*v1.ProfileRule{r}} }
	for _, c := range []struct {
		name string
		doc  *v1.ProfileDocument
	}{
		{"whole value removed", one(rl("", valuePat(`MRN\d{6}`, false, ""), remove()))},
		{"search hashed", one(rl("", valuePat(`MRN\d{6}`, true, ""), hash()))},
		{"search of one VR cleaned", one(rl("", valuePat(`MRN`, true, "LO"), clean()))},
		{"private values too", one(rl("", &v1.ProfileSelector{Kind: valuePat(`x`, true, "").Kind, IncludePrivate: true}, remove()))},
		{"redact by value pattern", one(rl("", valuePat(`MRN\d{6}`, true, ""), redactAct(`MRN\d{6}`, "")))},
		{"redact a description", one(rl("", kw("StudyDescription"), redactAct(`MRN\d{6}`, "[MRN]")))},
		{"redact names by VR", one(rl("", vr("PN"), redactAct(`Dr\.?`, "")))},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, problems := profile.Expand(c.doc, cat); len(problems) > 0 {
				t.Errorf("refused: %v", problems)
			}
		})
	}
	for _, c := range []struct {
		name, field string
		code        profile.Code
		doc         *v1.ProfileDocument
	}{
		{"no pattern", "selector.valuePattern.pattern", profile.CodeRequired, one(rl("", valuePat("", false, ""), remove()))},
		{"pattern too long", "selector.valuePattern.pattern", profile.CodeBadPattern, one(rl("", valuePat(strings.Repeat("a", 257), false, ""), remove()))},
		{"repetition too large", "selector.valuePattern.pattern", profile.CodeBadPattern, one(rl("", valuePat(`x{101}`, true, ""), remove()))},
		{"not RE2", "selector.valuePattern.pattern", profile.CodeBadPattern, one(rl("", valuePat(`(?=x)`, true, ""), remove()))},
		{"search matching nothing", "selector.valuePattern.pattern", profile.CodeBadPattern, one(rl("", valuePat(`\d*`, true, ""), remove()))},
		{"a VR not read", "selector.valuePattern.vr", profile.CodeBadVR, one(rl("", valuePat(`x`, false, "CS"), remove()))},
		{"clean of a name", "action.clean", profile.CodeActionVR, one(rl("", valuePat(`x`, false, ""), clean()))},
		{"redact of a date", "action.redact", profile.CodeActionVR, one(rl("", vr("DA"), redactAct(`\d`, "")))},
		{"redact without a pattern", "action.redact.pattern", profile.CodeRequired, one(rl("", kw("StudyDescription"), redactAct("", "")))},
		{"redact matching the empty string", "action.redact.pattern", profile.CodeBadPattern, one(rl("", kw("StudyDescription"), redactAct(`\d*`, "")))},
		{"replacement with a caret in a name", "action.redact.replacement", profile.CodeBadValue, one(rl("", kw("ReferringPhysicianName"), redactAct(`Dr`, "^")))},
		{"replacement with a backslash", "action.redact.replacement", profile.CodeBadValue, one(rl("", kw("StudyDescription"), redactAct(`x`, `a\b`)))},
		{"replacement the pattern matches", "action.redact.replacement", profile.CodeBadValue, one(rl("", kw("StudyDescription"), redactAct(`\d+`, "000")))},
		{"replacement too long", "action.redact.replacement", profile.CodeTooLong, one(rl("", kw("StudyDescription"), redactAct(`x`, strings.Repeat("y", 65))))},
		{"replacement too long for its VR", "action.redact.replacement", profile.CodeBadValue, one(rl("", kw("StationName"), redactAct(`x`, strings.Repeat("y", 17))))},
		{"set by value pattern", "action.set", profile.CodeActionSelector, one(rl("", valuePat(`x`, false, ""), set("X")))},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, problems := profile.Expand(c.doc, cat)
			for _, p := range problems {
				if p.Field == c.field && p.Code == c.code {
					return
				}
			}
			t.Errorf("want %s at %s, got %v", c.code, c.field, problems)
		})
	}
}

// TestValuePatternEdition guards that a document using a value pattern or
// redact is expanded as language edition 2 and runs, that one using
// neither is still edition 1 with the checksum it always had (the shipped
// bases' are pinned in TestAnnexEBasesAreFrozen, a rich document's in
// richChecksum), and that an edition 1 form holding a value pattern, which
// no build made, does not compile.
func TestValuePatternEdition(t *testing.T) {
	cat := testCatalog(t)
	if e := mustExpand(t, richDocument(), cat); e.GetLanguageEdition() != 1 {
		t.Errorf("a document of edition 1's language is written as edition %d", e.GetLanguageEdition())
	}
	for _, r := range []*v1.ProfileRule{
		rl("", valuePat(`MRN\d{6}`, true, ""), remove()),
		rl("", kw("StudyDescription"), redactAct(`MRN\d{6}`, "")),
	} {
		e := mustExpand(t, &v1.ProfileDocument{Bases: []string{baseB}, Rules: []*v1.ProfileRule{r}}, cat)
		if e.GetLanguageEdition() != 2 {
			t.Errorf("a document with %v is written as edition %d", r, e.GetLanguageEdition())
		}
		if _, err := profile.Compile(e); err != nil {
			t.Errorf("edition 2 does not compile: %v", err)
		}
		e.LanguageEdition = 1
		if _, err := profile.Compile(e); err == nil || !strings.Contains(err.Error(), string(profile.CodeBadEdition)) {
			t.Errorf("an edition 1 form with %v compiled: %v", r, err)
		}
	}
	stored := mustExpand(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("", valuePat(`x`, false, "LO"), remove())}}, cat)
	if got := stored.GetRules()[0].GetVrs(); !slices.Equal(got, []string{"LO"}) {
		t.Errorf("a value pattern of one VR stores %v", got)
	}
	stored.GetRules()[0].Vrs = []string{"LO", "PN"}
	if _, err := profile.Compile(stored); err == nil {
		t.Error("a value pattern whose stored VRs differ from what it reads compiled")
	}
}

// TestValuePatternSelects guards what a value pattern selects: a whole
// value, or a match anywhere with search; each value of a multi-valued
// element apart; a name as written with its carets; nothing of another
// VR, of a private element without include_private, or of an empty value;
// a value the engine could not read is not selected, the decision saying
// so; matches are each text once, the longest at each place, each value
// trimmed; and more distinct matches than the bound are said to be.
func TestValuePatternSelects(t *testing.T) {
	cat := testCatalog(t)
	whole := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("w", valuePat(`MRN\d{6}`, false, ""), remove())}}, cat)
	search := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("s", valuePat(`MRN\d{6}`, true, ""), remove())}}, cat)
	name := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("n", valuePat(`DOE\^.*`, false, "PN"), remove())}}, cat)
	at := func(vrName, value string) profile.Location {
		return profile.Location{Tag: 0x00081030, VR: vrName, Value: value}
	}
	for _, c := range []struct {
		name    string
		p       *profile.Program
		loc     profile.Location
		want    bool
		matches []string
	}{
		{"whole value", whole, at("LO", "MRN123456"), true, []string{"MRN123456"}},
		{"whole value, inside text", whole, at("LO", "Head CT MRN123456"), false, nil},
		{"search inside text", search, at("LO", "Head CT MRN123456 follow-up"), true, []string{"MRN123456"}},
		{"search, two matches", search, at("LT", "MRN123456 and MRN654321"), true, []string{"MRN123456", "MRN654321"}},
		{"search, each text once", search, at("LT", "MRN123456 MRN123456 MRN654321"), true, []string{"MRN123456", "MRN654321"}},
		{"a padded value", whole, at("LO", `HEAD\ MRN123456 `), true, []string{"MRN123456"}},
		{"the second of two values", whole, at("LO", `CT\MRN123456`), true, []string{"MRN123456"}},
		{"a backslash in LT is text", whole, at("LT", `CT\MRN123456`), false, nil},
		{"a name with its caret", name, profile.Location{Tag: 0x00080090, VR: "PN", Value: "DOE^JOHN"}, true, []string{"DOE^JOHN"}},
		{"a VR not read", whole, at("CS", "MRN123456"), false, nil},
		{"empty", search, at("LO", ""), false, nil},
		{"private without include_private", search, profile.Location{Tag: 0x00191010, Creator: "ACME", VR: "LO", Value: "MRN123456"}, false, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			d := c.p.Decide(c.loc, facts{})
			if (d.Index == 0) != c.want {
				t.Fatalf("selected %v, want %v", d.Index == 0, c.want)
			}
			if !slices.Equal(d.PatternMatches, c.matches) {
				t.Errorf("matches %q, want %q", d.PatternMatches, c.matches)
			}
		})
	}
	long := at("UT", "")
	long.ValueUnread = true
	if d := search.Decide(long, facts{}); d.Index != -1 || !d.PatternUnread {
		t.Errorf("a value the engine could not read: rule %d, unread %v", d.Index, d.PatternUnread)
	}
	if d := search.Decide(profile.Location{Tag: 0x00080060, VR: "CS", ValueUnread: true}, facts{}); d.PatternUnread {
		t.Error("an unread value of a VR no value pattern reads is answered as unread")
	}
	var many strings.Builder
	for i := range profile.MaxRedactMatches + 1 {
		fmt.Fprintf(&many, "MRN%06d ", i)
	}
	if d := search.Decide(at("LT", many.String()), facts{}); !d.PatternOver || len(d.PatternMatches) != profile.MaxRedactMatches {
		t.Errorf("more distinct matches than the bound: over %v, %d matches", d.PatternOver, len(d.PatternMatches))
	}
	prefix := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("p", valuePat(`MRN|MRN\d+`, true, ""), remove())}}, cat)
	if d := prefix.Decide(at("LO", "x MRN123456"), facts{}); !slices.Equal(d.PatternMatches, []string{"MRN123456"}) {
		t.Errorf("a prefix alternation matched %q, want the longest", d.PatternMatches)
	}
}

// TestValuePatternPrecedence guards where a value pattern stands: a tag,
// a path and a group rule win over it, whatever their order, and it wins
// over a VR rule, whatever theirs. Explain's word for it is
// "value_pattern".
func TestValuePatternPrecedence(t *testing.T) {
	cat := testCatalog(t)
	vp := rl("vp", valuePat(`MRN\d{6}`, true, ""), remove())
	loc := profile.Location{Tag: 0x00081030, VR: "LO", Value: "Head CT MRN123456"}
	for _, c := range []struct {
		name  string
		rules []*v1.ProfileRule
		want  string
	}{
		{"tag over value pattern", []*v1.ProfileRule{rl("tag", kw("StudyDescription"), keep()), vp}, "tag"},
		{"tag over value pattern, either order", []*v1.ProfileRule{vp, rl("tag", kw("StudyDescription"), keep())}, "tag"},
		{"path over value pattern", []*v1.ProfileRule{vp, rl("path", path("StudyDescription"), keep())}, "path"},
		{"group over value pattern", []*v1.ProfileRule{vp, rl("group", grp("0008"), keep())}, "group"},
		{"value pattern over VR", []*v1.ProfileRule{vp, rl("vr", vr("LO"), keep())}, "vp"},
		{"value pattern over VR, either order", []*v1.ProfileRule{rl("vr", vr("LO"), keep()), vp}, "vp"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := mustCompile(t, &v1.ProfileDocument{Rules: c.rules}, cat)
			d := p.Decide(loc, facts{})
			if d.Rule == nil || d.Rule.GetRule().GetName() != c.want {
				t.Fatalf("decided by %v, want %s", d.Rule, c.want)
			}
		})
	}
	p := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{rl("vr", vr("LO"), keep()), vp}}, cat)
	m := p.Matches(loc, facts{})
	if len(m) != 2 || m[0].Tier.String() != "value_pattern" || m[1].Tier.String() != "vr" {
		t.Errorf("matches %+v, want the value pattern then the VR rule", m)
	}
}

// TestValuePatternIsFastOnAHostileValue guards that a pattern that
// backtracks exponentially in other engines stays fast on a value at the
// bound: RE2 is linear in the value. (Its constant is the pattern's
// compiled size, which the engine bounds per file.)
func TestValuePatternIsFastOnAHostileValue(t *testing.T) {
	p := mustCompile(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("hostile", valuePat(`(a|aa)+(a+)+b`, true, ""), remove()),
		rl("hostile-whole", valuePat(`(a*)*b`, false, ""), remove()),
	}}, testCatalog(t))
	value := strings.Repeat("a", profile.MaxPatternValue)
	start := time.Now()
	d := p.Decide(profile.Location{Tag: 0x0040A160, VR: "UT", Value: value}, facts{})
	if took := time.Since(start); took > time.Second {
		t.Errorf("deciding a %d-byte value took %v", len(value), took)
	}
	if d.Index != -1 {
		t.Errorf("rule %d selected a value it does not match", d.Index)
	}
}

// TestDiffShowsValuePatternsAndRedact guards that a comparison lists a
// value pattern rule added and a redact whose pattern changed, and the
// effect of each by its selector.
func TestDiffShowsValuePatternsAndRedact(t *testing.T) {
	cat := testCatalog(t)
	from := mustExpand(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("mrn", kw("StudyDescription"), redactAct(`MRN\d{6}`, "")),
	}}, cat)
	to := mustExpand(t, &v1.ProfileDocument{Rules: []*v1.ProfileRule{
		rl("mrn", kw("StudyDescription"), redactAct(`MRN\d{7}`, "")),
		rl("ids", valuePat(`ID\d+`, true, ""), remove()),
	}}, cat)
	d := profile.Diff(from, to)
	changes := map[string]v1.ProfileChange{}
	for _, r := range d.GetRules() {
		changes[r.GetTo().GetName()+r.GetFrom().GetName()] = r.GetChange()
	}
	if changes["mrnmrn"] != v1.ProfileChange_PROFILE_CHANGE_CHANGED || changes["ids"] != v1.ProfileChange_PROFILE_CHANGE_ADDED {
		t.Errorf("rule changes %v", changes)
	}
	var redacted, patterned bool
	for _, e := range d.GetEffects() {
		if e.GetTo().GetRedact().GetPattern() == `MRN\d{7}` && e.GetFrom().GetRedact().GetPattern() == `MRN\d{6}` {
			redacted = true
		}
		if e.GetSelector().GetValuePattern().GetPattern() == `ID\d+` && e.GetFrom() == nil {
			patterned = true
		}
	}
	if !redacted || !patterned || d.GetSameEffect() {
		t.Errorf("effects %v", d.GetEffects())
	}
}

// TestValuePatternWarnings guards the warnings a value pattern or redact
// gives over a base: a value pattern of the document that a base's tag
// rule overrides for some elements is shadowed, whatever the order; and
// redact on what a base removes keeps what it does not cut.
func TestValuePatternWarnings(t *testing.T) {
	cat := testCatalog(t)
	codes := func(doc *v1.ProfileDocument) map[profile.Code]string {
		v, err := profile.New("p", 1, doc, cat)
		if err != nil {
			t.Fatal(err)
		}
		ws, err := profile.Warnings(v.Expanded())
		if err != nil {
			t.Fatal(err)
		}
		out := map[profile.Code]string{}
		for _, w := range ws {
			out[w.Code] = w.Field
		}
		return out
	}
	shadow := codes(&v1.ProfileDocument{Bases: []string{baseA}, Rules: []*v1.ProfileRule{rl("", valuePat(`DOE`, true, "PN"), hash())}})
	if _, ok := shadow[profile.CodeShadowedByBase]; !ok {
		t.Errorf("a value pattern a base's tag rule overrides: %v", shadow)
	}
	keeps := codes(&v1.ProfileDocument{Bases: []string{baseA}, Rules: []*v1.ProfileRule{rl("", kw("PatientID"), redactAct(`\d+`, ""))}})
	if keeps[profile.CodeKeepsBaseChange] != "action.redact" {
		t.Errorf("redact of what a base removes: %v", keeps)
	}
}
