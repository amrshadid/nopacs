package sqlkit

import (
	"context"
	"slices"
	"time"

	"github.com/amrshadid/nopacs/internal/core/label"
	corr "github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// A move (tenant.Moves): a session filed under another subject, or a
// subject merged into another, on the relabel's tables. Its scans' new
// paths are their old ones under the destination's label; its pointer row
// in relabels names the destination as new_subject (a session's move) or
// as new_label at the subject's level (a merge), so a lookup by the old
// labels answers where the data went.

// moveDest is the subject a move files its sessions under.
type moveDest struct {
	id    int64
	label string
}

// destIn reads the subject labeled to, exactly as every lookup by label
// reads one; NotFound when the tenant holds none.
func destIn(ctx context.Context, tx SendingTx, to string) (moveDest, error) {
	d := moveDest{label: to}
	found, err := tx.QueryRow(ctx, `SELECT id FROM subjects WHERE folded = ? AND label = ?`, []any{label.Fold(to), to}, &d.id)
	if err != nil {
		return d, err
	}
	if !found {
		return d, notFound("the tenant holds no subject with the label to move to")
	}
	return d, nil
}

// movedSessions returns the ids and labels of the sessions of r's scans, in
// order of label.
func movedSessions(ctx context.Context, tx SendingTx, r targetRows) ([]int64, []string, error) {
	seen := map[int64]bool{}
	var ids []int64
	for _, s := range r.scans {
		if !seen[s.sessionID] {
			seen[s.sessionID] = true
			ids = append(ids, s.sessionID)
		}
	}
	slices.Sort(ids)
	type session struct {
		id    int64
		label string
	}
	var all []session
	for _, id := range ids {
		var l string
		if _, err := tx.QueryRow(ctx, `SELECT label FROM sessions WHERE id = ?`, []any{id}, &l); err != nil {
			return nil, nil, err
		}
		all = append(all, session{id, l})
	}
	slices.SortFunc(all, func(a, b session) int {
		if a.label < b.label {
			return -1
		}
		if a.label > b.label {
			return 1
		}
		return 0
	})
	outIDs, outLabels := make([]int64, len(all)), make([]string, len(all))
	for i, s := range all {
		outIDs[i], outLabels[i] = s.id, s.label
	}
	return outIDs, outLabels, nil
}

// clashesIn returns, in order, the labels of sessions that the destination
// holds already, case folded.
func clashesIn(ctx context.Context, tx SendingTx, dest int64, sessions []string) ([]string, error) {
	var out []string
	for _, l := range sessions {
		held, err := exists(ctx, tx, `SELECT 1 FROM sessions WHERE subject_id = ? AND folded = ?`, dest, label.Fold(l))
		if err != nil {
			return nil, err
		}
		if held {
			out = append(out, l)
		}
	}
	return out, nil
}

// moveHolds returns, in order, the ids of the active holds that cover the
// target's labels or the destination subject.
func moveHolds(ctx context.Context, tx SendingTx, target tenant.CorrectionTarget, to string) ([]string, error) {
	holds, err := activeHolds(ctx, tx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, h := range holds {
		if h.target.Covers(target.Path()) || h.target.Covers(label.Path{Subject: to}) {
			out = append(out, h.id)
		}
	}
	return out, nil
}

// previewMoveIn makes the preview of a move of target under to in tx. A
// session's move of its subject's last session is refused: the subject
// would be left empty, which is what a merge does on purpose.
func (k Sending) previewMoveIn(ctx context.Context, tx SendingTx, target tenant.CorrectionTarget, to string,
	now time.Time) (tenant.MovePreview, error) {
	r, err := resolveTarget(ctx, tx, target, false)
	if err != nil {
		return tenant.MovePreview{}, err
	}
	dest, err := destIn(ctx, tx, to)
	if err != nil {
		return tenant.MovePreview{}, err
	}
	p := tenant.MovePreview{Target: target, To: to, ToID: dest.id, Storages: r.storages(), Issued: now.UTC().Truncate(time.Microsecond)}
	ids, labels, err := movedSessions(ctx, tx, r)
	if err != nil {
		return p, err
	}
	if target.Level() == corr.LevelSession {
		var n int64
		if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM sessions WHERE subject_id = ?`, []any{r.subjectID}, &n); err != nil {
			return p, err
		}
		if n <= int64(len(ids)) {
			return p, tenant.Precondition("the session is its subject's last: merge the subject instead")
		}
	}
	p.Sessions = labels
	c := r.counts()
	p.Counts = tenant.CorrectionCounts{Sessions: int64(len(ids)), Scans: c.Scans, Instances: c.Instances, Bytes: c.Bytes}
	for _, s := range p.Storages {
		replicas, err := replicasOf(ctx, tx, s)
		if err != nil {
			return p, err
		}
		for _, name := range replicas {
			p.Replicas = append(p.Replicas, tenant.ReplicaCopy{Replica: name, Primary: s})
		}
	}
	if p.Holds, err = moveHolds(ctx, tx, target, to); err != nil {
		return p, err
	}
	if p.Clashes, err = clashesIn(ctx, tx, dest.id, labels); err != nil {
		return p, err
	}
	scope, arg := r.scanScope()
	// #nosec G202 -- the scope is a constant of resolveTarget; values are parameters.
	if err := tx.Query(ctx, `SELECT s.id, s.manifest_sha256 FROM scans AS s WHERE s.id IN `+scope+` ORDER BY s.id`, []any{arg},
		func(row Row) error {
			var id tenant.ScanIdentity
			var sum []byte
			err := row.Scan(&id.ID, &sum)
			id.Manifest = ToSHA256(sum)
			p.Scans = append(p.Scans, id)
			return err
		}); err != nil {
		return p, err
	}
	p.Confirm = tenant.MoveConfirm(p)
	return p, nil
}

// PreviewMove implements tenant.Moves.
func (k Sending) PreviewMove(ctx context.Context, target tenant.CorrectionTarget, to string, now time.Time) (tenant.MovePreview, error) {
	if err := tenant.ValidateMove(target, to); err != nil {
		return tenant.MovePreview{}, err
	}
	if err := master.ValidateTime("preview time", now); err != nil {
		return tenant.MovePreview{}, err
	}
	var out tenant.MovePreview
	err := k.Host.Read(ctx, "the move cannot be previewed", func(tx SendingTx) error {
		var err error
		out, err = k.previewMoveIn(ctx, tx, target, to, now)
		return err
	})
	return out, err
}

// IdentityRewritten implements tenant.Moves.
func (k Sending) IdentityRewritten(ctx context.Context, id string) error {
	return k.stepRow(ctx, id, "the move's identity rewrite cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		if c.Kind != corr.KindMove {
			return tenant.Precondition("the correction is not a move")
		}
		_, err := tx.Exec(ctx, `UPDATE corrections SET rewrite_identity = ? WHERE id = ?`, k.D.Bool(false), id)
		return err
	})
}

// IdentityRefused implements tenant.Moves.
func (k Sending) IdentityRefused(ctx context.Context, id, why string) error {
	if why != tenant.IdentityRefusedProfile && why != tenant.IdentityRefusedCharset && why != tenant.IdentityRefusedNoID &&
		why != tenant.IdentityRefusedTooLarge {
		return fault.New(fault.Invalid, "an identity rewrite is refused for one of the server's phrases")
	}
	return k.stepRow(ctx, id, "the move's identity rewrite cannot be recorded", func(tx SendingTx, c *tenant.Correction) error {
		if c.Kind != corr.KindMove {
			return tenant.Precondition("the correction is not a move")
		}
		_, err := tx.Exec(ctx, `UPDATE corrections SET rewrite_identity = ?, identity_refused = ? WHERE id = ?`, k.D.Bool(false), why, id)
		return err
	})
}

// OwedIdentityRewrites implements tenant.Moves.
func (k Sending) OwedIdentityRewrites(ctx context.Context, after string, limit int) ([]tenant.Correction, error) {
	if limit < 1 || limit > 1000 {
		return nil, fault.New(fault.Invalid, "a limit is 1 to 1000")
	}
	return k.readCorrections(ctx, `SELECT `+correctionSelect+` FROM corrections WHERE id > ? AND kind = 'move' AND state = 'committed'
		AND rewrite_identity = ? ORDER BY id LIMIT ?`, after, k.D.Bool(true), limit)
}

// moveHeld is the refusal of a move, or of its undo, an active hold covers.
func moveHeld(hold string) error {
	return fault.Wrap(fault.Precondition, &tenant.HeldRefusal{Hold: hold}, "a hold covers the data the move files elsewhere, or its destination (HELD)")
}

// prepareMove records a move: its token checked against the preview made
// now, no hold and no other correction over its source or its destination,
// no session label the destination holds, no reception of it open; then its
// rows and its pointer.
func (k Sending) prepareMove(ctx context.Context, tx SendingTx, p tenant.CorrectionPrepare) error {
	issued, ok := tenant.ConfirmIssued(p.Confirm)
	if !ok || p.Now.Sub(issued) > tenant.MaxConfirmAge || issued.After(p.Now.Add(time.Minute)) {
		return tenant.Precondition("the confirm token is not one of a move's preview made within the hour")
	}
	t, to := p.Target, p.ToLabel
	if err := checkOverlap(ctx, tx, t.Path(), p.ID); err != nil {
		return err
	}
	if err := checkOverlap(ctx, tx, label.Path{Subject: to}, p.ID); err != nil {
		return err
	}
	preview, err := k.previewMoveIn(ctx, tx, t, to, issued)
	if err != nil {
		return err
	}
	if preview.Confirm != p.Confirm {
		return tenant.Precondition("the confirm token does not match what the move would now do")
	}
	if len(preview.Holds) > 0 {
		return moveHeld(preview.Holds[0])
	}
	if len(preview.Clashes) > 0 {
		return tenant.LabelConflictError(corr.LevelSession, tenant.FaultHeldByAnotherSession)
	}
	r, err := resolveTarget(ctx, tx, t, false)
	if err != nil {
		return err
	}
	if err := k.checkReceptions(ctx, tx, r, t.Subject); err != nil {
		return err
	}
	c := preview.Counts
	if t.Level() == corr.LevelSubject {
		c.Subjects = 1
	}
	if err := k.insertCorrection(ctx, tx, p, t.Level(), r, c, func(q label.Path) label.Path { return tenant.MovedPath(q, to) },
		nil); err != nil {
		return err
	}
	if p.RewriteIdentity {
		if _, err := tx.Exec(ctx, `UPDATE corrections SET rewrite_identity = ? WHERE id = ?`, k.D.Bool(true), p.ID); err != nil {
			return err
		}
	}
	// The pointer: a merge moves every label under the subject, as a
	// subject's relabel does; a session's move moves the session's.
	level, newLabel, newSubject := corr.LevelSubject, to, ""
	if t.Level() == corr.LevelSession {
		level, newLabel, newSubject = corr.LevelSession, t.Session, to
	}
	_, err = tx.Exec(ctx, `INSERT INTO relabels (correction_id, level, old_subject, old_session, old_scan, new_label, new_subject)
		VALUES (?, ?, ?, ?, '', ?, ?)`, p.ID, level, t.Subject, t.Session, newLabel, newSubject)
	return err
}

// publishMove publishes a move: each session it moves filed under the
// destination at once, the keys the catalog holds whole under the old
// labels, the manifests it wrote, both subjects' counts, a merge's emptied
// subject hidden. What it owns (the old tree, as the catalog and the
// snapshots name it) is fixed first.
func (k Sending) publishMove(ctx context.Context, tx SendingTx, c tenant.Correction, manifests map[string]tenant.SHA256,
	snapshots map[string]tenant.ScanSnapshot) error {
	if err := checkUnchanged(ctx, tx, c); err != nil {
		return err
	}
	r, err := resolveTarget(ctx, tx, c.Target, false)
	if err != nil {
		return err
	}
	var copied int64
	if _, err := tx.QueryRow(ctx, `SELECT count(*) FROM correction_scans WHERE correction_id = ?`, []any{c.ID}, &copied); err != nil {
		return err
	}
	if copied != int64(len(r.scans)) {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "a scan was added to what the move copied")
	}
	dest, err := destIn(ctx, tx, c.ToLabel)
	if fault.KindOf(err) == fault.NotFound {
		return fault.Wrap(fault.Precondition, tenant.ErrTargetChanged, "the subject the move files its sessions under is gone")
	}
	if err != nil {
		return err
	}
	ids, labels, err := movedSessions(ctx, tx, r)
	if err != nil {
		return err
	}
	clashes, err := clashesIn(ctx, tx, dest.id, labels)
	if err != nil {
		return err
	}
	if len(clashes) > 0 {
		return tenant.LabelConflictError(corr.LevelSession, tenant.FaultHeldByAnotherSession)
	}
	if err := ownInstances(ctx, tx, c.ID, r); err != nil {
		return err
	}
	if err := ownSnapshots(ctx, tx, c.ID, r, snapshots); err != nil {
		return err
	}
	if err := fileSessions(ctx, tx, ids, dest); err != nil {
		return err
	}
	for _, s := range r.scans {
		if m, ok := manifests[s.series]; ok && !m.IsZero() {
			if _, err := tx.Exec(ctx, `UPDATE scans SET manifest_sha256 = ? WHERE id = ?`, m[:], s.id); err != nil {
				return err
			}
		}
		if err := rekeyRevisions(ctx, tx, s.id, tenant.MovedPath(s.path, dest.label)); err != nil {
			return err
		}
	}
	if err := recountSubject(ctx, tx, dest.id); err != nil {
		return err
	}
	if c.Level != corr.LevelSubject {
		return recountSubject(ctx, tx, r.subjectID)
	}
	// The merged subject leaves the catalog; its label stays held for
	// arrivals until the merge ends (correctionHold), and an undo makes its
	// row again.
	if _, err := tx.Exec(ctx, `DELETE FROM subjects WHERE id = ?`, r.subjectID); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE catalog_totals SET subjects = subjects - 1`)
	return err
}

// fileSessions files the sessions ids under the subject to: their rows and
// the copies of the subject's label in their scans.
func fileSessions(ctx context.Context, tx SendingTx, ids []int64, to moveDest) error {
	folded := label.Fold(to.label)
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `UPDATE sessions SET subject_id = ?, subject_label = ?, subject_folded = ? WHERE id = ?`, to.id, to.label,
			folded, id); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE scans SET subject_label = ?, subject_folded = ? WHERE session_id = ?`, to.label, folded, id); err != nil {
			return err
		}
	}
	return nil
}

// undoMoveIn takes a committed move back: each session it moved filed
// under its subject again (a merge's made again), each scan's manifest and
// metadata digests and keys those of the old tree, which its purge never
// removed; it then owns nothing, and its copies are the caller's to sweep.
// It is refused while a hold covers what it moved, and once what it moved
// changed since its publish: the old tree would not be what the catalog
// then names.
func (k Sending) undoMoveIn(ctx context.Context, tx SendingTx, c *tenant.Correction, reason string, now time.Time) error {
	if err := k.checkNoRebuild(ctx, tx); err != nil {
		return err
	}
	if hold, err := correctionHeldIn(ctx, tx, c.ID); err != nil || hold != "" {
		if err != nil {
			return err
		}
		return moveHeld(hold)
	}
	// Any later correction of its scans, ended or not but abandoned: an
	// edit's new revisions are under the destination only, and even one
	// undone wrote the scans' manifests again.
	later, err := exists(ctx, tx, `SELECT 1 FROM correction_scans AS cs JOIN corrections AS o ON o.id = cs.correction_id
		WHERE o.id > ? AND o.state <> 'abandoned' AND cs.series_uid IN (SELECT series_uid FROM correction_scans WHERE correction_id = ?)`,
		c.ID, c.ID)
	if err != nil {
		return err
	}
	if later {
		return tenant.Precondition("a later correction touched what the move moved: the move can no longer be undone")
	}
	if err := checkUnchanged(ctx, tx, *c); err != nil {
		if fault.KindOf(err) != fault.Precondition {
			return err
		}
		return tenant.Precondition("what the move moved changed since it was published: it can no longer be undone")
	}
	scans, err := correctionScansIn(ctx, tx, c.ID)
	if err != nil {
		return err
	}
	// Each session it moved is under the destination still, with the scans
	// it moved and no other.
	perStudy := map[string]int64{}
	for _, s := range scans {
		perStudy[s.StudyUID]++
	}
	var ids []int64
	destFolded := label.Fold(c.ToLabel)
	var destID int64
	for study, n := range perStudy {
		var id, subject, count int64
		var folded string
		found, err := tx.QueryRow(ctx, `SELECT id, subject_id, subject_folded, scan_count FROM sessions WHERE study_uid = ?`, []any{study},
			&id, &subject, &folded, &count)
		if err != nil {
			return err
		}
		if !found || folded != destFolded || count != n {
			return tenant.Precondition("what the move moved changed since it was published: it can no longer be undone")
		}
		ids, destID = append(ids, id), subject
	}
	slices.Sort(ids)
	src, err := k.sourceSubject(ctx, tx, *c)
	if err != nil {
		return err
	}
	if err := fileSessions(ctx, tx, ids, src); err != nil {
		return err
	}
	for _, s := range scans {
		var id int64
		if _, err := tx.QueryRow(ctx, `SELECT id FROM scans WHERE series_uid = ?`, []any{s.SeriesUID}, &id); err != nil {
			return err
		}
		manifest, metadata, err := oldDigests(ctx, tx, c.ID, s)
		if err != nil {
			return err
		}
		var m any
		if !manifest.IsZero() {
			m = manifest[:]
		}
		if _, err := tx.Exec(ctx, `UPDATE scans SET manifest_sha256 = ? WHERE id = ?`, m, id); err != nil {
			return err
		}
		if err := SetSidecarInTx(ctx, tx, s.SeriesUID, manifest, metadata); err != nil {
			return err
		}
		if err := rekeyRevisions(ctx, tx, id, s.Path); err != nil {
			return err
		}
	}
	if err := recountSubject(ctx, tx, src.id); err != nil {
		return err
	}
	if err := recountSubject(ctx, tx, destID); err != nil {
		return err
	}
	// The objects of the old tree are the catalog's again; its copies under
	// the destination are swept.
	if err := countOwned(ctx, tx, c.ID, func() error {
		_, err := tx.Exec(ctx, `DELETE FROM correction_keys WHERE correction_id = ?`, c.ID)
		return err
	}); err != nil {
		return err
	}
	if err := k.QueueCorrectionCopies(ctx, tx, c.ID, now, nil); err != nil {
		return err
	}
	// An identity rewrite not yet made is not made for a move taken back.
	if _, err := tx.Exec(ctx, `UPDATE corrections SET undo_reason = ?, unresolved = ?, rewrite_identity = ? WHERE id = ?`, reason,
		k.D.Bool(false), k.D.Bool(false), c.ID); err != nil {
		return err
	}
	if err := k.setState(ctx, tx, c, corr.StateUndone, now, ""); err != nil {
		return err
	}
	if err := changedEvent(ctx, tx, *c, now); err != nil {
		return err
	}
	e, err := tenant.MoveEvent(*c, now)
	if err != nil {
		return err
	}
	return tx.WriteEvents(ctx, e)
}

// sourceSubject returns the subject a move's undo files its sessions under
// again: a session's move's, which must still be in the catalog (a delete
// of its other sessions since takes it, and its hidden row is that
// delete's), or a merge's, made again under its label, which the merge held
// for arrivals.
func (k Sending) sourceSubject(ctx context.Context, tx SendingTx, c tenant.Correction) (moveDest, error) {
	from := c.Target.Subject
	d, err := destIn(ctx, tx, from)
	switch {
	case err == nil && c.Level == corr.LevelSession:
		return d, nil
	case err == nil:
		return d, tenant.Precondition("the catalog holds the merged subject's label again: the merge can no longer be undone")
	case fault.KindOf(err) != fault.NotFound:
		return d, err
	case c.Level == corr.LevelSession:
		return d, tenant.Precondition("the subject the session came from is gone: the move can no longer be undone")
	}
	if held, err := exists(ctx, tx, `SELECT 1 FROM subjects WHERE folded = ?`, label.Fold(from)); err != nil || held {
		if err != nil {
			return d, err
		}
		return d, tenant.Precondition("the catalog holds the merged subject's label again: the merge can no longer be undone")
	}
	// Its times and counts are the sessions' once they are filed under it
	// (recountSubject).
	if _, err := tx.Exec(ctx, `INSERT INTO subjects (label, folded, created) VALUES (?, ?, ?)`, from, label.Fold(from),
		k.D.Time(c.Created)); err != nil {
		return d, err
	}
	if _, err := tx.Exec(ctx, `UPDATE catalog_totals SET subjects = subjects + 1`); err != nil {
		return d, err
	}
	return destIn(ctx, tx, from)
}

// oldDigests returns the SHA-256 a move read of a scan's manifest and
// metadata under its old labels before its publish (ownSnapshots); zero for
// one it did not read.
func oldDigests(ctx context.Context, tx SendingTx, id string, s tenant.CorrectionScan) (tenant.SHA256, tenant.SHA256, error) {
	read := func(ord int) (tenant.SHA256, error) {
		var sum []byte
		_, err := tx.QueryRow(ctx, `SELECT sha256 FROM correction_keys WHERE correction_id = ? AND ord = ? AND storage = ?
			AND subject_label = ? AND session_label = ? AND scan_label = ? AND sop_instance_uid = ''`,
			[]any{id, ord, s.Storage, s.Path.Subject, s.Path.Session, s.Path.Scan}, &sum)
		return ToSHA256(sum), err
	}
	m, err := read(tenant.TargetManifest)
	if err != nil {
		return m, tenant.SHA256{}, err
	}
	meta, err := read(tenant.TargetMetadata)
	return m, meta, err
}
