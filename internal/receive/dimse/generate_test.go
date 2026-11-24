package dimse_test

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// newGenerator returns the generator of a DICOM file of any size, made from
// the public file MR_small.dcm (testkit.Generator).
func newGenerator(t testing.TB) *testkit.Generator {
	t.Helper()
	return testkit.NewGenerator(t, corpusBytes(t, "MR_small.dcm"))
}
