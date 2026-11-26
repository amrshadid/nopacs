package tenanttest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// runRetention runs the tests of retention rules, the times stored per
// session, the clock, holds and disposals.
func runRetention(t *testing.T, open OpenEvents) {
	tests := []struct {
		name string
		fn   func(*tenv)
	}{
		{"RetentionRules", testRetentionRules},
		{"RetentionMatching", testRetentionMatching},
		{"RetentionClamp", testRetentionClamp},
		{"RetentionFollowsScansAndDeletes", testRetentionFollowsScans},
		{"RetentionImportStart", testRetentionImportStart},
		{"RetentionClock", testRetentionClock},
		{"RetentionClockSetBack", testRetentionClockSetBack},
		{"RetentionClockDrift", testRetentionClockDrift},
		{"PurgeClockStep", testPurgeClockStep},
		{"RetentionKeepsAtLeast", testRetentionKeepsAtLeast},
		{"QuarantineAging", testQuarantineAging},
		{"Holds", testHolds},
		{"HoldRefusesDelete", testHoldRefusesDelete},
		{"HoldStopsPurge", testHoldStopsPurge},
		{"HoldFollowsRelabel", testHoldFollowsRelabel},
		{"HoldRecords", testHoldRecords},
		{"HoldRecordsFollowTarget", testHoldRecordsFollowTarget},
		{"HoldRecordsOfAStorageGone", testHoldRecordsOfAStorageGone},
		{"DisposalPreview", testDisposalPreview},
		{"DisposalTokenKeepsItsTime", testDisposalTokenKeepsItsTime},
		{"DisposalDeletes", testDisposalDeletes},
		{"DisposalEnds", testDisposalEnds},
		{"DisposalAfterRelabel", testDisposalAfterRelabel},
		{"DisposalStopsWhenEnded", testDisposalStopsWhenEnded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := open(t, SuiteTenant)
			tt.fn(&tenv{cenv: &cenv{senv: &senv{eenv: &eenv{penv: &penv{env: &env{T: t, s: s, ctx: context.Background()}, p: s}, ev: s}}}})
		})
	}
}

// tenv is a retention test's store and its helpers; it has cenv's too.
type tenv struct {
	*cenv
}

// day is a day of the server's clock.
const day = 24 * time.Hour

// notice is the notice the suite changes rules with.
const notice = tenant.DefaultShortenNotice

// change returns a change of rules at second s.
func change(s int) tenant.RetentionChange { return tenant.RetentionChange{Now: at(s), Notice: notice} }

// changeAt returns a change of rules at t.
func changeAt(t time.Time) tenant.RetentionChange {
	return tenant.RetentionChange{Now: t, Notice: notice}
}

// rule returns a rule of the whole tenant that disposes after days, for
// review.
func rule(name string, keepDays, disposeDays int) tenant.RetentionRule {
	return tenant.RetentionRule{Name: name, KeepAtLeast: tenant.RetentionPeriod{Days: keepDays},
		DisposeAfter: tenant.RetentionPeriod{Days: disposeDays}, Action: tenant.RetentionReview}
}

// addRule adds r at ch, failing the test on an error.
func (e *tenv) addRule(r tenant.RetentionRule, ch tenant.RetentionChange) tenant.RetentionRule {
	e.Helper()
	out, err := e.p.AddRetentionRule(e.ctx, r, ch, e.audit(), nil)
	e.ok("AddRetentionRule", err)
	return out
}

// sessionTimes reads the retention row of study n.
func (e *tenv) sessionTimes(study int) tenant.SessionRetention {
	e.Helper()
	r, err := e.p.SessionRetention(e.ctx, uidOf(1, study))
	e.ok("SessionRetention", err)
	return r
}

