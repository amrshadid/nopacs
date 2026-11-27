package check

import (
	"context"
	"errors"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// accentedTerms returns n distinct terms of length letters outside ASCII.
func accentedTerms(r *rand.Rand, n, length int) []string {
	letters := []rune("àáâãäåèéêëìíîïòóôõöùúûüýÿçñ")
	var out []string
	seen := map[string]bool{}
	for len(out) < n {
		rs := make([]rune, length)
		for i := range rs {
			rs[i] = letters[r.IntN(len(letters))]
		}
		if s := string(rs); !seen[profile.FoldText(s)] {
			seen[profile.FoldText(s)] = true
			out = append(out, s)
		}
	}
	return out
}

// TestTextSentWithABinaryVRIsSearched guards the list search against the
// VR a sender declares: a standard text element (Series Description) sent
// with VR OB holds text, since the data dictionary says it is text, and a
// term in it is found; a private value is searched whatever its VR.
func TestTextSentWithABinaryVRIsSearched(t *testing.T) {
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{"Zebulonquist"}}})
	if problems != nil {
		t.Fatal(problems)
	}
	n := engine.Needle{Kind: engine.NeedleList, Lists: l}
	v := []byte("CT by Zebulonquist ")
	if len(v)%2 == 1 {
		v = append(v, ' ')
	}
	_, err := run(t, writeFile(t, el{0x0008103E, "OB", v}), n)
	var res *Residual
	if !errors.As(err, &res) || res.First.Kind != KindListTerm || res.First.Path != "0008103E" {
		t.Errorf("a term in a text element sent as OB: %v", err)
	}
	if dictBinary(0x0008103E) || !dictBinary(0x60003000) || !dictBinary(0x7FE00010) || dictBinary(0x00091003) {
		t.Error("the dictionary's VRs read wrong")
	}
}

// TestValidListsAreNeverRefusedForACharacterSet guards the bound of raw
// forms: lists the profile accepts (their forms counted exactly in the
// character set that writes them longest, within MaxRawFormBytes), 6,000
// accented names of 10 letters or 20,000 of 5, are searched in a file
// received in ISO 8859-1, ISO 8859-2, ISO 2022 IR 87, ISO 2022 IR 100,
// GB 18030 or UTF-8, never refused for its set, and a name of them in a
// Latin-1 file is found; 20,000 accented names of 20 letters are past the
// bound and refused when the profile is made.
func TestValidListsAreNeverRefusedForACharacterSet(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6)) //nolint:gosec // G404: test data, not a secret.
	// A list holds at most profile.MaxListTerms terms: 20,000 are two lists.
	lists := func(terms []string) []profile.TextList {
		half := len(terms) / 2
		return []profile.TextList{{Name: "a", Version: 1, Terms: terms[:half]}, {Name: "b", Version: 1, Terms: terms[half:]}}
	}
	if _, problems := profile.CompileTextLists(lists(accentedTerms(r, 20000, 20))); len(problems) == 0 || problems[0].Code != profile.CodeTooLarge {
		t.Errorf("lists past the bound of their forms: %v", problems)
	}
	sets := []string{"ISO_IR 100", "ISO_IR 101", `\ISO 2022 IR 87`, `ISO 2022 IR 6\ISO 2022 IR 100`, "GB18030", "ISO_IR 192"}
	for _, terms := range [][]string{accentedTerms(r, 6000, 10), accentedTerms(r, 20000, 5)} {
		l, problems := profile.CompileTextLists(lists(terms))
		if problems != nil {
			t.Fatalf("%d names of %d letters, within the bound: %v", len(terms), len([]rune(terms[0])), problems)
		}
		n := engine.Needles{{Kind: engine.NeedleList, Lists: l}}
		for _, cs := range sets {
			path := writeFile(t, el{0x00080005, "CS", cs}, el{0x00081030, "LO", "nothing here"})
			if _, err := New(Options{}).checkPath(t, context.Background(), path, n, &cs); err != nil {
				t.Errorf("%d names of %d letters, a file received in %s: %v", len(terms), len([]rune(terms[0])), cs, err)
			}
		}
	}
	// A name of the list in a Latin-1 file is still found in its raw bytes.
	terms := accentedTerms(r, 20000, 5)
	l, problems := profile.CompileTextLists(lists(terms))
	if problems != nil {
		t.Fatal(problems)
	}
	latin1, ok := encodeLatin1(terms[123])
	if !ok {
		t.Fatalf("%q is not Latin-1", terms[123])
	}
	cs := "ISO_IR 100"
	path := writeFile(t, el{0x00080005, "CS", cs}, el{0x00081030, "LO", append([]byte("by "), latin1...)})
	_, err := New(Options{}).checkPath(t, context.Background(), path, engine.Needles{{Kind: engine.NeedleList, Lists: l}}, &cs)
	var res *Residual
	if !errors.As(err, &res) || res.First.Kind != KindListTerm {
		t.Errorf("a name of the list in a Latin-1 file: %v", err)
	}
}

// encodeLatin1 writes s in ISO 8859-1, one byte a character.
func encodeLatin1(s string) ([]byte, bool) {
	var out []byte
	for _, c := range s {
		if c > 0xFF {
			return nil, false
		}
		out = append(out, byte(c)) //nolint:gosec // G115: at most 0xFF, checked above.
	}
	return out, true
}

// TestRawListsAreBuiltOncePerCharacterSet guards the raw automata's
// cache: files received in five character sets in turn build each set's
// automaton once, then take it from the cache; spellings of one set (its
// encodings in another order, or twice) share it; many files at once
// asking for a set not built yet build it once; and lists of ASCII only
// have one automaton for every set.
func TestRawListsAreBuiltOncePerCharacterSet(t *testing.T) {
	r := rand.New(rand.NewPCG(7, 8)) //nolint:gosec // G404: test data, not a secret.
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "a", Version: 1, Terms: accentedTerms(r, 5000, 8)}})
	if problems != nil {
		t.Fatal(problems)
	}
	var sets [][]string
	for _, cs := range []string{"ISO_IR 100", "ISO_IR 101", "ISO_IR 109", "ISO_IR 110", "ISO_IR 148"} {
		sets = append(sets, sourceEncodings(&cs))
	}
	for _, s := range sets {
		rawListsFor(l, s)
	}
	start := time.Now()
	for i := range 50 {
		rawListsFor(l, sets[i%len(sets)])
	}
	if took := time.Since(start); took > 50*time.Millisecond {
		t.Errorf("50 files in 5 character sets in turn took %v: the automata were built again", took)
	}
	two := []string{"iso_ir_87", "iso8859"}
	if a, b := rawListsFor(l, two), rawListsFor(l, []string{two[1], two[0], two[1]}); a != b {
		t.Error("one set's spellings built two automata")
	}
	fresh, _ := profile.CompileTextLists([]profile.TextList{{Name: "a", Version: 1, Terms: accentedTerms(r, 3000, 8)}})
	var wg sync.WaitGroup
	got := make([]*rawLists, 16)
	for i := range got {
		wg.Add(1)
		go func() { defer wg.Done(); got[i] = rawListsFor(fresh, sets[0]) }()
	}
	wg.Wait()
	for _, g := range got {
		if g != got[0] {
			t.Fatal("files asking at once built a set more than once")
		}
	}
	ascii, _ := profile.CompileTextLists([]profile.TextList{{Name: "a", Version: 1, Terms: []string{"Ann Lee", "North Clinic"}}})
	if rawListsFor(ascii, sets[0]) != rawListsFor(ascii, sets[3]) {
		t.Error("lists of ASCII built an automaton per character set")
	}
}
