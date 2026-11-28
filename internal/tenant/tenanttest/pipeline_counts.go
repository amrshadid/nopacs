package tenanttest

import (
	"strconv"
	"time"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// testHoldCountsReceived guards the count identity for a file that arrived
// and could not be recorded as a received file (too large, not DICOM): its
// hold counts it received and quarantined, once, so at the end received
// equals archived plus duplicate plus quarantined. A hold locks its
// reception before it counts, as a received batch does, so a hold and a
// batch of the same file at once are ordered; SQLite's one writer orders
// them anyway, and a Postgres run exercises the lock.
func testHoldCountsReceived(e *penv) {
	e.addReceiver("a", "store", 11112)
	r := e.openReception("a", 1, at(1))
	f := file(r, 1, 1)
	_, err := e.p.AddReceivedFiles(e.ctx, []tenant.ReceivedFile{f}, at(2), nil)
	e.ok("AddReceivedFiles", err)
	large := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineTooLarge, Details: map[string]string{"limit": "4294967296"},
		Receiver: "a", ReceptionID: r.ID, Size: 4096, StudyUID: r.StudyUID, Quarantined: at(3)}
	for range 2 {
		_, err = e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{large}, nil)
		e.ok("HoldFiles", err)
	}
	r = e.stepTo(r, reception.Identified, "store")
	e.commit(r, f)
	r = e.stepTo(r, reception.Done, "store")
	c := r.Counts
	if c.FilesReceived != 2 || c.FilesReceived != c.FilesArchived+c.FilesDuplicate+c.FilesQuarantined ||
		c.BytesReceived != c.BytesArchived+c.BytesDuplicate+c.BytesQuarantined {
		e.Errorf("done: %+v, want received = archived + duplicate + quarantined, with the large file received once", c)
	}
}

