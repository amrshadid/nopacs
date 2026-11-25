package sqlkit

import (
	"context"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A tenant's holds (tenant.Holds) on a SQL database: the holds, which
// storages each record goes on and is written to, and what a hold covers.

// ListHolds is the holds' list's name, the first field of its cursor.
const ListHolds = "tenant-holds"

// holdColumns are a hold's columns as the store reads them.
const holdColumns = `id, subject_label, session_label, scan_label, level, reason, state, placed_by, placed_at, release_reason,
	released_by, released_at, found_on_storage, seq`

// scanHold reads one row of holdColumns.
func scanHold(r Row) (tenant.Hold, error) {
	var h tenant.Hold
	var level, state string
	var placed, released DBTime
	err := r.Scan(&h.ID, &h.Target.Subject, &h.Target.Session, &h.Target.Scan, &level, &h.Reason, &state, &h.PlacedBy, &placed,
		&h.ReleaseReason, &h.ReleasedBy, &released, &h.FoundOnStorage, &h.Seq)
	h.Target.WholeTenant, h.State, h.Placed, h.Released = level == "tenant", tenant.HoldState(state), placed.T, released.T
	return h, err
}

// fillHoldRecords fills a hold's storages and those owed its record.
func fillHoldRecords(ctx context.Context, tx SendingTx, h *tenant.Hold) error {
	h.Storages, h.StoragesPending = nil, nil
	return tx.Query(ctx, `SELECT storage, written_seq FROM hold_records WHERE hold_id = ? ORDER BY storage`, []any{h.ID}, func(r Row) error {
		var s string
		var seq int64
		if err := r.Scan(&s, &seq); err != nil {
			return err
		}
		h.Storages = append(h.Storages, s)
		if seq < h.Seq {
			h.StoragesPending = append(h.StoragesPending, s)
		}
		return nil
	})
}

// holdIn reads the hold of that id with its records, locked with lock;
// false when there is none.
func holdIn(ctx context.Context, tx SendingTx, id, lock string) (tenant.Hold, bool, error) {
	var out tenant.Hold
	found := false
	if err := tx.Query(ctx, `SELECT `+holdColumns+` FROM holds WHERE id = ?`+lock, []any{id}, func(r Row) error {
		var err error
		out, err = scanHold(r)
		found = true
		return err
	}); err != nil || !found {
		return out, found, err
	}
	return out, true, fillHoldRecords(ctx, tx, &out)
}

// heldTarget is one active hold, as what it covers is matched.
type heldTarget struct {
	id     string
	target tenant.HoldTarget
}

// heldSet is the tenant's active holds: few, read whole.
type heldSet []heldTarget

// activeHolds reads every active hold, by id.
func activeHolds(ctx context.Context, tx SendingTx) (heldSet, error) {
	var out heldSet
	err := tx.Query(ctx, `SELECT id, level, subject_folded, session_folded, scan_folded FROM holds WHERE state = 'active' ORDER BY id`, nil,
		func(r Row) error {
			var h heldTarget
			var level string
			if err := r.Scan(&h.id, &level, &h.target.Subject, &h.target.Session, &h.target.Scan); err != nil {
				return err
			}
			h.target.WholeTenant = level == "tenant"
			out = append(out, h)
			return nil
		})
	return out, err
}

// coveringPath returns the id of the first active hold that covers any of
// what p names, or "".
func (s heldSet) coveringPath(p label.Path) string {
	for _, h := range s {
		if h.target.Covers(p) {
			return h.id
		}
	}
	return ""
}

// covering returns the id of the first active hold that covers any of the
// session of those folded labels (a hold of one of its scans covers it, as
// a disposal takes the whole session), or "".
func (s heldSet) covering(subjectFolded, sessionFolded string) string {
	return s.coveringPath(label.Path{Subject: subjectFolded, Session: sessionFolded})
}

// correctionHeldIn returns the id of an active hold that covers any scan
// of the correction, by its labels before the correction or by those a
// relabel gives it, or "". Both are matched because a relabel's publish
// moves the holds of what it moves to the new labels (relabelHolds), and
// a hold placed after the publish names the new labels too, while the
// relabel's purge removes the objects under the old ones: matching only
// the old labels would let that purge remove what a hold keeps.
func correctionHeldIn(ctx context.Context, tx SendingTx, id string) (string, error) {
	holds, err := activeHolds(ctx, tx)
	if err != nil || len(holds) == 0 {
		return "", err
	}
	found := ""
	err = tx.Query(ctx, `SELECT subject_label, session_label, scan_label, new_subject, new_session, new_scan FROM correction_scans
		WHERE correction_id = ? ORDER BY series_uid`, []any{id}, func(r Row) error {
		var old, moved label.Path
		if err := r.Scan(&old.Subject, &old.Session, &old.Scan, &moved.Subject, &moved.Session, &moved.Scan); err != nil {
			return err
		}
		if found == "" {
			found = holds.coveringPath(old)
		}
		if found == "" && moved.Subject != "" {
			found = holds.coveringPath(moved)
		}
		return nil
	})
	return found, err
}

// CorrectionHeld implements tenant.Holds.
func (k Sending) CorrectionHeld(ctx context.Context, id string) (string, error) {
	if err := tenant.ValidateCorrectionID(id); err != nil {
		return "", err
	}
	var out string
	err := k.Host.Read(ctx, "the correction's holds cannot be read", func(tx SendingTx) error {
		var err error
		out, err = correctionHeldIn(ctx, tx, id)
		return err
	})
	return out, err
}

// holdScope returns the condition on a table's folded labels (the columns
// named) that selects what t names, and its arguments.
func holdScope(t tenant.HoldTarget, subject, session, scan string) (string, []any) {
	where, args := []string{subject + " = ?"}, []any{label.Fold(t.Subject)}
	if t.Session != "" {
		where, args = append(where, session+" = ?"), append(args, label.Fold(t.Session))
	}
	if t.Scan != "" {
		where, args = append(where, scan+" = ?"), append(args, label.Fold(t.Scan))
	}
	return strings.Join(where, " AND "), args
}

// holdStoragesIn returns the storages a hold of t goes on, by name: those
// of the scans it covers, in the catalog or hidden by a delete not yet
// purged, and their replicas; every storage for the whole tenant. A target
// the tenant holds nothing of is NotFound.
func holdStoragesIn(ctx context.Context, tx SendingTx, t tenant.HoldTarget) ([]tenant.HoldStorage, error) {
	var out []tenant.HoldStorage
	if t.WholeTenant {
		err := tx.Query(ctx, `SELECT name, replica_of FROM storages ORDER BY name`, nil, func(r Row) error {
			var s tenant.HoldStorage
			var of string
			err := r.Scan(&s.Name, &of)
			s.Replica = of != ""
			out = append(out, s)
			return err
		})
		return out, err
	}
	var primaries []string
	add := func(r Row) error {
		var s string
		if err := r.Scan(&s); err != nil {
			return err
		}
		if !slices.Contains(primaries, s) {
			primaries = append(primaries, s)
		}
		return nil
	}
	live, args := holdScope(t, "subject_folded", "session_folded", "folded")
	// #nosec G202 -- every fragment is a constant; values are parameters.
	if err := tx.Query(ctx, `SELECT DISTINCT storage FROM scans WHERE `+live, args, add); err != nil {
		return nil, err
	}
	hidden, hargs := holdScope(t, "lower(cs.subject_label)", "lower(cs.session_label)", "lower(cs.scan_label)")
	// #nosec G202 -- every fragment is a constant; values are parameters.
	if err := tx.Query(ctx, `SELECT DISTINCT cs.storage FROM correction_scans AS cs JOIN corrections AS c ON c.id = cs.correction_id
		WHERE c.kind = 'delete' AND c.state IN ('committed', 'purging') AND `+hidden, hargs, add); err != nil {
		return nil, err
	}
	if len(primaries) == 0 {
		return nil, notFound("the tenant holds no such subject, session or scan, nor one under a delete not yet purged")
	}
	for _, p := range primaries {
		out = append(out, tenant.HoldStorage{Name: p})
		if err := tx.Query(ctx, `SELECT name FROM storages WHERE replica_of = ? ORDER BY name`, []any{p}, func(r Row) error {
			var s string
			err := r.Scan(&s)
			out = append(out, tenant.HoldStorage{Name: s, Replica: true})
			return err
		}); err != nil {
			return nil, err
		}
	}
	slices.SortFunc(out, func(a, b tenant.HoldStorage) int { return strings.Compare(a.Name, b.Name) })
	return slices.CompactFunc(out, func(a, b tenant.HoldStorage) bool { return a.Name == b.Name }), nil
}

// HoldStorages implements tenant.Holds.
func (k Sending) HoldStorages(ctx context.Context, target tenant.HoldTarget) ([]tenant.HoldStorage, error) {
	if err := target.Validate(); err != nil {
		return nil, err
	}
	var out []tenant.HoldStorage
	err := k.Host.Read(ctx, "the hold's storages cannot be read", func(tx SendingTx) error {
		var err error
		out, err = holdStoragesIn(ctx, tx, target)
		return err
	})
	return out, err
}

// matchingHoldIn returns the active hold with target and reason, and false
// when there is none.
func matchingHoldIn(ctx context.Context, tx SendingTx, t tenant.HoldTarget, reason string) (tenant.Hold, bool, error) {
	id := ""
	if _, err := tx.QueryRow(ctx, `SELECT id FROM holds WHERE state = 'active' AND level = ? AND subject_folded = ? AND session_folded = ?
		AND scan_folded = ? AND reason = ? ORDER BY id LIMIT 1`, []any{t.Level(), label.Fold(t.Subject), label.Fold(t.Session),
		label.Fold(t.Scan), reason}, &id); err != nil || id == "" {
		return tenant.Hold{}, false, err
	}
	return holdIn(ctx, tx, id, "")
}

// MatchingHold implements tenant.Holds.
func (k Sending) MatchingHold(ctx context.Context, target tenant.HoldTarget, reason string) (tenant.Hold, bool, error) {
	if err := target.Validate(); err != nil {
		return tenant.Hold{}, false, err
	}
	var out tenant.Hold
	var found bool
	err := k.Host.Read(ctx, "the holds cannot be read", func(tx SendingTx) error {
		var err error
		out, found, err = matchingHoldIn(ctx, tx, target, reason)
		return err
	})
	return out, found, err
}

// PlaceHold implements tenant.Holds.
func (k Sending) PlaceHold(ctx context.Context, p tenant.HoldPlace, audit master.AuditEntry, guard tenant.Guard) (tenant.Hold, bool, error) {
	if err := p.Validate(); err != nil {
		return tenant.Hold{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Hold{}, false, err
	}
	var out tenant.Hold
	var made bool
	err := k.Host.Write(ctx, "the hold cannot be placed", func(tx SendingTx) error {
		made = false
		if err := k.lockRetention(ctx, tx); err != nil {
			return err
		}
		h, found, err := matchingHoldIn(ctx, tx, p.Target, p.Reason)
		if err != nil {
			return err
		}
		if found {
			out = h
			return tx.Audited(ctx, withObject(audit, h.ID), guard)
		}
		if taken, err := exists(ctx, tx, `SELECT 1 FROM holds WHERE id = ?`, p.ID); err != nil || taken {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "a hold with this id exists")
		}
		storages, err := holdStoragesIn(ctx, tx, p.Target)
		if err != nil {
			return err
		}
		if err := tenant.CheckHoldWritten(storages, p.Written); err != nil {
			return err
		}
		t := p.Target
		if _, err := tx.Exec(ctx, `INSERT INTO holds (id, level, subject_label, session_label, scan_label, subject_folded, session_folded,
			scan_folded, reason, state, placed_by, placed_at, release_reason, released_by, released_at, found_on_storage, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, '', '', NULL, ?, 1)`, p.ID, t.Level(), t.Subject, t.Session, t.Scan,
			label.Fold(t.Subject), label.Fold(t.Session), label.Fold(t.Scan), p.Reason, p.KeyID, k.D.Time(p.Now), k.D.Bool(false)); err != nil {
			return err
		}
		for _, s := range storages {
			written := 0
			if slices.Contains(p.Written, s.Name) {
				written = 1
			}
			if _, err := tx.Exec(ctx, `INSERT INTO hold_records (hold_id, storage, written_seq) VALUES (?, ?, ?)`, p.ID, s.Name,
				written); err != nil {
				return err
			}
		}
		if out, _, err = holdIn(ctx, tx, p.ID, ""); err != nil {
			return err
		}
		made = true
		return tx.Audited(ctx, withObject(audit, p.ID), guard)
	})
	return out, made, err
}

// ReleaseHold implements tenant.Holds.
func (k Sending) ReleaseHold(ctx context.Context, r tenant.HoldRelease, audit master.AuditEntry, guard tenant.Guard) (tenant.Hold, error) {
	if err := r.Validate(); err != nil {
		return tenant.Hold{}, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Hold{}, err
	}
	var out tenant.Hold
	err := k.Host.Write(ctx, "the hold cannot be released", func(tx SendingTx) error {
		if err := k.lockRetention(ctx, tx); err != nil {
			return err
		}
		h, found, err := holdIn(ctx, tx, r.ID, k.D.Lock)
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no hold with this id")
		}
		if h.State == tenant.HoldActive {
			if _, err := tx.Exec(ctx, `UPDATE holds SET state = 'released', release_reason = ?, released_by = ?, released_at = ?,
				seq = seq + 1 WHERE id = ?`, r.Reason, r.KeyID, k.D.Time(r.Now), r.ID); err != nil {
				return err
			}
		}
		if out, _, err = holdIn(ctx, tx, r.ID, ""); err != nil {
			return err
		}
		return tx.Audited(ctx, withObject(audit, r.ID), guard)
	})
	return out, err
}

