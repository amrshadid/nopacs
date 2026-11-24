package sqlkit

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The writes of a build into its shadow (tenant.Rebuilding's RebuildScan),
// and what the worker asks of the shadow before a scan's first write.

// RebuildScan implements tenant.Rebuilding.
func (k Rebuild) RebuildScan(ctx context.Context, w tenant.RebuildScanWrite) (tenant.RebuildProgress, error) {
	if err := w.Validate(); err != nil {
		return tenant.RebuildProgress{}, err
	}
	r, err := k.unitJob(ctx, w.Fence)
	if err != nil {
		return tenant.RebuildProgress{}, err
	}
	scratch := r.rebuild.Scratch
	if err := k.Shadow.Open(ctx, scratch); err != nil {
		return tenant.RebuildProgress{}, err
	}
	var out tenant.RebuildProgress
	err = k.Host.Write(ctx, "the scan cannot be written to the shadow", func(tx SendingTx) error {
		_, st, err := k.buildIn(ctx, tx, w.Fence)
		if err != nil {
			return err
		}
		if st.Complete {
			return tenant.Precondition("the build is complete")
		}
		if st.Cursor != w.From {
			return tenant.RebuildMoved()
		}
		p := k.Shadow.Prefix(scratch)
		if w.Scan != nil {
			if err := k.writeScan(ctx, tx, p, w); err != nil {
				return err
			}
		}
		next := w.From
		if w.Done {
			next.After, next.Partial, next.PartialUID = w.Key, "", ""
		} else {
			next.Partial = w.Key
			if n := len(w.Instances); n > 0 {
				next.PartialUID = w.Instances[n-1].SOPInstanceUID
			}
		}
		met := int64(0)
		if w.From.Partial != w.Key {
			met = 1
		}
		if _, err := tx.Exec(ctx, `UPDATE `+p+`rebuild_state SET after_key = ?, partial_key = ?, partial_uid = ?, scans = scans + ?,
			left_out = left_out + ?, left_out_bytes = left_out_bytes + ?`, next.After, next.Partial, next.PartialUID, met, w.LeftOut,
			w.LeftOutBytes); err != nil {
			return err
		}
		if err := addShadowTallies(ctx, tx, p, w.Tallies); err != nil {
			return err
		}
		out, err = k.stateIn(ctx, tx, scratch)
		return err
	})
	return out, err
}

// ShadowHolder implements tenant.Rebuilding.
func (k Rebuild) ShadowHolder(ctx context.Context, fence tenant.WorkFence, uids []string) (string, error) {
	if err := fence.Validate(); err != nil {
		return "", err
	}
	if err := tenant.ValidateShadowUIDs(uids); err != nil {
		return "", err
	}
	r, err := k.unitJob(ctx, fence)
	if err != nil {
		return "", err
	}
	scratch := r.rebuild.Scratch
	if err := k.Shadow.Open(ctx, scratch); err != nil {
		return "", err
	}
	p := k.Shadow.Prefix(scratch)
	marks := strings.TrimSuffix(strings.Repeat("?, ", len(uids)), ", ")
	args := make([]any, len(uids))
	for i, u := range uids {
		args[i] = u
	}
	var out string
	err = k.Host.Write(ctx, "the shadow cannot be read", func(tx SendingTx) error {
		out = ""
		if _, _, err := k.buildIn(ctx, tx, fence); err != nil {
			return err
		}
		// The shadow's unique index of SOP Instance UIDs answers each, then
		// the scan's key.
		_, err := tx.QueryRow(ctx, `SELECT c.storage FROM `+p+`instances AS i JOIN `+p+`scans AS c ON c.id = i.scan_id
			WHERE i.sop_instance_uid IN (`+marks+`) LIMIT 1`, args, &out)
		return err
	})
	return out, err
}

// addShadowTallies adds tallies to the build's, each key once.
func addShadowTallies(ctx context.Context, tx SendingTx, p string, tallies map[string]int64) error {
	keys := make([]string, 0, len(tallies))
	for key := range tallies {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		changed, err := tx.Exec(ctx, `UPDATE `+p+`rebuild_tallies SET count = count + ? WHERE tally = ?`, tallies[key], key)
		if err != nil {
			return err
		}
		if changed == 0 {
			if _, err := tx.Exec(ctx, `INSERT INTO `+p+`rebuild_tallies (tally, count) VALUES (?, ?)`, key, tallies[key]); err != nil {
				return err
			}
		}
	}
	return nil
}

// elsewhere returns the refusal of a scan whose series or instance (cause)
// a scan of the shadow on the storage held holds, with ErrOtherStorage
// beside it when that storage is not the scan's own.
func elsewhere(cause error, held, storage, message string) error {
	if held != storage {
		cause = errors.Join(cause, tenant.ErrOtherStorage)
	}
	return fault.Wrap(fault.Conflict, cause, message)
}

// writeScan writes a scan's subject, session and scan rows into the shadow
// when they are not there, and the write's instances. Each row is looked
// for before it is written, so a label, a series or an instance another
// row holds is refused before anything of the write is made: the unit is
// the shadow's one writer, so what it read stays true to its commit.
func (k Rebuild) writeScan(ctx context.Context, tx SendingTx, p string, w tenant.RebuildScanWrite) error {
	s := w.Scan
	continuing := w.From.Partial == w.Key
	var scanID int64
	var heldOn string
	found, err := tx.QueryRow(ctx, `SELECT id, storage FROM `+p+`scans WHERE series_uid = ?`, []any{s.SeriesUID}, &scanID, &heldOn)
	if err != nil {
		return err
	}
	switch {
	case found && !continuing:
		return elsewhere(tenant.ErrSeriesElsewhere, heldOn, s.Storage, "another scan of the shadow holds the series")
	case !found && continuing:
		return tenant.Precondition("the scan written in part is not in the shadow")
	case !found:
		subjectID, err := k.shadowSubject(ctx, tx, p, s)
		if err != nil {
			return err
		}
		sessionID, err := k.shadowSession(ctx, tx, p, s, subjectID)
		if err != nil {
			return err
		}
		if scanID, err = k.shadowScan(ctx, tx, p, s, sessionID); err != nil {
			return err
		}
	}
	profiles := map[tenant.ProfileRef]int64{}
	for _, in := range w.Instances {
		var holder string
		taken, err := tx.QueryRow(ctx, `SELECT c.storage FROM `+p+`instances AS i JOIN `+p+`scans AS c ON c.id = i.scan_id
			WHERE i.sop_instance_uid = ?`, []any{in.SOPInstanceUID}, &holder)
		if err != nil {
			return err
		}
		if taken {
			return elsewhere(tenant.ErrInstanceElsewhere, holder, s.Storage, "another scan of the shadow holds an instance of the scan")
		}
		var profile any
		if !in.Profile.IsZero() {
			id, ok := profiles[in.Profile]
			if !ok {
				has, err := tx.QueryRow(ctx, `SELECT id FROM profile_versions WHERE name = ? AND version = ?`,
					[]any{in.Profile.Name, in.Profile.Version}, &id)
				if err != nil {
					return err
				}
				if !has {
					return fault.Wrap(fault.Precondition, tenant.ErrProfileUnknown, "the tenant has no profile version "+in.Profile.String())
				}
				profiles[in.Profile] = id
			}
			profile = id
		}
		key := in.Key
		if plain, err := layout.InstanceKey(s.Path, in.SOPInstanceUID); err == nil && plain == key {
			key = ""
		}
		var archived any
		if in.ArchivedSHA256 != in.ReceivedSHA256 {
			archived = in.ArchivedSHA256[:]
		}
		if _, err := tx.Exec(ctx, `INSERT INTO `+p+`instances (scan_id, sop_instance_uid, reception_id, sha256, archived_sha256,
			storage_key, byte_offset, byte_length, size, held_at, profile_version_id) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			scanID, in.SOPInstanceUID, in.Reception, in.ReceivedSHA256[:], archived, key, in.Offset, in.Length, in.Size,
			k.D.Time(in.HeldAt), profile); err != nil {
			return err
		}
	}
	return nil
}

// shadowSubject finds or makes the scan's subject in the shadow.
func (k Rebuild) shadowSubject(ctx context.Context, tx SendingTx, p string, s *tenant.RebuiltScan) (int64, error) {
	folded := label.Fold(s.Path.Subject)
	var id int64
	var held string
	found, err := tx.QueryRow(ctx, `SELECT id, label FROM `+p+`subjects WHERE folded = ?`, []any{folded}, &id, &held)
	if err != nil {
		return 0, err
	}
	if found {
		if held != s.Path.Subject {
			return 0, tenant.LabelConflictError("subject", tenant.FaultCaseCollision)
		}
		return id, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+p+`subjects (label, folded, created, updated) VALUES (?, ?, ?, ?)`,
		s.Path.Subject, folded, k.D.Time(s.Created), k.D.Time(s.Updated)); err != nil {
		return 0, err
	}
	if _, err := tx.QueryRow(ctx, `SELECT id FROM `+p+`subjects WHERE folded = ?`, []any{folded}, &id); err != nil {
		return 0, err
	}
	return id, nil
}

// shadowSession finds or makes the scan's session in the shadow, under
// its subject, refusing a session of another subject, label or storage.
// A session's receiver and route are those of the reception that made it
// in the catalog in use, the first to commit into it; of its scans, the
// one created first was that commit's (a scan's created time is its first
// commit's), so the shadow's session takes the receiver and route of the
// scan of the earliest created time met so far, its created time the mark
// (finishShadow sets it again from the scans). Two scans created in the
// same microsecond keep the first written; and a reception whose commit
// began earlier but ended later than another's gives the catalog the other
// one's receiver, which the compare then counts as a session changed.
func (k Rebuild) shadowSession(ctx context.Context, tx SendingTx, p string, s *tenant.RebuiltScan, subjectID int64) (int64, error) {
	var id, heldSubject int64
	var heldLabel, storage, date string
	var created DBTime
	found, err := tx.QueryRow(ctx, `SELECT id, subject_id, label, storage, study_date, created FROM `+p+`sessions WHERE study_uid = ?`,
		[]any{s.StudyUID}, &id, &heldSubject, &heldLabel, &storage, &date, &created)
	if err != nil {
		return 0, err
	}
	route, err := ReceptionRouteText(s.Route)
	if err != nil {
		return 0, err
	}
	if found {
		switch {
		case heldSubject != subjectID:
			return 0, tenant.LabelConflictError("subject", tenant.FaultTwoSubjectsForOneStudy)
		case heldLabel != s.Path.Session:
			return 0, tenant.LabelConflictError("session", tenant.FaultHeldByAnotherSession)
		case storage != s.Storage:
			return 0, elsewhere(tenant.ErrSeriesElsewhere, storage, s.Storage, "another storage holds a scan of the session")
		}
		if date == "" && s.Values.StudyDate != "" {
			if _, err := tx.Exec(ctx, `UPDATE `+p+`sessions SET study_date = ? WHERE id = ?`, s.Values.StudyDate, id); err != nil {
				return 0, err
			}
		}
		if s.Created.Before(created.T) {
			if _, err := tx.Exec(ctx, `UPDATE `+p+`sessions SET receiver = ?, route = ?, created = ? WHERE id = ?`, s.SessionReceiver, route,
				k.D.Time(s.Created), id); err != nil {
				return 0, err
			}
		}
		return id, nil
	}
	folded := label.Fold(s.Path.Session)
	var held string
	taken, err := tx.QueryRow(ctx, `SELECT label FROM `+p+`sessions WHERE subject_id = ? AND folded = ?`, []any{subjectID, folded}, &held)
	if err != nil {
		return 0, err
	}
	if taken {
		if held == s.Path.Session {
			return 0, tenant.LabelConflictError("session", tenant.FaultHeldByAnotherSession)
		}
		return 0, tenant.LabelConflictError("session", tenant.FaultCaseCollision)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+p+`sessions (study_uid, subject_id, label, folded, storage, created, subject_label,
		subject_folded, study_date, receiver, updated, route) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		s.StudyUID, subjectID, s.Path.Session, folded, s.Storage, k.D.Time(s.Created), s.Path.Subject, label.Fold(s.Path.Subject),
		s.Values.StudyDate, s.SessionReceiver, k.D.Time(s.Updated), route); err != nil {
		return 0, err
	}
	if _, err := tx.QueryRow(ctx, `SELECT id FROM `+p+`sessions WHERE study_uid = ?`, []any{s.StudyUID}, &id); err != nil {
		return 0, err
	}
	return id, nil
}

// shadowScan makes the scan's row in the shadow, under its session.
func (k Rebuild) shadowScan(ctx context.Context, tx SendingTx, p string, s *tenant.RebuiltScan, sessionID int64) (int64, error) {
	folded := label.Fold(s.Path.Scan)
	var held string
	taken, err := tx.QueryRow(ctx, `SELECT label FROM `+p+`scans WHERE session_id = ? AND folded = ?`, []any{sessionID, folded}, &held)
	if err != nil {
		return 0, err
	}
	if taken {
		if held == s.Path.Scan {
			return 0, tenant.LabelConflictError("scan", tenant.FaultHeldByAnotherScan)
		}
		return 0, tenant.LabelConflictError("scan", tenant.FaultCaseCollision)
	}
	var manifest any
	if !s.ManifestSHA256.IsZero() {
		manifest = s.ManifestSHA256[:]
	}
	var number any
	if s.Values.HasSeriesNumber {
		number = s.Values.SeriesNumber
	}
	if _, err := tx.Exec(ctx, `INSERT INTO `+p+`scans (session_id, series_uid, label, folded, instances, bytes, manifest_sha256,
		created, updated, subject_label, subject_folded, session_label, session_folded, storage, modality, series_description,
		description_folded, series_number, receiver, study_uid) VALUES (?, ?, ?, ?, 0, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sessionID, s.SeriesUID, s.Path.Scan, folded, manifest, k.D.Time(s.Created), k.D.Time(s.Updated), s.Path.Subject,
		label.Fold(s.Path.Subject), s.Path.Session, label.Fold(s.Path.Session), s.Storage, s.Values.Modality,
		s.Values.SeriesDescription, tenant.FoldDescription(s.Values.SeriesDescription), number, s.Receiver, s.StudyUID); err != nil {
		return 0, err
	}
	var id int64
	if _, err := tx.QueryRow(ctx, `SELECT id FROM `+p+`scans WHERE series_uid = ?`, []any{s.SeriesUID}, &id); err != nil {
		return 0, err
	}
	return id, nil
}
