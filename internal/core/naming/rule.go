package naming

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// The bounds of a rule and of what it reads. A template longer than 256
// bytes cannot make a label of 128; a pattern of 1 KiB is ample for any
// convention typed into a scanner and keeps the compiled program small; 64
// KiB holds the longest Patient Comments or Image Comments (LT, 10240
// characters, PS3.5 6.2) in any character set, and a value past it is
// refused, not cut.
const (
	MaxTemplateLength = 256
	MaxPatternLength  = 1024
	MaxValueLength    = 64 << 10
)

// Attributes gives the rule the attributes of one data set. The caller
// implements it over the data set it decoded.
type Attributes interface {
	// Lookup returns the value of the attribute with tag t as text, with
	// the padding PS3.5 6.2 says is not part of the value removed, and
	// whether the data set holds the attribute at all. A data set that
	// holds it with no value returns "" and true.
	Lookup(t Tag) (value string, present bool)
}

// Spec is a rule in its written form, as a person sets it on a receiver and
// the server stores it.
type Spec struct {
	// Subject, Session and Scan are the templates of the three labels.
	Subject, Session, Scan string
	// PatternAttribute names the attribute the pattern reads, by keyword
	// or tag; empty when the rule has no pattern.
	PatternAttribute string
	// Pattern is a regular expression with named groups (?P<name>...),
	// which must match the attribute's whole value; empty when the rule
	// has no pattern.
	Pattern string
}

// DefaultSpec returns the rule a receiver has until one is set: subject
// sub-{#SubjectIdentity}, session ses-{#StudyInstanceUID}, scan
// scan-{#SeriesInstanceUID}.
//
// The subject is the keyed hash of the file's subject identity
// (SubjectIdentity): its Patient ID with the issuer that gave it, or with
// the receiver's namespace when the file names none, since a Patient ID is
// unique only within its issuer (PS3.3 C.7.1.1), and two patients of two
// hospitals given one Patient ID must not become one subject. A file that
// names no issuer, through a receiver of the default namespace, has the
// label the keyed hash of its Patient ID alone gave before issuers were
// told apart (SubjectIdentity.Key).
//
// Every label is a keyed hash, so by default no label, and so no storage
// key, list or event that carries one, holds an identifier the sender
// gave: a Patient ID is often a hospital's record number, and a UID can be
// traced back to the site and the scanner that made it. The same value
// under the same tenant's key always gives the same label, so a late file
// lands where the first did. A site that wants readable labels writes its
// own rule, and its labels are then exactly as identifying as that rule.
//
// A default may lean only on attributes every instance must carry with a
// value: Study Instance UID and Series Instance UID are Type 1 (PS3.3
// C.7.2.1 and C.7.3.1), so a well-formed file always gives a session and a
// scan label. Study Date and Series Number are Type 2 and may be empty, and
// a default built on them would send whole studies to quarantine for no
// fault of the data. Patient ID is Type 2 as well (PS3.3 C.7.1.1), but a
// subject's label has to come from somewhere, and a file that names no
// patient is a real reason to hold it for a person.
func DefaultSpec() Spec {
	return Spec{
		Subject: "sub-{#" + IdentityPlaceholder + "}",
		Session: "ses-{#StudyInstanceUID}",
		Scan:    "scan-{#SeriesInstanceUID}",
	}
}

// Default returns the parsed DefaultSpec. It panics if DefaultSpec does not
// parse, which its test rules out.
func Default() *Rule {
	r, err := Parse(DefaultSpec())
	if err != nil {
		panic(err)
	}
	return r
}

// piece is a literal text or one placeholder of a template.
type piece struct {
	literal string // set for literal text only
	attr    string // the attribute as written, for an attribute placeholder
	tag     Tag
	uid     bool   // the attribute's value must also be a UID
	part    string // the part's name, for a part placeholder
	given   bool   // the part is given from outside the attributes, not the pattern's
	hash    bool   // the placeholder gives the keyed hash of the value
	ident   bool   // the placeholder is the subject identity (IdentityPlaceholder)
}

// template is one label's parsed template.
type template struct {
	level  string
	pieces []piece
}

// Rule is a parsed naming rule. It is not changed once made, so it is safe
// for concurrent use.
type Rule struct {
	spec      Spec
	templates [3]template
	patAttr   string
	patTag    Tag
	pattern   *regexp.Regexp
	namespace string
}

// Spec returns the written form the rule was parsed from.
func (r *Rule) Spec() Spec { return r.spec }

