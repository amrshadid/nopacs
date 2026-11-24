package naming

import (
	"errors"
	"math/rand/v2"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/uid"
)

// TestHashTextVectors guards the text of a keyed hash against vectors
// computed outside Go, with Python's hmac and base64 modules (the first
// HMAC also with openssl dgst -sha256 -hmac): HMAC-SHA-256, first 80 bits,
// lower case base32 without padding. A change here changes every hashed
// label of every tenant.
func TestHashTextVectors(t *testing.T) {
	for _, c := range []struct{ key, value, want string }{
		{"0123456789abcdef0123456789abcdef", "SUB-001", "3hsvpcs76azocuc3"},
		{"0123456789abcdef0123456789abcdef", "1.2.826.0.1.3680043.8.498.1", "c5wqt65watttzhoo"},
		{"another key, 32 bytes or longer!", "SUB-001", "ucrb2gatxhctbdnw"},
		{"key", "", "lvorhfldzfnvsz5z"},
	} {
		got := HashText([]byte(c.key), []byte(c.value))
		if got != c.want || len(got) != HashLength || !isHashText(got) {
			t.Errorf("%q under %q: %q, want %q", c.value, c.key, got, c.want)
		}
	}
}

// TestNewHasherKey guards the key: one shorter than MinKeyLength is
// refused, and the Hasher keeps its own copy, so clearing the caller's key
// does not change its labels.
func TestNewHasherKey(t *testing.T) {
	if _, err := NewHasher(testKey[:MinKeyLength-1]); err == nil {
		t.Error("a short key accepted")
	}
	if _, err := NewHasher(nil); err == nil {
		t.Error("no key accepted")
	}
	key := append([]byte(nil), testKey...)
	h, err := NewHasher(key)
	if err != nil {
		t.Fatal(err)
	}
	clear(key)
	if got := h.Hash([]byte("SUB-001")); got != "3hsvpcs76azocuc3" {
		t.Errorf("after the caller cleared its key: %q", got)
	}
}

// spy is a Hasher that records the values it hashes and returns a fixed
// text, or HashText under testKey when the text is empty.
type spy struct {
	seen []string
	out  string
}

func (s *spy) Hash(v []byte) string {
	s.seen = append(s.seen, string(v))
	if s.out != "" {
		return s.out
	}
	return HashText(testKey, v)
}

