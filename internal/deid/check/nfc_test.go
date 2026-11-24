package check

import (
	"context"
	"errors"
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// The two ways Unicode writes the name Müller: the "ü" as one character
// (NFC), and as "u" followed by the combining diaeresis U+0308 (NFD).
const (
	mullerComposed   = "Müller"
	mullerDecomposed = "Müller"
)

// TestComposedAndDecomposedNamesAreOne guards the text search in NFC: a
// name written with a combining mark in a description is found for a
// needle written composed, and a name written composed for a needle
// written with the combining mark, as a text and in a value of unknown
// kind; an ASCII name is found as before.
func TestComposedAndDecomposedNamesAreOne(t *testing.T) {
	for _, c := range []struct{ text, needle string }{
		{"seen by " + mullerDecomposed + " today", mullerComposed + "^HANS"},
		{"seen by " + mullerComposed + " today", mullerDecomposed + "^HANS"},
		{"seen by MULLER today", "Muller^Hans"},
	} {
		n := needle(engine.NeedleName, c.needle)
		mustFind(t, writeFile(t, el{0x00081030, "LO", c.text}), KindName, "00081030", n)
		mustFind(t, writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{0x00111002, "UN", even(c.text)}), KindName, "00111002", n)
	}
}

// TestRawFindsComposedAndDecomposedBytes guards the raw search, whose bytes
// no reader normalized: a name written with a combining mark in a private
// binary value, never decoded as text, is found in the raw bytes for a
// needle written composed (its NFD form), and one written composed for a
// needle written with the mark (its NFC form).
func TestRawFindsComposedAndDecomposedBytes(t *testing.T) {
	for _, c := range []struct{ bytes, needle string }{
		{mullerDecomposed, mullerComposed + "^HANS"},
		{mullerComposed, mullerDecomposed + "^HANS"},
		{"MÜLLER", mullerDecomposed + "^HANS"},
	} {
		p := writeFile(t, el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OW", even(" " + c.bytes + " ")})
		rep, err := run(t, p, needle(engine.NeedleName, c.needle))
		findRaw(t, rep, err, "00111003", true)
	}
}

// TestRawMatchJoinedByAMarkAgreesWithClean guards that the raw search
// judges a match in NFC, as clean does: "RENE" followed by U+0301 is
// "RENÉ" in NFC, another word that clean leaves, so a UTF-8 file holding
// it passes for the needle RENE^XAVIER, in a description and in a private
// binary value. It guards the other direction too: the same bytes are
// found for the needle René^Xavier (its decomposed raw form and its NFC
// text), a mark that composes with nothing ("MARQ" then U+0301) leaves
// the match standing, and a file received in Latin-1, where no byte is a
// combining mark, is still held.
func TestRawMatchJoinedByAMarkAgreesWithClean(t *testing.T) {
	utf8Set := "ISO_IR 192"
	check := func(cs *string, needleValue string, els ...el) (Report, error) {
		p := writeFile(t, append([]el{{0x00080005, "CS", "ISO_IR 192"}}, els...)...)
		return New(Options{}).checkPath(t, context.Background(), p, engine.Needles{needle(engine.NeedleName, needleValue)}, cs)
	}
	desc := el{0x00081030, "LO", even("seen by René today")}
	blob := []el{{0x00110010, "LO", "ACME 1.0"}, {privateOW, "OW", even(" RENÉ ")}}
	for name, els := range map[string][]el{"description": {desc}, "binary": blob} {
		if rep, err := check(&utf8Set, "RENE^XAVIER", els...); err != nil {
			t.Errorf("%s: want a pass for RENE^XAVIER, got %v %+v", name, err, rep.Findings)
		}
	}
	rep, err := check(&utf8Set, "René^Xavier", desc)
	var res *Residual
	if !errors.As(err, &res) || rep.Findings[0].Path != "00081030" {
		t.Errorf("description for René^Xavier: want a finding, got %v %+v", err, rep.Findings)
	}
	rep, err = check(&utf8Set, "René^Xavier", blob...)
	findRaw(t, rep, err, "00111003", true)
	rep, err = check(&utf8Set, "MARQ^PAUL", el{0x00110010, "LO", "ACME 1.0"}, el{privateOW, "OW", even(" MARQ́ ")})
	findRaw(t, rep, err, "00111003", true)
	rep, err = check(nil, "RENE^XAVIER", blob...)
	findRaw(t, rep, err, "00111003", true)
}

// TestFoldTextIsNFC guards the one form forms and texts are folded to:
// composed and decomposed are one text, and ASCII is unchanged but for its
// case and its runs of white space.
func TestFoldTextIsNFC(t *testing.T) {
	if a, b := foldText(mullerComposed), foldText(mullerDecomposed); a != b {
		t.Errorf("folded %q and %q", a, b)
	}
	if got := foldText("Doe,  John\t2024"); got != "DOE, JOHN 2024" {
		t.Errorf("ASCII folded to %q", got)
	}
}
