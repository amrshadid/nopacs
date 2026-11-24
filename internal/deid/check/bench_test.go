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
