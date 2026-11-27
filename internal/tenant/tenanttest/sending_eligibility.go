package tenanttest

import (
	"context"
	"errors"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// markEvents returns the marks the sender_allow_identified events of the
// feed after the cursor report, in order, as "<sender>=<mark>".
func (e *senv) markEvents(after string) []string {
	e.Helper()
	got, _ := e.readAll(after, 100, tenant.EventSenderAllowIdentified)
	var out []string
	for _, ev := range got {
		p, ok := ev.Payload.(*tenant.SenderAllowIdentifiedChanged)
		if !ok {
			e.Errorf("a sender_allow_identified event carries %T", ev.Payload)
			continue
		}
		mark := "false"
		if p.AllowIdentified {
			mark = "true"
		}
		out = append(out, p.Sender+"="+mark)
	}
	return out
}

// auditByID returns the audit entry of the id given, failing the test when
// there is none.
func (e *senv) auditByID(id string) master.AuditEntry {
	e.Helper()
	for _, a := range e.allAudit(master.AuditFilter{}) {
		if a.ID == id {
			return a
		}
	}
	e.Fatalf("no audit entry %s", id)
	return master.AuditEntry{}
}

// testSenderMark guards a sender's AllowIdentified as the store keeps it:
// false unless given; a sender added with it set reads it, and its
// addition is audited with DetailAllowIdentified and reported by an event
// in the same transaction; a change that names it sets and clears it, each
// time with the detail and an event; a change that does not name it, or
// names it with the value it has, keeps it and reports nothing; a change a
// guard refuses keeps the mark and writes no event; a list answers it.
func testSenderMark(e *senv) {
	start := e.now()
	plain := e.addSender("plain")
	if plain.AllowIdentified {
		e.Errorf("a sender added without the mark reads it set")
	}
	marked := dimseSender("open")
	marked.AllowIdentified = true
	a := e.audit()
	got, err := e.p.AddSender(e.ctx, marked, T0, a, nil)
	e.ok("AddSender marked", err)
	if !got.AllowIdentified {
		e.Errorf("a sender added marked reads %+v", got)
	}
	if v := e.auditByID(a.ID).Details[tenant.DetailAllowIdentified]; v != "true" {
		e.Errorf("the addition of a marked sender is audited with allow_identified %q, want true", v)
	}
	set := func(s tenant.Sender, fields ...tenant.SenderField) (tenant.Sender, master.AuditEntry, error) {
		a := e.audit()
		out, err := e.p.SetSender(e.ctx, s, fields, at(10), a, nil)
		return out, a, err
	}
	// Not named: kept, nothing reported.
	out, a, err := set(tenant.Sender{Name: "open", MaxAssociations: 3}, tenant.FieldSenderAssociations)
	e.ok("SetSender of another field", err)
	if !out.AllowIdentified {
		e.Error("a change that does not name the mark cleared it")
	}
	if _, has := e.auditByID(a.ID).Details[tenant.DetailAllowIdentified]; has {
		e.Error("a change that does not move the mark is audited with it")
	}
	// Named with the value it has: nothing moves.
	_, _, err = set(tenant.Sender{Name: "open", AllowIdentified: true}, tenant.FieldSenderAllowIdentified)
	e.ok("SetSender of the mark it has", err)
	// Cleared, then set again on the other.
	out, a, err = set(tenant.Sender{Name: "open"}, tenant.FieldSenderAllowIdentified)
	e.ok("SetSender clearing the mark", err)
	if out.AllowIdentified {
		e.Error("a change clearing the mark left it set")
	}
	if v := e.auditByID(a.ID).Details[tenant.DetailAllowIdentified]; v != "false" {
		e.Errorf("a change clearing the mark is audited with allow_identified %q, want false", v)
	}
	_, _, err = set(tenant.Sender{Name: "plain", AllowIdentified: true}, tenant.FieldSenderAllowIdentified)
	e.ok("SetSender setting the mark", err)
	// A change its guard refuses: nothing changes, nothing is reported.
	refused := errors.New("the caller's grant was revoked")
	_, err = e.p.SetSender(e.ctx, tenant.Sender{Name: "plain"}, []tenant.SenderField{tenant.FieldSenderAllowIdentified}, at(20), e.audit(),
		func(context.Context) error { return refused })
	if err == nil {
		e.Error("a change its guard refused was made")
	}
	if s, err := e.p.Sender(e.ctx, "plain"); err != nil || !s.AllowIdentified {
		e.Errorf("after a refused change the mark reads %v, %v; want it kept", s.AllowIdentified, err)
	}
	if got := e.markEvents(start); len(got) != 3 || got[0] != "open=true" || got[1] != "open=false" || got[2] != "plain=true" {
		e.Errorf("the mark's events %v, want [open=true open=false plain=true]", got)
	}
	marks := map[string]bool{}
	for _, s := range allPages(e.penv, 1, func(p master.Page) ([]tenant.Sender, string, error) { return e.p.ListSenders(e.ctx, p) }) {
		marks[s.Name] = s.AllowIdentified
	}
	if len(marks) != 2 || !marks["plain"] || marks["open"] {
		e.Errorf("ListSenders answers the marks %v, want plain set and open cleared", marks)
	}
}

// testInstancePixelRisk guards the fact a send judges each instance by
// (tenant.SendRefusal): an instance committed with an accepted pixel risk is
// answered with it by a job's stream and by the catalog's reads of
// instances, one committed without it is answered without, and a commit
// that records an accepted pixel risk with no profile version is Invalid
// and commits nothing.
func testInstancePixelRisk(e *senv) {
	e.addReceiver("recv", "store", 11112)
	e.addSender("pacs")
	v, _, err := e.p.CreateProfileVersion(e.ctx, newVersion("research", "one"), at(0), e.audit(), nil)
	e.ok("CreateProfileVersion", err)
	e.profile, e.pixelRisk = v.Ref(), true
	risky := e.archive("recv", 1, 1, 2, 0, at(-3600), "ses-1")
	e.pixelRisk = false
	clean := e.archive("recv", 2, 1, 2, 100, at(-3600), "ses-2")
	e.profile = tenant.ProfileRef{}
	want := map[string]bool{}
	for _, f := range risky.files {
		want[f.SOPInstanceUID] = true
	}
	for _, f := range clean.files {
		want[f.SOPInstanceUID] = false
	}
	e.createJob("j1", "pacs", tenant.JobTarget{SubjectLabel: "sub-01"}, at(0))
	e.list("pacs", at(0))
	seen := 0
	for range 2 {
		w := e.claim("pacs", "worker", at(10))
		_, err := e.p.StartJobUnit(e.ctx, w.Fence(), at(10))
		e.ok("StartJobUnit", err)
		for _, in := range e.instances(w, "") {
			seen++
			if in.PixelRiskAccepted != want[in.SOPInstanceUID] || in.Profile != v.Ref() {
				e.Errorf("a job's stream answers %s with risk %v and profile %v, want %v and %v",
					in.SOPInstanceUID, in.PixelRiskAccepted, in.Profile, want[in.SOPInstanceUID], v.Ref())
			}
		}
	}
	if seen != len(want) {
		e.Errorf("the job's streams answered %d instances, want %d", seen, len(want))
	}
	for _, a := range []archived{risky, clean} {
		path := e.labelsOf(a.r.StudyUID, a.files[0].SeriesUID)
		held, _, err := e.p.ListHeldInstances(e.ctx, path, master.Page{Limit: 10})
		e.ok("ListHeldInstances", err)
		for _, in := range held {
			if in.PixelRiskAccepted != want[in.SOPInstanceUID] {
				e.Errorf("ListHeldInstances answers %s with risk %v", in.SOPInstanceUID, in.PixelRiskAccepted)
			}
		}
		one, err := e.p.HeldInstance(e.ctx, path, a.files[0].SOPInstanceUID)
		if e.ok("HeldInstance", err); one.PixelRiskAccepted != want[one.SOPInstanceUID] {
			e.Errorf("HeldInstance answers %s with risk %v", one.SOPInstanceUID, one.PixelRiskAccepted)
		}
	}
	p := label.Path{Subject: "sub-01", Session: "ses-3", Scan: "scan-1"}
	key, err := layout.InstanceKey(p, "1.2.3.4.5")
	e.ok("InstanceKey", err)
	c := tenant.ScanCommit{ReceptionID: risky.r.ID, StudyUID: risky.r.StudyUID, SeriesUID: "1.2.3.4", SubjectLabel: p.Subject,
		SessionLabel: p.Session, ScanLabel: p.Scan, Storage: "store", At: at(0), Instances: []tenant.ConfirmedInstance{{
			SOPInstanceUID: "1.2.3.4.5", Key: key, Length: 1, Size: 1, ArchivedSHA256: sum(1), PixelRiskAccepted: true}}}
	_, err = e.p.CommitScan(e.ctx, c)
	if fault.KindOf(err) != fault.Invalid || !strings.Contains(fault.Message(err), "pixel risk") {
		e.Errorf("a commit of an accepted pixel risk without a profile version: %v, want Invalid naming it", err)
	}
}
