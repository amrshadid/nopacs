package tenanttest

import (
	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// recallSetup adds the storage "main" and its archive-tier S3 replica
// "cold", which recalls for 3 days at the bulk speed.
func (e *renv) recallSetup() {
	e.Helper()
	e.addStorage("main")
	c := s3Storage("cold", "cold/")
	c.ReplicaOf, c.Tier, c.S3.RecallDays, c.S3.RecallSpeed = "main", tenant.TierArchive, 3, tenant.RecallBulk
	_, err := e.p.AddStorage(e.ctx, c, at(1), e.audit(), nil)
	e.ok("AddStorage of an archive-tier replica", err)
}

// ask records a recall of the keys on cold at second s, with an audit entry
// when audited; it fails the test on an error.
func (e *renv) ask(s int, audited bool, keys ...string) tenant.RecallAsked {
	e.Helper()
	a := tenant.RecallAsk{Storage: "cold", Keys: keys, Days: 3, Speed: tenant.RecallBulk, Now: at(s)}
	if audited {
		entry := e.audit()
		a.Audit = &entry
	}
	got, err := e.p.RequestRecalls(e.ctx, a)
	e.ok("RequestRecalls", err)
	return got
}

// recallOf reads the recall of key on cold, failing the test when there is
// none.
func (e *renv) recallOf(key string) tenant.Recall {
	e.Helper()
	r, found, err := e.p.Recall(e.ctx, "cold", key)
	e.ok("Recall", err)
	if !found {
		e.Fatalf("there is no recall of %s", key)
	}
	return r
}

// testRecalls guards that a recall is shared: one row per object, asked once
// however many ask (a request joins one requested or in progress, and
// leaves one online as it is), moved by the store's answers to in progress,
// online until its time, or failed, after which, or once its time is past,
// a request asks again; the worker reads the rows due in order; the counts
// leave out an object online no more.
func testRecalls(e *renv) {
	e.recallSetup()
	before := e.auditIDs()
	if got := e.ask(10, true, "data/a.dcm", "data/b.dcm"); got != (tenant.RecallAsked{Requested: 2}) {
		e.Errorf("a first recall answered %+v, want both requested", got)
	}
	if len(e.auditIDs()) != len(before)+1 {
		e.Error("an operator's recall wrote no audit entry")
	}
	r := e.recallOf("data/a.dcm")
	if r.State != tenant.RecallRequested || r.Days != 3 || r.Speed != tenant.RecallBulk || !r.Due.Equal(at(10)) || !r.Live() ||
		r.Request() != (blob.RecallRequest{Days: 3, Speed: blob.RecallBulk}) {
		e.Errorf("a recall requested reads %+v", r)
	}
	if got := e.ask(11, false, "data/a.dcm", "data/c.dcm"); got != (tenant.RecallAsked{Requested: 1, Joined: 1}) {
		e.Errorf("a job unit's recall answered %+v, want one joined and one requested", got)
	}
	due, err := e.p.DueRecalls(e.ctx, at(20), 10)
	e.ok("DueRecalls", err)
	if len(due) != 3 || due[0].Key != "data/a.dcm" || due[1].Key != "data/b.dcm" || due[2].Key != "data/c.dcm" {
		e.Errorf("the recalls due are %+v", due)
	}
	r, err = e.p.RecordRecall(e.ctx, tenant.RecallAnswer{Storage: "cold", Key: "data/a.dcm",
		State: tenant.RecallStateOf(blob.RecallStarted), Next: at(100), Now: at(21)})
	e.ok("RecordRecall in progress", err)
	if r.State != tenant.RecallInProgress || !r.Due.Equal(at(100)) || !r.Updated.Equal(at(21)) {
		e.Errorf("a recall the store took reads %+v", r)
	}
	if due, _ := e.p.DueRecalls(e.ctx, at(50), 10); len(due) != 2 || due[0].Key != "data/b.dcm" {
		e.Errorf("the recalls due before the poll are %+v", due)
	}
	r, err = e.p.RecordRecall(e.ctx, tenant.RecallAnswer{Storage: "cold", Key: "data/a.dcm",
		State: tenant.RecallStateOf(blob.RecallOnline), OnlineUntil: at(1000), Now: at(100)})
	e.ok("RecordRecall online", err)
	if r.State != tenant.RecallOnline || !r.OnlineUntil.Equal(at(1000)) || !r.Readable(at(999)) || r.Readable(at(1000)) || r.Live() {
		e.Errorf("a recall online reads %+v", r)
	}
	if got := e.ask(500, false, "data/a.dcm"); got != (tenant.RecallAsked{Online: 1}) {
		e.Errorf("a recall of an object online answered %+v", got)
	}
	_, err = e.p.RecordRecall(e.ctx, tenant.RecallAnswer{Storage: "cold", Key: "data/a.dcm", State: tenant.RecallFailed,
		Reason: "late", Now: at(501)})
	e.wantPrecondition("RecordRecall of a recall online", err)
	r, err = e.p.RecordRecall(e.ctx, tenant.RecallAnswer{Storage: "cold", Key: "data/b.dcm", State: tenant.RecallFailed,
		Reason: "the store refused the speed", Now: at(502)})
	e.ok("RecordRecall failed", err)
	if r.State != tenant.RecallFailed || r.Reason != "the store refused the speed" {
		e.Errorf("a recall failed reads %+v", r)
	}
	counts, err := e.p.RecallCounts(e.ctx, at(600))
	e.ok("RecallCounts", err)
	if counts != (tenant.RecallCounts{Requested: 1, Online: 1, Failed: 1}) {
		e.Errorf("the recall counts are %+v", counts)
	}
	if st := e.status("cold"); st.RecallsInProgress != 1 {
		e.Errorf("the replica counts %d recalls in progress, want 1", st.RecallsInProgress)
	}
	if got := e.ask(700, false, "data/b.dcm"); got != (tenant.RecallAsked{Requested: 1}) {
		e.Errorf("a recall of a failed one answered %+v, want it asked again", got)
	}
	if got := e.ask(2000, false, "data/a.dcm"); got != (tenant.RecallAsked{Requested: 1}) {
		e.Errorf("a recall of one past its time answered %+v, want it asked again", got)
	}
	if r := e.recallOf("data/a.dcm"); r.State != tenant.RecallRequested || !r.OnlineUntil.IsZero() || !r.Requested.Equal(at(2000)) {
		e.Errorf("a recall asked again reads %+v", r)
	}
	counts, err = e.p.RecallCounts(e.ctx, at(2001))
	e.ok("RecallCounts", err)
	if counts != (tenant.RecallCounts{Requested: 3}) {
		e.Errorf("the recall counts after asking again are %+v", counts)
	}
}

// testRecallsRefused guards what a recall refuses: a storage of the online
// tier, one the tenant does not have, days out of bounds, a key twice, and
// an answer that is not one of the store's.
func testRecallsRefused(e *renv) {
	e.recallSetup()
	_, err := e.p.RequestRecalls(e.ctx, tenant.RecallAsk{Storage: "main", Keys: []string{"k"}, Days: 1, Now: at(1)})
	e.wantPrecondition("RequestRecalls on an online storage", err)
	_, err = e.p.RequestRecalls(e.ctx, tenant.RecallAsk{Storage: "gone", Keys: []string{"k"}, Days: 1, Now: at(1)})
	e.wantKind("RequestRecalls on a storage the tenant does not have", err, fault.NotFound)
	_, err = e.p.RequestRecalls(e.ctx, tenant.RecallAsk{Storage: "cold", Keys: []string{"k"}, Days: 0, Now: at(1)})
	e.wantKind("RequestRecalls of 0 days", err, fault.Invalid)
	_, err = e.p.RequestRecalls(e.ctx, tenant.RecallAsk{Storage: "cold", Keys: []string{"k", "k"}, Days: 1, Now: at(1)})
	e.wantKind("RequestRecalls of a key twice", err, fault.Invalid)
	bad := master.AuditEntry{ID: "x"}
	_, err = e.p.RequestRecalls(e.ctx, tenant.RecallAsk{Storage: "cold", Keys: []string{"k"}, Days: 1, Now: at(1), Audit: &bad})
	e.wantKind("RequestRecalls with a bad audit entry", err, fault.Invalid)
	if _, found, err := e.p.Recall(e.ctx, "cold", "k"); err != nil || found {
		e.Errorf("a refused recall left a row: %v, %v", found, err)
	}
	_, err = e.p.RecordRecall(e.ctx, tenant.RecallAnswer{Storage: "cold", Key: "k", State: tenant.RecallOnline, Now: at(2)})
	e.wantKind("RecordRecall of an object never asked for", err, fault.NotFound)
	_, err = e.p.RecordRecall(e.ctx, tenant.RecallAnswer{Storage: "cold", Key: "k", State: tenant.RecallRequested, Now: at(2)})
	e.wantKind("RecordRecall of the state requested", err, fault.Invalid)
	_, err = e.p.RecordRecall(e.ctx, tenant.RecallAnswer{Storage: "cold", Key: "k", State: tenant.RecallFailed, Now: at(2)})
	e.wantKind("RecordRecall of a failure with no reason", err, fault.Invalid)
	_, err = e.p.DueRecalls(e.ctx, at(2), 0)
	e.wantKind("DueRecalls of none", err, fault.Invalid)
}
