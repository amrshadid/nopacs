package output_test

import (
	"strings"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/cli/output"
)

// TestQuarantineWarningWords guards the status line for a quarantine: no
// warning, or none sent, gives no line; each warning that stands names its
// setting and its figure; both stand together in one line.
func TestQuarantineWarningWords(t *testing.T) {
	old := durationpb.New(41 * 24 * time.Hour)
	for _, c := range []struct {
		name string
		q    *v1.QuarantineWarnings
		want []string
	}{
		{"none sent", nil, nil},
		{"nothing past", &v1.QuarantineWarnings{OldestAge: old, HeldBytes: 5 << 20}, nil},
		{"age", &v1.QuarantineWarnings{OldestAge: old, AgePassed: true}, []string{"41d", "quarantine.warn-age"}},
		{"bytes", &v1.QuarantineWarnings{HeldBytes: 5 << 20, BytesPassed: true}, []string{"5.0 MiB", "quarantine.warn-bytes"}},
		{"both", &v1.QuarantineWarnings{OldestAge: old, AgePassed: true, HeldBytes: 5 << 20, BytesPassed: true},
			[]string{"41d", "quarantine.warn-age", "5.0 MiB", "quarantine.warn-bytes"}},
	} {
		got := output.QuarantineWarning(c.q)
		if c.want == nil {
			if got != "" {
				t.Errorf("%s: the line is %q; want none", c.name, got)
			}
			continue
		}
		for _, w := range c.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s: the line %q lacks %q", c.name, got, w)
			}
		}
	}
}
