package engine

import "testing"

// TestPixelWorkRiskAccepted guards what a reception records as an accepted
// pixel risk: only an image at risk of burned-in text that an accept rule
// let through as it is; not one at risk that a redact or detect rule
// filled, not one an accept rule matched while it was not at risk, and not
// a file without pixels (nil work).
func TestPixelWorkRiskAccepted(t *testing.T) {
	for _, c := range []struct {
		name string
		w    *PixelWork
		want bool
	}{
		{"no pixels", nil, false},
		{"at risk, accepted", &PixelWork{AtRisk: true, RiskCause: "modality", Action: pixelAccept}, true},
		{"at risk, redacted", &PixelWork{AtRisk: true, RiskCause: "modality", Action: pixelRedact}, false},
		{"at risk, detected", &PixelWork{AtRisk: true, RiskCause: "secondary_capture", Action: pixelDetect}, false},
		{"not at risk, accepted", &PixelWork{Action: pixelAccept}, false},
		{"not at risk, no rule", &PixelWork{}, false},
	} {
		if got := c.w.RiskAccepted(); got != c.want {
			t.Errorf("%s: RiskAccepted %v, want %v", c.name, got, c.want)
		}
	}
}