// WithNamespace returns the rule under the receiver's namespace ns (empty
// for the tenant's default): the namespace of the subject identity of a
// file that names no issuer (SubjectIdentity.Namespace). The rule itself is
// not changed.
func (r *Rule) WithNamespace(ns string) *Rule {
	out := *r
	out.namespace = ns
	return &out
}

// Namespace returns the namespace WithNamespace gave the rule.
func (r *Rule) Namespace() string { return r.namespace }

// SyntaxError is a rule refused as written. Its text may show the rule,
// which is configuration, never a value read from a file.
type SyntaxError struct {
	// Field is "subject", "session" or "scan" for a template, or
	// "pattern" for the pattern and its attribute.
	Field string
	// Offset is the byte offset in the field where the fault was found,
	// or -1 when the fault is not at one place.
	Offset int
	// Msg says what is wrong.
	Msg string
}

// Error returns the field, the offset and the fault.
func (e *SyntaxError) Error() string {
	s := "naming: the " + e.Field + " "
	if e.Field == "pattern" {
		s = "naming: the pattern"
	} else {
		s += "template"
	}
	if e.Offset >= 0 {
		s += " at offset " + strconv.Itoa(e.Offset)
	}
	return s + ": " + e.Msg
}

// Parse returns the rule written as s, or a *SyntaxError. Everything that
// can be checked without a file is checked here, so a rule that parses
// fails on a file only because of that file's values.
func Parse(s Spec) (*Rule, error) { return ParseWithParts(s, nil) }

// ParseWithParts is Parse for a rule whose templates may also use parts
// given from outside the file's attributes, named by given: the parts an
// import's layout takes from a file's path, and the one its map gives.
// A template part is then the pattern's when the pattern has a group of
// that name, else a given one; a name both the pattern and given hold is
// refused, since one placeholder would have two sources, and so is a
// template part neither has. That is how a rule that uses a part its
// receiver's layout never gives is refused when it is set, not on a file.
func ParseWithParts(s Spec, given []string) (*Rule, error) {
	r := &Rule{spec: s}
	parts, err := r.parsePattern(s.PatternAttribute, s.Pattern)
	if err != nil {
		return nil, err
	}
	gave, err := givenSet(given, parts)
	if err != nil {
		return nil, err
	}
	used := false
	for i, f := range []struct{ level, text string }{
		{"subject", s.Subject}, {"session", s.Session}, {"scan", s.Scan},
	} {
		t, err := parseTemplate(f.level, f.text)
		if err != nil {
			return nil, err
		}
		for j, p := range t.pieces {
			if p.part == "" {
				continue
			}
			if gave[p.part] {
				t.pieces[j].given = true
				continue
			}
			if r.pattern == nil {
				return nil, &SyntaxError{Field: f.level, Offset: -1, Msg: "{" + p.part +
					"} names a part of a pattern, and the rule has none (part names start with a lower case letter; attribute keywords with an upper case one)" + givenHint(given)}
			}
			if !parts[p.part] {
				return nil, &SyntaxError{Field: f.level, Offset: -1, Msg: "{" + p.part + "} is not a named group of the pattern" + givenHint(given)}
			}
			used = true
		}
		r.templates[i] = t
	}
	if r.pattern != nil && !used {
		return nil, &SyntaxError{Field: "pattern", Offset: -1, Msg: "no template uses a part of it"}
	}
	return r, nil
}

// parsePattern compiles the pattern and returns its part names.
func (r *Rule) parsePattern(attr, pattern string) (map[string]bool, error) {
	fail := func(off int, msg string) error { return &SyntaxError{Field: "pattern", Offset: off, Msg: msg} }
	switch {
	case attr == "" && pattern == "":
		return nil, nil
	case attr == "":
		return nil, fail(-1, "names no attribute to read")
	case pattern == "":
		return nil, fail(-1, "is empty, and an attribute is named for it")
	case len(pattern) > MaxPatternLength:
		return nil, fail(-1, "is longer than "+strconv.Itoa(MaxPatternLength)+" bytes")
	}
	tag, err := ParseAttribute(attr)
	if err != nil {
		return nil, fail(-1, "attribute "+strconv.Quote(attr)+" "+err.Error())
	}
	full, names, err := CompilePattern(pattern)
	if err != nil {
		return nil, fail(-1, err.Error())
	}
	parts := map[string]bool{}
	for _, n := range names {
		parts[n] = true
	}
	r.pattern = full
	r.patAttr, r.patTag = attr, tag
	return parts, nil
}