// due lists every session due at now under rule ("" for all), held ones
// included when held.
func (e *tenv) due(rule string, now time.Time, held bool) []tenant.DueSession {
	e.Helper()
	return allPages(e.penv, 1, func(pg master.Page) ([]tenant.DueSession, string, error) {
		return e.p.DueSessions(e.ctx, tenant.DueFilter{Rule: rule, IncludeHeld: held, Now: now}, pg)
	})
}

// archiveScans receives, at the receiver, one instance in each series of
// series (uidOf(2, n)) of study, identifies the reception as session of
// sub-01 on "store", archives each scan and takes the reception to done.
// imp, when not nil, is the import unit that handed the files in.
func (e *tenv) archiveScans(receiver string, study int, series []int, base int, when time.Time, session string, imp *tenant.Work) {
	e.Helper()
	e.archiveScansOf("sub-01", "store", receiver, study, series, base, when, session, imp)
}

// archiveScansOf is archiveScans with the session under subject, on
// storage.
func (e *tenv) archiveScansOf(subject, storage, receiver string, study int, series []int, base int, when time.Time, session string,
	imp *tenant.Work) {
	e.Helper()
	r, _, err := e.p.OpenReception(e.ctx, receiver, uidOf(1, study), e.id("rc"), when)
	e.ok("OpenReception", err)
	var files []tenant.ReceivedFile
	for i, s := range series {
		f := file(r, study, base+i)
		f.SeriesUID = uidOf(2, s)
		if imp != nil {
			f = importFile(r, study, base+i, *imp, "dir/f"+uidOf(3, base+i))
			f.SeriesUID = uidOf(2, s)
		}
		files = append(files, f)
	}
	_, err = e.p.AddReceivedFiles(e.ctx, files, when)
	e.ok("AddReceivedFiles", err)
	step := func(from reception.State, ev reception.Event) {
		e.Helper()
		st := tenant.ReceptionStep{ID: r.ID, From: from, Event: ev, At: r.StepTime.Add(time.Minute)}
		if ev == reception.Identify {
			st.SubjectLabel, st.SessionLabel, st.Storage = subject, session, storage
		}
		r, err = e.p.StepReception(e.ctx, st, nil)
		e.ok("StepReception "+string(ev), err)
	}
	step(reception.Receiving, reception.Quiet)
	step(reception.Complete, reception.Identify)
	claims := make([]tenant.InstanceClaim, len(files))
	for i, f := range files {
		claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
	}
	_, err = e.p.ClaimInstances(e.ctx, r.ID, claims, r.StepTime, nil)
	e.ok("ClaimInstances", err)
	for _, f := range files {
		p := label.Path{Subject: r.SubjectLabel, Session: r.SessionLabel, Scan: "scan-" + f.SeriesUID}
		key, err := layout.InstanceKey(p, f.SOPInstanceUID)
		e.ok("InstanceKey", err)
		_, err = e.p.CommitScan(e.ctx, tenant.ScanCommit{ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: f.SeriesUID,
			SubjectLabel: p.Subject, SessionLabel: p.Session, ScanLabel: p.Scan, Storage: storage, ManifestSHA256: sum(len(f.SeriesUID)),
			At: r.StepTime, Instances: []tenant.ConfirmedInstance{{SOPInstanceUID: f.SOPInstanceUID, Key: key, Length: f.Size,
				Size: f.Size, ArchivedSHA256: f.SHA256}}})
		e.ok("CommitScan", err)
	}
	step(reception.Identified, reception.Archive)
	e.finish(archived{r: r, files: files})
}

