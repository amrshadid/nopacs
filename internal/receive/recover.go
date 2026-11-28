package receive

import (
	"context"
	"encoding/hex"

	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// RecoverReport is what Recover found and did.
type RecoverReport struct {
	// Temp counts files being written when the process stopped, removed.
	Temp int
	// Made counts files made from a received one (its de-identified copy)
	// that a step left, removed: each is made again when it is needed.
	Made int
	// Renamed counts recorded files that still had their landed name.
	Renamed int
	// Rechecked counts landed files with no row, checked and recorded or
	// held again.
	Rechecked int
	// Removed counts recorded files no row keeps any more, removed.
	Removed int
	// Missing counts files the database says are in the incoming area,
	// owed to a step or a person, and that are not there: each is a fault,
	// logged with its reception or record, never dropped silently.
	Missing int
	// Strangers counts names in the incoming area this server did not
	// make, left alone.
	Strangers int
	// ImportLetGo counts landed files of an import receiver with no row,
	// removed: their import units hand them in again.
	ImportLetGo int
}

// importReceiver reports whether the receiver named name is an import
// receiver. One that was removed is not. One that cannot be read now is an
// error: taking it for a receiver of another type would record an import's
// copy without its arrival, named without its path's parts, so the caller
// keeps the copy for the next start instead.
func (p *Pipeline) importReceiver(ctx context.Context, name string) (bool, error) {
	return p.receiverOfType(ctx, name, tenant.ReceiverImport)
}

// jobReceiver reports whether the receiver named name is a receiver of type
// job, as importReceiver does for an import receiver: an error when it
// cannot be read now, so the file is kept for the next start.
func (p *Pipeline) jobReceiver(ctx context.Context, name string) (bool, error) {
	return p.receiverOfType(ctx, name, tenant.ReceiverJob)
}

// receiverOfType reports whether the receiver named name is of type typ.
func (p *Pipeline) receiverOfType(ctx context.Context, name string, typ tenant.ReceiverType) (bool, error) {
	rcv, err := p.receivers.get(ctx, name)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return false, nil
	case err != nil:
		return false, err
	}
	return rcv.Type == typ, nil
}

// owed is what the database says of one file in the incoming area.
type owed struct {
	// needed says its bytes are owed: to a step that has not run, or to a
	// person.
	needed bool
	// receptionID or recordID say who owes it, for the log.
	receptionID, recordID string
	// file is the received file, for a check of a missing one.
	file *tenant.ReceivedFile
}