// TestHashedForm guards the {#...} form: by keyword, by tag and of a
// pattern's part it parses, survives Spec, gives the same label for the
// same value and key and another for another key, and hashes values that
// hold characters a label may not.
func TestHashedForm(t *testing.T) {
	s := Spec{
		Subject: "sub-{#PatientName}", Session: "{#(0020,000D)}", Scan: "v-{visit}-{#visit}",
		PatternAttribute: "PatientComments", Pattern: `[^^]+\^[^^]+\^(?P<visit>.+)`,
	}
	r, err := Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	if r.Spec() != s || !r.Hashes() {
		t.Fatalf("spec %+v, hashes %v", r.Spec(), r.Hashes())
	}
	again, err := Parse(r.Spec())
	if err != nil || again.Spec() != s {
		t.Fatalf("round trip: %v", err)
	}
	a := studyAttrs(t)
	got, err := r.Labels(a, testHasher(t))
	if err != nil {
		t.Fatal(err)
	}
	want := label.Path{
		Subject: "sub-" + HashText(testKey, []byte("Doe^Jane")),
		Session: HashText(testKey, []byte("1.2.826.0.1.3680043.8.498.1")),
		Scan:    "v-Visit2-" + HashText(testKey, []byte("Visit2")),
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if got.Session != "c5wqt65watttzhoo" {
		t.Fatalf("session %q does not match the outside vector", got.Session)
	}
	same, _ := r.Labels(a, testHasher(t))
	if same != got {
		t.Error("the same value and key gave another label")
	}
	other, err := NewHasher([]byte("another key, 32 bytes or longer!"))
	if err != nil {
		t.Fatal(err)
	}
	diff, err := r.Labels(a, other)
	if err != nil || diff.Subject == got.Subject || diff.Session == got.Session {
		t.Errorf("another key gave %+v, %v", diff, err)
	}
	if Default().Hashes() != true {
		t.Error("the default rule does not hash")
	}
	plain, _ := Parse(Spec{Subject: "{PatientID}", Session: "{StudyInstanceUID}", Scan: "{SeriesInstanceUID}"})
	if plain.Hashes() {
		t.Error("a rule without '#' hashes")
	}
	if _, err := plain.Labels(a, nil); err != nil {
		t.Errorf("a rule without '#' needs a hasher: %v", err)
	}
}

// TestHashedFaults guards the faults of a hashed placeholder: no hasher is
// an error, never an unhashed value; a missing or empty value is an error
// and is never hashed; a UID is checked before it is hashed; a hasher that
// returns anything but a keyed hash's text is refused; and no error's text
// carries the value.
func TestHashedFaults(t *testing.T) {
	for _, c := range []struct {
		name  string
		key   string
		value string // "<missing>" removes it
		h     *spy   // nil: no hasher
		fault Fault
	}{
		{"no hasher", "PatientID", "JaneDoe1", nil, NoHasher},
		{"missing", "PatientID", "<missing>", &spy{}, Missing},
		{"empty", "PatientID", "", &spy{}, Empty},
		{"too long", "PatientID", "Jane" + strings.Repeat("x", MaxValueLength), &spy{}, TooLong},
		{"bad uid", "StudyInstanceUID", "1.02.3", &spy{}, BadUID},
		{"uid padded", "SeriesInstanceUID", "1.2.3 ", &spy{}, BadUID},
		{"upper case hash", "PatientID", "JaneDoe1", &spy{out: "ABCDEFGHIJKLMNOP"}, BadHash},
		{"short hash", "PatientID", "JaneDoe1", &spy{out: "abcdefghijklmno"}, BadHash},
		{"value as hash", "PatientID", "JaneDoe1", &spy{out: "JaneDoe1"}, BadHash},
		{"hex hash", "PatientID", "JaneDoe1", &spy{out: "0123456789abcdef"}, BadHash},
	} {
		a := studyAttrs(t)
		if c.value == "<missing>" {
			delete(a, kw(t, c.key))
		} else {
			a[kw(t, c.key)] = c.value
		}
		var h Hasher
		if c.h != nil {
			h = c.h
		}
		p, err := Default().Labels(a, h)
		var ne *Error
		if !errors.As(err, &ne) || ne.Fault != c.fault || ne.Attribute != c.key {
			t.Errorf("%s: got %+v, %v", c.name, p, err)
			continue
		}
		// A value refused before hashing never reaches the hasher.
		if c.h != nil && c.fault != BadHash {
			for _, v := range c.h.seen {
				if v == c.value {
					t.Errorf("%s: the refused value was hashed", c.name)
				}
			}
		}
		if strings.Contains(err.Error(), "Jane") {
			t.Errorf("%s: text %q carries the value", c.name, err)
		}
	}
	a := studyAttrs(t)
	a[kw(t, "StudyInstanceUID")] = "1..2"
	if _, err := Default().Labels(a, testHasher(t)); !errors.Is(err, uid.ErrComponent) {
		t.Errorf("uid fault not wrapped: %v", err)
	}
	// A part of the pattern that is empty is not hashed either.
	r, _ := Parse(Spec{Subject: "{#visit}", Session: "{StudyInstanceUID}", Scan: "{SeriesInstanceUID}",
		PatternAttribute: "PatientComments", Pattern: `[^^]+\^[^^]+\^(?P<visit>.*)`})
	a = studyAttrs(t)
	a[kw(t, "PatientComments")] = "Proj^Sub01^"
	s := &spy{}
	if _, err := r.Labels(a, s); !isFault(err, Empty) || len(s.seen) != 0 {
		t.Errorf("empty part: %v, hashed %q", err, s.seen)
	}
}

// TestHashedParse guards what Parse refuses in the hashed form, and that a
// hash's 16 characters count toward the 128 of a label.
func TestHashedParse(t *testing.T) {
	d := DefaultSpec()
	for _, subject := range []string{"{#}", "{##PatientID}", "{#Nope}", "{#(0002,0010)}", "{# PatientID}", "{#sub}", ".{#PatientID}", "{#PatientID}."} {
		s := d
		s.Subject = subject
		if _, err := Parse(s); err == nil {
			t.Errorf("%q accepted", subject)
		}
	}
	fits, over := d, d
	fits.Subject = strings.Repeat("a", label.MaxLength-HashLength) + "{#PatientID}"
	over.Subject = strings.Repeat("a", label.MaxLength-HashLength+1) + "{#PatientID}"
	for _, c := range []struct {
		s  Spec
		ok bool
	}{{fits, true}, {over, false}} {
		r, err := Parse(c.s)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.Labels(studyAttrs(t), testHasher(t))
		if (err == nil) != c.ok || (!c.ok && !isFault(err, BadLabel)) {
			t.Errorf("%d bytes of literal: %v", len(c.s.Subject)-len("{#PatientID}"), err)
		}
	}
}

// TestDefaultHidesValues guards the reason the default rule hashes: over
// many drawn data sets, no label it makes holds the Patient ID, the Study
// Instance UID or the Series Instance UID, nor any 6 bytes in a row of one
// of them.
func TestDefaultHidesValues(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // G404: a fixed seed, so a failure is reproduced; nothing secret is drawn.
	h := testHasher(t)
	text := func(alpha string, n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alpha[r.IntN(len(alpha))]
		}
		return string(b)
	}
	id := func() string {
		s := "1." + text("123456789", 1)
		for len(s) < 50 && r.IntN(6) != 0 {
			s += "." + text("123456789", 1) + text("0123456789", r.IntN(8))
		}
		return s
	}
	for range 20000 {
		a := studyAttrs(t)
		vals := []string{
			text("abcdefghijklmnopqrstuvwxyz234567-_.ABC ^", 1+r.IntN(64)),
			id(), id(),
		}
		a[kw(t, "PatientID")], a[kw(t, "StudyInstanceUID")], a[kw(t, "SeriesInstanceUID")] = vals[0], vals[1], vals[2]
		p, err := Default().Labels(a, h)
		if err != nil {
			t.Fatalf("%q: %v", vals, err)
		}
		for _, l := range []string{p.Subject, p.Session, p.Scan} {
			for _, v := range vals {
				for i := 0; i+6 <= len(v); i++ {
					if strings.Contains(l, v[i:i+6]) {
						t.Fatalf("label %q holds %q of the value %q", l, v[i:i+6], v)
					}
				}
			}
		}
	}
}

// TestTrimValueRemovesOnlyPadding guards the one trimming of a value
// before it is hashed or keys a subject (PS3.5 6.2): trailing spaces and
// NULs always, leading spaces only where the VR says they are not
// significant, and nothing inside the value.
func TestTrimValueRemovesOnlyPadding(t *testing.T) {
	for _, c := range []struct{ vr, in, want string }{
		{"LO", " PAT 001 \x00", "PAT 001"},
		{"UI", "1.2.3\x00", "1.2.3"},
		{"DA", " 20240315 ", "20240315"},
		{"PN", " Doe^John  ", " Doe^John"},
		{"LT", "  text\x00", "  text"},
		{"UN", "AB\x00", "AB"},
	} {
		if got := TrimValue(c.vr, c.in); got != c.want {
			t.Errorf("TrimValue(%s, %q) = %q, want %q", c.vr, c.in, got, c.want)
		}
	}
}
