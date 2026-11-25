package servecmd

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/checkedread"
)

// TestReadCountersStartEmpty guards the metrics of the reads replicas
// serve: none is given before a replica served a read, so a scrape of a
// server with no replica is unchanged, and a nil count gives none.
func TestReadCountersStartEmpty(t *testing.T) {
	if got := readCounters(&checkedread.Counts{}); len(got) != 0 {
		t.Errorf("an empty count gives %+v", got)
	}
	if got := readCounters(nil); len(got) != 0 {
		t.Errorf("a nil count gives %+v", got)
	}
}