// Recover reconciles the incoming area with the tenant's database, before
// any worker runs:
//
//   - the database's side: every file a reception that has not ended keeps
//     (its received files), every file of a failed reception neither
//     retried nor discarded, and every file a held quarantine record keeps;
//   - the disk's side, each file by the state its name says (package
//     incoming): a file being written is removed, since its sender was
//     never answered; a file made from a received one (its de-identified
//     copy) is removed, since the step that needs it makes it again; a landed file the database names is given its
//     recorded name, and one it does not name is checked and recorded or
//     held, as if it had just arrived, since its sender was answered; a
//     recorded file the database names stays, and one it does not is
//     removed, since its row was let go by a step or a person that recorded
//     why first;
//   - a landed file with no row whose latest quarantine record was released
//     into no reception (releasedRecord) is checked again as a release of
//     that record, by the rules of the receiver the release ran through: a
//     release gives the file its landed name before the
//     store records it (Release), so this is a release whose check had not
//     been written when the process stopped. Its new record's id is made
//     from the released record's, so it never meets the old one, which the
//     store would skip as already held, leaving no record to keep the file.
//
// A file the database owes and the disk does not have is a fault, counted
// and logged; nothing is dropped from the database for it. A received file
// whose bytes were let go because the catalog held it already (an early
// duplicate) is not missing.
//
// Its memory holds one entry per file the database names in the incoming
// area: the file's id, and for a received file a copy of its row.
func (p *Pipeline) Recover(ctx context.Context) (RecoverReport, error) {
	var rep RecoverReport
	known := map[[16]byte]owed{}
	note := func(path string, o owed) {
		_, id, err := incoming.ParsePath(path)
		if err != nil {
			return
		}
		var k [16]byte
		if _, err := hex.Decode(k[:], []byte(id)); err != nil {
			return
		}
		if prev, ok := known[k]; ok && prev.needed {
			return
		}
		known[k] = o
	}
	add := func(r tenant.Reception, needed bool) error {
		return p.eachReceived(ctx, r.ID, func(f tenant.ReceivedFile) error {
			note(f.Incoming, owed{needed: needed, receptionID: r.ID, file: &f})
			return nil
		})
	}
	if err := p.eachReception(ctx, tenant.ReceptionFilter{InProgress: true}, func(r tenant.Reception) error {
		// Past verify, a file's copy may already be gone, by design.
		return add(r, r.State != reception.Verified)
	}); err != nil {
		return rep, err
	}
	if err := p.eachReception(ctx, tenant.ReceptionFilter{States: []reception.State{reception.Failed}}, func(r tenant.Reception) error {
		if r.RetryReception != "" || !r.DiscardTime.IsZero() {
			return nil
		}
		return add(r, true)
	}); err != nil {
		return rep, err
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		qs, next, err := p.store.ListQuarantined(ctx, tenant.QuarantineFilter{State: tenant.QuarantineHeld}, page)
		if err != nil {
			return rep, err
		}
		for _, q := range qs {
			if q.ContentKept {
				note(q.Incoming, owed{needed: true, recordID: q.ID})
			}
		}
		if next == "" {
			break
		}
		page.Cursor = next
	}

	seen := make(map[[16]byte]bool, len(known))
	var recheck []incoming.Entry
	strangers, err := p.area.Scan(func(e incoming.Entry) error {
		var k [16]byte
		_, _ = hex.Decode(k[:], []byte(e.ID))
		_, isKnown := known[k]
		switch {
		case e.State == incoming.Made:
			// A de-identified copy is made again by the step that puts
			// it, from the file it was made from, and never read after a
			// restart: one a crash left is let go.
			rep.Made++
			return p.area.RemoveMade(e)
		case e.State == incoming.Temp:
			rep.Temp++
			return p.area.RemoveTemp(e)
		case e.State == incoming.Landed && isKnown:
			seen[k] = true
			rep.Renamed++
			return p.area.Commit(e.Path)
		case e.State == incoming.Landed:
			recheck = append(recheck, e)
		case isKnown:
			seen[k] = true
		default:
			p.at("recover.before-remove")
			rep.Removed++
			return p.area.Remove(e.Path)
		}
		return nil
	})
	rep.Strangers = strangers
	if err != nil {
		return rep, err
	}
	var lost []owed
	for k, o := range known {
		if o.needed && !seen[k] {
			lost = append(lost, o)
		}
	}
	for _, o := range lost {
		if o.file != nil && p.heldElsewhere(ctx, *o.file) {
			continue
		}
		rep.Missing++
		p.stats.MissingFiles.Add(1)
		p.log.Error("a file the database keeps in the incoming area is not there",
			"reception", o.receptionID, "record", o.recordID)
	}
	for _, e := range recheck {
		rep.Rechecked++
		from, via, err := p.releasedRecord(ctx, e.ID)
		if err != nil {
			return rep, err
		}
		f, err := p.landedFile(e)
		if err != nil {
			return rep, err
		}
		// A release names the receiver whose rules the file runs by
		// (afterRelease does the same): one that de-identifies lands it
		// remapped and keyed, whichever receiver it first came to.
		if via == "" {
			via = e.Receiver
		}
		imported := false
		if from == "" {
			if imported, err = p.importReceiver(ctx, e.Receiver); err != nil {
				return rep, err
			}
		}
		if imported {
			// A file an import handed in is recorded with its arrival and
			// its unit's cursor before the unit is answered, so one landed
			// with no row was never counted: the unit, resuming after its
			// cursor, hands it in again, and recording this copy without
			// its arrival would name it without its path's parts.
			rep.ImportLetGo++
			if err := p.area.Remove(e.Path); err != nil {
				return rep, err
			}
			continue
		}
		l := landed{file: f, receiver: via, releasedFrom: from}
		if from == "" {
			// What a transfer's source required of the file (de-identified,
			// Arrival.RequireDeidentified) was in memory only: a file of a
			// transfer job that landed and was never recorded is judged as
			// one that must be de-identified, so a receiver that archives as
			// received holds it for a person where it would archive it, and
			// the transfer's confirmation reports the copy as not found. Its
			// record is protected (check), so a release through a receiver
			// that de-identifies is the way out for a file held here that
			// was in fact a legitimate as-received copy; through one that
			// archives as received the store refuses it.
			if l.told.RequireDeidentified, err = p.jobReceiver(ctx, e.Receiver); err != nil {
				return rep, err
			}
		}
		p.intakeNow(ctx, l)
	}
	return rep, nil
}

