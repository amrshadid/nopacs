package sidecar

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

// TestPixelRiskRoundTrips guards the accepted pixel risk a line records, so
// a catalog made again from the metadata refuses to send what the one
// before refused: written only when set, read back as written, kept in a
// sidecar of the older version too (it is not dropped as a field of a
// later version is), and refused without a profile, on writing and on
// reading.
func TestPixelRiskRoundTrips(t *testing.T) {
	risky := inst(t, "1.2.3", false)
	risky.Profile, risky.PixelRiskAccepted = "research@3", true
	clean := inst(t, "1.2.4", true)
	clean.Profile = "research@3"
	var b bytes.Buffer
	w := NewWriter(&b)
	for _, in := range []Instance{risky, clean} {
		if err := w.Add(in); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	text := b.String()
	if strings.Count(text, `"pixel_risk_accepted":true`) != 1 {
		t.Errorf("the field is written for other lines than the one that has it:\n%s", text)
	}
	if VersionOf([]byte(text[:HeadLength])) != VersionFrames {
		t.Errorf("a sidecar holding the field is version %d, want %d: the field belongs to no later version", VersionOf([]byte(text[:HeadLength])), VersionFrames)
	}
	got := readAll(t, b.Bytes())
	if !got[0].PixelRiskAccepted || got[1].PixelRiskAccepted {
		t.Errorf("read back: %v and %v", got[0].PixelRiskAccepted, got[1].PixelRiskAccepted)
	}
	if kept := risky.withoutV3(); !kept.PixelRiskAccepted {
		t.Error("a line joining an older sidecar loses its accepted pixel risk")
	}
	bad := inst(t, "1.2.5", false)
	bad.PixelRiskAccepted = true
	if err := NewWriter(&bytes.Buffer{}).Add(bad); !errors.Is(err, ErrFormat) {
		t.Errorf("an accepted pixel risk without a profile written: %v, want ErrFormat", err)
	}
	tampered := strings.Replace(text, `"profile":"research@3",`, "", 1)
	r := NewReader(strings.NewReader(tampered))
	if _, _, err := r.Next(); !errors.Is(err, ErrFormat) {
		t.Errorf("a line with an accepted pixel risk and no profile read: %v, want ErrFormat", err)
	}
}