// parseTemplate parses one label's template.
func parseTemplate(level, text string) (template, error) {
	t := template{level: level}
	fail := func(off int, msg string) error { return &SyntaxError{Field: level, Offset: off, Msg: msg} }
	switch {
	case text == "":
		return t, fail(-1, "is empty")
	case len(text) > MaxTemplateLength:
		return t, fail(-1, "is longer than "+strconv.Itoa(MaxTemplateLength)+" bytes")
	}
	lit := 0
	for i := 0; i < len(text); i++ {
		switch c := text[i]; {
		case c == '{':
			if i > lit {
				t.pieces = append(t.pieces, piece{literal: text[lit:i]})
			}
			end := strings.IndexByte(text[i:], '}')
			if end < 0 {
				return t, fail(i, "'{' is not closed")
			}
			name := text[i+1 : i+end]
			if strings.IndexByte(name, '{') >= 0 {
				return t, fail(i, "'{' inside a placeholder")
			}
			p, err := parsePlaceholder(name)
			if err != nil {
				return t, fail(i, err.Error())
			}
			if p.ident && !p.hash {
				return t, fail(i, "{"+IdentityPlaceholder+"} is only hashed: the identity is bytes, not text a label holds; write {#"+IdentityPlaceholder+"}")
			}
			t.pieces = append(t.pieces, p)
			i += end
			lit = i + 1
		case c == '}':
			return t, fail(i, "'}' without '{'")
		case !label.Allowed(c):
			return t, fail(i, "literal text holds "+strconv.QuoteRune(rune(c))+
				"; a label holds only A-Z, a-z, 0-9, '.', '_' and '-'")
		}
	}
	if lit < len(text) {
		t.pieces = append(t.pieces, piece{literal: text[lit:]})
	}
	first, last := t.pieces[0], t.pieces[len(t.pieces)-1]
	switch {
	case len(t.pieces) == 1 && first.literal != "":
		// A constant: every label it makes is this one.
		if err := label.Check(first.literal); err != nil {
			return t, fail(-1, "is a constant that is not a valid label: "+err.Error())
		}
	case strings.HasPrefix(first.literal, "."):
		return t, fail(0, "starts with a dot, so no label it makes is valid")
	case strings.HasSuffix(last.literal, "."):
		return t, fail(len(text)-1, "ends with a dot, so no label it makes is valid")
	case strings.Contains(first.literal, ".") && label.IsDeviceName(first.literal):
		return t, fail(0, "starts with a name Windows reserves for a device, so no label it makes is valid")
	}
	return t, nil
}

// parsePlaceholder parses what is between a placeholder's braces: an
// attribute or a part, or either after '#' for its keyed hash.
func parsePlaceholder(name string) (piece, error) {
	if rest, ok := strings.CutPrefix(name, "#"); ok {
		if rest == "" {
			return piece{}, errorString("'{#}' names nothing to hash")
		}
		p, err := parsePlaceholder(rest)
		if err != nil || p.hash {
			if err == nil {
				err = errorString("{" + name + "} hashes twice")
			}
			return piece{}, err
		}
		p.hash = true
		return p, nil
	}
	switch {
	case name == "":
		return piece{}, errorString("empty placeholder '{}'")
	case partName(name):
		return piece{part: name}, nil
	case name == IdentityPlaceholder:
		return piece{attr: name, ident: true}, nil
	}
	tag, err := ParseAttribute(name)
	if err != nil {
		return piece{}, errorString("{" + name + "} " + err.Error())
	}
	return piece{attr: name, tag: tag, uid: isUID(name)}, nil
}

// partName reports whether s is a valid name for a part: a lower case
// letter, then lower case letters, digits and '_'. Attribute keywords start
// with an upper case letter, so a placeholder is one or the other by its
// first byte.
func partName(s string) bool {
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '_' {
			return false
		}
	}
	return true
}

type errorString string

func (e errorString) Error() string { return string(e) }

// Hashes reports whether the rule has a {#...} placeholder, and so needs a
// Hasher to make labels.
func (r *Rule) Hashes() bool {
	for _, t := range r.templates {
		for _, p := range t.pieces {
			if p.hash {
				return true
			}
		}
	}
	return false
}

// Plain returns the placeholders of the rule that put a value in a label as
// it is, not hashed, as they are written ("PatientID", or a pattern's part
// "site of PatientComments"), each once, in the order of the labels: a rule
// with any puts what the sender gave into labels, which a receiver that
// de-identifies must not.
func (r *Rule) Plain() []string {
	var out []string
	seen := map[string]bool{}
	for _, t := range r.templates {
		for _, p := range t.pieces {
			if p.literal != "" || p.hash {
				continue
			}
			name := p.attr
			switch {
			case p.given:
				name = p.part + " of " + GivenSource
			case p.part != "":
				name = p.part + " of " + r.patAttr
			}
			if !seen[name] {
				seen[name] = true
				out = append(out, name)
			}
		}
	}
	return out
}