// testRetentionRules guards adding, reading, listing, changing and
// removing rules: each refusal of a malformed rule, of a selector the
// tenant does not have or that names a replica, of a name taken; a change
// to the same values keeps its update time; a removal answers the rule as
// it was and a second finds none.
func testRetentionRules(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.addStorage("copy")
	if _, _, _, err := e.p.AttachReplica(e.ctx, "copy", "store", at(0), e.audit(), nil); err != nil {
		e.Fatalf("AttachReplica: %v", err)
	}
	r := rule("trial7", 10, 20)
	r.Selector = tenant.RetentionSelector{Receiver: "recv"}
	got := e.addRule(r, change(10))
	if got.Name != "trial7" || !got.Created.Equal(at(10)) || !got.Updated.Equal(at(10)) || got.Selector != r.Selector ||
		got.KeepAtLeast != r.KeepAtLeast || got.DisposeAfter != r.DisposeAfter || got.Action != tenant.RetentionReview {
		e.Fatalf("an added rule is %+v", got)
	}
	_, err := e.p.AddRetentionRule(e.ctx, r, change(11), e.audit(), nil)
	e.wantKind("a rule's name taken", err, fault.Conflict)
	for _, bad := range []struct {
		what string
		r    tenant.RetentionRule
		kind fault.Kind
	}{
		{"no period", tenant.RetentionRule{Name: "x", Action: tenant.RetentionReview}, fault.Invalid},
		{"a dispose shorter than the keep", rule("x", 20, 10), fault.Invalid},
		{"a period past 100 years", tenant.RetentionRule{Name: "x", KeepAtLeast: tenant.RetentionPeriod{Years: 100, Days: 1},
			Action: tenant.RetentionReview}, fault.Invalid},
		{"no action", tenant.RetentionRule{Name: "x", KeepAtLeast: tenant.RetentionPeriod{Days: 1}}, fault.Invalid},
		{"a receiver and a storage", func() tenant.RetentionRule {
			x := rule("x", 1, 2)
			x.Selector = tenant.RetentionSelector{Receiver: "recv", Storage: "store"}
			return x
		}(), fault.Invalid},
		{"a route without its receiver", func() tenant.RetentionRule {
			x := rule("x", 1, 2)
			x.Selector = tenant.RetentionSelector{Route: "r1"}
			return x
		}(), fault.Invalid},
		{"a receiver the tenant does not have", func() tenant.RetentionRule {
			x := rule("x", 1, 2)
			x.Selector = tenant.RetentionSelector{Receiver: "nope"}
			return x
		}(), fault.Precondition},
		{"a route the receiver does not have", func() tenant.RetentionRule {
			x := rule("x", 1, 2)
			x.Selector = tenant.RetentionSelector{Receiver: "recv", Route: "nope"}
			return x
		}(), fault.Precondition},
		{"a replica", func() tenant.RetentionRule {
			x := rule("x", 1, 2)
			x.Selector = tenant.RetentionSelector{Storage: "copy"}
			return x
		}(), fault.Precondition},
	} {
		_, err := e.p.AddRetentionRule(e.ctx, bad.r, change(12), e.audit(), nil)
		e.wantKind(bad.what, err, bad.kind)
	}
	e.addRule(rule("a-first", 0, 5), change(13))
	list := allPages(e.penv, 1, func(pg master.Page) ([]tenant.RetentionRule, string, error) {
		return e.p.ListRetentionRules(e.ctx, at(14), pg)
	})
	if len(list) != 2 || list[0].Name != "a-first" || list[1].Name != "trial7" {
		e.Fatalf("the rules listed are %+v", list)
	}
	same, err := e.p.SetRetentionRule(e.ctx, r, change(20), e.audit(), nil)
	e.ok("SetRetentionRule with the same values", err)
	if !same.Updated.Equal(at(10)) {
		e.Fatalf("a change to the same values moved the update time to %v", same.Updated)
	}
	r.Action, r.Selector = tenant.RetentionDelete, tenant.RetentionSelector{Storage: "store"}
	changed, err := e.p.SetRetentionRule(e.ctx, r, change(30), e.audit(), nil)
	e.ok("SetRetentionRule", err)
	if !changed.Updated.Equal(at(30)) || changed.Action != tenant.RetentionDelete || changed.Selector.Storage != "store" {
		e.Fatalf("a changed rule is %+v", changed)
	}
	_, err = e.p.SetRetentionRule(e.ctx, rule("nope", 1, 2), change(31), e.audit(), nil)
	e.wantKind("a change of a rule not held", err, fault.NotFound)
	gone, err := e.p.RemoveRetentionRule(e.ctx, "trial7", change(40), e.audit(), nil)
	e.ok("RemoveRetentionRule", err)
	if gone.Name != "trial7" || gone.Action != tenant.RetentionDelete {
		e.Fatalf("a removed rule is answered %+v", gone)
	}
	_, err = e.p.RemoveRetentionRule(e.ctx, "trial7", change(41), e.audit(), nil)
	e.wantKind("a second removal", err, fault.NotFound)
	_, err = e.p.RetentionRule(e.ctx, "trial7", at(42))
	e.wantKind("a removed rule read", err, fault.NotFound)
}

