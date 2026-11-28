package sidecar

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// withUnnamed returns a de-identified instance that lists the elements no
// rule named: a standard one, a vendor's with its creator, and one whose
// creator is withheld.
func withUnnamed(t *testing.T, u string) Instance {
	t.Helper()
	in := full(t, u)
	in.Unnamed = []UnnamedElement{
		{Tag: 0x00180050, VR: "DS", Count: 3},
		{Tag: 0x00290001, VR: "LO", Count: 1, Creator: "ACME RESEARCH"},
		{Tag: 0x00310001, VR: "UN", Count: 2, Withheld: true},
		{Tag: 0x00290002, VR: "LO", Count: 1},
	}
	in.UnnamedOver = 0
	return in
}

// TestUnnamedRoundTrips guards the list of elements no rule named: written
// after the build's version in the compact form, each as an array of its tag
// in eight upper case digits, VR and count, with a creator or a withheld flag
// only where there is one; a list that is empty is written and read back as
// present and empty, apart from a line with none; and what is read is what
// was written, a withheld creator still withheld.
func TestUnnamedRoundTrips(t *testing.T) {
	with := withUnnamed(t, "1.2.3")
	empty := full(t, "1.2.4")
	empty.Unnamed = []UnnamedElement{}
	none := full(t, "1.2.5")
	text := v3sidecar(t, head(), with, empty, none)
	s := string(text)
	if !strings.Contains(s, `"nopacs_version":"v1.2.3-rc.1+build","unnamed":[["00180050","DS",3],["00290001","LO",1,"ACME RESEARCH"],["00310001","UN",2,"",true],["00290002","LO",1]],"pixel_data"`) {
		t.Errorf("the list is not written as the form says:\n%s", s)
	}
	if strings.Count(s, `"unnamed":`) != 2 || !strings.Contains(s, `"unnamed":[],"pixel_data"`) {
		t.Errorf("a line with none is written with a list, or an empty list is left out:\n%s", s)
	}
	_, _, got, err := drain(text)
	if err != nil || len(got) != 3 {
		t.Fatalf("read back: %d lines, %v", len(got), err)
	}
	if !reflect.DeepEqual(got[0].Unnamed, with.Unnamed) || got[1].Unnamed == nil || len(got[1].Unnamed) != 0 || got[2].Unnamed != nil {
		t.Errorf("read back %+v, %+v and %+v", got[0].Unnamed, got[1].Unnamed, got[2].Unnamed)
	}
	if !got[0].Unnamed[2].Withheld || got[0].Unnamed[2].Creator != "" {
		t.Errorf("a withheld creator is read back as %+v", got[0].Unnamed[2])
	}
	over := withUnnamed(t, "1.2.6")
	over.Unnamed = nil
	for i := range MaxUnnamed {
		over.Unnamed = append(over.Unnamed, UnnamedElement{Tag: 0x00090001, VR: "LO", Count: 1, Creator: "C" + strconv.Itoa(i)})
	}
	over.UnnamedOver = 9
	if _, _, back, err := drain(v3sidecar(t, head(), over)); err != nil || len(back) != 1 || back[0].UnnamedOver != 9 || len(back[0].Unnamed) != MaxUnnamed {
		t.Errorf("a full list with 9 over: %v", err)
	}
}

