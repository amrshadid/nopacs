package naming

import (
	"errors"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// attrs is a data set for tests: tag to value; a tag absent from the map is
// missing.
type attrs map[Tag]string

func (a attrs) Lookup(t Tag) (string, bool) { v, ok := a[t]; return v, ok }

// kw returns the tag of a keyword, failing the test if it is unknown.
func kw(t *testing.T, k string) Tag {
	t.Helper()
	tag, err := ParseAttribute(k)
	if err != nil {
		t.Fatalf("%s: %v", k, err)
	}
	return tag
}

func studyAttrs(t *testing.T) attrs {
	return attrs{
		kw(t, "PatientID"):         "SUB-001",
		kw(t, "StudyDate"):         "20240131",
		kw(t, "StudyInstanceUID"):  "1.2.826.0.1.3680043.8.498.1",
		kw(t, "SeriesNumber"):      "3",
		kw(t, "SeriesInstanceUID"): "1.2.826.0.1.3680043.8.498.2",
		kw(t, "PatientComments"):   "Proj^Sub01^Visit2",
		kw(t, "PatientName"):       "Doe^Jane",
	}
}

// testKey is a hashing key for tests, 32 bytes.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// testHasher returns the Hasher of testKey.
func testHasher(t testing.TB) Hasher {
	t.Helper()
	h, err := NewHasher(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// defaultWant is the default rule's labels for studyAttrs under testKey,
// each hash computed outside Go with Python's hmac and base64 modules (and
// the SHA-256 HMAC of the Patient ID also with openssl dgst -sha256 -hmac).
var defaultWant = label.Path{Subject: "sub-3hsvpcs76azocuc3", Session: "ses-c5wqt65watttzhoo", Scan: "scan-v2wjualh6ydxyfox"}

// TestDefault guards the default rule's labels on a well-formed study, and
// that its written form survives Spec.
func TestDefault(t *testing.T) {
	got, err := Default().Labels(studyAttrs(t), testHasher(t))
	if err != nil {
		t.Fatal(err)
	}
	want := defaultWant
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if Default().Spec() != DefaultSpec() {
		t.Error("Spec does not return what was parsed")
	}
}

// TestDefaultIgnoresType2 guards that the default rule gives labels for a
// data set whose Study Date and Series Number, both Type 2 (PS3.3 C.7.2.1,
// C.7.3.1), are empty or missing: a default must not quarantine a
// well-formed study.
func TestDefaultIgnoresType2(t *testing.T) {
	for _, missing := range []bool{false, true} {
		a := studyAttrs(t)
		for _, k := range []string{"StudyDate", "SeriesNumber"} {
			if missing {
				delete(a, kw(t, k))
			} else {
				a[kw(t, k)] = ""
			}
		}
		got, err := Default().Labels(a, testHasher(t))
		if err != nil || got != defaultWant {
			t.Errorf("missing %v: got %+v, %v", missing, got, err)
		}
	}
}

// TestKeywordTags guards the keyword table against PS3.6, entry by entry,
// as pydicom's data dictionary gives it; a wrong tag would name labels from
// the wrong attribute without any other test noticing.
func TestKeywordTags(t *testing.T) {
	want := map[string]string{
		"PatientID": "(0010,0020)", "PatientName": "(0010,0010)", "IssuerOfPatientID": "(0010,0021)",
		"PatientBirthDate": "(0010,0030)", "PatientSex": "(0010,0040)", "PatientAge": "(0010,1010)",
		"PatientComments": "(0010,4000)", "StudyInstanceUID": "(0020,000D)", "StudyID": "(0020,0010)",
		"StudyDate": "(0008,0020)", "StudyTime": "(0008,0030)", "StudyDescription": "(0008,1030)",
		"AccessionNumber": "(0008,0050)", "RequestedProcedureID": "(0040,1001)",
		"SeriesInstanceUID": "(0020,000E)", "SeriesNumber": "(0020,0011)", "SeriesDate": "(0008,0021)",
		"SeriesTime": "(0008,0031)", "SeriesDescription": "(0008,103E)", "ProtocolName": "(0018,1030)",
		"BodyPartExamined": "(0018,0015)", "Modality": "(0008,0060)", "AcquisitionDate": "(0008,0022)",
		"AcquisitionTime": "(0008,0032)", "AcquisitionNumber": "(0020,0012)", "ContentDate": "(0008,0023)",
		"InstanceNumber": "(0020,0013)", "ImageComments": "(0020,4000)", "SOPInstanceUID": "(0008,0018)",
		"SOPClassUID": "(0008,0016)", "FrameOfReferenceUID": "(0020,0052)", "InstitutionName": "(0008,0080)",
		"InstitutionalDepartmentName": "(0008,1040)", "StationName": "(0008,1010)",
		"Manufacturer": "(0008,0070)", "ManufacturerModelName": "(0008,1090)",
		"ClinicalTrialSponsorName": "(0012,0010)", "ClinicalTrialProtocolID": "(0012,0020)",
		"ClinicalTrialSiteID": "(0012,0030)", "ClinicalTrialSubjectID": "(0012,0040)",
		"ClinicalTrialTimePointID": "(0012,0050)", "ClinicalTrialSeriesID": "(0012,0071)",
	}
	got := Keywords()
	if len(got) != len(want) {
		t.Errorf("%d keywords, want %d", len(got), len(want))
	}
	for k, tag := range got {
		if want[k] != tag.String() {
			t.Errorf("%s is %s, want %s", k, tag, want[k])
		}
	}
	for _, k := range []string{"StudyInstanceUID", "SeriesInstanceUID", "SOPInstanceUID", "SOPClassUID", "FrameOfReferenceUID"} {
		if !isUID(k) {
			t.Errorf("%s not marked as a UID", k)
		}
	}
}

// TestParseAttribute guards the tag form and the tags it refuses.
func TestParseAttribute(t *testing.T) {
	for in, want := range map[string]Tag{
		"(0010,4000)": 0x00104000, "(0019,10ab)": 0x001910AB, "(0008,0020)": 0x00080020, "(FFFC,FFFC)": 0xFFFCFFFC,
	} {
		if got, err := ParseAttribute(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{
		"", "patientID", "PatientId", "(0010,400)", "(0010,40000)", "0010,4000", "(0010 4000)",
		"(001g,4000)", "(+010,4000)", "(0002,0010)", "(0000,0100)", "(0007,0010)", "(FFFE,E000)",
		"(7FE0,0010)", "(0010,0000)", "[0010,4000]",
	} {
		if _, err := ParseAttribute(in); err == nil {
			t.Errorf("%q accepted", in)
		}
	}
	if NewTag(0x7fe0, 0x10).String() != "(7FE0,0010)" {
		t.Error(NewTag(0x7fe0, 0x10).String())
	}
}

// TestParseRefuses guards every check made when a rule is set, so a rule
// that cannot work is refused before any file arrives.
func TestParseRefuses(t *testing.T) {
	d := DefaultSpec()
	with := func(f func(*Spec)) Spec { s := d; f(&s); return s }
	pat := func(attr, p, subject string) Spec {
		return with(func(s *Spec) { s.PatternAttribute, s.Pattern, s.Subject = attr, p, subject })
	}
	for name, s := range map[string]Spec{
		"empty template":       with(func(s *Spec) { s.Subject = "" }),
		"long template":        with(func(s *Spec) { s.Scan = strings.Repeat("{PatientID}", 30) }),
		"unclosed":             with(func(s *Spec) { s.Subject = "{PatientID" }),
		"stray close":          with(func(s *Spec) { s.Subject = "PatientID}" }),
		"nested":               with(func(s *Spec) { s.Subject = "{{PatientID}}" }),
		"empty placeholder":    with(func(s *Spec) { s.Subject = "x{}" }),
		"unknown keyword":      with(func(s *Spec) { s.Subject = "{PatientFavoriteColor}" }),
		"bad tag":              with(func(s *Spec) { s.Subject = "{(0010,00ZZ)}" }),
		"meta tag":             with(func(s *Spec) { s.Subject = "{(0002,0003)}" }),
		"space in literal":     with(func(s *Spec) { s.Subject = "sub {PatientID}" }),
		"slash in literal":     with(func(s *Spec) { s.Subject = "a/{PatientID}" }),
		"leading dot":          with(func(s *Spec) { s.Subject = ".{PatientID}" }),
		"trailing dot":         with(func(s *Spec) { s.Subject = "{PatientID}." }),
		"device prefix":        with(func(s *Spec) { s.Subject = "nul.{PatientID}" }),
		"device constant":      with(func(s *Spec) { s.Subject = "CON" }),
		"long constant":        with(func(s *Spec) { s.Subject = strings.Repeat("a", 129) }),
		"part with no pattern": with(func(s *Spec) { s.Subject = "{subject}" }),
		"pattern without attr": pat("", "(?P<s>.*)", "{s}"),
		"attr without pattern": pat("PatientComments", "", "{PatientID}"),
		"pattern bad attr":     pat("Nope", "(?P<s>.*)", "{s}"),
		"pattern bad syntax":   pat("PatientComments", "(?P<s>.*", "{s}"),
		"pattern unbalanced":   pat("PatientComments", "a)|(?P<s>b", "{s}"),
		"pattern no group":     pat("PatientComments", ".*", "{PatientID}"),
		"pattern upper group":  pat("PatientComments", "(?P<Sub>.*)", "{PatientID}"),
		"pattern dup group":    pat("PatientComments", "(?P<s>a)|(?P<s>b)", "{s}"),
		"pattern unused":       pat("PatientComments", "(?P<s>.*)", "{PatientID}"),
		"unknown part":         pat("PatientComments", "(?P<s>.*)", "{t}"),
		"pattern too long":     pat("PatientComments", "(?P<s>"+strings.Repeat("a", MaxPatternLength)+")", "{s}"),
		"pattern huge repeat":  pat("PatientComments", "(?P<s>a{1001})", "{s}"),
	} {
		_, err := Parse(s)
		var se *SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("%s: got %v, want a *SyntaxError", name, err)
		}
	}
}

// TestParseAccepts guards forms a rule may take: constants, literal text,
// tags, adjacent placeholders, and a pattern used by several labels.
func TestParseAccepts(t *testing.T) {
	for _, s := range []Spec{
		{Subject: "phantom", Session: "{StudyDate}-{StudyInstanceUID}", Scan: "{SeriesNumber}{(0020,000E)}"},
		{Subject: "s_{(0010,0020)}.v1", Session: "{StudyID}_{StudyDate}", Scan: "{SeriesInstanceUID}"},
		{Subject: "CON{PatientID}", Session: "{StudyInstanceUID}", Scan: "{SeriesInstanceUID}"},
		{
			Subject: "{project}-{subject}", Session: "{visit}-{StudyInstanceUID}", Scan: "{SeriesInstanceUID}",
			PatternAttribute: "(0010,4000)", Pattern: `(?P<project>[^^]+)\^(?P<subject>[^^]+)\^(?P<visit>[^^]+)`,
		},
	} {
		if _, err := Parse(s); err != nil {
			t.Errorf("%+v: %v", s, err)
		}
	}
}

// TestPattern guards the pattern form: whole-value match, parts in labels,
// and the faults of the attribute and of a part.
func TestPattern(t *testing.T) {
	r, err := Parse(Spec{
		Subject: "{subject}", Session: "{visit}-{StudyInstanceUID}", Scan: "{SeriesNumber}",
		PatternAttribute: "PatientComments",
		Pattern:          `(?m)^(?P<project>[A-Za-z0-9]+)\^(?P<subject>[A-Za-z0-9 ]+)\^(?P<visit>[A-Za-z0-9]*)$`,
	})
	if err != nil {
		t.Fatal(err)
	}
	a := studyAttrs(t)
	got, err := r.Labels(a, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != "Sub01" || got.Session != "Visit2-1.2.826.0.1.3680043.8.498.1" || got.Scan != "3" {
		t.Fatalf("%+v", got)
	}
	tag := kw(t, "PatientComments")
	for v, want := range map[string]Fault{
		"Proj^Sub01^Visit2^extra":             NoMatch,
		"x\nProj^Sub01^Visit2":                NoMatch, // (?m) must not let a later line match
		"Proj^Sub01^Visit2\n":                 NoMatch,
		"Proj^Sub 01^Visit2":                  BadCharacter,
		"Proj^Sub01^":                         Empty,
		"":                                    Empty,
		strings.Repeat("a", MaxValueLength+1): TooLong,
	} {
		b := attrs{}
		for k, x := range a {
			b[k] = x
		}
		b[tag] = v
		_, err := r.Labels(b, nil)
		var ne *Error
		if !errors.As(err, &ne) || ne.Fault != want || ne.Attribute != "PatientComments" {
			t.Errorf("%.40q: got %v, want %v", v, err, want)
		}
	}
	delete(a, tag)
	if _, err := r.Labels(a, nil); !isFault(err, Missing) {
		t.Errorf("missing: %v", err)
	}
}

func isFault(err error, f Fault) bool {
	var ne *Error
	return errors.As(err, &ne) && ne.Fault == f
}

// TestLabelsFaults guards each fault of an attribute in a template, that the
// error names the label and the attribute, and that its text never carries
// the value. It uses a rule that also reads Study Date and Series Number, so
// the faults of a date and a number are covered too.
func TestLabelsFaults(t *testing.T) {
	dated, err := Parse(Spec{
		Subject: "{PatientID}",
		Session: "{StudyDate}-{StudyInstanceUID}",
		Scan:    "{SeriesNumber}-{SeriesInstanceUID}",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		key   string
		value string // "<missing>" removes it
		level string
		fault Fault
	}{
		{"missing", "PatientID", "<missing>", "subject", Missing},
		{"empty", "PatientID", "", "subject", Empty},
		{"space", "PatientID", "Jane Doe", "subject", BadCharacter},
		{"caret", "PatientID", "Doe^Jane", "subject", BadCharacter},
		{"slash", "PatientID", "Jane/Doe", "subject", BadCharacter},
		{"too long", "PatientID", "Jane" + strings.Repeat("x", 125), "subject", TooLong},
		{"device", "PatientID", "con", "subject", BadLabel},
		{"leading dot", "PatientID", ".JaneDoe", "subject", BadLabel},
		{"trailing dot", "PatientID", "JaneDoe.", "subject", BadLabel},
		{"date empty", "StudyDate", "", "session", Empty},
		{"bad uid", "StudyInstanceUID", "1.02.3", "session", BadUID},
		{"uid padded", "SeriesInstanceUID", "1.2.3\x00", "scan", BadCharacter},
		{"number signed", "SeriesNumber", "+3", "scan", BadCharacter},
		{"number padded", "SeriesNumber", " 3", "scan", BadCharacter},
	} {
		a := studyAttrs(t)
		if c.value == "<missing>" {
			delete(a, kw(t, c.key))
		} else {
			a[kw(t, c.key)] = c.value
		}
		_, err := dated.Labels(a, nil)
		var ne *Error
		if !errors.As(err, &ne) || ne.Fault != c.fault || ne.Level != c.level || !strings.Contains(ne.Attribute, c.key) {
			t.Errorf("%s: got %#v (%v)", c.name, ne, err)
			continue
		}
		if c.value != "" && len(c.value) > 2 && strings.Contains(err.Error(), c.value) {
			t.Errorf("%s: text %q carries the value", c.name, err)
		}
		if strings.Contains(err.Error(), "Jane") {
			t.Errorf("%s: text %q carries the value", c.name, err)
		}
	}
	// A bad UID's fault is reachable with errors.Is.
	a := studyAttrs(t)
	a[kw(t, "StudyInstanceUID")] = "1..2"
	if _, err := Default().Labels(a, testHasher(t)); !errors.Is(err, uid.ErrComponent) {
		t.Errorf("uid fault not wrapped: %v", err)
	}
	// Two values that are each valid can join into a label that is too long.
	long := strings.Repeat("1", 64)
	r, _ := Parse(Spec{Subject: "{PatientID}{AccessionNumber}", Session: "{StudyInstanceUID}", Scan: "{SeriesInstanceUID}"})
	a = studyAttrs(t)
	a[kw(t, "PatientID")], a[kw(t, "AccessionNumber")] = long, long+"1"
	if _, err := r.Labels(a, nil); !isFault(err, BadLabel) {
		t.Errorf("joined too long: %v", err)
	}
}

// TestPatternAnchorsHold guards that the anchors around a pattern cannot be
// quoted away by a trailing \Q or escaped by an unbalanced pattern: such a
// pattern is either refused or still matches whole values only.
func TestPatternAnchorsHold(t *testing.T) {
	r, err := Parse(Spec{Subject: "{s}", Session: "{StudyInstanceUID}", Scan: "{SeriesInstanceUID}",
		PatternAttribute: "PatientComments", Pattern: `(?P<s>a+)\Q`})
	if err != nil {
		t.Fatal(err)
	}
	a := studyAttrs(t)
	tag := kw(t, "PatientComments")
	a[tag] = "aa"
	if p, err := r.Labels(a, nil); err != nil || p.Subject != "aa" {
		t.Fatalf("%+v %v", p, err)
	}
	a[tag] = "aab"
	if _, err := r.Labels(a, nil); !isFault(err, NoMatch) {
		t.Fatalf("a value with more than the pattern matched: %v", err)
	}
}

// FuzzTemplate guards the template parser: it never panics, and a rule it
// accepts makes, from any values, either an error or three valid labels.
func FuzzTemplate(f *testing.F) {
	for _, s := range []string{"{PatientID}", "{StudyDate}-{StudyInstanceUID}", "a{(0010,4000)}b", "{", "}", "{{}}",
		".{PatientID}", "nul.{PatientID}", "CON", "x{PatientID}.", "{(7FE0,0010)}", "{sub}",
		"sub-{#PatientID}", "{#(0010,4000)}", "{#}", "{##PatientID}", "{#sub}", "a{#StudyInstanceUID}.", strings.Repeat("{#PatientID}", 8)} {
		f.Add(s, "SUB-1", "20240101")
	}
	f.Fuzz(func(t *testing.T, tmpl, v1, v2 string) {
		r, err := Parse(Spec{Subject: tmpl, Session: "{StudyInstanceUID}", Scan: "{SeriesInstanceUID}"})
		if err != nil {
			return
		}
		a := anyValues{v1, v2}
		p, err := r.Labels(a, testHasher(t))
		if err == nil && p.Check() != nil {
			t.Fatalf("template %q made invalid labels %+v", tmpl, p)
		}
	})
}

// anyValues answers every tag: UIDs with a fixed valid UID, the rest with
// two fuzzed values in turn by tag parity.
type anyValues [2]string

func (a anyValues) Lookup(t Tag) (string, bool) {
	switch t {
	case 0x0020000D, 0x0020000E:
		return "1.2.3", true
	}
	return a[t&1], true
}

// FuzzPattern guards the pattern parser and matcher: they never panic, and
// a rule they accept makes, from any value, either an error or three valid
// labels.
func FuzzPattern(f *testing.F) {
	for _, s := range []string{`(?P<s>.*)`, `(?P<s>[^^]+)\^(?P<t>.*)`, `(?m)(?P<s>^a$)`, `a)|(?P<s>b`, `(?P<s>a{1000})`, `(?P<s>(a*)*)`, `(?<a>)\Q`, `(?P<s>a)\Q)`} {
		f.Add(s, "Proj^Sub01")
	}
	f.Fuzz(func(t *testing.T, pattern, value string) {
		r, err := Parse(Spec{Subject: "{s}", Session: "{#s}", Scan: "{SeriesInstanceUID}",
			PatternAttribute: "PatientComments", Pattern: pattern})
		if err != nil {
			return
		}
		p, err := r.Labels(anyValues{value, value}, testHasher(t))
		if err == nil && p.Check() != nil {
			t.Fatalf("pattern %q made invalid labels %+v", pattern, p)
		}
	})
}

// TestPlain guards the list of placeholders that put a value in a label as
// it is: none for the default rule, whose every placeholder is hashed; each
// plain attribute once, and a pattern's part by its name and attribute.
func TestPlain(t *testing.T) {
	if got := Default().Plain(); len(got) != 0 {
		t.Errorf("the default rule: %v", got)
	}
	r, err := Parse(Spec{Subject: "sub-{PatientID}", Session: "ses-{#StudyInstanceUID}-{PatientID}", Scan: "scan-{site}",
		PatternAttribute: "PatientComments", Pattern: `(?P<site>[a-z]+)`})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Plain(); len(got) != 2 || got[0] != "PatientID" || got[1] != "site of PatientComments" {
		t.Errorf("plain placeholders %v", got)
	}
}
