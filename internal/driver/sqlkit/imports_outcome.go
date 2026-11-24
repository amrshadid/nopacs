package sqlkit

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/amrshadid/nopacs/internal/core/importpath"
	"github.com/amrshadid/nopacs/internal/core/reception"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What an import's files became, written in the pipeline's own
// transactions: a file's arrival with its unit's cursor (RecordImportArrival,
// ImportHeld for a file held as it arrived), and each outcome in the
// transaction that decides it (ImportArchived, ImportHeld, ImportEnded,
// ImportRetried, ImportDiscarded). A driver calls these from its pipeline
// methods with its transaction; each returns the events of the jobs it
// ended, which the driver writes with its own.

// deltaArgs returns d's amounts in the order of importDeltaSet.
func deltaArgs(d tenant.ImportDelta) []any {
	return []any{d.Found, d.Skipped, d.HandedIn, d.Archived, d.AlreadyHeld, d.Quarantined, d.Lost, d.Failed, d.Discarded,
		d.Processing, d.Bytes}
}

// importDeltaSet is the SET list that adds a tenant.ImportDelta to a row
// of import_units or import_jobs, whose CHECKs hold the identities after
// each statement.
const importDeltaSet = `found = found + ?, skipped = skipped + ?, handed_in = handed_in + ?, archived = archived + ?,
	already_held = already_held + ?, quarantined = quarantined + ?, lost = lost + ?, failed = failed + ?,
	discarded = discarded + ?, processing = processing + ?, bytes_handed_in = bytes_handed_in + ?`

// addToUnit adds d to a unit's counts and, when the unit's walk ended
// walked and nothing it handed in is processing, makes it done
// (tenant.ImportWalkEnd's walked, done once its last file has an outcome).
func (k Sending) addToUnit(ctx context.Context, tx SendingTx, jobID, unitID string, d tenant.ImportDelta) error {
	if _, err := tx.Exec(ctx, `UPDATE import_units SET `+importDeltaSet+` WHERE job_id = ? AND unit_id = ?`,
		append(deltaArgs(d), jobID, unitID)...); err != nil {
		return err
	}
	if d.Processing >= 0 {
		return nil
	}
	_, err := tx.Exec(ctx, `UPDATE import_units SET state = ? WHERE job_id = ? AND unit_id = ? AND state = ? AND processing = 0`,
		string(tenant.ImportUnitDone), jobID, unitID, string(tenant.ImportUnitWalked))
	return err
}

// saveImportJob adds d to a job's counts and writes how far its listing of
// files got, as ij holds it.
func (k Sending) saveImportJob(ctx context.Context, tx SendingTx, ij *importJobRow, d tenant.ImportDelta) error {
	_, err := tx.Exec(ctx, `UPDATE import_jobs SET `+importDeltaSet+`, next_file = ?, files_listed = ?, files_not_listed = ?
		WHERE job_id = ?`, append(deltaArgs(d), ij.nextFile, ij.filesListed, ij.spec.FilesNotListed, ij.jobID)...)
	return err
}

// addImportTally adds n to a unit's and its job's count of one code.
func addImportTally(ctx context.Context, tx SendingTx, jobID, unitID, kind, code string, n int64) error {
	for _, u := range []string{unitID, ""} {
		if _, err := tx.Exec(ctx, `INSERT INTO import_tallies (job_id, unit_id, kind, code, count) VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (job_id, unit_id, kind, code) DO UPDATE SET count = import_tallies.count + excluded.count`,
			jobID, u, kind, code, n); err != nil {
			return err
		}
	}
	return nil
}

// importListed is a row of import_files to write.
type importListed struct {
	pathKey    []byte
	kind       tenant.ImportFileKind
	unitID     string
	skipCode   tenant.ImportSkipCode
	quarantine string
	reason     tenant.QuarantineReason
	reception  string
	size       int64
	detail     string
	at         time.Time
}

