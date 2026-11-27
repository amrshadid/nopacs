package check

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"os"
	"testing"

	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/deid/profile"
)

// benchFile writes a file holding a private OB value of n pseudo-random
// bytes, a value a profile may keep, and returns its bytes.
func benchFile(b *testing.B, n int) []byte {
	b.Helper()
	r := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: test data, not a secret.
	v := make([]byte, n)
	for i := 0; i+8 <= n; i += 8 {
		binary.LittleEndian.PutUint64(v[i:], r.Uint64())
	}
	p := writeFile(b, el{0x00090010, "LO", "ACME 1.0"}, el{0x00091003, "OB", v},
		el{0x00204000, "LT", "nothing identifying here"})
	data, err := os.ReadFile(p) //nolint:gosec // G304: the test's own temporary file.
	if err != nil {
		b.Fatal(err)
	}
	return data
}

// dateNeedles returns n distinct dates, each with its written forms.
func dateNeedles(n int) engine.Needles {
	out := make(engine.Needles, 0, n)
	for i := range n {
		out = append(out, needle(engine.NeedleDate, fmt.Sprintf("19%02d%02d%02d", 30+i%60, 1+i%12, 1+i%28)))
	}
	return out
}

// BenchmarkCheckWithList measures BenchmarkCheck's 1 MB file and 30 dates
// with the needle of a list of 10,000 terms besides: the lists' automata
// are built once, on the first file, as a profile version's are, so the
// loop measures the passes over the text and the raw bytes they add.
func BenchmarkCheckWithList(b *testing.B) {
	terms := make([]string, profile.MaxListTerms)
	for i := range terms {
		terms[i] = fmt.Sprintf("Given%05d Family%05d", i, len(terms)-i)
	}
	l, problems := profile.CompileTextLists([]profile.TextList{{Name: "staff", Version: 1, Terms: terms}})
	if problems != nil {
		b.Fatal(problems)
	}
	data := benchFile(b, 1<<20)
	needles := append(dateNeedles(30), engine.Needle{Kind: engine.NeedleList, Lists: l})
	c := New(Options{})
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		if _, err := c.Check(context.Background(), bytes.NewReader(data), int64(len(data)), needles, nil); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCheck measures a check of a file with a 20 MB kept binary value
// for many date needles, each searched in its written forms in the text
// and in the raw bytes.
func BenchmarkCheck(b *testing.B) {
	for _, tc := range []struct {
		name    string
		size    int
		needles int
	}{
		{"20MB-300dates", 20 << 20, 300},
		{"1MB-30dates", 1 << 20, 30},
	} {
		b.Run(tc.name, func(b *testing.B) {
			data := benchFile(b, tc.size)
			needles := dateNeedles(tc.needles)
			c := New(Options{})
			b.SetBytes(int64(len(data)))
			b.ResetTimer()
			for b.Loop() {
				if _, err := c.Check(context.Background(), bytes.NewReader(data), int64(len(data)), needles, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
