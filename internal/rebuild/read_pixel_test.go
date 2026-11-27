package rebuild

import (
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestRebuiltInstanceKeepsPixelRisk guards that a catalog made again from
// the metadata keeps an accepted pixel risk a line records, with its
// profile version, so a sender refuses the instance after a rebuild or a
// restore as it did before; a line without it is rebuilt without it.
func TestRebuiltInstanceKeepsPixelRisk(t *testing.T) {
	sum := strings.Repeat("ab", 32)
	key := "sub-01/ses-1/mr/1.2.3.dcm"
	files := map[string]manifest.File{"1.2.3": {Key: key, Length: 10, Size: 10, SHA256: sum, SOPInstanceUID: "1.2.3"}}
	line := sidecar.Instance{SOPInstanceUID: "1.2.3", Key: key, Size: 10, SHA256: sum, Profile: "research@3", PixelRiskAccepted: true,
		Reception: "rc1", HeldAt: time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)}
	in, why := instanceOf(line, files)
	if why != "" || !in.PixelRiskAccepted || in.Profile != (tenant.ProfileRef{Name: "research", Version: 3}) {
		t.Fatalf("rebuilt %+v (%s), want the profile and the accepted pixel risk", in, why)
	}
	line.PixelRiskAccepted = false
	if in, _ := instanceOf(line, files); in.PixelRiskAccepted {
		t.Error("a line without an accepted pixel risk was rebuilt with one")
	}
}
