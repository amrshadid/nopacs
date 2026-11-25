package postgres

import (
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// retentionKeep is a rule of the whole tenant that keeps every session ten
// years.
var retentionKeep = tenant.RetentionRule{Name: "keep", KeepAtLeast: tenant.RetentionPeriod{Years: 10}, Action: tenant.RetentionReview}

// identifiedOne opens, identifies and claims a reception of one file of
// study 0 at receiver "a" on "store", and returns it with its file.
func (e *cenv) identifiedOne() (tenant.Reception, []tenant.ReceivedFile) {
	e.t.Helper()
	e.storage("store")
	e.receiver("a", "store", 11112)
	r, fs := e.identified("a", cUID(1, 0), "store", "sub-01", "ses-1", func(r tenant.Reception) []tenant.ReceivedFile {
		return []tenant.ReceivedFile{cFile(r, 0, 0)}
	})
	_, err := e.p().ClaimInstances(e.ctx, r.ID, []tenant.InstanceClaim{claimOfFile(fs[0])}, cT0, nil)
	e.ok("ClaimInstances", err)
	return r, fs
}

// TestCommitBesideRuleChangeKeepsTheKeep guards a keep-at-least added
// while a commit makes a new session: at read committed the rule's change
// cannot see the commit's session before it commits, so if the commit read
// the rules before the change and the change stored every session's times
// but the uncommitted one, the session would be stored with no keep and a
// person's delete would pass. The commit holds retention's row shared
// before it reads the rules, and the change takes it for update, so one
// waits for the other. The order is made so on every run: the commit is
// held after it stored its session's times, at the catalog's totals, whose
// row an open transaction of the test has locked; the rule is added then,
// and the test lets the commit go once the change waits (or has ended, as
// it would if nothing ordered them). The session must then be kept.
func TestCommitBesideRuleChangeKeepsTheKeep(t *testing.T) {
	e := newCenv(t, 2, 4)
	r, fs := e.identifiedOne()

	held, err := e.db.Begin(e.ctx)
	e.ok("begin", err)
	defer func() { _ = held.Rollback(e.ctx) }()
	if _, err := held.Exec(e.ctx, `SELECT 1 FROM catalog_totals FOR UPDATE`); err != nil {
		t.Fatal(err)
	}
	committed := make(chan error, 1)
	go func() {
		_, err := e.stores[1].CommitScan(e.ctx, scanCommit(r, "sub-01", "ses-1", "scan-1", "store", fs))
		committed <- err
	}()
	e.waitBlocked(1)
	ruled := make(chan error, 1)
	go func() {
		_, err := e.stores[0].AddRetentionRule(e.ctx, retentionKeep, tenant.RetentionChange{Now: cT0.Add(5 * time.Minute),
			Notice: tenant.DefaultShortenNotice}, e.audit(), nil)
		ruled <- err
	}()
	var ruleErr error
	ruleEnded := false
	for deadline := time.Now().Add(30 * time.Second); !ruleEnded; {
		select {
		case ruleErr = <-ruled:
			ruleEnded = true
		default:
		}
		if ruleEnded || e.scalar(`SELECT count(*) FROM pg_stat_activity WHERE application_name = $1 AND wait_event_type = 'Lock'`,
			e.app) >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the rule's change neither waits nor ends after 30s")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := held.Rollback(e.ctx); err != nil {
		t.Fatal(err)
	}
	e.ok("CommitScan", <-committed)
	if !ruleEnded {
		ruleErr = <-ruled
	}
	e.ok("AddRetentionRule", ruleErr)
	got, err := e.p().SessionRetention(e.ctx, cUID(1, 0))
	e.ok("SessionRetention", err)
	if want := got.Start.AddDate(10, 0, 0); !got.KeepUntil.Equal(want) {
		t.Errorf("a session committed beside the keep rule's addition is kept until %v, want %v: the keep was lost", got.KeepUntil, want)
	}
}

// TestPersonDeleteReadsTheRulesAgain guards a person's delete of a session
// whose stored keep is missing while a rule keeps it: the prepare reads the
// session's times again from the rules as they stand and refuses the
// delete (RETENTION_MINIMUM) with the keep the rules give, so a keep lost
// from the stored row never lets a delete through.
func TestPersonDeleteReadsTheRulesAgain(t *testing.T) {
	e := newCenv(t, 1, 2)
	r, fs := e.identifiedOne()
	_, err := e.p().CommitScan(e.ctx, scanCommit(r, "sub-01", "ses-1", "scan-1", "store", fs))
	e.ok("CommitScan", err)
	for i, s := range []struct {
		from reception.State
		ev   reception.Event
	}{{reception.Identified, reception.Archive}, {reception.Archived, reception.Verify}, {reception.Verified, reception.Finish}} {
		_, err = e.p().StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: s.from, Event: s.ev, At: cT0.Add(time.Duration(4+i) * time.Minute)}, nil)
		e.ok("StepReception "+string(s.ev), err)
	}
	_, err = e.p().AddRetentionRule(e.ctx, retentionKeep, tenant.RetentionChange{Now: cT0.Add(5 * time.Minute),
		Notice: tenant.DefaultShortenNotice}, e.audit(), nil)
	e.ok("AddRetentionRule", err)
	stored, err := e.p().SessionRetention(e.ctx, cUID(1, 0))
	e.ok("SessionRetention", err)
	if _, err := e.db.Exec(e.ctx, `UPDATE retention_sessions SET keep_until = NULL, due_at = NULL, rule = '', action = ''`); err != nil {
		t.Fatal(err)
	}
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	now := cT0.Add(10 * time.Minute)
	p, err := e.p().PreviewDelete(e.ctx, target, now)
	e.ok("PreviewDelete", err)
	_, _, err = e.p().PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: "01jd0000000000000000000001", Kind: record.KindDelete,
		Target: target, Reason: "consent withdrawn", Confirm: p.Confirm, Now: now}, e.audit(), nil)
	var kept *tenant.RetentionMinimum
	if !errors.As(err, &kept) || !kept.KeepUntil.Equal(stored.KeepUntil) {
		t.Fatalf("a person's delete of a session whose stored keep is gone answers %v, want RETENTION_MINIMUM until %v", err,
			stored.KeepUntil)
	}
}
