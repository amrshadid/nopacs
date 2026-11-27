package check

import (
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"runtime"
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// randomTerms returns n distinct terms of 4 to 7 ASCII letters, as a list
// of surnames is.
func randomTerms(r *rand.Rand, n int) []string {
	seen := map[string]bool{}
	var out []string
	for len(out) < n {
		b := make([]byte, 4+r.IntN(4))
		for i := range b {
			b[i] = "abcdefghijklmnopqrstuvwxyz"[r.IntN(26)]
		}
		if s := string(b); !seen[profile.FoldText(s)] {
			seen[profile.FoldText(s)] = true
			out = append(out, s)
		}
	}
	return out
}

// randomBytes returns n pseudo-random bytes.
func randomBytes(r *rand.Rand, n int) []byte {
	b := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		binary.LittleEndian.PutUint64(b[i:], r.Uint64())
	}
	return b
}

// TestListTermsAreNotSearchedInBinary guards that a list's terms are
// words of text, searched wherever text may be and nowhere else: a file
// whose only binary values are 1 MB of random bytes in standard elements
// the dictionary gives binary VRs (an overlay's data, a waveform's, a LUT
// of US, an ICC profile of OB, an encapsulated PDF, the ICC profile sent
// as OW) passes a list of 10,000 short names, though such names occur by
// chance in random bytes; an encapsulated document of text (a CDA,
// text/XML) holding a term is held as list_term, as it is for the
// patient's name; a PDF kept under the lists is held as
// list_unsearchable at its path, whatever it holds, and passes with no
// lists or replaced by a dummy of zero bytes; and a term in a private
// value, which may be text, is found.
func TestListTermsAreNotSearchedInBinary(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: test data, not a secret.
	terms := randomTerms(r, 10000)
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: terms}})
	if problems != nil {
		t.Fatal(problems)
	}
	n := engine.Needle{Kind: engine.NeedleList, Lists: l}
	noise := randomBytes(r, 1<<20)
	for _, els := range [][]el{
		{{0x60003000, "OW", noise}},
		{{0x54001010, "OW", noise}},
		{{0x00283006, "OW", noise}},
		{{0x00282000, "OB", noise}},
		{{0x00282000, "OW", noise}},
		{{0x00420012, "LO", "application/octet-stream"}, {0x00420011, "OB", noise}},
	} {
		if rep, err := run(t, writeFile(t, els...), n); err != nil && !errors.As(err, new(*Residual)) {
			t.Fatal(err)
		} else if err != nil && rep.Findings[0].Kind == KindListTerm {
			t.Errorf("random bytes in %08X: %v; %d findings, first %+v", els[len(els)-1].tag, err, rep.Total, rep.Findings)
		}
	}
	// Ten documents of 256 KB of random bytes, of a type not text, hold no
	// term (the type alone holds them, below).
	for range 10 {
		rep, err := run(t, writeFile(t, el{0x00282000, "OB", randomBytes(r, 256<<10)}), n)
		if err != nil {
			t.Errorf("256 KB of random bytes in an ICC profile: %v %+v", err, rep.Findings)
		}
	}
	cda := []byte(`<?xml version="1.0"?><ClinicalDocument><author><assignedPerson><name>` + terms[1] +
		`</name></assignedPerson></author><component>findings</component></ClinicalDocument>`)
	if len(cda)%2 == 1 {
		cda = append(cda, ' ')
	}
	for _, needle := range []engine.Needle{n, {Kind: engine.NeedleName, Value: "Zebulonquist^Ann", Path: "00100010"}} {
		doc := cda
		want := KindListTerm
		if needle.Kind == engine.NeedleName {
			doc, want = []byte(strings.Replace(string(cda), terms[1], "Zebulonquist Ann", 1)), KindName
		}
		if len(doc)%2 == 1 {
			doc = append(doc, ' ')
		}
		_, err := run(t, writeFile(t, el{0x00420012, "LO", "text/XML"}, el{0x00420011, "OB", doc}), needle)
		var res *Residual
		if !errors.As(err, &res) || res.First.Path != "00420011" || res.First.Kind != want {
			t.Errorf("a CDA holding a %v in Encapsulated Document: %v", needle.Kind, err)
		}
	}
	pdf := append([]byte("%PDF-1.7\n"), randomBytes(r, 4096)...)
	_, err := run(t, writeFile(t, el{0x00420012, "LO", "application/pdf"}, el{0x00420011, "OB", pdf}), n)
	var held *Residual
	if !errors.As(err, &held) || held.First.Path != "00420011" || held.First.Kind != KindListUnsearchable {
		t.Errorf("a PDF kept under text lists: %v", err)
	}
	if _, err := run(t, writeFile(t, el{0x00420012, "LO", "application/pdf"}, el{0x00420011, "OB", pdf}),
		engine.Needle{Kind: engine.NeedleName, Value: "Zebulonquist^Ann", Path: "00100010"}); err != nil {
		t.Errorf("a PDF under a profile with no lists: %v", err)
	}
	// The PDF replaced by a rule's dummy, two zero bytes, holds no text.
	if _, err := run(t, writeFile(t, el{0x00420012, "LO", "application/pdf"}, el{0x00420011, "OB", []byte{0, 0}}), n); err != nil {
		t.Errorf("a PDF replaced by a dummy under text lists: %v", err)
	}
	text := append([]byte(strings.Repeat("..", 64)+" by "+terms[0]+" "), randomBytes(r, 64)...)
	if len(text)%2 == 1 {
		text = append(text, 0)
	}
	_, err = run(t, writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OW", text}), n)
	var res *Residual
	if !errors.As(err, &res) || res.First.Kind != KindListTerm {
		t.Errorf("a term in a private binary value: %v", err)
	}
}

