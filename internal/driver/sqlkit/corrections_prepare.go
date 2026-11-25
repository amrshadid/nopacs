package sqlkit

import (
	"context"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// targetScan is one scan of a correction's target as the catalog holds it.
type targetScan struct {
	id, sessionID          int64
	series, study, storage string
	path                   label.Path
	instances, bytes       int64
}

// targetRows is what a correction's target covers in the catalog, and what
// a delete of it hides: subjectGone, the sessions and the scans removed
// whole, or one instance of a scan that stays.
type targetRows struct {
	level        string
	subjectID    int64
	subjectGone  bool
	sessionID    int64
	sessionsGone []int64
	scans        []targetScan
	instance     *targetInstance
	// scope selects the target's scans from scans AS s, with scopeArg: a
	// statement over a subject of many scans names them by this, never by
	// a list of every id.
	scope    string
	scopeArg any
}

// targetInstance is one instance a delete or replace names: key is its
// object's key, raw the key the catalog holds ("" for the layout's).
type targetInstance struct {
	uid, key, raw, receptionID string
	scanID, size               int64
	archived, received         tenant.SHA256
	revision                   int
}

// scanScope returns the statement fragment that selects r's scans' ids,
// and its argument.
func (r targetRows) scanScope() (string, any) {
	return `(SELECT s.id FROM scans AS s WHERE ` + r.scope + `)`, r.scopeArg
}

// instanceKey returns the key of the instance's object: its own when the
// catalog holds one, else the layout's for the scan at p.
func instanceKey(storageKey string, p label.Path, uid string) (string, error) {
	if storageKey != "" {
		return storageKey, nil
	}
	return layout.InstanceKey(p, uid)
}

// resolveTarget reads what target covers; NotFound when the catalog holds
// no such subject, session, scan or instance. With escalate, a delete of a
// scan's last instance is a delete of the scan, of a session's last scan
// takes the session, and of a subject's last session the subject.
func resolveTarget(ctx context.Context, tx SendingTx, t tenant.CorrectionTarget, escalate bool) (targetRows, error) {
	out := targetRows{level: t.Level()}
	found, err := tx.QueryRow(ctx, `SELECT id FROM subjects WHERE folded = ? AND label = ?`, []any{label.Fold(t.Subject), t.Subject},
		&out.subjectID)
	if err != nil {
		return out, err
	}
	if !found {
		return out, notFound("the tenant holds no subject with this label")
	}
	where, arg := `s.session_id IN (SELECT id FROM sessions WHERE subject_id = ?)`, any(out.subjectID)
	if t.Session != "" {
		found, err := tx.QueryRow(ctx, `SELECT id FROM sessions WHERE subject_id = ? AND folded = ? AND label = ?`,
			[]any{out.subjectID, label.Fold(t.Session), t.Session}, &out.sessionID)
		if err != nil {
			return out, err
		}
		if !found {
			return out, notFound("the tenant holds no session with this label")
		}
		where, arg = `s.session_id = ?`, out.sessionID
	}
	if t.Scan != "" {
		var id int64
		found, err := tx.QueryRow(ctx, `SELECT id FROM scans WHERE session_id = ? AND folded = ? AND label = ?`,
			[]any{out.sessionID, label.Fold(t.Scan), t.Scan}, &id)
		if err != nil {
			return out, err
		}
		if !found {
			return out, notFound("the tenant holds no scan with this label")
		}
		where, arg = `s.id = ?`, id
	}
	out.scope, out.scopeArg = where, arg
	err = tx.Query(ctx, `SELECT s.id, s.session_id, s.series_uid, s.study_uid, s.storage, s.subject_label, s.session_label,
		s.label, s.instances, s.bytes FROM scans AS s WHERE `+where+` ORDER BY s.storage, s.series_uid`, []any{arg},
		func(r Row) error {
			var s targetScan
			err := r.Scan(&s.id, &s.sessionID, &s.series, &s.study, &s.storage, &s.path.Subject, &s.path.Session, &s.path.Scan,
				&s.instances, &s.bytes)
			out.scans = append(out.scans, s)
			return err
		})
	if err != nil {
		return out, err
	}
	if len(out.scans) == 0 {
		return out, notFound("the target holds no scan")
	}
	if t.Instance != "" {
		s := out.scans[0]
		in := targetInstance{uid: t.Instance, scanID: s.id}
		var key string
		var archived, received []byte
		found, err := tx.QueryRow(ctx, `SELECT storage_key, size, sha256, archived_sha256, reception_id FROM instances
			WHERE scan_id = ? AND sop_instance_uid = ?`, []any{s.id, t.Instance}, &key, &in.size, &received, &archived, &in.receptionID)
		if err != nil {
			return out, err
		}
		if !found {
			return out, notFound("the scan holds no instance with this id")
		}
		in.received, in.archived = ToSHA256(received), ToSHA256(archived)
		if in.archived.IsZero() {
			in.archived = in.received
		}
		in.revision, in.raw = tenant.RevisionOfKey(key), key
		if in.key, err = instanceKey(key, s.path, t.Instance); err != nil {
			return out, fault.Wrap(fault.Internal, err, "an instance's key cannot be made")
		}
		out.instance = &in
		if !escalate || s.instances > 1 {
			return out, nil
		}
		out.level, out.instance = corr.LevelScan, nil
		out.scope, out.scopeArg = `s.id = ?`, s.id
	}
	if !escalate {
		return out, nil
	}
	return out, out.escalate(ctx, tx)
}

// escalate fills what a delete removes whole: the target's scans, and each
// session left with none, and the subject left with none.
func (r *targetRows) escalate(ctx context.Context, tx SendingTx) error {
	gone := map[int64]int64{}
	for _, s := range r.scans {
		gone[s.sessionID]++
	}
	left := int64(0)
	err := tx.Query(ctx, `SELECT id, scan_count FROM sessions WHERE subject_id = ? ORDER BY id`, []any{r.subjectID},
		func(row Row) error {
			var id, n int64
			if err := row.Scan(&id, &n); err != nil {
				return err
			}
			if g, ok := gone[id]; ok && g >= n {
				r.sessionsGone = append(r.sessionsGone, id)
				return nil
			}
			left++
			return nil
		})
	if err != nil {
		return err
	}
	r.subjectGone = left == 0
	return nil
}

// counts are what a delete of r removes.
func (r targetRows) counts() tenant.CorrectionCounts {
	var c tenant.CorrectionCounts
	if r.instance != nil {
		return tenant.CorrectionCounts{Instances: 1, Bytes: r.instance.size}
	}
	if r.subjectGone {
		c.Subjects = 1
	}
	c.Sessions, c.Scans = int64(len(r.sessionsGone)), int64(len(r.scans))
	for _, s := range r.scans {
		c.Instances += s.instances
		c.Bytes += s.bytes
	}
	return c
}

// storages are the names of the storages r's scans are on, in order.
func (r targetRows) storages() []string {
	var out []string
	for _, s := range r.scans {
		if len(out) == 0 || out[len(out)-1] != s.storage {
			out = append(out, s.storage)
		}
	}
	return out
}

// existingCorrection returns the id of the correction a repeated request
// answers, or "". A relabel asked again answers only one that is prepared
// or committed: one purged or abandoned is done, and the same move asked
// later (back and forth) is a new relabel.
func existingCorrection(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) (string, error) {
	var id string
	var err error
	switch p.Kind {
	case corr.KindDelete:
		_, err = tx.QueryRow(ctx, `SELECT id FROM corrections WHERE confirm_token = ?`, []any{p.Confirm}, &id)
	case corr.KindReplace:
		_, err = tx.QueryRow(ctx, `SELECT id FROM corrections WHERE quarantine_id = ? AND kind = 'replace' AND state <> 'abandoned'
			ORDER BY id DESC LIMIT 1`, []any{p.Quarantine}, &id)
	case corr.KindRelabel:
		t := p.Target
		_, err = tx.QueryRow(ctx, `SELECT id FROM corrections WHERE kind = 'relabel' AND state IN ('prepared', 'committed') AND subject_label = ?
			AND session_label = ? AND scan_label = ? AND to_label = ? ORDER BY id DESC LIMIT 1`,
			[]any{t.Subject, t.Session, t.Scan, p.ToLabel}, &id)
	}
	return id, err
}

// checkReceptions refuses a delete or relabel while a reception of any of
// the target's studies, or of its subject, has not ended: it would commit
// into rows the correction moves.
func (k Sending) checkReceptions(ctx context.Context, tx SendingTx, r targetRows, subject string) error {
	// #nosec G202 -- the scope is a constant of resolveTarget; values are parameters.
	busy, err := exists(ctx, tx, `SELECT 1 FROM receptions WHERE ended = ? AND (subject_label = ? OR study_uid IN
		(SELECT s.study_uid FROM scans AS s WHERE `+r.scope+`))`, k.D.Bool(false), subject, r.scopeArg)
	if err != nil {
		return err
	}
	if busy {
		return tenant.Precondition("a reception of the target has not ended")
	}
	return nil
}

// PrepareCorrection implements tenant.Corrections.
func (k Sending) PrepareCorrection(ctx context.Context, p tenant.CorrectionPrepare, audit master.AuditEntry,
	guard tenant.Guard) (tenant.Correction, bool, error) {
	if err := p.Validate(); err != nil {
		return tenant.Correction{}, false, err
	}
	if err := audit.Validate(); err != nil {
		return tenant.Correction{}, false, err
	}
	var out tenant.Correction
	var made bool
	err := k.Host.Write(ctx, "the correction cannot be recorded", func(tx SendingTx) error {
		out, made = tenant.Correction{}, false
		if err := tx.LockConfig(ctx); err != nil {
			return err
		}
		if id, err := existingCorrection(ctx, tx, p); err != nil || id != "" {
			if err != nil {
				return err
			}
			if out, err = k.correctionIn(ctx, tx, id, ""); err != nil {
				return err
			}
			return tx.Audited(ctx, withObject(audit, id), guard)
		}
		if taken, err := exists(ctx, tx, `SELECT 1 FROM corrections WHERE id = ?`, p.ID); err != nil || taken {
			if err != nil {
				return err
			}
			return fault.New(fault.Conflict, "a correction with this id exists")
		}
		if err := k.checkNoRebuild(ctx, tx); err != nil {
			return err
		}
		var err error
		switch p.Kind {
		case corr.KindDelete:
			err = k.prepareDelete(ctx, tx, p)
		case corr.KindRelabel:
			err = k.prepareRelabel(ctx, tx, p)
		default:
			err = k.prepareReplace(ctx, tx, &p)
		}
		if err != nil {
			return err
		}
		if err := checkNoSwitch(ctx, tx, p.ID); err != nil {
			return err
		}
		// A repair puts an object of a scan it touches meanwhile: what the
		// correction read of the scan would not be what is on the storage.
		if err := k.checkCorrectionNotHeld(ctx, tx, p.ID, p.Now); err != nil {
			return err
		}
		if out, err = k.correctionIn(ctx, tx, p.ID, ""); err != nil {
			return err
		}
		made = true
		return tx.Audited(ctx, withObject(audit, p.ID), guard)
	})
	if err != nil {
		return tenant.Correction{}, false, err
	}
	return out, made, nil
}

// withObject returns a correction's audit entry naming the correction it
// made or answered as its object, when the caller left the object empty:
// the caller makes the entry before the operation id exists, or before it
// knows which correction a repeated request answers.
func withObject(audit master.AuditEntry, id string) master.AuditEntry {
	if audit.Object == "" {
		audit.Object = id
	}
	return audit
}

// insertCorrection writes a correction's row, prepared, and its scans.
func (k Sending) insertCorrection(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare, level string, r targetRows,
	c tenant.CorrectionCounts, newPath func(label.Path) label.Path, in *targetInstance) error {
	var confirm, received, oldSum, newSum any
	if p.Confirm != "" {
		confirm = p.Confirm
	}
	t := p.Target
	key, size, oldRev, newRev := "", int64(0), 0, 0
	if in != nil {
		key, size = in.key, in.size
		oldSum = in.archived[:]
		if p.Kind == corr.KindReplace {
			oldRev, newRev = in.revision, in.revision+1
			newSum = p.NewSHA256[:]
		}
	}
	var restore any
	if len(p.RestoreLine) > 0 {
		restore = p.RestoreLine
	}
	if _, err := tx.Exec(ctx, `INSERT INTO corrections (`+correctionColumns+`, confirm_token)
		VALUES (?, ?, ?, 'prepared', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, NULL, NULL, NULL, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?,
			?, ?, ?, ?, 0, ?, ?)`,
		p.ID, string(p.Kind), level, tenant.OriginPerson, t.Subject, t.Session, t.Scan, t.Instance, p.ToLabel, p.Quarantine,
		p.Reason, p.KeyID, k.D.Time(p.Now), oldRev, newRev, oldSum, newSum, received, p.NewSize, key, size, restore,
		k.D.Bool(false), k.D.Bool(false), c.Subjects, c.Sessions, c.Scans, c.Instances, c.Bytes, superseded(p.Kind, r, c), confirm); err != nil {
		return err
	}
	for _, s := range r.scans {
		np := newPath(s.path)
		if _, err := tx.Exec(ctx, `INSERT INTO correction_scans (correction_id, series_uid, study_uid, storage, subject_label,
			session_label, scan_label, new_subject, new_session, new_scan, instances, bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.ID, s.series, s.study, s.storage, s.path.Subject, s.path.Session, s.path.Scan, np.Subject, np.Session, np.Scan,
			s.instances, s.bytes); err != nil {
			return err
		}
	}
	return nil
}

// superseded counts the objects a relabel or replace keeps until its
// purge: a relabel's whole old tree (each instance and each scan's two
// files), a replace's old revision.
func superseded(kind corr.Kind, r targetRows, c tenant.CorrectionCounts) int64 {
	switch kind {
	case corr.KindRelabel:
		return c.Instances + 2*int64(len(r.scans))
	case corr.KindReplace:
		return 1
	}
	return 0
}

// samePath is the new path of a correction that changes no label.
func samePath(p label.Path) label.Path { return p }

// prepareDelete records a delete: its token checked against the preview
// made now, its rows, and what names its data.
func (k Sending) prepareDelete(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) error {
	issued, ok := tenant.ConfirmIssued(p.Confirm)
	if !ok || p.Now.Sub(issued) > tenant.MaxConfirmAge || issued.After(p.Now.Add(time.Minute)) {
		return tenant.Precondition("the confirm token is not one of a preview made within the hour")
	}
	if err := checkOverlap(ctx, tx, p.Target.Path(), p.ID); err != nil {
		return err
	}
	preview, err := k.previewIn(ctx, tx, p.Target, issued)
	if err != nil {
		return err
	}
	if preview.Confirm != p.Confirm {
		return tenant.Precondition("the confirm token does not match what the delete would now remove")
	}
	r, err := resolveTarget(ctx, tx, p.Target, true)
	if err != nil {
		return err
	}
	if err := k.checkReceptions(ctx, tx, r, p.Target.Subject); err != nil {
		return err
	}
	if err := k.checkRetentionDelete(ctx, tx, p, r); err != nil {
		return err
	}
	if err := k.insertCorrection(ctx, tx, p, r.level, r, r.counts(), samePath, r.instance); err != nil {
		return err
	}
	if err := k.markRetention(ctx, tx, p); err != nil {
		return err
	}
	return writeDeletedTargets(ctx, tx, p.ID, p.Target, r)
}

// writeDeletedTargets records what names a delete's data.
func writeDeletedTargets(ctx context.Context, tx SendingTx, id string, t tenant.CorrectionTarget, r targetRows) error {
	add := func(kind, value string) error {
		_, err := tx.Exec(ctx, `INSERT INTO deleted_targets (kind, value, correction_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			kind, value, id)
		return err
	}
	if r.instance != nil {
		return add("instance", r.instance.uid)
	}
	if t.Level() == corr.LevelSubject {
		labels, err := subjectLabels(ctx, tx, t.Subject, "")
		if err != nil {
			return err
		}
		for _, l := range labels {
			if err := add("subject", l); err != nil {
				return err
			}
		}
	}
	for _, s := range r.scans {
		if t.Level() != corr.LevelScan && t.Level() != corr.LevelInstance {
			if err := add("study", s.study); err != nil {
				return err
			}
		}
		if err := add("series", s.series); err != nil {
			return err
		}
	}
	return nil
}

// maxEarlierLabels bounds how far back a subject's relabels are followed.
const maxEarlierLabels = 64

// subjectLabels returns a subject's label, then each label it had before a
// relabel published since, newest first: a scanner or a peer that still
// knows the subject by an earlier label would bring its data back under
// that label, which a withdrawal must hold as well. before, when not
// empty, is the id of the delete the labels are for: only relabels whose
// ids sort before it are followed, those a delete prepared then found
// published (an id is made from the time its correction began, and a
// subject with a relabel not yet purged cannot be deleted), so a relabel
// that later gave the label to another subject is not taken for one.
func subjectLabels(ctx context.Context, tx SendingTx, subject, before string) ([]string, error) {
	out := []string{subject}
	seen := map[string]bool{label.Fold(subject): true}
	query := `SELECT r.old_subject FROM relabels AS r JOIN corrections AS c ON c.id = r.correction_id
			WHERE r.level = 'subject' AND lower(r.new_label) = ? AND c.state IN ('committed', 'purging', 'purged')`
	if before != "" {
		query += ` AND r.correction_id < ?`
	}
	query += ` ORDER BY r.correction_id DESC`
	for i := 0; i < len(out) && len(out) < maxEarlierLabels; i++ {
		var earlier []string
		args := []any{label.Fold(out[i])}
		if before != "" {
			args = append(args, before)
		}
		err := tx.Query(ctx, query, args, func(row Row) error {
			var s string
			err := row.Scan(&s)
			earlier = append(earlier, s)
			return err
		})
		if err != nil {
			return nil, err
		}
		for _, s := range earlier {
			if !seen[label.Fold(s)] {
				seen[label.Fold(s)] = true
				out = append(out, s)
			}
		}
	}
	return out, nil
}

// relabeledPath returns p with its label at level changed to to.
func relabeledPath(p label.Path, level, to string) label.Path {
	switch level {
	case corr.LevelSubject:
		p.Subject = to
	case corr.LevelSession:
		p.Session = to
	default:
		p.Scan = to
	}
	return p
}

// prepareRelabel records a relabel: the new label free at its level, under
// no other correction's paths, and its rows.
func (k Sending) prepareRelabel(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) error {
	r, err := resolveTarget(ctx, tx, p.Target, false)
	if err != nil {
		return err
	}
	level := p.Target.Level()
	if err := checkOverlap(ctx, tx, p.Target.Path(), p.ID); err != nil {
		return err
	}
	to := relabeledPath(p.Target.Path(), level, p.ToLabel)
	if err := checkOverlap(ctx, tx, to, p.ID); err != nil {
		return err
	}
	if err := checkLabelFree(ctx, tx, r, level, p.ToLabel); err != nil {
		return err
	}
	if err := k.checkReceptions(ctx, tx, r, p.Target.Subject); err != nil {
		return err
	}
	c := r.counts()
	c.Subjects, c.Sessions = 0, 0
	if err := k.insertCorrection(ctx, tx, p, level, r, c, func(q label.Path) label.Path { return relabeledPath(q, level, p.ToLabel) },
		nil); err != nil {
		return err
	}
	t := p.Target
	_, err = tx.Exec(ctx, `INSERT INTO relabels (correction_id, level, old_subject, old_session, old_scan, new_label)
		VALUES (?, ?, ?, ?, ?, ?)`, p.ID, level, t.Subject, t.Session, t.Scan, p.ToLabel)
	return err
}

// checkLabelFree refuses a relabel whose new label, folded, is held at its
// level: by another subject, another session of the subject, another scan
// of the session.
func checkLabelFree(ctx context.Context, tx SendingTx, r targetRows, level, to string) error {
	// The target's own label, or one that differs from it in case alone,
	// is held at its level by the target itself.
	p := r.scans[0].path
	cur := map[string]string{corr.LevelSubject: p.Subject, corr.LevelSession: p.Session, corr.LevelScan: p.Scan}[level]
	if label.Fold(to) == label.Fold(cur) {
		return tenant.LabelConflictError(level, tenant.FaultCaseCollision)
	}
	var q string
	var args []any
	switch level {
	case corr.LevelSubject:
		q, args = `SELECT 1 FROM subjects WHERE folded = ? AND id <> ?`, []any{label.Fold(to), r.subjectID}
	case corr.LevelSession:
		q, args = `SELECT 1 FROM sessions WHERE subject_id = ? AND folded = ? AND id <> ?`, []any{r.subjectID, label.Fold(to), r.sessionID}
	default:
		q, args = `SELECT 1 FROM scans WHERE session_id = ? AND folded = ? AND id <> ?`, []any{r.sessionID, label.Fold(to), r.scans[0].id}
	}
	held, err := exists(ctx, tx, q, args...)
	if err != nil {
		return err
	}
	if held {
		return tenant.LabelConflictError(level, tenant.FaultCaseCollision)
	}
	return nil
}

// prepareReplace records a replace of the instance a held quarantined file
// names, of reason different file or repairs damaged, in the same study and
// series; p's target is filled from it.
func (k Sending) prepareReplace(ctx context.Context, tx SendingTx, p *tenant.CorrectionPrepare) error {
	var state, reason, uid, study, series string
	var kept bool
	var received []byte
	found, err := tx.QueryRow(ctx, `SELECT state, reason, sop_instance_uid, study_uid, series_uid, content_kept, sha256 FROM quarantine
		WHERE id = ?`, []any{p.Quarantine}, &state, &reason, &uid, &study, &series, &kept, &received)
	if err != nil {
		return err
	}
	if !found {
		return notFound("the tenant has no quarantined file with this id")
	}
	switch {
	case state != string(tenant.QuarantineHeld):
		return tenant.Precondition("the quarantined file is not held")
	case reason != string(tenant.QuarantineDifferentFile) && reason != string(tenant.QuarantineRepairsDamaged):
		return tenant.Precondition("the quarantined file is not of reason different file or repairs damaged")
	case !kept:
		return tenant.Precondition("the quarantined file's content was never kept")
	}
	var path label.Path
	var scanSeries, scanStudy string
	found, err = tx.QueryRow(ctx, `SELECT s.subject_label, s.session_label, s.label, s.series_uid, s.study_uid FROM instances AS i
		JOIN scans AS s ON s.id = i.scan_id WHERE i.sop_instance_uid = ?`, []any{uid}, &path.Subject, &path.Session, &path.Scan,
		&scanSeries, &scanStudy)
	if err != nil {
		return err
	}
	if !found || scanSeries != series || scanStudy != study {
		return tenant.Precondition("the tenant does not hold the file's instance in the same study and series")
	}
	p.Target = tenant.CorrectionTarget{Subject: path.Subject, Session: path.Session, Scan: path.Scan, Instance: uid}
	if err := checkOverlap(ctx, tx, path, p.ID); err != nil {
		return err
	}
	r, err := resolveTarget(ctx, tx, p.Target, false)
	if err != nil {
		return err
	}
	if r.instance.revision >= layout.MaxRevision {
		return tenant.Precondition("the instance has reached its last revision")
	}
	c := tenant.CorrectionCounts{Scans: 1, Instances: 1, Bytes: p.NewSize}
	r.scans = r.scans[:1]
	if err := k.insertCorrection(ctx, tx, *p, corr.LevelInstance, r, c, samePath, r.instance); err != nil {
		return err
	}
	// The checksum the file was received with, which the instance takes
	// as its received checksum.
	if len(received) != 32 {
		received = p.NewSHA256[:]
	}
	_, err = tx.Exec(ctx, `UPDATE corrections SET received_sha256 = ? WHERE id = ?`, received, p.ID)
	return err
}