// listImportFile writes f while the job lists fewer than its bound, and
// counts it not listed past it; ij (locked) keeps the counts, which the
// caller saves (saveImportJob). A row hidden before (a failed file
// retried, failing again) is shown again with its number. It returns the
// row's number, 0 when it was not listed.
func (k Sending) listImportFile(ctx context.Context, tx SendingTx, ij *importJobRow, f importListed) (int64, error) {
	if ij.filesListed >= int64(ij.spec.FailuresKept) {
		ij.spec.FilesNotListed++
		return 0, nil
	}
	var seq int64
	found, err := tx.QueryRow(ctx, `SELECT seq FROM import_files WHERE job_id = ? AND path_key = ? AND kind = ? AND hidden = ?`,
		[]any{ij.jobID, f.pathKey, string(f.kind), k.D.Bool(true)}, &seq)
	if err != nil {
		return 0, err
	}
	if found {
		if _, err := tx.Exec(ctx, `UPDATE import_files SET hidden = ?, unit_id = ?, quarantine_id = ?, quarantine_reason = ?,
			reception_id = ?, size = ?, at = ? WHERE job_id = ? AND seq = ?`, k.D.Bool(false), f.unitID, f.quarantine,
			string(f.reason), f.reception, f.size, k.D.Time(f.at), ij.jobID, seq); err != nil {
			return 0, err
		}
		ij.filesListed++
		return seq, nil
	}
	seq = ij.nextFile
	n, err := tx.Exec(ctx, `INSERT INTO import_files (job_id, path_key, kind, seq, unit_id, skip_code, quarantine_id,
		quarantine_reason, reception_id, size, detail, hidden, at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (job_id, path_key, kind) DO NOTHING`, ij.jobID, f.pathKey, string(f.kind), seq, f.unitID,
		string(f.skipCode), f.quarantine, string(f.reason), f.reception, f.size, f.detail, k.D.Bool(false), k.D.Time(f.at))
	if err != nil || n == 0 {
		return 0, err // listed already: the same entry, judged once
	}
	ij.nextFile++
	ij.filesListed++
	return seq, nil
}

// hideFailed hides a failed arrival's row (its reception retried or
// discarded), so the job lists it no more and lists another in its place;
// one that was not listed, past the job's bound, is no longer counted so.
func (k Sending) hideFailed(ctx context.Context, tx SendingTx, ij *importJobRow, a arrival) error {
	if a.listedSeq == 0 {
		ij.spec.FilesNotListed--
		return nil
	}
	n, err := tx.Exec(ctx, `UPDATE import_files SET hidden = ? WHERE job_id = ? AND seq = ? AND hidden = ?`,
		k.D.Bool(true), ij.jobID, a.listedSeq, k.D.Bool(false))
	if n == 1 {
		ij.filesListed--
	}
	return err
}

// errNotImportUnit is the refusal of a unit no import job queued.
func errNotImportUnit() error {
	return tenant.Precondition("the unit of work is not one of an import job's")
}

// judgeImportPath refuses a path outside the unit's folder, and one at or
// before its cursor; it reports false for the path its cursor holds, which
// was recorded before. No message names the path.
func judgeImportPath(u importUnitRow, p string) (bool, error) {
	if !tenant.ImportPathUnder(u.unit.Folder, p) {
		return false, tenant.Precondition("the path is not under the unit's folder")
	}
	if !u.cursorSet {
		return true, nil
	}
	switch c := importpath.Compare(p, u.unit.Cursor); {
	case c == 0:
		return false, nil
	case c < 0:
		return false, tenant.Precondition("the path is before the unit's cursor")
	}
	return true, nil
}

// CheckImportFences checks the fence of each arrival, in the order given,
// each once, locking each unit of work's row first: a driver calls it
// first in the transaction of a batch of received or held files, before
// any reception's row, as every fenced write locks its unit first. A fence
// that no longer holds refuses the whole batch with ErrLeaseLost.
func CheckImportFences(ctx context.Context, tx SendingTx, arrivals []*tenant.ImportArrival) error {
	var seen []tenant.WorkFence
	for _, a := range arrivals {
		if a == nil || slices.Contains(seen, a.Fence) {
			continue
		}
		seen = append(seen, a.Fence)
		if err := tx.CheckFence(ctx, a.Fence); err != nil {
			return err
		}
	}
	return nil
}

// ReceivedArrivals returns the import arrivals a batch of received files
// carries, for CheckImportFences; nil when it carries none.
func ReceivedArrivals(files []tenant.ReceivedFile) []*tenant.ImportArrival {
	var out []*tenant.ImportArrival
	for _, f := range files {
		if f.Import != nil {
			out = append(out, f.Import)
		}
	}
	return out
}