// testRetentionMatching guards which sessions a rule matches: a session
// whose scans all came through a receiver is due under its dispose rule;
// one with a scan through another is not (a dispose rule needs all), yet a
// keep rule of that other receiver keeps it (a keep rule needs any); the
// earliest dispose names the rule, the latest keep delays the due time;
// the start is the session's archive time, never a DICOM date.
func testRetentionMatching(e *tenv) {
	e.addReceiver("recv1", "store", 11112)
	e.addReceiver("recv2", "store", 11113)
	e.archiveScans("recv1", 1, []int{100, 101}, 0, at(0), "ses-1", nil)
	e.archiveScans("recv1", 2, []int{200}, 100, at(0), "ses-2", nil)
	e.archiveScans("recv2", 2, []int{201}, 200, at(0), "ses-2", nil)
	start := e.sessionTimes(1).Start
	if start.Before(at(0)) || start.After(at(600)) {
		e.Fatalf("a session's start is %v, want its archive time", start)
	}
	// Periods past the notice, so no rule added here is clamped.
	d := rule("d1", 0, 60)
	d.Selector = tenant.RetentionSelector{Receiver: "recv1"}
	e.addRule(d, change(1))
	k := tenant.RetentionRule{Name: "k2", Selector: tenant.RetentionSelector{Receiver: "recv2"}, KeepAtLeast: tenant.RetentionPeriod{Days: 90},
		Action: tenant.RetentionReview}
	e.addRule(k, change(2))
	late := rule("d0-late", 0, 70)
	e.addRule(late, change(3))
	one, two := e.sessionTimes(1), e.sessionTimes(2)
	if one.Rule != "d1" || !one.Due.Equal(start.AddDate(0, 0, 60)) || !one.KeepUntil.IsZero() {
		e.Fatalf("a session all through recv1 has %+v, want due under d1 60 days after %v", one, start)
	}
	if two.Rule != "d0-late" || !two.KeepUntil.Equal(two.Start.AddDate(0, 0, 90)) || !two.Due.Equal(two.KeepUntil) {
		e.Fatalf("a session through both has %+v, want kept 90 days by k2 and due then under d0-late", two)
	}
	got := e.due("", start.AddDate(0, 0, 61), false)
	if len(got) != 1 || got[0].StudyUID != uidOf(1, 1) || got[0].Rule != "d1" || got[0].SubjectLabel != "sub-01" ||
		got[0].SessionLabel != "ses-1" || got[0].Instances != 2 {
		e.Fatalf("due at 61 days: %+v", got)
	}
	if got := e.due("d0-late", start.AddDate(0, 0, 91), false); len(got) != 1 || got[0].StudyUID != uidOf(1, 2) {
		e.Fatalf("due under d0-late at 91 days: %+v", got)
	}
	if r, err := e.p.RetentionRule(e.ctx, "d1", start.AddDate(0, 0, 61)); err != nil || r.SessionsDue != 1 {
		e.Fatalf("d1 counts %d due (%v), want 1", r.SessionsDue, err)
	}
}

