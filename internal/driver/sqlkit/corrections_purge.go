package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What a purge may remove (correction_keys), what else it clears of a
// withdrawn subject, and what a correction not ended holds for arrivals.
// The clock a purge trusts is corrections_clock.go's.

// PurgeTargets implements tenant.Corrections.
func (k Sending) PurgeTargets(ctx context.Context, id string, limit int) ([]tenant.PurgeTarget, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return nil, err
	}
	if limit < 1 || limit > tenant.MaxPurgeTargets {
		return nil, fault.New(fault.Invalid, "a limit of purge targets is 1 to 1000")
	}
	var out []tenant.PurgeTarget
	err := k.Host.Read(ctx, "the correction's objects cannot be read", func(tx SendingTx) error {
		out = nil
		// A purge of what a hold covers waits before its next page of
		// objects: nothing held is removed (tenant.Holds).
		hold, err := correctionHeldIn(ctx, tx, id)
		if err != nil {
			return err
		}
		if hold != "" {
			return tenant.HoldRefusalError(hold)
		}
		// A switch of its storages pauses the purge: the copies it removes
		// from are named, and a swap points the names at other places.
		if err := checkNoSwitch(ctx, tx, id); err != nil {
			return err
		}
		gens := &generations{tx: tx}
		var dones []string
		// An instance's object is shared when a live row resolves to its
		// key: the same UID on the same storage, by the key the catalog
		// holds whole, or by the layout's key under the same labels.
		err = tx.Query(ctx, `SELECT ck.ord, ck.storage, ck.subject_label, ck.session_label, ck.scan_label, ck.sop_instance_uid,
			ck.storage_key, ck.size, ck.sha256, ck.copies_done,
			EXISTS (SELECT 1 FROM instances AS i JOIN scans AS s ON s.id = i.scan_id WHERE ck.ord = 1
				AND i.sop_instance_uid = ck.sop_instance_uid AND s.storage = ck.storage
				AND ((ck.storage_key <> '' AND i.storage_key = ck.storage_key)
					OR (ck.storage_key = '' AND i.storage_key = '' AND s.subject_label = ck.subject_label
						AND s.session_label = ck.session_label AND s.label = ck.scan_label)))
			FROM correction_keys AS ck WHERE ck.correction_id = ? AND ck.outcome = ''
			ORDER BY ck.ord, ck.storage, ck.subject_label, ck.session_label, ck.scan_label, ck.sop_instance_uid LIMIT ?`,
			[]any{id, limit}, func(r Row) error {
				var t tenant.PurgeTarget
				var sum []byte
				var done string
				if err := r.Scan(&t.Ord, &t.Storage, &t.Path.Subject, &t.Path.Session, &t.Path.Scan, &t.UID, &t.StorageKey, &t.Size,
					&sum, &done, &t.Shared); err != nil {
					return err
				}
				t.SHA256 = ToSHA256(sum)
				out, dones = append(out, t), append(dones, done)
				return nil
			})
		if err != nil {
			return err
		}
		// A copy's removal stands only at the generation it was made at:
		// once a name points at another place, every copy is asked again.
		for i := range out {
			if out[i].Done, err = tenant.CurrentCopiesDone(tenant.ParseCopiesDone(dones[i]), gens.of(ctx)); err != nil {
				return err
			}
		}
		// A scan's manifest or metadata is shared when a live scan is at
		// its labels on its storage.
		for i := range out {
			t := &out[i]
			if t.Ord == tenant.TargetInstance {
				continue
			}
			live, err := exists(ctx, tx, `SELECT 1 FROM scans WHERE subject_folded = ? AND session_folded = ? AND folded = ? AND storage = ?`,
				label.Fold(t.Path.Subject), label.Fold(t.Path.Session), label.Fold(t.Path.Scan), t.Storage)
			if err != nil {
				return err
			}
			t.Shared = live
		}
		return nil
	})
	return out, err
}

