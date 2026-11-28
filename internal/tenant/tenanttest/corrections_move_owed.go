package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testMoveOwedIdentity guards the list of the identity rewrites a move
// asked for and not yet made (tenant.Moves.OwedIdentityRewrites): a move
// still prepared, or one that asked none, is not listed; a committed one
// that asked is, until IdentityRewritten records it made or IdentityRefused
// records it refused for good with its fixed phrase, which a read answers.
func testMoveOwedIdentity(e *cenv) {
	e.setupMove()
	m := e.p.(tenant.Moves)
	owed := func(when string, want ...string) {
		e.Helper()
		got, err := m.OwedIdentityRewrites(e.ctx, "", 100)
		e.ok("OwedIdentityRewrites "+when, err)
		if len(got) != len(want) {
			e.Fatalf("the rewrites owed %s are %d, want %d", when, len(got), len(want))
		}
		for i, c := range got {
			if c.ID != want[i] || !c.RewriteIdentity {
				e.Fatalf("the rewrite owed %s is %+v, want %s", when, c, want[i])
			}
		}
	}
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	p, err := e.p.PreviewMove(e.ctx, target, "sub-02", at(500))
	e.ok("PreviewMove", err)
	c, made, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(500), Kind: record.KindMove, Target: target,
		ToLabel: "sub-02", Reason: "filed under the wrong patient", Confirm: p.Confirm, RewriteIdentity: true, Now: at(501)}, e.audit(), nil)
	e.ok("PrepareCorrection of a move asking its identity rewritten", err)
	if !made || !c.RewriteIdentity {
		e.Fatalf("a new move asking its identity rewritten is %+v", c)
	}
	owed("while the move is prepared")
	e.commitMove(c, 510)
	owed("once the move is published", c.ID)
	_, err = m.OwedIdentityRewrites(e.ctx, "", 0)
	e.wantKind("a limit of none", err, fault.Invalid)
	e.wantKind("a refusal for a phrase of the caller's", m.IdentityRefused(e.ctx, c.ID, "a value"), fault.Invalid)
	owed("after a refusal that was not recorded", c.ID)
	e.ok("IdentityRefused", m.IdentityRefused(e.ctx, c.ID, tenant.IdentityRefusedProfile))
	owed("once the rewrite is refused for good")
	got, err := e.p.Correction(e.ctx, c.ID)
	e.ok("Correction", err)
	if got.RewriteIdentity || got.IdentityRefused != tenant.IdentityRefusedProfile {
		e.Fatalf("a refused rewrite is answered as %+v", got)
	}
	e.wantKind("a refusal of a correction the tenant lacks", m.IdentityRefused(e.ctx, "01jd5x7k2m3n4p5q6r7s8t9v0w",
		tenant.IdentityRefusedProfile), fault.NotFound)
}