// testRetentionClamp guards the notice: a rule added that would make data
// due at once makes it due no sooner than the change plus the notice; a
// rule shortened is clamped likewise, never later than before, and marks
// its notice; a rule lengthened applies at once; a keep removed still
// keeps until the notice ends.
func testRetentionClamp(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	start := e.sessionTimes(1).Start
	now := start.AddDate(0, 0, 400)
	added := e.addRule(rule("r", 0, 1), changeAt(now))
	if got := e.sessionTimes(1); !got.Due.Equal(now.Add(notice)) {
		e.Fatalf("a rule added 400 days after the start makes the session due at %v, want the notice's end %v", got.Due, now.Add(notice))
	}
	if !added.NoticeUntil.Equal(now.Add(notice)) {
		e.Fatalf("the clamping rule's notice ends %v, want %v", added.NoticeUntil, now.Add(notice))
	}
	if got := e.due("", now.Add(notice-time.Second), false); len(got) != 0 {
		e.Fatalf("a session is due before the notice ends: %+v", got)
	}
	// Lengthened: at once.
	long := tenant.RetentionRule{Name: "r", DisposeAfter: tenant.RetentionPeriod{Years: 10}, Action: tenant.RetentionReview}
	e.ok("SetRetentionRule", func() error {
		_, err := e.p.SetRetentionRule(e.ctx, long, changeAt(now.Add(day)), e.audit(), nil)
		return err
	}())
	if got := e.sessionTimes(1); !got.Due.Equal(start.AddDate(10, 0, 0)) {
		e.Fatalf("a lengthened rule makes the session due at %v, want at once %v", got.Due, start.AddDate(10, 0, 0))
	}
	// Shortened again: no sooner than the change plus the notice.
	at2 := now.Add(2 * day)
	e.ok("SetRetentionRule", func() error {
		_, err := e.p.SetRetentionRule(e.ctx, rule("r", 0, 1), changeAt(at2), e.audit(), nil)
		return err
	}())
	if got := e.sessionTimes(1); !got.Due.Equal(at2.Add(notice)) {
		e.Fatalf("a shortened rule makes the session due at %v, want %v", got.Due, at2.Add(notice))
	}
	// A keep removed keeps until the notice ends.
	keep := tenant.RetentionRule{Name: "k", KeepAtLeast: tenant.RetentionPeriod{Years: 10}, Action: tenant.RetentionReview}
	e.addRule(keep, changeAt(at2.Add(day)))
	if got := e.sessionTimes(1); !got.KeepUntil.Equal(start.AddDate(10, 0, 0)) || !got.Due.Equal(got.KeepUntil) {
		e.Fatalf("a keep added at once gives %+v, want kept and due 10 years after the start", got)
	}
	at3 := at2.Add(2 * day)
	if _, err := e.p.RemoveRetentionRule(e.ctx, "k", changeAt(at3), e.audit(), nil); err != nil {
		e.Fatalf("RemoveRetentionRule: %v", err)
	}
	if got := e.sessionTimes(1); !got.KeepUntil.Equal(at3.Add(notice)) || got.Due.Before(at3.Add(notice)) {
		e.Fatalf("a keep removed leaves %+v, want kept until %v", got, at3.Add(notice))
	}
}

