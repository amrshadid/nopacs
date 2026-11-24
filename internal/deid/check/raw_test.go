package check

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"

	"github.com/amrshadid/go-dicom/charset"
	"github.com/amrshadid/go-dicom/config"
	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// mustEncodings converts a Specific Character Set to the library's
// encodings.
func mustEncodings(t testing.TB, terms ...string) []string {
	t.Helper()
	enc, err := charset.ConvertEncodings(terms)
	if err != nil {
		t.Fatal(err)
	}
	return enc
}

// mustEncode writes s in the encodings of a Specific Character Set.
func mustEncode(t testing.TB, s string, terms ...string) []byte {
	t.Helper()
	b, err := charset.EncodeString(s, mustEncodings(t, terms...))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// checkCS checks path for needles with characterSet as the file was
// received in.
func checkCS(t *testing.T, path, characterSet string, needles ...engine.Needle) (Report, error) {
	t.Helper()
	return New(Options{}).checkPath(t, t.Context(), path, needles, &characterSet)
}

// findRaw fails unless err is a residual whose first finding is at path,
// in the raw bytes when raw is set, in the decoded text otherwise.
func findRaw(t *testing.T, rep Report, err error, path string, raw bool) {
	t.Helper()
	var res *Residual
	if !errors.As(err, &res) || rep.Findings[0].Path != path || rep.Findings[0].Raw != raw {
		t.Fatalf("want a finding at %s (raw %v), got %v %+v", path, raw, err, rep.Findings)
	}
}

// TestTextAndRawSearchEachFindWhatTheOtherCannot guards the two searches
// together: a name written in ISO 2022 (PS3.5 6.1.2.5), whose bytes are
// neither UTF-8 nor the Latin-1 the raw search tries when the file came
// with no character set, is found in the decoded text; a name in the value
// of a standard binary element, which is not decoded, only in the raw
// bytes. Both in one file, both reported.
func TestTextAndRawSearchEachFindWhatTheOtherCannot(t *testing.T) {
	jis := mustEncode(t, "山田^太郎", "", "ISO 2022 IR 87")
	if !bytes.HasPrefix(jis, []byte("\x1b$B")) {
		t.Fatalf("not written in ISO 2022: %q", jis)
	}
	p := writeFile(t, el{0x00080005, "CS", `\ISO 2022 IR 87`}, el{0x00081050, "PN", jis},
		el{overlay, "OW", even(" SMITH^ANNA ")})
	rep, err := run(t, p, needle(engine.NeedleName, "山田^太郎"), needle(engine.NeedleName, "SMITH^ANNA"))
	var res *Residual
	if !errors.As(err, &res) {
		t.Fatalf("want a residual, got %v", err)
	}
	var text, raw bool
	for _, f := range rep.Findings {
		switch {
		case f.Path == "00081050" && !f.Raw:
			text = true
		case f.Path == "60003000" && f.Raw:
			raw = true
		}
	}
	if !text || !raw {
		t.Errorf("text %v, raw %v: %+v", text, raw, rep.Findings)
	}
}

// TestRawSearchUsesTheSourceCharacterSet guards that a name hidden in a
// binary value in the character set the file was received in (ISO 8859-5,
// Cyrillic) is found when that character set is given, as no UTF-8 or
// Latin-1 form matches its bytes.
func TestRawSearchUsesTheSourceCharacterSet(t *testing.T) {
	p := writeFile(t, el{overlay, "OW", append(mustEncode(t, "ИВАНОВ^ПЁТР", "ISO_IR 144"), ' ', ' ')})
	n := needle(engine.NeedleName, "Иванов^Пётр")
	if _, err := run(t, p, n); err != nil {
		t.Fatalf("found without the source character set: %v", err)
	}
	rep, err := checkCS(t, p, "ISO_IR 144", n)
	findRaw(t, rep, err, "60003000", true)
}

// TestRawISO2022BetweenEscapes guards that the raw form of a name written
// in a code extension has its escape sequences taken out (PS3.5
// 6.1.2.5.3): a file writes escapes around a whole run of Kanji, so a name
// inside the run has none of its own, and is still found.
func TestRawISO2022BetweenEscapes(t *testing.T) {
	run := mustEncode(t, "患者山田", "", "ISO 2022 IR 87")
	p := writeFile(t, el{overlay, "OW", append(run, ' ', ' ')})
	rep, err := checkCS(t, p, `\ISO 2022 IR 87`, needle(engine.NeedleName, "山田^XYZW"))
	findRaw(t, rep, err, "60003000", true)
}

// TestRawFormsOfEachEncodingAlone guards the forms written in each of the
// received character sets alone: under "\ISO 2022 IR 13\ISO 2022 IR 87"
// Kanji are written in JIS X 0208 by the second, which the library's
// encoding of the whole list does not give.
func TestRawFormsOfEachEncodingAlone(t *testing.T) {
	jis := mustEncode(t, "山田", "", "ISO 2022 IR 87")
	p := writeFile(t, el{overlay, "OW", append(jis, ' ', ' ')})
	rep, err := checkCS(t, p, `\ISO 2022 IR 13\ISO 2022 IR 87`, needle(engine.NeedleName, "山田^XYZW"))
	findRaw(t, rep, err, "60003000", true)
}

// TestRawLowerCaseOutsideASCII guards the lower-case raw form: the raw
// bytes have only their ASCII letters upper-cased, so a Latin-1 "müller"
// is found for "MÜLLER" only by searching the lower case too.
func TestRawLowerCaseOutsideASCII(t *testing.T) {
	p := writeFile(t, el{overlay, "OW", append(mustEncode(t, "müller", "ISO_IR 100"), ' ', ' ')})
	rep, err := checkCS(t, p, "ISO_IR 100", needle(engine.NeedleName, "MÜLLER^HANS"))
	findRaw(t, rep, err, "60003000", true)
}

// TestOneCharacterNamesEverywhere guards a name of one Han character
// wherever text may hide: a value of VR UN in a file declaring no
// character set (read as UTF-8), one in GB 18030 in a file of ISO_IR 192
// that was received in GB 18030, the preamble, and a private value of VR
// OW; and that the same character inside the value of a standard binary
// element, where numbers spell any three bytes by chance, is not taken
// for it.
func TestOneCharacterNamesEverywhere(t *testing.T) {
	n := needle(engine.NeedleName, "王^小明")
	priv := el{0x00090010, "LO", "ACME 1.0"}
	t.Run("UN as UTF-8", func(t *testing.T) {
		p := writeFile(t, priv, el{0x00091002, "UN", "pt 王 seen "})
		rep, err := run(t, p, n)
		findRaw(t, rep, err, "00091002", false)
	})
	t.Run("UN in the received character set", func(t *testing.T) {
		p := writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}, priv, el{0x00091002, "UN", []byte("pt \xcd\xf5 x ")})
		rep, err := checkCS(t, p, "GB18030", n)
		findRaw(t, rep, err, "00091002", false)
	})
	t.Run("preamble", func(t *testing.T) {
		p := writeFile(t)
		patch(t, p, 0, []byte("王"))
		rep, err := run(t, p, n)
		findRaw(t, rep, err, "preamble", false)
	})
	t.Run("private OW", func(t *testing.T) {
		p := writeFile(t, priv, el{0x00091003, "OW", even("pt 王 x")})
		rep, err := run(t, p, n)
		findRaw(t, rep, err, "00091003", true)
	})
	t.Run("not in a standard binary value", func(t *testing.T) {
		mustPass(t, writeFile(t, el{overlay, "OW", even("pt 王 x")}), n)
	})
}