// Hold implements tenant.Holds.
func (k Sending) Hold(ctx context.Context, id string) (tenant.Hold, error) {
	if err := tenant.ValidateHoldID(id); err != nil {
		return tenant.Hold{}, err
	}
	var out tenant.Hold
	err := k.Host.Read(ctx, "the hold cannot be read", func(tx SendingTx) error {
		h, found, err := holdIn(ctx, tx, id, "")
		if err != nil {
			return err
		}
		if !found {
			return notFound("the tenant has no hold with this id")
		}
		out = h
		return nil
	})
	return out, err
}

// ListHolds implements tenant.Holds.
func (k Sending) ListHolds(ctx context.Context, f tenant.HoldFilter, page master.Page) ([]tenant.Hold, string, error) {
	if err := f.Validate(); err != nil {
		return nil, "", err
	}
	if err := page.Validate(); err != nil {
		return nil, "", err
	}
	states := make([]string, len(f.States))
	for i, s := range f.States {
		states[i] = string(s)
	}
	slices.Sort(states)
	states = slices.Compact(states)
	binding := k.Host.Binding(strings.Join(states, ","), f.Subject)
	where, args := []string{"1 = 1"}, []any{}
	if page.Cursor != "" {
		fs, err := DecodeCursor(page.Cursor, ListHolds, binding, 1)
		if err != nil {
			return nil, "", err
		}
		where, args = append(where, "id < ?"), append(args, fs[0])
	}
	if len(states) > 0 {
		where = append(where, "state IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(states)), ", ")+")")
		for _, s := range states {
			args = append(args, s)
		}
	}
	if f.Subject != "" {
		where, args = append(where, "(subject_folded = ? OR level = 'tenant')"), append(args, label.Fold(f.Subject))
	}
	args = append(args, page.Limit+1)
	var out []tenant.Hold
	err := k.Host.Read(ctx, "the holds cannot be listed", func(tx SendingTx) error {
		out = nil
		// #nosec G202 -- every fragment is a constant; values are parameters.
		if err := tx.Query(ctx, `SELECT `+holdColumns+` FROM holds WHERE `+strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT ?`, args,
			func(r Row) error {
				h, err := scanHold(r)
				out = append(out, h)
				return err
			}); err != nil {
			return err
		}
		for i := range out {
			if err := fillHoldRecords(ctx, tx, &out[i]); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	out, last, more := Cut(out, page.Limit)
	if !more {
		return out, "", nil
	}
	return out, EncodeCursor(ListHolds, binding, last.ID), nil
}

// HoldRecordsOwed implements tenant.Holds.
func (k Sending) HoldRecordsOwed(ctx context.Context, limit int) ([]tenant.HoldRecordOwed, error) {
	if limit < 1 || limit > tenant.MaxHoldList {
		return nil, fault.New(fault.Invalid, "a limit of hold records is 1 to 1000")
	}
	var out []tenant.HoldRecordOwed
	err := k.Host.Read(ctx, "the hold records owed cannot be read", func(tx SendingTx) error {
		out = nil
		type owed struct{ id, storage string }
		var rows []owed
		if err := tx.Query(ctx, `SELECT r.hold_id, r.storage FROM hold_records AS r JOIN holds AS h ON h.id = r.hold_id
			WHERE r.written_seq < h.seq ORDER BY r.hold_id, r.storage LIMIT ?`, []any{limit}, func(r Row) error {
			var o owed
			err := r.Scan(&o.id, &o.storage)
			rows = append(rows, o)
			return err
		}); err != nil {
			return err
		}
		for _, o := range rows {
			h, _, err := holdIn(ctx, tx, o.id, "")
			if err != nil {
				return err
			}
			out = append(out, tenant.HoldRecordOwed{Hold: h, Storage: o.storage})
		}
		return nil
	})
	return out, err
}

// MarkHoldRecord implements tenant.Holds.
func (k Sending) MarkHoldRecord(ctx context.Context, id, storage string, seq int64) error {
	if err := tenant.ValidateHoldID(id); err != nil {
		return err
	}
	if err := tenant.ValidateName("storage", storage); err != nil {
		return err
	}
	if seq < 1 {
		return fault.New(fault.Invalid, "a hold's count is 1 or more")
	}
	return k.Host.Write(ctx, "the hold record cannot be marked", func(tx SendingTx) error {
		h, found, err := holdIn(ctx, tx, id, k.D.Lock)
		if err != nil {
			return err
		}
		if !found || !slices.Contains(h.Storages, storage) {
			return notFound("the hold is not on this storage")
		}
		if seq > h.Seq {
			return tenant.Precondition("the hold has not reached this count")
		}
		_, err = tx.Exec(ctx, `UPDATE hold_records SET written_seq = ? WHERE hold_id = ? AND storage = ? AND written_seq < ?`, seq, id,
			storage, seq)
		return err
	})
}

// AdoptHold implements tenant.Holds.
func (k Sending) AdoptHold(ctx context.Context, f tenant.HoldFound, audit master.AuditEntry) (tenant.Hold, bool, error) {
	if err := f.Validate(); err != nil {
		return tenant.Hold{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Hold{}, false, err
	}
	var out tenant.Hold
	var adopted bool
	err := k.Host.Write(ctx, "the hold found cannot be adopted", func(tx SendingTx) error {
		adopted = false
		if err := k.lockRetention(ctx, tx); err != nil {
			return err
		}
		h, found, err := holdIn(ctx, tx, f.ID, k.D.Lock)
		if err != nil {
			return err
		}
		if found {
			out = h
			return nil
		}
		t := f.Target
		if _, err := tx.Exec(ctx, `INSERT INTO holds (id, level, subject_label, session_label, scan_label, subject_folded, session_folded,
			scan_folded, reason, state, placed_by, placed_at, release_reason, released_by, released_at, found_on_storage, seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'active', ?, ?, '', '', NULL, ?, 1)`, f.ID, t.Level(), t.Subject, t.Session, t.Scan,
			label.Fold(t.Subject), label.Fold(t.Session), label.Fold(t.Scan), f.Reason, f.PlacedBy, k.D.Time(f.Placed),
			k.D.Bool(true)); err != nil {
			return err
		}
		// The storages its target touches now are owed its record; the one
		// it was found on has it. A target the catalog no longer shows
		// still holds: keeping data longer is the lesser error.
		storages, err := holdStoragesIn(ctx, tx, t)
		if err != nil && fault.KindOf(err) != fault.NotFound {
			return err
		}
		if !slices.ContainsFunc(storages, func(s tenant.HoldStorage) bool { return s.Name == f.Storage }) {
			storages = append(storages, tenant.HoldStorage{Name: f.Storage})
		}
		for _, s := range storages {
			written := 0
			if s.Name == f.Storage {
				written = 1
			}
			if _, err := tx.Exec(ctx, `INSERT INTO hold_records (hold_id, storage, written_seq) VALUES (?, ?, ?)`, f.ID, s.Name,
				written); err != nil {
				return err
			}
		}
		if out, _, err = holdIn(ctx, tx, f.ID, ""); err != nil {
			return err
		}
		adopted = true
		return tx.Audited(ctx, withObject(audit, f.ID), nil)
	})
	return out, adopted, err
}