// Labels returns the three labels the rule makes from a's attributes, or an
// *Error naming the label, the attribute and the fault. h gives the keyed
// hash of a value for each {#...} placeholder; it may be nil only for a rule
// without one (Hashes reports false), and a rule with one and no h is an
// error: a value is never put in a label unhashed because the hash was not
// at hand.
func (r *Rule) Labels(a Attributes, h Hasher) (label.Path, error) {
	return r.LabelsWithParts(a, nil, h)
}

// LabelsWithParts is Labels for a rule parsed by ParseWithParts: given
// holds the value of each given part, by name. A given part the rule uses
// and given lacks is an *Error with the fault Missing; its value is held to
// the same rules as an attribute's.
func (r *Rule) LabelsWithParts(a Attributes, given map[string]string, h Hasher) (label.Path, error) {
	var st renderState
	var out [3]string
	for i, t := range r.templates {
		v, err := r.render(t, a, given, h, &st)
		if err != nil {
			return label.Path{}, err
		}
		out[i] = v
	}
	return label.Path{Subject: out[0], Session: out[1], Scan: out[2]}, nil
}

// SubjectWithParts returns the subject label alone that LabelsWithParts
// makes from a's attributes and the given parts, or its *Error: the label
// one file would be filed under by itself, which a caller compares with
// the subject its session was named under.
func (r *Rule) SubjectWithParts(a Attributes, given map[string]string, h Hasher) (string, error) {
	var st renderState
	return r.render(r.templates[0], a, given, h, &st)
}

// renderState is what one file's labels read once: the pattern's
// submatches and the subject identity's key.
type renderState struct {
	parts []string
	ident []byte
}

// render makes one label of template t, held to the rule for a label.
func (r *Rule) render(t template, a Attributes, given map[string]string, h Hasher, st *renderState) (string, error) {
	var b strings.Builder
	var from []string
	for _, p := range t.pieces {
		switch {
		case p.literal != "":
			b.WriteString(p.literal)
		case p.given:
			v, ok := given[p.part]
			if !ok {
				return "", &Error{Level: t.level, Attribute: GivenSource, Part: p.part, Fault: Missing}
			}
			v, err := place(v, p, h)
			if err != nil {
				err.Level, err.Attribute, err.Part = t.level, GivenSource, p.part
				return "", err
			}
			b.WriteString(v)
			from = append(from, p.part+" of "+GivenSource)
		case p.part != "":
			if st.parts == nil {
				parts, err := r.match(t.level, a)
				if err != nil {
					return "", err
				}
				st.parts = parts
			}
			v, err := place(st.parts[r.pattern.SubexpIndex(p.part)], p, h)
			if err != nil {
				err.Level, err.Attribute, err.Part = t.level, r.patAttr, p.part
				return "", err
			}
			b.WriteString(v)
			from = append(from, p.part+" of "+r.patAttr)
		case p.ident:
			if st.ident == nil {
				id, err := readIdentity(a, r.namespace)
				if err != nil {
					err.Level = t.level
					return "", err
				}
				st.ident = id.Key()
			}
			v, err := place(string(st.ident), p, h)
			if err != nil {
				err.Level, err.Attribute = t.level, "PatientID"
				return "", err
			}
			b.WriteString(v)
			from = append(from, "PatientID", "IssuerOfPatientID")
		default:
			v, ok := a.Lookup(p.tag)
			if !ok {
				return "", &Error{Level: t.level, Attribute: p.attr, Fault: Missing}
			}
			v, err := place(v, p, h)
			if err != nil {
				err.Level, err.Attribute = t.level, p.attr
				return "", err
			}
			b.WriteString(v)
			from = append(from, p.attr)
		}
	}
	out := b.String()
	if err := label.Check(out); err != nil {
		return "", &Error{Level: t.level, Attribute: strings.Join(from, ", "), Fault: BadLabel, Err: err}
	}
	return out, nil
}

// match reads the pattern's attribute and returns its submatches.
func (r *Rule) match(level string, a Attributes) ([]string, error) {
	v, ok := a.Lookup(r.patTag)
	if !ok {
		return nil, &Error{Level: level, Attribute: r.patAttr, Fault: Missing}
	}
	switch {
	case v == "":
		return nil, &Error{Level: level, Attribute: r.patAttr, Fault: Empty}
	case len(v) > MaxValueLength:
		return nil, &Error{Level: level, Attribute: r.patAttr, Fault: TooLong}
	}
	m := r.pattern.FindStringSubmatch(v)
	if m == nil {
		return nil, &Error{Level: level, Attribute: r.patAttr, Fault: NoMatch}
	}
	return m, nil
}