// TestUnknownVRIsDecodedAsText guards that a value of VR UN is decoded as
// text under the file's character set: a name in ISO 2022 IR 87 held as
// UN is found in its decoded text, not in its raw bytes, which match no
// form of the default character set the file was received in.
func TestUnknownVRIsDecodedAsText(t *testing.T) {
	jis := mustEncode(t, "山田", "", "ISO 2022 IR 87")
	p := writeFile(t, el{0x00080005, "CS", `\ISO 2022 IR 87`}, el{0x00090010, "LO", "ACME 1.0"},
		el{0x00091002, "UN", append(jis, ' ', ' ')})
	rep, err := run(t, p, needle(engine.NeedleName, "山田^XYZW"))
	findRaw(t, rep, err, "00091002", false)
}

// TestItemsInheritTheirCharacterSet guards that an item with no Specific
// Character Set of its own is decoded by its parent's (PS3.3
// C.12.1.1.2): a Cyrillic name in an item of a file of ISO_IR 144 is
// found, though no raw form of it in UTF-8 or Latin-1 matches.
func TestItemsInheritTheirCharacterSet(t *testing.T) {
	item := build(t, el{0x00081030, "LO", mustEncode(t, "ИВАНОВ", "ISO_IR 144")})
	p := writeFile(t, el{0x00080005, "CS", "ISO_IR 144"}, el{0x00081115, "SQ", []*dataset.Dataset{item}})
	rep, err := run(t, p, needle(engine.NeedleName, "Иванов^Петр"))
	findRaw(t, rep, err, "00081115[0]/00081030", false)
}