// TestUnnamedIsRefusedWhenItBreaksTheForm guards what a list may hold, on
// writing and on reading: an unlisted over count, more than the bound, an
// element listed twice, a VR that is not two upper case letters, a count below
// one, a private block number other than 00, a creator on a standard element,
// a creator that is not printable or is longer than 64 bytes, a creator both
// given and withheld, and a list on a line with no profile.
func TestUnnamedIsRefusedWhenItBreaksTheForm(t *testing.T) {
	good := UnnamedElement{Tag: 0x00290001, VR: "LO", Count: 1, Creator: "ACME"}
	for name, mutate := range map[string]func(in *Instance){
		"an over count with no list":         func(in *Instance) { in.UnnamedOver = 3 },
		"an over count with a list not full": func(in *Instance) { in.Unnamed, in.UnnamedOver = []UnnamedElement{good}, 3 },
		"a negative over count":              func(in *Instance) { in.Unnamed, in.UnnamedOver = []UnnamedElement{good}, -1 },
		"an element listed twice":            func(in *Instance) { in.Unnamed = []UnnamedElement{good, good} },
		"a VR in lower case":                 func(in *Instance) { in.Unnamed = []UnnamedElement{{Tag: 0x00180050, VR: "ds", Count: 1}} },
		"a VR of three letters":              func(in *Instance) { in.Unnamed = []UnnamedElement{{Tag: 0x00180050, VR: "DSS", Count: 1}} },
		"a count of none":                    func(in *Instance) { in.Unnamed = []UnnamedElement{{Tag: 0x00180050, VR: "DS"}} },
		"a block number kept":                func(in *Instance) { in.Unnamed = []UnnamedElement{{Tag: 0x00291001, VR: "LO", Count: 1}} },
		"a creator on a standard element":    func(in *Instance) { in.Unnamed = []UnnamedElement{{Tag: 0x00180050, VR: "DS", Count: 1, Creator: "X"}} },
		"a creator that is not printable": func(in *Instance) {
			in.Unnamed = []UnnamedElement{{Tag: 0x00290001, VR: "LO", Count: 1, Creator: "a\x00b"}}
		},
		"a creator past 64 bytes": func(in *Instance) {
			in.Unnamed = []UnnamedElement{{Tag: 0x00290001, VR: "LO", Count: 1, Creator: strings.Repeat("x", 65)}}
		},
		"a creator both given and withheld": func(in *Instance) {
			in.Unnamed = []UnnamedElement{{Tag: 0x00290001, VR: "LO", Count: 1, Creator: "ACME", Withheld: true}}
		},
		"a list on a line with no profile": func(in *Instance) {
			in.Unnamed, in.Profile, in.ProfileSHA256, in.PixelRiskAccepted = []UnnamedElement{good}, "", "", false
		},
		"more than the bound": func(in *Instance) {
			for i := range MaxUnnamed + 1 {
				in.Unnamed = append(in.Unnamed, UnnamedElement{Tag: 0x00090001, VR: "LO", Count: 1, Creator: "C" + strconv.Itoa(i)})
			}
		},
	} {
		in := full(t, "1.2.3")
		mutate(&in)
		if err := NewWriterV3(io.Discard, head()).Add(in); !errors.Is(err, ErrFormat) {
			t.Errorf("%s written: %v, want ErrFormat", name, err)
		}
	}
	// A tampered line is refused on reading as well.
	text := string(v3sidecar(t, head(), withUnnamed(t, "1.2.3")))
	for name, bad := range map[string]string{
		"a tag in lower case":             strings.Replace(text, `"00180050"`, `"0018005a"`, 1),
		"a short tag":                     strings.Replace(text, `"00180050"`, `"180050"`, 1),
		"a fifth member that is not true": strings.Replace(text, `"",true]`, `"",false]`, 1),
		"an element of two members":       strings.Replace(text, `["00180050","DS",3]`, `["00180050","DS"]`, 1),
	} {
		if _, _, _, err := drain([]byte(bad)); !errors.Is(err, ErrFormat) {
			t.Errorf("%s read: %v, want ErrFormat", name, err)
		}
	}
}

// TestUnnamedIsNotKeptInAnOlderSidecar guards that a line joining a sidecar
// of version 2 has no place for the list: it is written without it, the
// rest of the line as it is, as the other fields of version 3 are.
func TestUnnamedIsNotKeptInAnOlderSidecar(t *testing.T) {
	in := withUnnamed(t, "1.2.3")
	if got := in.withoutV3(); got.Unnamed != nil || got.UnnamedOver != 0 || got.Profile != in.Profile {
		t.Errorf("the line for an older sidecar is %+v", got)
	}
	var b bytes.Buffer
	w := NewWriter(&b)
	if err := w.Add(in.withoutV3()); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(b.String(), `"unnamed"`) {
		t.Errorf("a sidecar of version 2 holds the list:\n%s", b.String())
	}
	if err := NewWriter(io.Discard).Add(in); !errors.Is(err, ErrFormat) {
		t.Errorf("a list written into a sidecar of version 2: %v, want ErrFormat", err)
	}
}