// place returns what placeholder p puts in a label for value v: v itself,
// held to the rule for a label's characters, or for a hashed placeholder
// the hash of v. A hashed value need not be made of label characters, since
// it never reaches the label, but it must be present, not empty, within
// MaxValueLength, and a well-formed UID if its attribute is one: the empty
// string hashes to a label as well as any value, and would make every file
// without the attribute one subject.
func place(v string, p piece, h Hasher) (string, *Error) {
	if !p.hash {
		if err := checkValue(v, p.uid); err != nil {
			return "", err
		}
		return v, nil
	}
	switch {
	case v == "":
		return "", &Error{Fault: Empty}
	case len(v) > MaxValueLength:
		return "", &Error{Fault: TooLong}
	case h == nil:
		return "", &Error{Fault: NoHasher}
	case p.tag == TagPatientID && !p.ident && strings.IndexByte(v, 0) >= 0:
		// A Patient ID holding a NUL could spell another subject
		// identity's encoding (identityMark), so its hash could equal
		// another population's patient's code; LO text holds no NUL.
		return "", &Error{Fault: BadCharacter}
	}
	if p.uid {
		if err := uid.Check(v); err != nil {
			return "", &Error{Fault: BadUID, Err: err}
		}
	}
	s := h.Hash([]byte(v))
	if !isHashText(s) {
		return "", &Error{Fault: BadHash}
	}
	return s, nil
}

// checkValue holds one value to the rule for the characters of a label,
// before it is joined to the others, so the error names the attribute that
// broke it. The value's place in the label (first or last) is checked on
// the whole label.
func checkValue(v string, isUID bool) *Error {
	switch {
	case v == "":
		return &Error{Fault: Empty}
	case len(v) > label.MaxLength:
		return &Error{Fault: TooLong}
	}
	for i := 0; i < len(v); i++ {
		if !label.Allowed(v[i]) {
			return &Error{Fault: BadCharacter, Err: &label.Error{Value: v, Fault: label.BadCharacter, Index: i}}
		}
	}
	if isUID {
		if err := uid.Check(v); err != nil {
			return &Error{Fault: BadUID, Err: err}
		}
	}
	return nil
}

// HashedSubject returns the subject label the rule makes for a file whose
// attribute t has the value v, and true, when the subject template holds
// only literal text and keyed hashes of t ({#X} of t's keyword or tag);
// and false when it reads anything else (another attribute, a pattern's
// part, or a value as it is), whose label one value cannot decide. It is
// the label Labels makes for such a file, so a person who knows a
// subject's identifier finds the subject's label (the forward lookup),
// while nothing in the archive leads back. An *Error names a value the
// placeholder refuses, as Labels would.
//
// A {#SubjectIdentity} placeholder is decided too when t is Patient ID:
// issuer gives the Issuer of Patient ID and its qualifiers of the subject
// asked for (its PatientID and Namespace are not read; v is the Patient
// ID, and the namespace is the rule's, WithNamespace), so the label is the
// one a file of that Patient ID from that issuer, or from no issuer
// through this receiver, is given.
func (r *Rule) HashedSubject(t Tag, v string, issuer SubjectIdentity, h Hasher) (string, bool, error) {
	tmpl := r.templates[0]
	var b strings.Builder
	for _, p := range tmpl.pieces {
		switch {
		case p.literal != "":
			b.WriteString(p.literal)
		case p.ident && t == TagPatientID:
			id := issuer
			id.PatientID, id.Namespace = v, r.namespace
			if v == "" {
				return "", true, &Error{Level: tmpl.level, Attribute: "PatientID", Fault: Empty}
			}
			if strings.IndexByte(v, 0) >= 0 {
				return "", true, &Error{Level: tmpl.level, Attribute: "PatientID", Fault: BadCharacter}
			}
			s, err := place(string(id.Key()), p, h)
			if err != nil {
				err.Level, err.Attribute = tmpl.level, "PatientID"
				return "", true, err
			}
			b.WriteString(s)
		case p.hash && p.part == "" && !p.ident && p.tag == t:
			s, err := place(v, p, h)
			if err != nil {
				err.Level, err.Attribute = tmpl.level, p.attr
				return "", true, err
			}
			b.WriteString(s)
		default:
			return "", false, nil
		}
	}
	out := b.String()
	if err := label.Check(out); err != nil {
		return "", true, &Error{Level: tmpl.level, Attribute: t.String(), Fault: BadLabel, Err: err}
	}
	return out, true, nil
}
