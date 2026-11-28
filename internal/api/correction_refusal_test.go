package api

import (
	"testing"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestCorrectionMessageAnswersARefusedIdentityRewrite guards that the
// correction every method answers (GetCorrection, the list, a move's own
// answer) says why a move's identity rewrite was refused for good, and says
// nothing of it for a move whose rewrite was not refused.
func TestCorrectionMessageAnswersARefusedIdentityRewrite(t *testing.T) {
	c := tenant.Correction{ID: "01jd5x7k2m3n4p5q6r7s8t9v0w", Kind: record.KindMove, State: record.StateCommitted,
		IdentityRefused: tenant.IdentityRefusedProfile}
	if got := correctionMessage(c, false).GetIdentityRewriteRefused(); got != tenant.IdentityRefusedProfile {
		t.Fatalf("the answer says the identity rewrite was refused for %q, want %q", got, tenant.IdentityRefusedProfile)
	}
	c.IdentityRefused = ""
	if got := correctionMessage(c, false).GetIdentityRewriteRefused(); got != "" {
		t.Fatalf("the answer of a move not refused says %q", got)
	}
}
