package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/amrshadid/go-dicom/dataelem"
	"github.com/amrshadid/go-dicom/tag"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// benchList is a list of n distinct terms of two words, as a list of
// staff names is, one of them written into the benchmark's text.
func benchList(b *testing.B, n int) *profile.TextLists {
	b.Helper()
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: benchTerms(n)}})
	if problems != nil {
		b.Fatal(problems)
	}
	return l
}

// benchTerms are n distinct terms of two words.
func benchTerms(n int) []string {
	terms := make([]string, n)
	for i := range terms {
		terms[i] = fmt.Sprintf("Given%05d Family%05d", i, n-i)
	}
	return terms
}

// BenchmarkApplyWithList measures BenchmarkApply's files with a list of
// 10,000 terms that every clean rule of the corpus profile names, and a
// Study Description and an Image Comments of 1 KiB written into each, one
// term among their words: the work a list adds to a reception.
func BenchmarkApplyWithList(b *testing.B) {
	rules := corpusRules()
	for _, r := range rules {
		if strings.HasPrefix(r.GetName(), "clean-") {
			r.Action = aCleanLists("staff")
		}
	}
	en := newDocEngine(b, testKeys, Options{}, &v1.ProfileDocument{
		TextLists:  []*v1.TextList{{Name: "staff", Version: 1, Terms: benchTerms(profile.MaxListTerms)}},
		Rules:      rules,
		PixelRules: []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the attributes are measured here"}},
	})
	words := strings.Repeat("contrast enhanced axial series follow-up ", 25)
	long := words[:512] + " Given04321 Family05679 " + words[:488]
	for _, name := range benchFiles {
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(receiveTestdata, name)
			for b.Loop() {
				b.StopTimer()
				ds, err := readPath(b, path)
				if err != nil {
					b.Fatal(err)
				}
				for _, t := range []uint32{tStudyDesc, tImageComments} {
					vr := "LO"
					if t == tImageComments {
						vr = "LT"
					}
					v := long
					if vr == "LO" {
						v = long[:64]
					}
					if err := ds.Add(dataelem.NewDataElement(tag.Tag(t), dataelem.VR(vr), []byte(v))); err != nil {
						b.Fatal(err)
					}
				}
				b.StartTimer()
				if _, err := en.Apply(context.Background(), ds, Reception{}); err != nil {
					b.Fatal(err)
				}
				if v, _ := text(ds, tImageComments); strings.Contains(v, "Given04321") {
					b.Fatal("the term stayed")
				}
			}
		})
	}
}

// BenchmarkCleanWithList measures clean of one text of 1 KiB, one term
// among its words, by a list of 10,000 terms.
func BenchmarkCleanWithList(b *testing.B) {
	l := benchList(b, profile.MaxListTerms)
	words := strings.Repeat("contrast enhanced axial series follow-up ", 25)
	text := words[:512] + " Given04321 Family05679 " + words[:488]
	b.SetBytes(int64(len(text)))
	for b.Loop() {
		out, err := newCleaner(nil, maxCleanWork).cleanWith(context.Background(), text, l, l.All())
		if err != nil || strings.Contains(out, "Given04321") {
			b.Fatal(err)
		}
	}
}

// BenchmarkCompileTextLists measures compiling a list of 10,000 terms,
// once per profile version.
func BenchmarkCompileTextLists(b *testing.B) {
	for b.Loop() {
		benchList(b, profile.MaxListTerms)
	}
}

// benchFiles are the receiver's public test files the engine's benchmarks
// de-identify: a CT and an MR image of a few kilobytes of header.
var benchFiles = []string{"ct5n-2062.dcm", "MR_small.dcm"}

// BenchmarkApply measures one file de-identified by the corpus profile
// (names removed, identifiers hashed, UIDs remapped, dates shifted,
// descriptions cleaned): the engine's share of a reception. Each loop
// reads the file again, outside the timer, since Apply changes it.
func BenchmarkApply(b *testing.B) {
	en := newDocEngine(b, testKeys, Options{}, &v1.ProfileDocument{
		Rules:      corpusRules(),
		PixelRules: []*v1.PixelRule{{Name: "accept-all", Action: &v1.PixelRule_Accept{Accept: &v1.AcceptPixels{}}, Reason: "the attributes are measured here"}},
	})
	for _, name := range benchFiles {
		b.Run(name, func(b *testing.B) {
			path := filepath.Join(receiveTestdata, name)
			for b.Loop() {
				b.StopTimer()
				ds, err := readPath(b, path)
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := en.Apply(context.Background(), ds, Reception{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