// AddPurgeTargets implements tenant.Corrections.
func (k Sending) AddPurgeTargets(ctx context.Context, id string, targets []tenant.PurgeTarget) error {
	if len(targets) > tenant.MaxPurgeTargets {
		return fault.New(fault.Invalid, "at most 1000 purge targets are added at once")
	}
	for _, t := range targets {
		if err := t.Validate(); err != nil {
			return err
		}
	}
	return k.stepRow(ctx, id, "the correction's objects cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		if c.State != corr.StatePurging {
			return tenant.Precondition("the correction is " + string(c.State) + ", not purging")
		}
		// An object not settled stops a switch's swap: none is added while
		// one runs, so the swap is not refused for it.
		if err := checkNoSwitch(ctx, tx, id); err != nil {
			return err
		}
		for _, t := range targets {
			var sum any
			if !t.SHA256.IsZero() {
				sum = t.SHA256[:]
			}
			if _, err := tx.Exec(ctx, `INSERT INTO correction_keys (correction_id, ord, storage, subject_label, session_label, scan_label,
				sop_instance_uid, storage_key, size, sha256, outcome) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '') ON CONFLICT DO NOTHING`,
				id, t.Ord, t.Storage, t.Path.Subject, t.Path.Session, t.Path.Scan, t.UID, t.StorageKey, t.Size, sum); err != nil {
				return err
			}
		}
		return nil
	})
}

// SettlePurgeTargets implements tenant.Corrections.
func (k Sending) SettlePurgeTargets(ctx context.Context, id string, settled []tenant.PurgeSettled) error {
	if len(settled) > tenant.MaxPurgeTargets {
		return fault.New(fault.Invalid, "at most 1000 purge targets are settled at once")
	}
	for _, s := range settled {
		if err := s.Validate(id); err != nil {
			return err
		}
	}
	return k.stepRow(ctx, id, "the correction's objects cannot be settled", func(tx SendingTx, c *tenant.Correction) error {
		if c.State != corr.StatePurging {
			return tenant.Precondition("the correction is " + string(c.State) + ", not purging")
		}
		// A purge going on (one in flight when a rebuild was made, or one a
		// stop left purging) stops at the end of its page of objects: the
		// objects it removed stay unsettled, and the purge run again once
		// the rebuild has ended asks the storage of each again.
		if err := k.checkNoRebuild(ctx, tx); err != nil {
			return err
		}
		// Nor while a switch of its storages runs: a removal is recorded at
		// its storage's generation now, which is the one it was made at
		// only because a switch neither begins while an object is not
		// settled nor lets one be settled while it runs.
		if err := checkNoSwitch(ctx, tx, id); err != nil {
			return err
		}
		places := map[string]purgePlace{}
		gens := &generations{tx: tx}
		const where = ` WHERE correction_id = ? AND ord = ? AND storage = ? AND subject_label = ? AND session_label = ? AND scan_label = ?
			AND sop_instance_uid = ? AND outcome = ''`
		for _, s := range settled {
			for _, o := range s.Removals {
				if err := k.insertPurged(ctx, tx, o, places); err != nil {
					return err
				}
			}
			t := s.Target
			key := []any{id, t.Ord, t.Storage, t.Path.Subject, t.Path.Session, t.Path.Scan, t.UID}
			if s.Outcome != tenant.OutcomePending {
				if _, err := tx.Exec(ctx, `UPDATE correction_keys SET outcome = ?`+where, append([]any{string(s.Outcome)}, key...)...); err != nil {
					return err
				}
				continue
			}
			// A target some of whose copies answered stays to settle, the
			// copies that did named with their generation now, so the purge
			// run again asks only the rest and records each copy's answer
			// once. The copies recorded before are read from the row, not
			// taken from the caller: those whose generation moved since are
			// dropped with the rest, and every copy is asked again.
			var stored string
			found, err := tx.QueryRow(ctx, `SELECT copies_done FROM correction_keys`+where, key, &stored)
			if err != nil || !found {
				return err
			}
			before := tenant.ParseCopiesDone(stored)
			current, err := tenant.CurrentCopiesDone(before, gens.of(ctx))
			if err != nil {
				return err
			}
			var done []tenant.CopyDone
			if len(current) == len(before) {
				done = before
			}
			for _, o := range s.Removals {
				g, err := gens.of(ctx)(o.Storage)
				if err != nil {
					return err
				}
				done = append(done, tenant.CopyDone{Storage: o.Storage, Generation: g})
			}
			if _, err := tx.Exec(ctx, `UPDATE correction_keys SET copies_done = ?`+where,
				append([]any{tenant.JoinCopiesDone(done...)}, key...)...); err != nil {
				return err
			}
		}
		return nil
	})
}

