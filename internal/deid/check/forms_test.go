package check

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// lt writes a file whose Image Comments (0020,4000) are s, in UTF-8.
func lt(t *testing.T, s string) string {
	t.Helper()
	return writeFile(t, el{0x00080005, "CS", "ISO_IR 192"}, el{0x00204000, "LT", s})
}

// TestNameForms guards that a name is found in each form people write it
// in, in any case, and by each part; a name of three short parts by its
// "Given Middle Family" form alone; and a name's component groups.
func TestNameForms(t *testing.T) {
	n := needle(engine.NeedleName, "DOE^JOHN^PAUL")
	for _, s := range []string{"DOE^JOHN^PAUL", "John Doe", "doe john", "Doe, John", "DOE,JOHN", "John Paul Doe",
		"met JOHN today", "Paul was there"} {
		t.Run(s, func(t *testing.T) {
			mustFind(t, lt(t, s), KindName, "00204000", n)
		})
	}
	t.Run("middle name", func(t *testing.T) {
		mustFind(t, lt(t, "seen by Bo Cy Al today"), KindName, "00204000", needle(engine.NeedleName, "Al^Bo^Cy"))
	})
	t.Run("short parts only as words", func(t *testing.T) {
		for pn, text := range map[string]string{"LE^AN": "SCAN LEFT KNEE", "LI^AL": "DELI ALSO", "NG^AN": "SCAN NG TUBE"} {
			mustPass(t, lt(t, text), needle(engine.NeedleName, pn))
		}
		mustFind(t, lt(t, "seen by An Le today"), KindName, "00204000", needle(engine.NeedleName, "LE^AN"))
		mustFind(t, lt(t, "LE, AN"), KindName, "00204000", needle(engine.NeedleName, "LE^AN"))
	})
	t.Run("component groups", func(t *testing.T) {
		g := needle(engine.NeedleName, "Yamada^Tarou=山田^太郎=やまだ^たろう")
		for _, s := range []string{"Tarou Yamada", "山田太郎", "たろう", "患者 山田"} {
			mustFind(t, lt(t, s), KindName, "00204000", g)
		}
	})
}

// TestDateForms guards that a date is found in each written form: the
// numeric ones with and without leading zeros and with a two-digit year,
// the month's name, its three letters, with a dot, the day as an ordinal,
// the Japanese form, and as the date of a date and time; and that a form
// standing for another date is not taken for it.
func TestDateForms(t *testing.T) {
	n := needle(engine.NeedleDate, "20240305")
	for _, s := range []string{"20240305", "2024-03-05", "2024/03/05", "2024.03.05", "05/03/2024", "03/05/2024",
		"05.03.2024", "05-03-2024", "3/5/2024", "5.3.2024", "2024-3-5", "03/05/24", "5/3/24", "5 March 2024",
		"March 5, 2024", "05-Mar-2024", "5MAR2024", "mar 05 2024", "Mar. 5, 2024", "5th March 2024",
		"March 5th, 2024", "2024年3月5日", "at 20240305101500.000", "on 2024-03-05T10:15", "2024年03月05日",
		"2024년 3월 5일", "2024. 3. 5.", "5 Mar 24", "id 05032024", "03052024", "ID05032024"} {
		t.Run(s, func(t *testing.T) {
			mustFind(t, lt(t, s), KindDate, "00204000", n)
		})
	}
	for _, s := range []string{"15 March 2024", "3/15/2024", "13/5/2024", "05/03/2410", "2024-3-51", "105032024", "030520241"} {
		t.Run("not "+s, func(t *testing.T) {
			mustPass(t, lt(t, s), n)
		})
	}
	t.Run("unknown month or day", func(t *testing.T) {
		mustFind(t, lt(t, "on 1957-00-00"), KindDate, "00204000", needle(engine.NeedleDate, "19570000"))
		mustFind(t, lt(t, "born 1957"), KindDate, "00204000", needle(engine.NeedleDate, "19570000"))
		mustFind(t, lt(t, "born March 1957"), KindDate, "00204000", needle(engine.NeedleDate, "19570300"))
		mustFind(t, lt(t, "born 1957, male"), KindDate, "00204000", needle(engine.NeedleDate, "195700"))
	})
	t.Run("date and time needle", func(t *testing.T) {
		mustFind(t, lt(t, "on 2024-03-05"), KindDate, "00204000", needle(engine.NeedleDate, "20240305101500"))
	})
	t.Run("partial dates", func(t *testing.T) {
		y := needle(engine.NeedleDate, "1957")
		mustFind(t, lt(t, "born 1957, male"), KindDate, "00204000", y)
		mustFind(t, lt(t, "1957年生まれ"), KindDate, "00204000", y)
		// The year of a longer date, or of a remapped number, is not the
		// year alone.
		mustPass(t, lt(t, "on 1957-03-05 and 19571 and 2.25.1957 and A1957"), y)
		ym := needle(engine.NeedleDate, "195703")
		mustFind(t, lt(t, "since March 1957"), KindDate, "00204000", ym)
		mustFind(t, lt(t, "since 3/1957"), KindDate, "00204000", ym)
		mustPass(t, lt(t, "on 1957-03-05"), ym)
	})
}