// testRetentionFollowsScans guards the times stored again with the scans:
// a scan through another receiver that joins a session makes a dispose
// rule of the first stop matching it, at once; its delete makes the rule
// match again, clamped by the notice; the undo stores the times again; a
// session hidden by a delete is not listed due.
func testRetentionFollowsScans(e *tenv) {
	e.addReceiver("recv1", "store", 11112)
	e.addReceiver("recv2", "store", 11113)
	e.archiveScans("recv1", 1, []int{100}, 0, at(0), "ses-1", nil)
	d := rule("d1", 0, 10)
	d.Selector = tenant.RetentionSelector{Receiver: "recv1"}
	e.addRule(d, change(1))
	start := e.sessionTimes(1).Start
	if got := e.sessionTimes(1); got.Rule != "d1" {
		e.Fatalf("a session through recv1 is %+v, want due under d1", got)
	}
	e.archiveScans("recv2", 1, []int{101}, 100, at(100), "ses-1", nil)
	if got := e.sessionTimes(1); !got.Due.IsZero() || got.Rule != "" || !got.Start.Equal(start) {
		e.Fatalf("after a scan through recv2 joined, the session is %+v, want never due and the same start", got)
	}
	c := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1", Scan: "scan-" + uidOf(2, 101)}, 200)
	e.commit(c.ID, 300, nil)
	if got := e.sessionTimes(1); got.Rule != "d1" || !got.Due.Equal(at(300).Add(notice)) {
		e.Fatalf("after the recv2 scan's delete the session is %+v, want due under d1 at the notice's end", got)
	}
	if _, err := e.p.UndoCorrection(e.ctx, c.ID, "the wrong scan", at(400), e.audit(), nil); err != nil {
		e.Fatalf("UndoCorrection: %v", err)
	}
	if got := e.sessionTimes(1); got.Rule != "" {
		e.Fatalf("after the undo the session is %+v, want never due", got)
	}
	whole := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}, 500)
	e.ok("RemoveRetentionRule", func() error { _, err := e.p.RemoveRetentionRule(e.ctx, "d1", change(501), e.audit(), nil); return err }())
	e.addRule(rule("all", 0, 1), change(502))
	e.commit(whole.ID, 600, nil)
	if got := e.due("", start.AddDate(1, 0, 0), true); len(got) != 0 {
		e.Fatalf("a session a delete hides is listed due: %+v", got)
	}
}

// testRetentionImportStart guards an import's retention start: a session
// an import job archives first counts from the start an admin gave, and is
// never due before its archive time plus the notice; a start after the
// job's time or before 1970 is refused.
func testRetentionImportStart(e *tenv) {
	ie := &ienv{senv: e.senv}
	ie.setupImport()
	given := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, bad := range []time.Time{T0.Add(time.Hour), time.Date(1969, 12, 31, 0, 0, 0, 0, time.UTC)} {
		_, err := e.p.CreateImportJob(e.ctx, tenant.NewImportJob{ID: e.id("j"), Receiver: importReceiver, FailuresKept: tenant.MinFailuresKept,
			RetentionStart: bad, Created: T0}, e.audit(), nil)
		e.wantKind("an import's retention start "+bad.String(), err, fault.Invalid)
	}
	j, err := e.p.CreateImportJob(e.ctx, tenant.NewImportJob{ID: e.id("j"), Receiver: importReceiver, FailuresKept: tenant.MinFailuresKept,
		RetentionStart: given, Created: T0}, e.audit(), nil)
	e.ok("CreateImportJob", err)
	if got, err := e.p.ImportJob(e.ctx, j.ID); err != nil || got.Import == nil || !got.Import.RetentionStart.Equal(given) {
		e.Fatalf("the import job is %+v (%v), want its retention start %v", got.Import, err, given)
	}
	e.addRule(rule("r", 0, 365), change(1))
	w := ie.claimImport("w", at(2))
	ie.start(w, at(2))
	e.archiveScans(importReceiver, 1, []int{100}, 0, at(3), "ses-1", &w)
	got := e.sessionTimes(1)
	if !got.Start.Equal(given) || got.Floor.IsZero() || !got.Due.Equal(got.Floor) || got.Floor.Before(got.Archived.Add(notice)) {
		e.Fatalf("an imported session is %+v, want started %v and due no sooner than its archive time plus the notice", got, given)
	}
}

