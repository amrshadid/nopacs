package tenant

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// TestCreateOutcome guards the rules every driver stores a profile version
// by: the first version is 1 and the next one more; a document equal to
// the latest stores nothing, whatever the expectation; an expectation
// other than the latest's number is a precondition failure; and a profile
// at MaxProfileVersions takes no more.
func TestCreateOutcome(t *testing.T) {
	doc := func(s string) NewProfileVersion { return NewProfileVersion{Source: []byte(s)} }
	latest := func(n int32, s string) *ProfileVersion { return &ProfileVersion{Version: n, Source: []byte(s)} }
	with := func(v NewProfileVersion, n int32) NewProfileVersion { v.ExpectLatest = &n; return v }
	for _, c := range []struct {
		what   string
		latest *ProfileVersion
		v      NewProfileVersion
		next   int32
		kind   fault.Kind
	}{
		{"the first", nil, doc("a"), 1, 0},
		{"the first expecting none", nil, with(doc("a"), 0), 1, 0},
		{"the first expecting one", nil, with(doc("a"), 1), 0, fault.Precondition},
		{"the next", latest(3, "a"), doc("b"), 4, 0},
		{"the next expecting the latest", latest(3, "a"), with(doc("b"), 3), 4, 0},
		{"the next expecting another", latest(3, "a"), with(doc("b"), 2), 0, fault.Precondition},
		{"equal to the latest", latest(3, "a"), doc("a"), 0, 0},
		{"equal to the latest expecting another", latest(3, "a"), with(doc("a"), 2), 0, 0},
		{"at the most", latest(MaxProfileVersions, "a"), doc("b"), 0, fault.Precondition},
		{"one below the most", latest(MaxProfileVersions-1, "a"), doc("b"), MaxProfileVersions, 0},
		{"equal at the most", latest(MaxProfileVersions, "a"), doc("a"), 0, 0},
	} {
		next, err := CreateOutcome(c.latest, c.v)
		if next != c.next || (c.kind == 0) != (err == nil) || (err != nil && fault.KindOf(err) != c.kind) {
			t.Errorf("%s: next %d, %v; want %d, kind %v", c.what, next, err, c.next, c.kind)
		}
	}
}

// TestLookupCodeForm guards a lookup table's codes and keys: the n-th code
// is n in decimal, a code of the form every text VR holds up to sixteen
// digits and not past; a key is sixteen characters of lower case base32.
func TestLookupCodeForm(t *testing.T) {
	if LookupCodeText(1) != "1" || !ValidLookupCode(LookupCodeText(9999999999999999)) || ValidLookupCode(LookupCodeText(10000000000000000)) {
		t.Error("the codes in order")
	}
	for _, bad := range []string{"", "a", "A-1", "A 1", "12345678901234567"} {
		if ValidLookupCode(bad) {
			t.Errorf("%q is a code", bad)
		}
	}
	if err := (LookupKey{Table: "acc", Key: "abcdefgh234567ab"}).Validate(); err != nil {
		t.Errorf("a keyed hash refused: %v", err)
	}
	for _, bad := range []LookupKey{{Table: "acc", Key: "abcdefgh234567a1"}, {Table: "acc", Key: "abcdefgh234567a"}, {Table: "ACC", Key: "abcdefgh234567ab"}} {
		if fault.KindOf(bad.Validate()) != fault.Invalid {
			t.Errorf("%v accepted", bad)
		}
	}
}