// testCountIdentity guards the contract's identity at the end of every
// reception that did not fail: received = archived + duplicate +
// quarantined, files and bytes. A seeded random run of receptions, by a
// pipeline that holds what it does not archive or ignore, mixes every kind
// of hold (a file with no received file, a received file), releases (into
// the same reception and into another), rejections, retries and discards,
// and checks the identity whenever a reception ends, and the catalog's
// running totals against a recount after every round.
func testCountIdentity(e *penv) {
	e.addReceiver("a", "store", 11112)
	e.addReceiver("b", "store", 11113)
	rng := uint64(42)
	next := func(n int) int {
		rng ^= rng << 13
		rng ^= rng >> 7
		rng ^= rng << 17
		return int(rng % uint64(n)) //nolint:gosec // G115: n is a small positive count and the result is below it.
	}
	clock := 100
	tick := func() time.Time { clock++; return at(clock) }
	var held []tenant.QuarantinedFile // records still held, to release or reject
	checkEnd := func(r tenant.Reception) {
		e.Helper()
		c := r.Counts
		if r.State != reception.Failed && (c.FilesReceived != c.FilesArchived+c.FilesDuplicate+c.FilesQuarantined ||
			c.BytesReceived != c.BytesArchived+c.BytesDuplicate+c.BytesQuarantined) {
			e.Errorf("reception %s ended %s with %+v: received is not archived + duplicate + quarantined", r.ID, r.State, c)
		}
	}
	// run takes one receiving reception through the pipeline's steps.
	run := func(r tenant.Reception, quarantinedNow map[string]bool) {
		e.Helper()
		r, _, _ = e.p.Reception(e.ctx, r.ID)
		if next(8) == 0 {
			checkEnd(e.failReception(r))
			return
		}
		r = e.stepTo(r, reception.Identified, "store")
		files := allPages(e, 1000, func(p master.Page) ([]tenant.ReceivedFile, string, error) { return e.p.ReceivedFiles(e.ctx, r.ID, p) })
		var todo []tenant.ReceivedFile
		var claims []tenant.InstanceClaim
		for _, f := range files {
			if quarantinedNow[f.SOPInstanceUID+f.SHA256.String()] {
				continue
			}
			todo, claims = append(todo, f), append(claims, claimOf(f))
		}
		answers, err := e.p.ClaimInstances(e.ctx, r.ID, claims, tick(), nil)
		e.ok("ClaimInstances", err)
		var dup tenant.StepCounts
		var holds []tenant.QuarantinedFile
		for i, a := range answers {
			f := todo[i]
			switch a.Kind {
			case tenant.SameFile:
				dup.FilesDuplicate, dup.BytesDuplicate = dup.FilesDuplicate+1, dup.BytesDuplicate+f.Size
			case tenant.DifferentFile, tenant.UIDElsewhere, tenant.Claimed:
				q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineDifferentFile, Receiver: r.Receiver, ReceptionID: r.ID,
					SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
					TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, KeptSHA256: a.HeldSHA256, Size: f.Size,
					Incoming: f.Incoming, ContentKept: true, Quarantined: tick()}
				if a.Kind != tenant.DifferentFile {
					q.Reason, q.KeptSHA256 = tenant.QuarantineUIDElsewhere, tenant.SHA256{}
				}
				holds = append(holds, q)
			}
		}
		e.confirm(r, todo, answers)
		if len(holds) > 0 {
			_, err := e.p.HoldFiles(e.ctx, holds, nil)
			e.ok("HoldFiles", err)
			held = append(held, holds...)
		}
		r, _, _ = e.p.Reception(e.ctx, r.ID)
		r, err = e.p.StepReception(e.ctx, tenant.ReceptionStep{ID: r.ID, From: r.State, Event: reception.Archive, At: tick(), Counts: &dup}, nil)
		e.ok("StepReception archive", err)
		checkEnd(e.stepTo(r, reception.Done, "store"))
	}
	for round := range 40 {
		recv := []string{"a", "b"}[next(2)]
		study := next(4)
		r := e.openReception(recv, study, tick())
		quarantinedNow := map[string]bool{}
		var batch []tenant.ReceivedFile
		for range next(6) + 1 {
			f := file(r, study, study*100+next(12))
			f.SeriesUID = uidOf(2, study)
			if next(4) == 0 {
				f.SHA256 = sum(next(3) + 900)
			}
			batch = append(batch, f)
		}
		_, err := e.p.AddReceivedFiles(e.ctx, batch, tick(), nil)
		e.ok("AddReceivedFiles", err)
		if next(3) == 0 {
			// A file that could not be recorded as a received file.
			q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineNotDICOM, Details: map[string]string{"offset": "0"},
				Receiver: recv, ReceptionID: r.ID, Size: int64(next(500) + 1), SHA256: sum(round), Incoming: "quarantine/" + e.id("x"),
				ContentKept: true, Quarantined: tick()}
			_, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
			e.ok("HoldFiles", err)
			held = append(held, q)
		}
		if next(3) == 0 {
			// A received file held at reception (a label it could not make).
			f := batch[0]
			q := tenant.QuarantinedFile{ID: e.id("q"), Reason: tenant.QuarantineBadLabel,
				Details: map[string]string{"label": "subject", "fault": tenant.FaultEmpty}, Receiver: recv, ReceptionID: r.ID,
				SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SOPClassUID: f.SOPClassUID,
				TransferSyntaxUID: f.TransferSyntaxUID, SHA256: f.SHA256, Size: f.Size, Incoming: f.Incoming, ContentKept: true, Quarantined: tick()}
			n, err := e.p.HoldFiles(e.ctx, []tenant.QuarantinedFile{q}, nil)
			e.ok("HoldFiles", err)
			if n == 1 {
				quarantinedNow[f.SOPInstanceUID+f.SHA256.String()] = true
				held = append(held, q)
				if next(2) == 0 {
					// Released back while its reception still receives.
					rel, err := e.p.ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, "", e.id("rc"), tick(), e.audit(), nil)
					e.ok("ReleaseQuarantined into the same reception", err)
					if rel.ReleaseReception == r.ID {
						delete(quarantinedNow, f.SOPInstanceUID+f.SHA256.String())
					}
				}
			}
		}
		run(r, quarantinedNow)
		// Decide some records held: released into another reception (each
		// mode, through either receiver) and run, or rejected.
		for len(held) > 0 && next(2) == 0 {
			i := next(len(held))
			q := held[i]
			held = append(held[:i], held[i+1:]...)
			if next(3) == 0 {
				_, err := e.p.RejectQuarantined(e.ctx, q.ID, "not wanted", tick(), e.audit(), nil)
				if fault.KindOf(err) != fault.Precondition {
					e.ok("RejectQuarantined", err)
				}
				continue
			}
			rel, err := e.p.ReleaseQuarantined(e.ctx, q.ID, tenant.ReleaseAsIs, []string{"a", "b"}[next(2)], e.id("rc"), tick(), e.audit(), nil)
			if fault.KindOf(err) == fault.Precondition {
				continue // released already, its bytes never kept, or a different file whose UID is held
			}
			e.ok("ReleaseQuarantined", err)
			if rel.ReleaseReception != "" {
				joined, _, _ := e.p.Reception(e.ctx, rel.ReleaseReception)
				if joined.State == reception.Receiving {
					run(joined, map[string]bool{})
				}
			}
		}
		// Retry or discard a failed reception now and then.
		failed, _, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{States: []reception.State{reception.Failed}}, master.Page{Limit: 5})
		e.ok("ListReceptions", err)
		for _, f := range failed {
			if f.RetryReception != "" || f.DiscardReason != "" {
				continue
			}
			if next(2) == 0 {
				_, err := e.p.DiscardReception(e.ctx, f.ID, "not wanted", tick(), e.audit(), nil)
				e.ok("DiscardReception", err)
				continue
			}
			_, retry, err := e.p.RetryReception(e.ctx, f.ID, e.id("rr"), "", tick(), e.audit(), nil)
			e.ok("RetryReception", err)
			run(retry, map[string]bool{})
		}
		// The catalog's running totals equal a recount after every round
		// of receptions, releases, rejections, retries and discards.
		e.wantTotalsRecounted("round " + strconv.Itoa(round))
	}
	ended, _, err := e.p.ListReceptions(e.ctx, tenant.ReceptionFilter{States: []reception.State{reception.Done}}, master.Page{Limit: 1000})
	e.ok("ListReceptions", err)
	if len(ended) < 20 {
		e.Errorf("only %d receptions ended done, so the test tested little", len(ended))
	}
}
