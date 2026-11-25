package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestPersonDeleteReadsTheRulesAgain guards a person's delete of a session
// whose stored keep is missing while a rule keeps it: the prepare reads the
// session's times again from the rules as they stand and refuses the
// delete (RETENTION_MINIMUM) with the keep the rules give, so a keep lost
// from the stored row never lets a delete through.
func TestPersonDeleteReadsTheRulesAgain(t *testing.T) {
	ctx := context.Background()
	p := openTenantTest(t, "T0000000000001").(*Tenant)
	fixtureSetup(t, p, "a")
	r, _, err := p.OpenReception(ctx, "a", "2.25.1.1", fixtureID("rc"), fixtureAt(1))
	if err != nil {
		t.Fatal(err)
	}
	f := fixtureFile(r, 0, 0)
	if _, err := p.AddReceivedFiles(ctx, []tenant.ReceivedFile{f}, fixtureAt(1)); err != nil {
		t.Fatal(err)
	}
	step := func(from reception.State, ev reception.Event, s int) {
		t.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: fixtureAt(s)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = "sub-01", "ses-1", "store"
		}
		if r, err = p.StepReception(ctx, st, nil); err != nil {
			t.Fatalf("StepReception %s: %v", ev, err)
		}
	}
	step(reception.Receiving, reception.Quiet, 100)
	step(reception.Complete, reception.Identify, 101)
	claim := tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	if _, err := p.ClaimInstances(ctx, r.ID, []tenant.InstanceClaim{claim}, fixtureAt(102), nil); err != nil {
		t.Fatal(err)
	}
	path := label.Path{Subject: "sub-01", Session: "ses-1", Scan: "scan-1"}
	key, err := layout.InstanceKey(path, f.SOPInstanceUID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.CommitScan(ctx, tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: f.SeriesUID, SubjectLabel: path.Subject,
		SessionLabel: path.Session, ScanLabel: path.Scan, Storage: "store", At: fixtureAt(103),
		Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size, Size: f.Size, ArchivedSHA256: f.SHA256}}}); err != nil {
		t.Fatal(err)
	}
	step(reception.Identified, reception.Archive, 104)
	step(reception.Archived, reception.Verify, 105)
	step(reception.Verified, reception.Finish, 106)
	keep := tenant.RetentionRule{Name: "keep", KeepAtLeast: tenant.RetentionPeriod{Years: 10}, Action: tenant.RetentionReview}
	if _, err := p.AddRetentionRule(ctx, keep, tenant.RetentionChange{Now: fixtureAt(200), Notice: tenant.DefaultShortenNotice},
		fixtureAudit(), nil); err != nil {
		t.Fatal(err)
	}
	stored, err := p.SessionRetention(ctx, r.StudyUID)
	if err != nil {
		t.Fatal(err)
	}
	mustExecTenant(t, p, `UPDATE retention_sessions SET keep_until = NULL, due_at = NULL, rule = '', action = ''`)
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	now := fixtureAt(300)
	pv, err := p.PreviewDelete(ctx, target, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = p.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: "01jd0000000000000000000001", Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: pv.Confirm, Now: now.Add(time.Second)}, fixtureAudit(), nil)
	var kept *tenant.RetentionMinimum
	if !errors.As(err, &kept) || !kept.KeepUntil.Equal(stored.KeepUntil) {
		t.Fatalf("a person's delete of a session whose stored keep is gone answers %v, want RETENTION_MINIMUM until %v", err,
			stored.KeepUntil)
	}
}
