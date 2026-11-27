package sqlkit

import (
	"context"
	"encoding/hex"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// An edit a restore takes from its records, as a replace is taken: made
// committed (record.Correction.Alone) with what its publish writes outside
// the catalog, each revision it superseded owned by its purge and in the
// instance's history, so the purge removes what the edit replaced. The
// rebuilt catalog reads the new revisions from each scan's manifest. No
// line of an old revision is kept, so such an edit is not undone.

// adoptEdits writes the instances an edit's record on one storage names
// into the edit's rows, for an edit known only from its records (a
// restore): each one's revisions and checksums, its old revision's key
// from its scan's labels; its sizes and line, which no record holds, left
// empty (OwnAdoptedEdit fills the old size from the storage). A row
// written already keeps what it holds but takes the record's new checksum
// when it had none.
func adoptEdits(ctx context.Context, tx SendingTx, rec corr.Correction) error {
	paths := make(map[string]label.Path, len(rec.Scans))
	for _, s := range rec.Scans {
		paths[s.SeriesUID] = label.Path{Subject: s.Subject, Session: s.Session, Scan: s.Scan}
	}
	for _, e := range rec.Edits {
		key, err := oldEditKey(paths[e.SeriesUID], e.SOPInstanceUID, e.OldRevision)
		if err != nil {
			return err
		}
		var newSum any
		if b, err := hex.DecodeString(e.NewSHA256); err == nil && len(b) == 32 {
			newSum = b
		}
		old, err := hex.DecodeString(e.OldSHA256)
		if err != nil {
			return fault.Wrap(fault.Invalid, err, "an edit's checksum cannot be read")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO correction_edits (correction_id, sop_instance_uid, series_uid, old_revision, new_revision,
			old_sha256, new_sha256, old_key, old_size, new_size, restore_line) VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, 0, NULL)
			ON CONFLICT (correction_id, sop_instance_uid) DO UPDATE SET new_sha256 = coalesce(correction_edits.new_sha256, excluded.new_sha256)`,
			rec.ID, e.SOPInstanceUID, e.SeriesUID, e.OldRevision, e.NewRevision, old, newSum, key); err != nil {
			return err
		}
	}
	return nil
}

// oldEditKey returns the key of an instance's revision rev in the scan at
// p: the layout's plain key for the first, its revision key for a later.
func oldEditKey(p label.Path, uid string, rev int) (string, error) {
	key, err := layout.InstanceKey(p, uid)
	if rev > 1 {
		key, err = layout.RevisionKey(p, uid, rev)
	}
	if err != nil {
		return "", fault.Wrap(fault.Invalid, err, "an edit's instance key cannot be made")
	}
	return key, nil
}

// rawKey returns the key the catalog holds for an instance's object at
// key: none ("") for the layout's plain key of its scan at p.
func rawKey(p label.Path, uid, key string) string {
	if plain, err := layout.InstanceKey(p, uid); err == nil && plain == key {
		return ""
	}
	return key
}

// publishMovedEdit writes, for an edit the backup holds prepared that a
// restore moves to committed, what its publish writes outside the catalog
// (publishEdit), from what its prepare recorded: each superseded revision
// owned by its purge and in the instance's history, each new checksum the
// record names. The lines its prepare kept are dropped: the catalog is the
// rebuild's, not the one they were read against, so the edit is not
// undone.
func (k Sending) publishMovedEdit(ctx context.Context, tx SendingTx, rec corr.Correction) error {
	edits, err := correctionEditsIn(ctx, tx, rec.ID)
	if err != nil {
		return err
	}
	for _, e := range edits {
		if err := k.ownSuperseded(ctx, tx, rec.ID, e.Storage, e.Path, e.UID, e.OldKey, rawKey(e.Path, e.UID, e.OldKey), e.OldSize,
			e.OldSHA256, e.OldRevision); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `UPDATE correction_edits SET restore_line = NULL, new_line = NULL WHERE correction_id = ?`, rec.ID); err != nil {
		return err
	}
	return adoptEdits(ctx, tx, rec)
}

// editToOwn is an instance of an adopted edit whose superseded revision is
// not yet owned.
type editToOwn struct {
	uid, key string
	path     label.Path
	revision int
	sum      tenant.SHA256
}

// OwnAdoptedEdit implements tenant.Restoring.
func (k Sending) OwnAdoptedEdit(ctx context.Context, rec corr.Correction, storage string,
	sizeOf func(ctx context.Context, key string) (int64, error), _ time.Time) error {
	if rec.Kind != corr.KindEdit {
		return nil
	}
	if err := tenant.ValidateName("storage", storage); err != nil {
		return err
	}
	var todo []editToOwn
	err := k.Host.Read(ctx, "the adopted edit's instances cannot be read", func(tx SendingTx) error {
		todo = nil
		return tx.Query(ctx, `SELECT e.sop_instance_uid, e.old_key, e.old_revision, e.old_sha256, cs.subject_label, cs.session_label,
			cs.scan_label FROM correction_edits AS e JOIN corrections AS c ON c.id = e.correction_id AND c.state = 'committed'
			JOIN correction_scans AS cs ON cs.correction_id = e.correction_id AND cs.series_uid = e.series_uid AND cs.storage = ?
			WHERE e.correction_id = ? AND NOT EXISTS (SELECT 1 FROM correction_keys AS ck WHERE ck.correction_id = e.correction_id
				AND ck.sop_instance_uid = e.sop_instance_uid) ORDER BY e.sop_instance_uid`, []any{storage, rec.ID}, func(r Row) error {
			var o editToOwn
			var sum []byte
			if err := r.Scan(&o.uid, &o.key, &o.revision, &sum, &o.path.Subject, &o.path.Session, &o.path.Scan); err != nil {
				return err
			}
			o.sum = ToSHA256(sum)
			todo = append(todo, o)
			return nil
		})
	})
	if err != nil || len(todo) == 0 {
		return err
	}
	sizes := make([]int64, len(todo))
	for i, o := range todo {
		size, err := sizeOf(ctx, o.key)
		switch {
		case fault.KindOf(err) == fault.NotFound:
			// A revision gone from the storage owns nothing.
			sizes[i] = -1
		case err != nil:
			return err
		default:
			sizes[i] = size
		}
	}
	return k.Host.Write(ctx, "the edit cannot be given what its publish writes", func(tx SendingTx) error {
		for i, o := range todo {
			if sizes[i] < 0 {
				continue
			}
			if err := k.ownSuperseded(ctx, tx, rec.ID, storage, o.path, o.uid, o.key, rawKey(o.path, o.uid, o.key), sizes[i], o.sum,
				o.revision); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE correction_edits SET old_size = ? WHERE correction_id = ? AND sop_instance_uid = ?`, sizes[i],
				rec.ID, o.uid); err != nil {
				return err
			}
		}
		return nil
	})
}
