package check

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// TestIdentifierEdges guards how the check matches an identifier needle by
// the edge the engine gave it (engine.IdentifierEdge), in the text and in
// the raw bytes. A value of letters alone, in a script written with
// spaces, is found only where no letter or "_" stands beside it: an
// institution "TOSHIBA" is not found in the maker's name "TOSHIBA_MEC", nor
// "Here" in "Where", but is standing alone, beside punctuation, a space or
// a digit, in any case. A value that holds a digit, or is in a script
// written without spaces, is found anywhere but next to a digit on a digit
// side: a Patient ID glued to letters ("MRNAB778812", "sub_AB778812",
// "ÉAB778812", "患者AB778812様") and an institution inside a longer name
// ("東京中央病院" in "東京中央病院放射線科") are found.
func TestIdentifierEdges(t *testing.T) {
	for _, c := range []struct {
		needle, text string
		found        bool
	}{
		{"TOSHIBA", "TOSHIBA_MEC", false},
		{"TOSHIBA", "OLD_TOSHIBA", false},
		{"Here", "Where it is", false},
		{"Here", "here", true},
		{"TOSHIBA", "toshiba.", true},
		{"TOSHIBA", "at TOSHIBA-MR", true},
		{"TOSHIBA", "TOSHIBA2", true},
		{"St. Mary", "St. Mary_s", false},
		{"AB778812", "series sub_AB778812 T1", true},
		{"AB778812", "MRNAB778812", true},
		{"AB778812", "ÉAB778812", true},
		{"AB778812", "患者AB778812様", true},
		{"AB778812", "AB7788123", false},
		{"AB778812", "9AB778812", true},
		{"東京中央病院", "東京中央病院放射線科", true},
		{"7788AB", "x 7788AB", true},
		{"7788AB", "17788AB", false},
	} {
		n := needle(engine.NeedleText, c.needle)
		n.Edge = engine.IdentifierEdge(c.needle)
		p := lt(t, c.text)
		if c.found {
			mustFind(t, p, KindIdentifier, "00204000", n)
		} else {
			mustPass(t, p, n)
		}
		// The same bytes outside any element: the raw search.
		raw := writeFile(t)
		patch(t, raw, 10, []byte(" "+c.text+" "))
		if c.found {
			mustFind(t, raw, KindIdentifier, "preamble", n)
		} else {
			mustPass(t, raw, n)
		}
	}
	for v, want := range map[string]engine.Edge{"TOSHIBA": engine.EdgeIdentifier, "St. Mary": engine.EdgeIdentifier,
		"AB778812": engine.EdgeAny, "778812": engine.EdgeAny, "1.2.3": engine.EdgeAny, "東京中央病院": engine.EdgeAny} {
		if got := engine.IdentifierEdge(v); got != want {
			t.Errorf("the edge of %q is %d, want %d", v, got, want)
		}
	}
}