// TestChecksQuietly guards that a check logs nothing to the DICOM
// library's logger: a file declaring a character set the library does not
// know, a value of VR UN, and forms its received character set cannot
// write would each log a line per file otherwise.
func TestChecksQuietly(t *testing.T) {
	var logged bytes.Buffer
	prev := config.GetLogger()
	config.SetLogger(slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { config.SetLogger(prev) })
	p := writeFile(t, el{0x00080005, "CS", "ISO_IR 999"}, el{0x00090010, "LO", "ACME 1.0"},
		el{0x00091002, "UN", []byte("pt \xcd\xf5 x ")}, el{0x00204000, "LT", "Müller"})
	if _, err := checkCS(t, p, "ISO_IR 100", needle(engine.NeedleName, "王^小明"), needle(engine.NeedleName, "MÜLLER^HANS")); err == nil {
		t.Fatal("the name passed")
	}
	if logged.Len() != 0 {
		t.Errorf("logged: %s", logged.String())
	}
}

// TestJapaneseDescriptionsAreNotNames guards the raw forms of a 7-bit set
// of two-byte characters (ISO 2022 IR 87), whose bytes are ordinary ASCII
// letters (林 is "NS", 東 "El"): they are matched only inside a run the
// file shifted into the set, so English descriptions of a Japanese file
// are not taken for names, while a name written in the set, in a private
// binary value, is found.
func TestJapaneseDescriptionsAreNotNames(t *testing.T) {
	cs := `\ISO 2022 IR 87`
	for pn, text := range map[string]string{"林^太郎": "CT CHEST TRANSVERSE", "東^健": "CT ABDOMEN PELVIS", "李^明": "M{ HEAD"} {
		p := writeFile(t, el{0x00080005, "CS", cs}, el{0x00081030, "LO", text}, el{overlay, "OW", even(" " + text + " ")})
		if _, err := checkCS(t, p, cs, needle(engine.NeedleName, pn)); err != nil {
			t.Errorf("%s in %q: %v", pn, text, err)
		}
	}
	jis := mustEncode(t, "患者林", "", "ISO 2022 IR 87")
	p := writeFile(t, el{0x00090010, "LO", "ACME 1.0"}, el{0x00091003, "OW", append(jis, ' ', ' ')})
	rep, err := checkCS(t, p, cs, needle(engine.NeedleName, "林^太郎"))
	findRaw(t, rep, err, "00091003", true)
}