// TestUIDAndIdentifierEdges guards that a needle beginning or ending with
// a digit is not found inside a longer number, such as the remapped UID
// that replaced it, while it is found standing alone; that a UID is not
// found as the end of a longer UID, but is as its start (a UID made by
// appending to the original shows the original); and that a UID the
// standard defines is no needle, listed by the library or not.
func TestUIDAndIdentifierEdges(t *testing.T) {
	id := needle(engine.NeedleText, "778812")
	mustPass(t, writeFile(t, el{0x0020000D, "UI", "2.25.97788123"}), id)
	mustFind(t, writeFile(t, el{0x00204000, "LT", "id 778812."}), KindIdentifier, "00204000", id)
	uid := needle(engine.NeedleUID, "1.2.3.44")
	mustPass(t, writeFile(t, el{0x0020000D, "UI", "1.2.3.445"}), uid)
	mustFind(t, writeFile(t, el{0x0020000D, "UI", "1.2.3.44.5"}), KindUID, "0020000D", uid)
	mustPass(t, writeFile(t, el{0x0020000D, "UI", "9.1.2.3.44"}), uid)
	mustFind(t, writeFile(t, el{0x0020000D, "UI", "1.2.3.44"}), KindUID, "0020000D", uid)
	mustFind(t, writeFile(t, el{0x00204000, "LT", "see 1.2.3.44."}), KindUID, "00204000", uid)
	for _, u := range []string{testSOPClass, "1.2.840.10008.5.1.4.1.1.481.2"} {
		if rep := mustPass(t, writeFile(t), needle(engine.NeedleUID, u)); rep.Searched != 0 {
			t.Errorf("the standard's UID %s was searched for: %+v", u, rep)
		}
	}
}

// TestFolding guards that letters match in any case beyond ASCII,
// precomposed (É and é, the Kelvin sign and K), that full-width letters and
// digits are their ASCII ones, and that runs of white space are one space;
// and that a decomposed é (e and a combining accent) is the precomposed
// one, as both are compared in NFC.
func TestFolding(t *testing.T) {
	mustFind(t, lt(t, "émile"), KindName, "00204000", needle(engine.NeedleName, "ÉMILE^DUPONT"))
	mustFind(t, lt(t, "id K1234"), KindIdentifier, "00204000", needle(engine.NeedleText, "k1234"))
	mustFind(t, lt(t, "ＤＯＥ ＪＯＨＮ"), KindName, "00204000", needle(engine.NeedleName, "DOE^JOHN"))
	mustFind(t, lt(t, "受付１２３４５６７８"), KindIdentifier, "00204000", needle(engine.NeedleText, "12345678"))
	mustFind(t, lt(t, "MRN  \t778812"), KindIdentifier, "00204000", needle(engine.NeedleText, "MRN 778812"))
	mustFind(t, lt(t, "MRN 778812"), KindIdentifier, "00204000", needle(engine.NeedleText, "MRN   778812"))
	mustFind(t, lt(t, "émile"), KindName, "00204000", needle(engine.NeedleName, "ÉMILE^XY"))
}

// TestMultiValuedNeedle guards that a needle of several values is found
// whole, backslash included, when each of its values is too short alone.
func TestMultiValuedNeedle(t *testing.T) {
	n := needle(engine.NeedleText, `AB\CD`)
	mustFind(t, lt(t, `codes AB\CD`), KindIdentifier, "00204000", n)
}
