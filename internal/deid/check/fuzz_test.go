package check

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/deid/engine"
)

// FuzzCheck guards the reader path against hostile files: whatever the
// bytes, the check returns a pass, a residual, a refusal or an I/O error,
// never panics, and stays within the bounds it is given. The seeds are
// files of each shape the tests write: nested sequences, private and UN
// values, a value in the preamble, ISO 2022 text.
func FuzzCheck(f *testing.F) {
	inner := build(f, el{0x00081030, "LO", "seen by John Doe"})
	for _, p := range []string{
		writeFile(f),
		writeFile(f, el{0x00081115, "SQ", []*dataset.Dataset{inner}}),
		writeFile(f, el{0x00090010, "LO", "ACME 1.0"}, el{0x00091002, "UN", "MRN-778812"}),
		writeFile(f, el{0x00080005, "CS", `\ISO 2022 IR 87`}, el{0x00081050, "PN", "\x1b$B;3ED\x1b(B^\x1b$BB@O:\x1b(B"}),
	} {
		b, err := os.ReadFile(p) //nolint:gosec // G304: the test's own temporary file.
		if err != nil {
			f.Fatal(err)
		}
		f.Add(b)
		f.Add(b[128:])
	}
	needles := engine.Needles{
		needle(engine.NeedleName, "DOE^JOHN"), needle(engine.NeedleText, "MRN-778812"),
		needle(engine.NeedleDate, "20240315"), needle(engine.NeedleDate, "195703"),
		needle(engine.NeedleUID, "1.2.826.0.1.3680043.2.1125.77"), needle(engine.NeedleName, "山田^太郎"),
	}
	c := New(Options{MaxFileBytes: 1 << 20, MaxValueBytes: 1 << 16})
	f.Fuzz(func(t *testing.T, b []byte) {
		cs := `\ISO 2022 IR 87`
		rep, err := c.Check(context.Background(), bytes.NewReader(b), int64(len(b)), needles, &cs)
		var res *Residual
		var ref *Refusal
		switch {
		case err == nil:
			if rep.Total != 0 {
				t.Fatalf("a pass with findings: %+v", rep)
			}
		case errors.As(err, &res):
			if rep.Total == 0 || len(rep.Findings) == 0 {
				t.Fatalf("a residual without findings: %+v", rep)
			}
		case errors.As(err, &ref):
		default:
			t.Fatalf("an error that is no verdict: %v", err)
		}
	})
}
