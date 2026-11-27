package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/amrshadid/go-dicom/dataset"
)

// TestCleanCostIsBounded guards the bound of clean's work: a file of
// thousands of items, each a name removed and a text cleaned, which once
// took minutes, is refused as too_large at once and left as it was; one
// whose cleans fit is cleaned; and clean stops when its context is
// canceled.
func TestCleanCostIsBounded(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("names", vrSel("PN"), aRemove),
		rule("text", kw("TextValue"), aClean),
	)
	many := func(n int) *dataset.Dataset {
		items := make([]*dataset.Dataset, n)
		for i := range items {
			items[i] = build(t,
				el{0x0040A123, "PN", fmt.Sprintf("Family%05d^Given%05d", i, i)},
				el{0x0040A160, "UT", fmt.Sprintf("seen by Family%05d with Given%05d and others", i, i)},
			)
		}
		return build(t, el{tContentSequence, "SQ", items})
	}
	big := many(8000)
	before := writeFile(t, big)
	start := time.Now()
	_, err := en.Apply(context.Background(), big, Reception{})
	var f *Failure
	if !errors.As(err, &f) || f.Cause != CauseTooLarge {
		t.Fatalf("8000 items: %v", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("refusing took %v", d)
	}
	if !bytes.Equal(before, writeFile(t, big)) {
		t.Error("a refused file was changed")
	}

	small := many(50)
	apply(t, en, small)
	got, _ := text(item(t, small, tContentSequence, 7), 0x0040A160)
	if got != "seen by  with  and others" {
		t.Errorf("cleaned %q", got)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	terms, _ := cleanTerms([]Needle{{Kind: NeedleText, Value: "abcdef"}})
	if _, err := newCleaner(terms, maxCleanWork).clean(ctx, "xx abcdef xx"); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled clean: %v", err)
	}
	if _, err := newCleaner(terms, 10).clean(context.Background(), "a text longer than the budget"); !errors.Is(err, errCleanTooLarge) {
		t.Errorf("a clean past its budget: %v", err)
	}
}

// TestShortAndCJKNamesAreCleaned guards clean on names of every script: a
// Japanese name of two-character parts, written with or without a space,
// is cut wherever it appears; a Korean name's one-syllable family name
// too; a Latin part of one or two letters only as a whole word, in any
// case; and a whole name in "Given Family" order, with the space between.
func TestShortAndCJKNamesAreCleaned(t *testing.T) {
	en := newTestEngine(t, testKeys, Options{},
		rule("names", vrSel("PN"), aRemove),
		rule("desc", kw("StudyDescription"), aClean),
		rule("comments", kw("PatientComments"), aClean),
		rule("image", kw("ImageComments"), aClean),
	)
	ds := build(t,
		el{tSpecificCharacterSet, "CS", "ISO_IR 192"},
		el{tPatientName, "PN", "Yamada^Tarou=山田^太郎=やまだ^たろう"},
		el{tReferring, "PN", "Li^Al"},
		el{0x00081060, "PN", "김^민수"},
		el{tStudyDesc, "LO", "山田 太郎 の頭部MRI、やまだたろう"},
		el{tPatientComments, "LT", "Dr LI saw al; Alpha clinic, Tarou Yamada, 김 민수 님"},
		el{0x00204000, "LT", "山田太郎さん"},
	)
	rec := apply(t, en, ds)
	for tg, want := range map[uint32]string{
		tStudyDesc:       " の頭部MRI、",
		tPatientComments: "Dr  saw ; Alpha clinic, ,  님",
		0x00204000:       "さん",
	} {
		if got := mustText(t, ds, tg); got != want {
			t.Errorf("%08X cleaned %q, want %q", tg, got, want)
		}
	}
	if v, _ := text(ds, tSpecificCharacterSet); v != "ISO_IR 192" {
		t.Errorf("the character set of the text kept: %q", v)
	}
	if len(rec.Needles) == 0 {
		t.Error("no needles")
	}
}

// TestCleanSeesThroughInvisibleCharacters guards clean against a removed
// value split by a code point a reader does not see (a soft hyphen, a
// zero-width space), which the check after's folding drops: the value is
// cut with what splits it, as the check would find it.
func TestCleanSeesThroughInvisibleCharacters(t *testing.T) {
	shy, zwsp := string(rune(0x00AD)), string(rune(0x200B))
	terms, ok := cleanTerms([]Needle{{Kind: NeedleText, Value: "PAT001"}, {Kind: NeedleName, Value: "Zebulonquist^Ann"}})
	if !ok {
		t.Fatal("too many terms")
	}
	in := "id PAT" + shy + "001, seen by Zebu" + zwsp + "lonquist."
	got, err := newCleaner(terms, maxCleanWork).clean(context.Background(), in)
	if err != nil || got != "id , seen by ." {
		t.Errorf("clean %q = %q, %v", in, got, err)
	}
}

// TestCleanIsRuneSafeAndRepeats guards clean's handling of text: a term
// cut that joins the text around it into the term again is cut again; a
// letter whose lower case is longer in bytes (İ) moves no offset; case
// folding matches the Kelvin sign to K; and dates written by people are
// cut in their common forms.
func TestCleanIsRuneSafeAndRepeats(t *testing.T) {
	cases := []struct {
		needles []Needle
		in      string
		want    string
	}{
		{[]Needle{{Kind: NeedleText, Value: "PAT001"}}, "id PAPAT001T001 end", "id  end"},
		{[]Needle{{Kind: NeedleText, Value: "PAT001"}}, "İİİ pat001 İİ", "İİİ  İİ"},
		{[]Needle{{Kind: NeedleName, Value: "Kim^Jo"}}, "KIM and KİM and jo", " and KİM and "},
		{[]Needle{{Kind: NeedleDate, Value: "20240315"}}, "on 2024-03-15, 15.03.2024, 03/15/2024 and 20240315", "on , ,  and "},
	}
	for _, c := range cases {
		terms, ok := cleanTerms(c.needles)
		if !ok {
			t.Fatal("too many terms")
		}
		got, err := newCleaner(terms, maxCleanWork).clean(context.Background(), c.in)
		if err != nil || got != c.want {
			t.Errorf("clean %q = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
}