// generations reads storages' switch generations in a transaction, each
// once.
type generations struct {
	tx SendingTx
	by map[string]int64
}

// of answers the function that reads a storage's generation, 0 for a name
// no swap has raised.
func (g *generations) of(ctx context.Context) func(storage string) (int64, error) {
	return func(storage string) (int64, error) {
		if n, ok := g.by[storage]; ok {
			return n, nil
		}
		var n int64
		if _, err := g.tx.QueryRow(ctx, `SELECT coalesce((SELECT generation FROM storage_generations WHERE storage = ?), 0)`,
			[]any{storage}, &n); err != nil {
			return 0, err
		}
		if g.by == nil {
			g.by = map[string]int64{}
		}
		g.by[storage] = n
		return n, nil
	}
}

// insertPurged records one version a purge removed, at the storage's switch
// generation now and naming the primary it is a replica of now; one
// recorded already is left as it is. A switch pauses every purge of its
// storages (a switch is not begun nor swapped while a purge of them has an
// object not settled, and while one runs a purge's targets are neither
// read nor settled, nor its removals recorded), so the generation now is
// the one the purge acted at.
// places, read once per storage in a transaction, are read again for
// none of a page's other objects.
func (k Sending) insertPurged(ctx context.Context, tx SendingTx, o tenant.PurgedObject, places map[string]purgePlace) error {
	var sum any
	if !o.SHA256.IsZero() {
		sum = o.SHA256[:]
	}
	p, ok := places[o.Storage]
	if !ok {
		if _, err := tx.QueryRow(ctx, `SELECT coalesce((SELECT generation FROM storage_generations WHERE storage = ?), 0),
			coalesce((SELECT replica_of FROM storages WHERE name = ?), '')`, []any{o.Storage, o.Storage}, &p.generation,
			&p.replicaOf); err != nil {
			return err
		}
		places[o.Storage] = p
	}
	_, err := tx.Exec(ctx, `INSERT INTO correction_objects (correction_id, storage, generation, replica_of, object_key, version, size,
		sha256, disposition, retain_until, purged_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, o.ID, o.Storage,
		p.generation, p.replicaOf, o.Key, o.Version, o.Size, sum, string(o.Disposition), k.nullTime(o.RetainUntil), k.D.Time(o.Now))
	return err
}

// purgePlace is where a storage's name points while a purge records what
// it did: its switch generation and the primary it is a replica of.
type purgePlace struct {
	generation int64
	replicaOf  string
}

// RetainedDue implements tenant.Corrections.
func (k Sending) RetainedDue(ctx context.Context, now time.Time, limit int) ([]tenant.PurgedObject, error) {
	if err := master.ValidateTime("time", now); err != nil {
		return nil, err
	}
	if limit < 1 || limit > 1000 {
		return nil, fault.New(fault.Invalid, "a limit is 1 to 1000")
	}
	var out []tenant.PurgedObject
	err := k.Host.Read(ctx, "the retained objects cannot be read", func(tx SendingTx) error {
		out = nil
		return tx.Query(ctx, `SELECT o.correction_id, o.storage, o.object_key, o.version, o.size, o.sha256, o.retain_until
			FROM correction_objects AS o JOIN corrections AS c ON c.id = o.correction_id
			WHERE c.state = 'purged' AND o.disposition = 'retained' AND o.version <> '' AND o.retain_until IS NOT NULL
			AND o.retain_until <= ? ORDER BY o.retain_until, o.correction_id, o.object_key LIMIT ?`, []any{k.D.Time(now), limit},
			func(r Row) error {
				var o tenant.PurgedObject
				var sum []byte
				var until DBTime
				if err := r.Scan(&o.ID, &o.Storage, &o.Key, &o.Version, &o.Size, &sum, &until); err != nil {
					return err
				}
				o.SHA256, o.RetainUntil, o.Disposition = ToSHA256(sum), until.T, tenant.DispositionRetained
				out = append(out, o)
				return nil
			})
	})
	return out, err
}

// withdrawnQuarantine is the condition on quarantine AS q of the files a
// delete of each level clears, with its arguments: its target's instance;
// its scans' series; its sessions' studies; and for a subject, the files of
// the receptions under any label it held. A file of a reception not ended
// is left to that reception.
func withdrawnQuarantine(c tenant.Correction) (string, []any) {
	var where string
	var args []any
	switch c.Level {
	case corr.LevelInstance:
		where, args = `q.sop_instance_uid = ?`, []any{c.Target.Instance}
	case corr.LevelScan:
		where, args = `q.series_uid IN (SELECT series_uid FROM correction_scans WHERE correction_id = ?)`, []any{c.ID}
	case corr.LevelSession:
		where, args = `q.study_uid IN (SELECT study_uid FROM correction_scans WHERE correction_id = ?)`, []any{c.ID}
	default:
		where = `(q.study_uid IN (SELECT study_uid FROM correction_scans WHERE correction_id = ?)
			OR q.reception_id IN (SELECT r.id FROM receptions AS r WHERE r.subject_label IN
				(SELECT value FROM deleted_targets WHERE correction_id = ? AND kind = 'subject')))`
		args = []any{c.ID, c.ID}
	}
	return `(` + where + `) AND NOT EXISTS (SELECT 1 FROM receptions AS rr WHERE rr.id = q.reception_id AND rr.ended = ?)`, args
}

// withdrawnReceptions is the condition on receptions AS r of the ended
// receptions a delete of a session or a subject blanks, with its
// arguments; a delete of a scan or an instance blanks none, since the
// study stays.
func withdrawnReceptions(c tenant.Correction) (string, []any, bool) {
	switch c.Level {
	case corr.LevelSession:
		return `r.study_uid IN (SELECT study_uid FROM correction_scans WHERE correction_id = ?)`, []any{c.ID}, true
	case corr.LevelSubject:
		return `(r.study_uid IN (SELECT study_uid FROM correction_scans WHERE correction_id = ?) OR r.subject_label IN
			(SELECT value FROM deleted_targets WHERE correction_id = ? AND kind = 'subject'))`, []any{c.ID, c.ID}, true
	}
	return "", nil, false
}

// ClearWithdrawn implements tenant.Corrections.
func (k Sending) ClearWithdrawn(ctx context.Context, id string) ([]string, error) {
	var paths []string
	err := k.stepRow(ctx, id, "what names the deleted data cannot be cleared", func(tx SendingTx, c *tenant.Correction) error {
		paths = nil
		if c.Kind != corr.KindDelete || c.State != corr.StatePurging {
			return tenant.Precondition("the correction is not a delete being purged")
		}
		where, args := withdrawnQuarantine(*c)
		args = append(args, k.D.Bool(false))
		var held, kept int64
		// #nosec G202 -- the condition is a constant of withdrawnQuarantine; values are parameters.
		if err := tx.Query(ctx, `SELECT q.state, q.content_kept, q.size, q.incoming FROM quarantine AS q WHERE `+where, args,
			func(r Row) error {
				var state, incoming string
				var contentKept bool
				var size int64
				if err := r.Scan(&state, &contentKept, &size, &incoming); err != nil {
					return err
				}
				if state == string(tenant.QuarantineHeld) {
					held++
					if contentKept {
						kept += size
					}
				}
				if contentKept && incoming != "" {
					paths = append(paths, incoming)
				}
				return nil
			}); err != nil {
			return err
		}
		// #nosec G202 -- as above.
		if _, err := tx.Exec(ctx, `DELETE FROM quarantine WHERE id IN (SELECT q.id FROM quarantine AS q WHERE `+where+`)`, args...); err != nil {
			return err
		}
		if held > 0 {
			if _, err := tx.Exec(ctx, `UPDATE quarantine_totals SET files = files - ?, bytes = bytes - ?`, held, kept); err != nil {
				return err
			}
		}
		rwhere, rargs, ok := withdrawnReceptions(*c)
		if !ok {
			return nil
		}
		var ids []string
		// #nosec G202 -- the condition is a constant of withdrawnReceptions; values are parameters.
		if err := tx.Query(ctx, `SELECT r.id FROM receptions AS r WHERE r.ended = ? AND `+rwhere+` ORDER BY r.id`,
			append([]any{k.D.Bool(true)}, rargs...), func(r Row) error {
				var s string
				err := r.Scan(&s)
				ids = append(ids, s)
				return err
			}); err != nil {
			return err
		}
		for _, rid := range ids {
			if err := tx.Query(ctx, `SELECT incoming FROM received_files WHERE reception_id = ?`, []any{rid}, func(r Row) error {
				var p string
				err := r.Scan(&p)
				if p != "" {
					paths = append(paths, p)
				}
				return err
			}); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `DELETE FROM received_files WHERE reception_id = ?`, rid); err != nil {
				return err
			}
			// The study's UID is unique with its arrival number, so each
			// reception's is the correction's id and its own.
			if _, err := tx.Exec(ctx, `UPDATE receptions SET subject_label = ?, session_label = ?, study_uid = ? WHERE id = ?`,
				c.ID, c.ID, c.ID+"-"+rid, rid); err != nil {
				return err
			}
		}
		return nil
	})
	return paths, err
}

// MarkSwept implements tenant.Corrections.
func (k Sending) MarkSwept(ctx context.Context, id string) error {
	return k.stepRow(ctx, id, "the correction's sweep cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		edit := c.Kind == corr.KindEdit && c.State == corr.StateUndone
		if !edit && (c.Kind != corr.KindRelabel || c.State != corr.StateAbandoned) {
			return tenant.Precondition("the correction is neither an abandoned relabel nor an undone edit")
		}
		if edit {
			// The new revisions the undo took back are removed from every
			// copy: no line of them is written back again.
			if _, err := tx.Exec(ctx, `UPDATE correction_edits SET restore_line = NULL, new_line = NULL WHERE correction_id = ?`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE correction_keys SET outcome = 'purged' WHERE correction_id = ? AND outcome = ''`, id); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE instance_revisions SET purged = ? WHERE correction_id = ?`, k.D.Bool(true), id); err != nil {
				return err
			}
		}
		_, err := tx.Exec(ctx, `UPDATE corrections SET swept = ? WHERE id = ?`, k.D.Bool(true), id)
		return err
	})
}

// SweepableScans implements tenant.Corrections.
func (k Sending) SweepableScans(ctx context.Context, id string) (map[string]bool, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return nil, err
	}
	out := map[string]bool{}
	err := k.Host.Read(ctx, "the relabel's scans cannot be read", func(tx SendingTx) error {
		clear(out)
		scans, err := correctionScansIn(ctx, tx, id)
		if err != nil {
			return err
		}
		for _, s := range scans {
			np := s.NewPath
			live, err := exists(ctx, tx, `SELECT 1 FROM scans WHERE subject_folded = ? AND session_folded = ? AND folded = ?`,
				label.Fold(np.Subject), label.Fold(np.Session), label.Fold(np.Scan))
			if err != nil {
				return err
			}
			if live {
				out[s.SeriesUID] = false
				continue
			}
			err = checkOverlap(ctx, tx, np, id)
			switch {
			case err == nil:
				out[s.SeriesUID] = true
			case fault.KindOf(err) == fault.Precondition:
				out[s.SeriesUID] = false
			default:
				return err
			}
		}
		return nil
	})
	return out, err
}

// HeldByCorrection implements tenant.Corrections.
func (k Sending) HeldByCorrection(ctx context.Context, p label.Path, studyUID, seriesUID string) (tenant.CorrectionHold, error) {
	if err := p.Check(); err != nil {
		return tenant.CorrectionHold{}, fault.Wrap(fault.Invalid, err, "a scan's labels pass the label rule")
	}
	var out tenant.CorrectionHold
	err := k.Host.Read(ctx, "what the corrections hold cannot be read", func(tx SendingTx) error {
		var err error
		out, err = correctionHold(ctx, tx, p, studyUID, seriesUID, nil)
		return err
	})
	return out, err
}

// CheckCorrectionHolds refuses, in a scan's commit, labels or UIDs a
// correction not ended holds, as the label conflict HeldError makes: the
// archive step looked before it put anything (HeldByCorrection), and the
// commit decides in its own transaction.
func CheckCorrectionHolds(ctx context.Context, tx SendingTx, c tenant.ScanCommit) error {
	uids := make([]string, len(c.Instances))
	for i, in := range c.Instances {
		uids[i] = in.SOPInstanceUID
	}
	h, err := correctionHold(ctx, tx, label.Path{Subject: c.SubjectLabel, Session: c.SessionLabel, Scan: c.ScanLabel}, c.StudyUID,
		c.SeriesUID, uids)
	if err != nil || h.Correction == "" {
		return err
	}
	return tenant.HeldError(h)
}

// correctionHold answers the correction not ended that holds the scan path
// p, its study, its series or one of the instances, and at which level: a
// delete's hidden rows, by label at each level and by UID; a relabel's old
// labels at its level, and its new ones while it is prepared (its copies
// are being written there). A relabel published owns the new labels' rows
// as the catalog's: what lands there is never in its purge.
func correctionHold(ctx context.Context, tx SendingTx, p label.Path, study, series string, uids []string) (tenant.CorrectionHold, error) {
	fs, fe, fc := label.Fold(p.Subject), label.Fold(p.Session), label.Fold(p.Scan)
	var id string
	for _, q := range []struct {
		level, query string
		args         []any
	}{
		{corr.LevelSubject, `SELECT correction_id FROM hidden_subjects WHERE folded = ? LIMIT 1`, []any{fs}},
		{corr.LevelSession, `SELECT correction_id FROM hidden_sessions WHERE study_uid = ? OR (subject_folded = ? AND folded = ?) LIMIT 1`,
			[]any{study, fs, fe}},
		{corr.LevelScan, `SELECT correction_id FROM hidden_scans WHERE series_uid = ?
			OR (subject_folded = ? AND session_folded = ? AND folded = ?) LIMIT 1`, []any{series, fs, fe, fc}},
	} {
		found, err := tx.QueryRow(ctx, q.query, q.args, &id)
		if err != nil {
			return tenant.CorrectionHold{}, err
		}
		if found {
			return tenant.CorrectionHold{Correction: id, Level: q.level}, nil
		}
	}
	if len(uids) > 0 {
		args := make([]any, len(uids))
		for i, u := range uids {
			args[i] = u
		}
		found, err := tx.QueryRow(ctx, `SELECT correction_id FROM hidden_instances WHERE sop_instance_uid IN (`+marks(len(uids))+`) LIMIT 1`,
			args, &id)
		if err != nil {
			return tenant.CorrectionHold{}, err
		}
		if found {
			return tenant.CorrectionHold{Correction: id, Level: corr.LevelInstance}, nil
		}
	}
	var out tenant.CorrectionHold
	err := tx.Query(ctx, `SELECT c.id, c.level, c.state, cs.subject_label, cs.session_label, cs.scan_label, cs.new_subject,
		cs.new_session, cs.new_scan FROM correction_scans AS cs JOIN corrections AS c ON c.id = cs.correction_id
		WHERE c.kind = 'relabel' AND c.state IN ('prepared', 'committed', 'purging')
		AND (lower(cs.subject_label) = ? OR lower(cs.new_subject) = ?) ORDER BY c.id`, []any{fs, fs}, func(r Row) error {
		var cid, level, state string
		var old, nw label.Path
		if err := r.Scan(&cid, &level, &state, &old.Subject, &old.Session, &old.Scan, &nw.Subject, &nw.Session, &nw.Scan); err != nil {
			return err
		}
		if out.Correction == "" && (atLevel(old, p, level) || state == string(corr.StatePrepared) && atLevel(nw, p, level)) {
			out = tenant.CorrectionHold{Correction: cid, Level: level}
		}
		return nil
	})
	return out, err
}

// atLevel reports whether the scan path p lies under a's label at level,
// case folded: a subject's relabel holds its whole subject, a session's its
// session, a scan's its scan.
func atLevel(a, p label.Path, level string) bool {
	same := func(x, y string) bool { return label.Fold(x) == label.Fold(y) }
	switch level {
	case corr.LevelSubject:
		return same(a.Subject, p.Subject)
	case corr.LevelSession:
		return same(a.Subject, p.Subject) && same(a.Session, p.Session)
	}
	return same(a.Subject, p.Subject) && same(a.Session, p.Session) && same(a.Scan, p.Scan)
}
