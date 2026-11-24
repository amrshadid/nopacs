package servecmd

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/settingvalue"
)

// limitsSource is a settings store the test changes: a name not in values is
// NotFound, as the master answers for a setting never set; fail makes every
// read an error.
type limitsSource struct {
	values map[string]string
	fail   bool
	reads  int
}

func (s *limitsSource) Setting(_ context.Context, name string) (string, error) {
	s.reads++
	if s.fail {
		return "", fault.New(fault.Unavailable, "down")
	}
	if v, ok := s.values[name]; ok {
		return v, nil
	}
	return "", fault.New(fault.NotFound, "no value")
}

// TestReadLimitsFollowTheSettings guards the live bounds of open reads: the
// defaults before anything is set, a change seen once what was read is a
// second old and not before (so a burst of reads asks the store once), and
// the last good bounds kept when the store cannot be read.
func TestReadLimitsFollowTheSettings(t *testing.T) {
	src := &limitsSource{values: map[string]string{}}
	at := time.Unix(1000, 0)
	limits := readLimits(settingvalue.Reader{Master: src}, func() time.Time { return at })
	if c, s := limits(); c != 16 || s != 256 {
		t.Fatalf("defaults: %d, %d", c, s)
	}
	asked := src.reads
	src.values["read.per-caller"], src.values["read.per-server"] = "40", "64"
	if c, s := limits(); c != 16 || s != 256 || src.reads != asked {
		t.Fatalf("within the second: %d, %d, %d reads more", c, s, src.reads-asked)
	}
	at = at.Add(time.Second)
	if c, s := limits(); c != 40 || s != 64 {
		t.Fatalf("after the second: %d, %d", c, s)
	}
	src.fail = true
	src.values["read.per-caller"] = "8"
	at = at.Add(time.Second)
	if c, s := limits(); c != 40 || s != 64 {
		t.Fatalf("with the store down: %d, %d, want the last good", c, s)
	}
	src.fail = false
	at = at.Add(time.Second)
	if c, s := limits(); c != 8 || s != 64 {
		t.Fatalf("with the store back: %d, %d", c, s)
	}
}
