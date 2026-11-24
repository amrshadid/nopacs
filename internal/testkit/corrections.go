package testkit

import (
	"context"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// DeleteTarget deletes what target names through the tenant port, as a
// person's delete does: the preview, the prepared delete with its confirm
// token, and the publish, with the default grace period. It reports a
// failure with t.Error, so a goroutine other than the test's may call it.
func DeleteTarget(t testing.TB, p tenant.Pipeline, audit master.AuditEntry, target tenant.CorrectionTarget, now time.Time) {
	t.Helper()
	ctx := context.Background()
	preview, err := p.PreviewDelete(ctx, target, now)
	if err != nil {
		t.Error("PreviewDelete:", err)
		return
	}
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		t.Error("NewCorrectionID:", err)
		return
	}
	c, _, err := p.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: preview.Confirm, Now: now}, audit, nil)
	if err != nil {
		t.Error("PrepareCorrection of a delete:", err)
		return
	}
	if _, err := p.CommitCorrection(ctx, tenant.CorrectionCommit{ID: c.ID, Now: now, Grace: tenant.DefaultGrace}); err != nil {
		t.Error("CommitCorrection of a delete:", err)
	}
}