// TestListTermInTheReceivedCharacterSet guards the raw forms of a term
// outside ASCII in the character set the file was received in: written in
// ISO 8859-1 inside a private value too long to be decoded whole, it is
// found, as a removed name in the same place is; a file received in
// UTF-8 holding the same bytes is not held for them.
func TestListTermInTheReceivedCharacterSet(t *testing.T) {
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: []string{"Müllerstein"}}})
	if problems != nil {
		t.Fatal(problems)
	}
	n := engine.Needles{{Kind: engine.NeedleList, Lists: l}}
	b := []byte(strings.Repeat("ab ", 100_000) + "by M\xfcllerstein ")
	if len(b)%2 == 1 {
		b = append(b, ' ')
	}
	latin1 := "ISO_IR 100"
	path := writeFile(t, el{0x00080005, "CS", latin1}, el{0x00090010, "LO", "ACME 1.0"}, el{privateOW, "OB", b})
	_, err := New(Options{}).checkPath(t, context.Background(), path, n, &latin1)
	var res *Residual
	if !errors.As(err, &res) || res.First.Kind != KindListTerm {
		t.Errorf("a term in ISO 8859-1 in a long private value: %v", err)
	}
}

// TestListAutomataAreBounded guards the memory a version's lists hold: two
// lists at the bounds, of terms of letters outside ASCII (each two bytes in
// UTF-8, written composed and decomposed in two cases), are refused as
// too_large past MaxRawFormBytes; lists within the bounds hold, compiled
// and searched in raw bytes, at most 64 MB, built without allocating more
// than a few times that; and the check's reservation counts them.
func TestListAutomataAreBounded(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4)) //nolint:gosec // G404: test data, not a secret.
	letters := []rune("àáâãäåèéêëìíîïòóôõöùúûüýÿçñ")
	mk := func(n, length int) []string {
		var out []string
		seen := map[string]bool{}
		for len(out) < n {
			rs := make([]rune, length)
			for i := range rs {
				rs[i] = letters[r.IntN(len(letters))]
			}
			if s := string(rs); !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
		return out
	}
	if _, problems := profile.CompileTextLists([]profile.TextList{{Name: "a", Version: 1, Terms: mk(10000, 24)}, {Name: "b", Version: 1, Terms: mk(10000, 24)}}); len(problems) == 0 ||
		problems[0].Code != profile.CodeTooLarge {
		t.Errorf("lists past the bound of raw forms: %v", problems)
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "a", Version: 1, Terms: mk(5000, 6)}, {Name: "b", Version: 1, Terms: randomTerms(r, 10000)}})
	if problems != nil {
		t.Fatal(problems)
	}
	rl := rawListsFor(l, []string{"UTF-8"})
	runtime.GC()
	runtime.ReadMemStats(&after)
	held := (float64(after.HeapAlloc) - float64(before.HeapAlloc)) / (1 << 20)
	built := float64(after.TotalAlloc-before.TotalAlloc) / (1 << 20)
	t.Logf("raw bytes %d, text nodes %d, raw nodes %d, held %.0f MB, allocated %.0f MB", l.RawBytes(), l.Matcher().Nodes(), rl.m.Nodes(), held, built)
	if held > 64 || built > 256 || rl.refused {
		t.Errorf("a version's lists hold %.0f MB, allocated %.0f MB", held, built)
	}
	n := engine.Needles{{Kind: engine.NeedleList, Lists: l}}
	if with, without := Reservation(context.Background(), 4096, 64, n, nil), Reservation(context.Background(), 4096, 64, nil, nil); with-without < int64(l.Matcher().Nodes()) {
		t.Errorf("the reservation does not count the lists' automata: %d against %d", with, without)
	}
	runtime.KeepAlive(rl)
}