// testRetentionClock guards the trusted clock: a step forward past the
// step allowed pauses retention; while paused nothing becomes due, an
// automatic purge is refused, an admin's purge goes on, and only a
// confirmation within five minutes of the clock resumes it. A step back
// pauses nothing and keeps the latest time trusted, so the next forward
// step is measured from it.
func testRetentionClock(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	start := e.sessionTimes(1).Start
	e.addRule(rule("r", 0, 3), changeAt(start))
	check := func(t time.Time) tenant.RetentionClock {
		e.Helper()
		c, err := e.p.CheckRetentionClock(e.ctx, tenant.RetentionClockCheck{Now: t, MaxStep: tenant.DefaultMaxClockStep, Notice: notice, Pass: true})
		e.ok("CheckRetentionClock", err)
		return c
	}
	t1 := start.Add(day)
	if c := check(t1); c.Paused || !c.Trusted.Equal(t1) || !c.LastPass.Equal(t1) {
		e.Fatalf("a first check is %+v", c)
	}
	if c := check(t1.Add(day)); c.Paused {
		e.Fatalf("a step of a day paused: %+v", c)
	}
	jump := t1.Add(400 * day)
	c := check(jump)
	if !c.Paused || !c.JustPaused || c.Reason != tenant.PauseClockJumped || c.Step != jump.Sub(t1.Add(day)) {
		e.Fatalf("a 400-day jump answers %+v, want paused for the jump", c)
	}
	if again := check(jump.Add(time.Hour)); !again.Paused || again.JustPaused {
		e.Fatalf("a check while paused answers %+v", again)
	}
	if got := e.due("", jump, false); len(got) != 0 {
		e.Fatalf("while paused a session became due: %+v", got)
	}
	// A delete whose grace the jump ended waits for a person.
	e.archiveScans("recv", 2, []int{200}, 100, at(50), "ses-2", nil)
	d := e.prepareDelete(tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-2"}, 100)
	d = e.commit(d.ID, 200, nil)
	_, err := e.p.StartPurge(e.ctx, d.ID, jump, nil, nil)
	if !errors.Is(err, tenant.ErrClock) {
		e.Fatalf("an automatic purge while paused answers %v, want the clock's refusal", err)
	}
	_, _, err = e.p.ConfirmClock(e.ctx, tenant.ClockConfirm{Observed: jump.Add(10 * time.Minute), Now: jump, Reason: "checked"}, e.audit(), nil)
	e.wantKind("a confirmation ten minutes off", err, fault.Precondition)
	conf, was, err := e.p.ConfirmClock(e.ctx, tenant.ClockConfirm{Observed: jump.Add(time.Minute), Now: jump, Reason: "checked against the site's clock"},
		e.audit(), nil)
	e.ok("ConfirmClock", err)
	if !was || conf.Paused || !conf.Trusted.Equal(jump) {
		e.Fatalf("a confirmation answers %+v (was paused %v)", conf, was)
	}
	if got := e.due("", jump, false); len(got) != 1 {
		e.Fatalf("after the confirmation the due sessions are %+v, want the one", got)
	}
	// A step back pauses nothing and leaves the time trusted at the latest.
	if c := check(jump.Add(-day)); c.Paused || !c.Trusted.Equal(jump) {
		e.Fatalf("a clock moved back a day answers %+v, want not paused and still trusting %v", c, jump)
	}
	// The next step forward is measured from the latest time trusted, not
	// from the earlier reading: 36 hours past it goes on (60 from the
	// reading would have paused), three days past that pause.
	later := jump.Add(36 * time.Hour)
	if c := check(later); c.Paused || !c.Trusted.Equal(later) {
		e.Fatalf("36 hours past the latest trusted time answers %+v, want trusted", c)
	}
	if c := check(later.Add(3 * day)); !c.Paused || c.Reason != tenant.PauseClockJumped || c.Step != 3*day {
		e.Fatalf("three days past the latest trusted time answers %+v, want paused for the jump", c)
	}
	if _, err := e.p.StartPurge(e.ctx, d.ID, jump, &master.AuditEntry{ID: e.id("a"), Time: jump, Method: "PurgeCorrection",
		Outcome: master.OutcomeDone}, nil); err != nil {
		e.Fatalf("an admin's purge while paused: %v", err)
	}
}

// testRetentionKeepsAtLeast guards keep-at-least: a person's delete of a
// session a rule keeps is refused with the time it is kept until, and the
// preview counts it; with override_retention and its own reason it is
// prepared, and the correction keeps the override, its reason and the key.
func testRetentionKeepsAtLeast(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	e.archiveScans("recv", 1, []int{100}, 0, at(0), "ses-1", nil)
	keep := tenant.RetentionRule{Name: "k", KeepAtLeast: tenant.RetentionPeriod{Years: 10}, Action: tenant.RetentionReview}
	e.addRule(keep, change(1))
	until := e.sessionTimes(1).KeepUntil
	target := tenant.CorrectionTarget{Subject: "sub-01", Session: "ses-1"}
	p, err := e.p.PreviewDelete(e.ctx, target, at(100))
	e.ok("PreviewDelete", err)
	if p.SessionsUnderMinimum != 1 || !p.KeepUntil.Equal(until) {
		e.Fatalf("the preview says %d kept until %v, want 1 until %v", p.SessionsUnderMinimum, p.KeepUntil, until)
	}
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(101), Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, Now: at(101)}, e.audit(), nil)
	var min *tenant.RetentionMinimum
	if !errors.As(err, &min) || !errors.Is(err, tenant.ErrRetentionMinimum) || fault.KindOf(err) != fault.Precondition || !min.KeepUntil.Equal(until) {
		e.Fatalf("a delete a rule keeps answers %v, want RETENTION_MINIMUM until %v", err, until)
	}
	_, _, err = e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(102), Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, OverrideRetention: true, Now: at(102)}, e.audit(), nil)
	e.wantKind("an override with no reason", err, fault.Invalid)
	c, made, err := e.p.PrepareCorrection(e.ctx, tenant.CorrectionPrepare{ID: e.newID(103), Kind: record.KindDelete, Target: target,
		Reason: "consent withdrawn", Confirm: p.Confirm, KeyID: "k-installation-admin", OverrideRetention: true,
		OverrideReason: "consent withdrawn outweighs the trial's keep", Now: at(103)}, e.audit(), nil)
	e.ok("PrepareCorrection with an override", err)
	if !made || !c.OverrideRetention || c.OverrideReason != "consent withdrawn outweighs the trial's keep" || c.KeyID != "k-installation-admin" ||
		c.Origin != tenant.OriginPerson {
		e.Fatalf("an overriding delete is %+v", c)
	}
}