// HeldArrivals returns the import arrivals a batch of quarantine records
// carries, for CheckImportFences; nil when it carries none.
func HeldArrivals(files []tenant.QuarantinedFile) []*tenant.ImportArrival {
	var out []*tenant.ImportArrival
	for _, q := range files {
		if q.Import != nil {
			out = append(out, q.Import)
		}
	}
	return out
}

// importLink is what links an arrival to its received file.
type importLink struct {
	reception, uid string
	sum            tenant.SHA256
}

// RecordImportArrival records a file an import unit handed in, as a
// received file of the reception with the UID and received checksum
// given, in the transaction of the batch that records it (after
// CheckImportFences): the arrival, processing, the unit's cursor, and
// Found, HandedIn and the bytes on the unit and its job. A file at the
// unit's cursor was recorded before and changes nothing.
func (k Sending) RecordImportArrival(ctx context.Context, tx SendingTx, a tenant.ImportArrival, receptionID, uid string,
	sum tenant.SHA256, at time.Time) error {
	_, err := k.recordArrival(ctx, tx, a, &importLink{reception: receptionID, uid: uid, sum: sum}, nil, at)
	return err
}

// recordArrival records a file handed in: into processing, linked to its
// received file (link), or decided at once as held (held, the record of a
// file held as it arrived). It reports false when the file was recorded
// before. A file handed in to a job that has ended is refused
// (tenant.ImportEnded), its batch with it.
func (k Sending) recordArrival(ctx context.Context, tx SendingTx, a tenant.ImportArrival, link *importLink,
	held *tenant.QuarantinedFile, at time.Time) (bool, error) {
	u, found, err := k.importUnitOf(ctx, tx, a.Fence.ID, k.D.lockOf("u"))
	if err != nil {
		return false, err
	}
	if !found {
		return false, errNotImportUnit()
	}
	fresh, err := judgeImportPath(u, a.Path)
	if err != nil || !fresh {
		return false, err
	}
	ij, found, err := importJobInTx(ctx, tx, u.jobID, k.D.Lock)
	if err != nil {
		return false, err
	}
	if !found {
		return false, errNotImportUnit()
	}
	// Read after the import job's row is locked: every change of an import
	// job's state (a cancel, the end of its last walk or outcome, a time
	// out) locks that row first, so the state read here holds to the
	// commit.
	var ended bool
	if _, err := tx.QueryRow(ctx, `SELECT ended FROM jobs WHERE id = ?`, []any{u.jobID}, &ended); err != nil {
		return false, err
	}
	if ended {
		return false, tenant.ImportEnded()
	}
	key := tenant.ImportPathKey(a.Path)
	var outcome tenant.ImportOutcome
	if held == nil {
		parts := ""
		if len(a.Parts) > 0 {
			if parts, err = JSONText(a.Parts); err != nil {
				return false, err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO import_arrivals (job_id, path_key, unit_id, reception_id, sop_instance_uid, sha256,
			size, outcome, listed_seq, at, parts) VALUES (?, ?, ?, ?, ?, ?, ?, '', 0, ?, ?)`, u.jobID, key, u.unit.ID, link.reception,
			link.uid, link.sum[:], a.Size, k.D.Time(at), parts); err != nil {
			return false, err
		}
	} else {
		var reason tenant.QuarantineReason
		outcome, reason = tenant.ImportHeldOutcome(held.Reason)
		if outcome == tenant.ImportOutcomeQuarantined {
			if err := addImportTally(ctx, tx, u.jobID, u.unit.ID, tallyQuarantined, string(reason), 1); err != nil {
				return false, err
			}
		}
		if _, err := k.listImportFile(ctx, tx, &ij, importListed{pathKey: key, kind: outcome.Listed(), unitID: u.unit.ID,
			quarantine: held.ID, reason: reason, reception: held.ReceptionID, size: a.Size, at: at}); err != nil {
			return false, err
		}
		if _, err := tx.Exec(ctx, `UPDATE quarantine SET import_job = ?, import_path = ? WHERE id = ?`,
			u.jobID, a.Path, held.ID); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE import_units SET cursor_key = ? WHERE job_id = ? AND unit_id = ?`, key, u.jobID, u.unit.ID); err != nil {
		return false, err
	}
	d := tenant.ImportArrivalDelta(a.Size, outcome)
	if err := k.addToUnit(ctx, tx, u.jobID, u.unit.ID, d); err != nil {
		return false, err
	}
	return true, k.saveImportJob(ctx, tx, &ij, d)
}

// ArrivalParts implements tenant.ImportWork: the parts of the first
// arrival, in path order, of the received file, read on the index the
// deciding transactions use.
func (k Sending) ArrivalParts(ctx context.Context, receptionID, sopInstanceUID string, sum tenant.SHA256) (map[string]string, bool, error) {
	if err := master.ValidateID("reception id", receptionID); err != nil {
		return nil, false, err
	}
	var text string
	var found bool
	err := k.Host.Read(ctx, "the parts of an imported file cannot be read", func(tx SendingTx) error {
		var err error
		found, err = tx.QueryRow(ctx, `SELECT parts FROM import_arrivals WHERE reception_id = ? AND sop_instance_uid = ? AND sha256 = ?
			ORDER BY path_key LIMIT 1`, []any{receptionID, sopInstanceUID, sum[:]}, &text)
		return err
	})
	if err != nil || !found {
		return nil, false, err
	}
	parts := map[string]string{}
	if text != "" {
		if err := FromJSON(text, &parts); err != nil {
			return nil, false, err
		}
	}
	return parts, true, nil
}

// arrival is a row of import_arrivals.
type arrival struct {
	jobID, unitID string
	pathKey       []byte
	reception     string
	size          int64
	listedSeq     int64
}

// arrivalColumns are an arrival's columns as the deciding transactions read
// them.
const arrivalColumns = `job_id, unit_id, path_key, reception_id, size, listed_seq`

// readArrivals reads the arrivals query selects, in its order.
func readArrivals(ctx context.Context, tx SendingTx, query string, args ...any) ([]arrival, error) {
	var out []arrival
	err := tx.Query(ctx, query, args, func(r Row) error {
		var a arrival
		err := r.Scan(&a.jobID, &a.unitID, &a.pathKey, &a.reception, &a.size, &a.listedSeq)
		out = append(out, a)
		return err
	})
	return out, err
}

// hasArrivals reports whether a reception has an import's file at all: the
// one probe of a deciding transaction of a reception no import fed, on the
// index it would read anyway.
func hasArrivals(ctx context.Context, tx SendingTx, receptionID string) (bool, error) {
	return exists(ctx, tx, `SELECT 1 FROM import_arrivals WHERE reception_id = ? LIMIT 1`, receptionID)
}

// outcome is what a deciding transaction makes of an arrival: o, and
// what it names.
type outcome struct {
	o tenant.ImportOutcome
	// retryInto names the reception a retried arrival moves into.
	retryInto string
	// quarantine and reason are the record and the reason of an arrival
	// quarantined (the record alone for one lost).
	quarantine string
	reason     tenant.QuarantineReason
}

// decide gives each arrival its outcome, in one pass per job and unit, in
// the order of their ids, so two deciding transactions lock the rows they
// share in one order (units, then the job), and returns the events of the
// jobs that ended.
func (k Sending) decide(ctx context.Context, tx SendingTx, arrivals []arrival, outs []outcome, now time.Time) ([]tenant.EventRecord, error) {
	if len(arrivals) == 0 {
		return nil, nil
	}
	type unitKey struct{ job, unit string }
	deltas := map[unitKey]tenant.ImportDelta{}
	var jobs []string
	for i, a := range arrivals {
		key := unitKey{a.jobID, a.unitID}
		deltas[key] = deltas[key].Add(outs[i].o.Delta())
		if !slices.Contains(jobs, a.jobID) {
			jobs = append(jobs, a.jobID)
		}
	}
	slices.Sort(jobs)
	keys := make([]unitKey, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	slices.SortFunc(keys, func(a, b unitKey) int {
		if c := strings.Compare(a.job, b.job); c != 0 {
			return c
		}
		return strings.Compare(a.unit, b.unit)
	})
	var events []tenant.EventRecord
	for _, jobID := range jobs {
		var total tenant.ImportDelta
		for _, key := range keys {
			if key.job != jobID {
				continue
			}
			if err := k.addToUnit(ctx, tx, key.job, key.unit, deltas[key]); err != nil {
				return nil, err
			}
			total = total.Add(deltas[key])
		}
		ij, found, err := importJobInTx(ctx, tx, jobID, k.D.Lock)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		for i, a := range arrivals {
			if a.jobID != jobID {
				continue
			}
			if err := k.settleArrival(ctx, tx, &ij, a, outs[i], now); err != nil {
				return nil, err
			}
		}
		if err := k.saveImportJob(ctx, tx, &ij, total); err != nil {
			return nil, err
		}
		more, err := k.settleImportJob(ctx, tx, jobID, now)
		if err != nil {
			return nil, err
		}
		events = append(events, more...)
	}
	return events, nil
}

// settleArrival writes what an outcome leaves of an arrival: its row
// removed, kept failed, or moved into a retry's reception; its listed row;
// its count by quarantine reason.
func (k Sending) settleArrival(ctx context.Context, tx SendingTx, ij *importJobRow, a arrival, o outcome, now time.Time) error {
	switch o.o {
	case tenant.ImportOutcomeRetried:
		if err := k.hideFailed(ctx, tx, ij, a); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE import_arrivals SET reception_id = ?, outcome = '', listed_seq = 0 WHERE job_id = ? AND path_key = ?`,
			o.retryInto, a.jobID, a.pathKey)
		return err
	case tenant.ImportOutcomeFailed:
		seq, err := k.listImportFile(ctx, tx, ij, importListed{pathKey: a.pathKey, kind: o.o.Listed(), unitID: a.unitID,
			reception: a.reception, size: a.size, at: now})
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE import_arrivals SET outcome = 'failed', listed_seq = ? WHERE job_id = ? AND path_key = ?`,
			seq, a.jobID, a.pathKey)
		return err
	case tenant.ImportOutcomeDiscarded:
		if err := k.hideFailed(ctx, tx, ij, a); err != nil {
			return err
		}
		fallthrough
	case tenant.ImportOutcomeQuarantined, tenant.ImportOutcomeLost:
		if _, err := k.listImportFile(ctx, tx, ij, importListed{pathKey: a.pathKey, kind: o.o.Listed(), unitID: a.unitID,
			quarantine: o.quarantine, reason: o.reason, reception: a.reception, size: a.size, at: now}); err != nil {
			return err
		}
		if o.o == tenant.ImportOutcomeQuarantined {
			if err := addImportTally(ctx, tx, a.jobID, a.unitID, tallyQuarantined, string(o.reason), 1); err != nil {
				return err
			}
		}
	}
	_, err := tx.Exec(ctx, `DELETE FROM import_arrivals WHERE job_id = ? AND path_key = ?`, a.jobID, a.pathKey)
	return err
}

// ArchivedFile is an instance a commit confirmed: its SOP Instance UID and
// received checksum, the key of the received file it was archived from.
type ArchivedFile struct {
	UID string
	Sum tenant.SHA256
}

// ImportArchived gives the arrivals of the instances a commit of the
// reception confirmed their outcome, in the commit's transaction: archived
// for the first in path order of each instance, already held for any other
// arrival of the same file (one instance in two folders).
func (k Sending) ImportArchived(ctx context.Context, tx SendingTx, receptionID string, files []ArchivedFile, now time.Time) ([]tenant.EventRecord, error) {
	if len(files) == 0 {
		return nil, nil
	}
	if has, err := hasArrivals(ctx, tx, receptionID); err != nil || !has {
		return nil, err
	}
	var arrivals []arrival
	var outs []outcome
	for _, f := range files {
		as, err := readArrivals(ctx, tx, `SELECT `+arrivalColumns+` FROM import_arrivals WHERE reception_id = ?
			AND sop_instance_uid = ? AND sha256 = ? AND outcome = '' ORDER BY path_key, job_id`, receptionID, f.UID, f.Sum[:])
		if err != nil {
			return nil, err
		}
		for i, o := range tenant.ImportCommitOutcomes(len(as)) {
			arrivals = append(arrivals, as[i])
			outs = append(outs, outcome{o: o})
		}
	}
	return k.decide(ctx, tx, arrivals, outs, now)
}

// ImportHeld writes, in the transaction of a quarantine record just made,
// what it means for an import: for a file held as it arrived (q.Import),
// its arrival decided at once (after CheckImportFences); for a file of a
// reception, the outcome of the arrivals of its received file
// (quarantined by its reason, or lost), the record naming the job and the
// path of the first of them in path order; for a record a release
// quarantined again, the job and the path of the record it came from.
func (k Sending) ImportHeld(ctx context.Context, tx SendingTx, q tenant.QuarantinedFile, now time.Time) ([]tenant.EventRecord, error) {
	if q.Import != nil {
		_, err := k.recordArrival(ctx, tx, *q.Import, nil, &q, now)
		return nil, err
	}
	if q.ReleasedFrom != "" {
		_, err := tx.Exec(ctx, `UPDATE quarantine SET import_job = (SELECT import_job FROM quarantine WHERE id = ?),
			import_path = (SELECT import_path FROM quarantine WHERE id = ?) WHERE id = ?`, q.ReleasedFrom, q.ReleasedFrom, q.ID)
		return nil, err
	}
	if q.ReceptionID == "" || q.SOPInstanceUID == "" || q.SHA256.IsZero() {
		return nil, nil
	}
	if has, err := hasArrivals(ctx, tx, q.ReceptionID); err != nil || !has {
		return nil, err
	}
	arrivals, err := readArrivals(ctx, tx, `SELECT `+arrivalColumns+` FROM import_arrivals WHERE reception_id = ?
		AND sop_instance_uid = ? AND sha256 = ? AND outcome = '' ORDER BY path_key, job_id`, q.ReceptionID, q.SOPInstanceUID, q.SHA256[:])
	if err != nil || len(arrivals) == 0 {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE quarantine SET import_job = ?, import_path = ? WHERE id = ?`,
		arrivals[0].jobID, tenant.ImportPathOfKey(arrivals[0].pathKey), q.ID); err != nil {
		return nil, err
	}
	o, reason := tenant.ImportHeldOutcome(q.Reason)
	outs := make([]outcome, len(arrivals))
	for i := range outs {
		outs[i] = outcome{o: o, quarantine: q.ID, reason: reason}
	}
	return k.decide(ctx, tx, arrivals, outs, now)
}

// ImportEnded gives the arrivals of a reception that reached the end
// state and still have no outcome theirs the outcome tenant.ImportEndOutcome
// gives that end, in the step's transaction.
func (k Sending) ImportEnded(ctx context.Context, tx SendingTx, receptionID string, state reception.State, now time.Time) ([]tenant.EventRecord, error) {
	o := tenant.ImportEndOutcome(state)
	if o == "" {
		return nil, nil
	}
	if has, err := hasArrivals(ctx, tx, receptionID); err != nil || !has {
		return nil, err
	}
	arrivals, err := readArrivals(ctx, tx, `SELECT `+arrivalColumns+` FROM import_arrivals WHERE reception_id = ? AND outcome = ''
		ORDER BY job_id, path_key`, receptionID)
	if err != nil {
		return nil, err
	}
	outs := make([]outcome, len(arrivals))
	for i := range outs {
		outs[i] = outcome{o: o}
	}
	return k.decide(ctx, tx, arrivals, outs, now)
}

// ImportRetried moves the failed arrivals of a failed reception back to
// processing, into the reception that runs its files again, in the retry's
// transaction.
func (k Sending) ImportRetried(ctx context.Context, tx SendingTx, failedID, retryID string, now time.Time) ([]tenant.EventRecord, error) {
	return k.fromFailed(ctx, tx, failedID, outcome{o: tenant.ImportOutcomeRetried, retryInto: retryID}, now)
}

// ImportDiscarded makes the failed arrivals of a reception whose files a
// person discarded discarded, in the discard's transaction.
func (k Sending) ImportDiscarded(ctx context.Context, tx SendingTx, receptionID string, now time.Time) ([]tenant.EventRecord, error) {
	return k.fromFailed(ctx, tx, receptionID, outcome{o: tenant.ImportOutcomeDiscarded}, now)
}

// fromFailed gives every failed arrival of a reception the outcome o.
func (k Sending) fromFailed(ctx context.Context, tx SendingTx, receptionID string, o outcome, now time.Time) ([]tenant.EventRecord, error) {
	if has, err := hasArrivals(ctx, tx, receptionID); err != nil || !has {
		return nil, err
	}
	arrivals, err := readArrivals(ctx, tx, `SELECT `+arrivalColumns+` FROM import_arrivals WHERE reception_id = ? AND outcome = 'failed'
		ORDER BY job_id, path_key`, receptionID)
	if err != nil {
		return nil, err
	}
	outs := make([]outcome, len(arrivals))
	for i := range outs {
		outs[i] = o
	}
	return k.decide(ctx, tx, arrivals, outs, now)
}
