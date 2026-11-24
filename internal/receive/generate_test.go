package receive

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/testkit"
)

// newGenerator returns the generator of files for scale tests and
// benchmarks, made from the public file MR_small.dcm (testkit.Generator).
func newGenerator(t testing.TB) *testkit.Generator {
	t.Helper()
	return testkit.NewGenerator(t, corpus(t, "MR_small.dcm"))
}