// testQuarantineAging guards what a status warns of the quarantine by: the
// oldest file held and the files and bytes held, which a release moves.
func testQuarantineAging(e *tenv) {
	e.addReceiver("recv", "store", 11112)
	if a, err := e.p.QuarantineAging(e.ctx); err != nil || !a.Oldest.IsZero() || a.Files != 0 || a.Bytes != 0 {
		e.Fatalf("an empty quarantine ages %+v (%v)", a, err)
	}
	old, young := e.heldFile("recv", 300, 10), e.heldFile("recv", 200, 20)
	if _, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{young, old}); err != nil {
		e.Fatalf("HoldFiles: %v", err)
	}
	a, err := e.p.QuarantineAging(e.ctx)
	e.ok("QuarantineAging", err)
	if !a.Oldest.Equal(at(10)) || a.Files != 2 || a.Bytes != 500 {
		e.Fatalf("the quarantine ages %+v, want the oldest at %v and 2 files of 500 bytes", a, at(10))
	}
	if _, err := e.p.RejectQuarantined(e.ctx, old.ID, "not ours to keep", at(30), e.audit(), nil); err != nil {
		e.Fatalf("RejectQuarantined: %v", err)
	}
	if a, _ := e.p.QuarantineAging(e.ctx); !a.Oldest.Equal(at(20)) || a.Files != 1 {
		e.Fatalf("after a reject the quarantine ages %+v, want the other at %v", a, at(20))
	}
}