// maxReleases bounds how many times one file's records are followed from
// release to release: each release of a file that joins nothing makes one
// more record, and a person would have to release the same file this many
// times for the bound to matter.
const maxReleases = 64

// releasedRecord returns the id of the latest quarantine record of the file
// with the incoming id fileID when that record was released into no
// reception, and "" otherwise. A file held when it arrived has the record
// derivedID("q", "receive", fileID); each check after a release holds it,
// if it holds it again, under derivedID("q", "release", <the released
// record>) (holdAtReceive). Following those ids needs nothing but the
// database, so a restart reads what a release in progress left. via is the
// receiver that release ran through (QuarantinedFile.ReleaseReceiver),
// empty when none is recorded.
func (p *Pipeline) releasedRecord(ctx context.Context, fileID string) (id, via string, err error) {
	id = derivedID("q", "receive", fileID)
	var latest tenant.QuarantinedFile
	for range maxReleases {
		q, err := p.store.QuarantinedFile(ctx, id)
		if fault.KindOf(err) == fault.NotFound {
			break
		}
		if err != nil {
			return "", "", err
		}
		latest = q
		if q.State != tenant.QuarantineReleased {
			break
		}
		id = derivedID("q", "release", q.ID)
	}
	if latest.ID == "" || latest.State != tenant.QuarantineReleased || latest.ReleaseReception != "" {
		return "", "", nil
	}
	return latest.ID, latest.ReleaseReceiver, nil
}

// heldElsewhere reports whether the catalog holds a file under its UID with
// its checksum, so its bytes were let go on purpose.
func (p *Pipeline) heldElsewhere(ctx context.Context, f tenant.ReceivedFile) bool {
	ds, err := p.store.CheckDuplicates(ctx, []tenant.DuplicateQuery{{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}})
	return err == nil && len(ds) == 1 && ds[0].Kind == tenant.SameFile
}

// eachReception calls fn for each reception the filter keeps, a page at a
// time.
func (p *Pipeline) eachReception(ctx context.Context, filter tenant.ReceptionFilter, fn func(tenant.Reception) error) error {
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rs, next, err := p.store.ListReceptions(ctx, filter, page)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := fn(r); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// landedFile reads a landed file found at start: its checksum, which only
// its bytes can give, by reading it once.
func (p *Pipeline) landedFile(e incoming.Entry) (incoming.File, error) {
	f, size, err := p.area.Open(e.Path)
	if err != nil {
		return incoming.File{}, err
	}
	defer func() { _ = f.Close() }()
	sum, err := sumReader(f)
	if err != nil {
		return incoming.File{}, err
	}
	return incoming.File{Receiver: e.Receiver, ID: e.ID, Path: e.Path, Size: size, SHA256: sum}, nil
}

// intakeNow checks a landed file and writes its row before it returns, for
// Recover, which runs before the batcher.
func (p *Pipeline) intakeNow(ctx context.Context, l landed) {
	d, err := p.check(ctx, l)
	if err != nil {
		p.log.Warn("a landed file found at start could not be checked yet; it is kept and checked again", "reason", safe(err))
		p.batch.later(l)
		return
	}
	p.batch.flush(ctx, []batchItem{{l: l, d: d}})
}

// repairWork queues the unit each reception that has not ended needs and
// lacks: the quiet unit of a receiving one (a crash between its opening and
// its unit's queuing), and the unit of the state of each one past receiving
// that has none (ReceptionsWithoutWork: a crash between a step and the
// queuing of the next unit). A unit queued twice is queued once.
func (p *Pipeline) repairWork(ctx context.Context) error {
	if err := p.eachReception(ctx, tenant.ReceptionFilter{States: []reception.State{reception.Receiving}}, func(r tenant.Reception) error {
		return p.ensureQuiet(ctx, r)
	}); err != nil {
		return err
	}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		rs, next, err := p.store.ReceptionsWithoutWork(ctx, page)
		if err != nil {
			return err
		}
		for _, r := range rs {
			if err := p.ensureNext(ctx, r); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}
