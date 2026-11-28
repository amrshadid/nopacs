package correct

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"

	"github.com/amrshadid/go-dicom/dataset"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/core/record"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// EditRequest is an edit a person asked for, with the token of its preview
// (PreviewEdit).
type EditRequest struct {
	// Target is the subject, session, scan or instance whose every
	// instance the edit changes; Changes what it does to each.
	Target  tenant.CorrectionTarget
	Changes []EditChange
	// Reason is why; Confirm the preview's token; KeyID the key that asked.
	Reason, Confirm, KeyID string
	// Audit is the request's entry; Guard the check of who asks.
	Audit master.AuditEntry
	Guard tenant.Guard
	// fromArchive says the values set were read from the tenant's own
	// archived files (EditedFile.FromArchive): a move's identity rewrite
	// sets it, never a person's request.
	fromArchive bool
}

// reasonRefused is the state reason of an edit the check after refused.
const reasonRefused = "the check after refused a new revision"

// reasonCharset is the state reason of an edit of a file whose character
// set a rewrite would not keep (receive.CheckRewritable).
const reasonCharset = "a file's character set would not be kept"

// Edit records, writes and publishes an edit, each instance of its target
// given a new revision through the path a replace takes: its row, its
// records on every storage it touches, then for each instance its bytes
// read and checked against the catalog, the changes made on its data set
// by the DICOM library's writer, the new file judged (Config.Judge, the
// check after) and put at its next revision key, read back; each scan's
// manifest and metadata written to name the new revisions; then the
// publish, which keeps every superseded revision for the purge (or an
// undo). A failure before the publish writes the manifests and metadata
// back, removes the new revisions, and abandons the edit with its reason.
// A request whose token and changes made an edit already, in this process,
// answers that edit (rememberEdit). From its prepare on it runs apart from
// ctx's end (detached).
func (e *Engine) Edit(ctx context.Context, r EditRequest) (tenant.Correction, error) {
	if err := e.fence(ctx); err != nil {
		return tenant.Correction{}, err
	}
	if e.cfg.Judge == nil {
		return tenant.Correction{}, fault.New(fault.Unavailable, "the server judges no edits, so it makes none")
	}
	if err := checkChanges(r.Changes); err != nil {
		return tenant.Correction{}, err
	}
	if err := checkTargetChanges(r.Target, r.Changes); err != nil {
		return tenant.Correction{}, err
	}
	key := editTokenKey(r.Confirm, r.Changes)
	if id, was, ok := e.usedEdit(key); ok {
		// The token and changes made an edit in this process already.
		if was != r.Target {
			return tenant.Correction{}, errTokenMismatch
		}
		return e.cfg.Store.Correction(ctx, id)
	}
	confirm, targets, err := e.unkeyEditConfirm(ctx, r.Target, r.Changes, r.Confirm)
	if err != nil {
		return tenant.Correction{}, err
	}
	lines, err := e.scanLines(ctx, targets)
	if err != nil {
		return tenant.Correction{}, err
	}
	edits := make([]tenant.EditPrepared, 0, len(targets))
	for _, t := range targets {
		edits = append(edits, tenant.EditPrepared{UID: t.UID, SeriesUID: t.SeriesUID, OldRevision: t.Revision, OldSHA256: t.SHA256,
			RestoreLine: lines[t.UID]})
	}
	now := e.now()
	id, err := tenant.NewCorrectionID(now)
	if err != nil {
		return tenant.Correction{}, err
	}
	c, made, err := e.cfg.Store.PrepareCorrection(ctx, tenant.CorrectionPrepare{ID: id, Kind: record.KindEdit, Target: r.Target,
		Confirm: confirm, Reason: r.Reason, KeyID: r.KeyID, Edits: edits, Now: now}, r.Audit, r.Guard)
	if err == nil {
		e.rememberEdit(key, c.ID, r.Target)
	}
	if err != nil || !made {
		return c, err
	}
	ctx, cancel := detached(ctx)
	defer cancel()
	if err := e.reached(c, "prepared"); err != nil {
		return c, err
	}
	if err := e.writeRecords(ctx, c); err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	if err := e.reached(c, "records"); err != nil {
		return c, err
	}
	return e.writeEdit(ctx, c, targets, editWith{changes: r.Changes, fromArchive: r.fromArchive})
}

// writeEdit writes a prepared edit's new revisions and each scan's
// manifest and metadata naming them, then publishes it. The storages'
// gates are held from before the first write until the publish is
// answered, as a replace holds its storage's, so a switch never leaves a
// new revision on a place the catalog then names no longer.
func (e *Engine) writeEdit(ctx context.Context, c tenant.Correction, targets []tenant.EditTarget, w editWith) (tenant.Correction, error) {
	prepared := make(map[string]tenant.EditedInstance, len(c.Edits))
	for _, in := range c.Edits {
		prepared[in.UID] = in
	}
	release, err := e.hold(ctx, c.Storages)
	if err != nil {
		return e.abandon(ctx, c, reasonStorage, err)
	}
	defer release()
	abandon := func(why string, err error) (tenant.Correction, error) {
		release()
		return e.abandon(ctx, c, why, err)
	}
	values, named := editValues(w.changes)
	cc := tenant.CorrectionCommit{ID: c.ID, Manifests: map[string]tenant.SHA256{}, Sidecars: map[string]tenant.SHA256{}}
	if named {
		cc.Values = map[string]tenant.EditValues{}
	}
	for start := 0; start < len(targets); {
		end := start + 1
		for end < len(targets) && targets[end].SeriesUID == targets[start].SeriesUID {
			end++
		}
		scan := targets[start:end]
		start = end
		st, err := e.cfg.OpenStorage(ctx, scan[0].Storage)
		if err != nil {
			return abandon(reasonStorage, err)
		}
		lines := make(map[string]sidecar.Instance, len(scan))
		files := make(map[string]manifest.File, len(scan))
		for _, t := range scan {
			in, ok := prepared[t.UID]
			if !ok {
				return abandon(reasonChanged, fault.New(fault.Internal, "an instance of the edit was not prepared"))
			}
			line, file, written, why, err := e.editOne(ctx, st, t, in, w)
			if err != nil {
				return abandon(why, err)
			}
			if err := e.reached(c, "object"); err != nil {
				return c, err
			}
			lines[t.UID], files[t.UID] = line, file
			cc.Edits = append(cc.Edits, written)
		}
		var change func(*sidecar.Values)
		if named {
			change = func(v *sidecar.Values) { applyValues(v, values) }
			cc.Values[scan[0].SeriesUID] = values
		}
		// The scan's search row, read from the new header of the changed file
		// with the lowest SOP Instance UID: the publish applies it when the
		// row was read from that file.
		low := scan[0].UID
		for _, t := range scan {
			low = min(low, t.UID)
		}
		if js := lines[low].Dataset; len(js) > 0 {
			if cc.Search == nil {
				cc.Search = map[string]search.Row{}
			}
			cc.Search[scan[0].SeriesUID] = search.ExtractJSON(js, low)
		}
		meta, err := e.rewriteMetaMany(ctx, st, scan[0].Storage, scan[0].Path, lines, files, change, nil)
		if err != nil {
			return abandon(reasonStorage, err)
		}
		if err := e.reached(c, "meta"); err != nil {
			return c, err
		}
		cc.Manifests[scan[0].SeriesUID] = meta.after
		if !meta.metadata.IsZero() {
			cc.Sidecars[scan[0].SeriesUID] = meta.metadata
		}
	}
	return e.published(ctx, c, func() (tenant.Correction, string, error) { return e.commit(ctx, cc) }, release)
}

// editOne makes, judges and puts one instance's new revision, and returns
// its line of the scan's metadata, its manifest entry and what was written;
// on failure, the state reason to abandon with. fromArchive is the
// request's (EditedFile.FromArchive).
func (e *Engine) editOne(ctx context.Context, st blob.Store, t tenant.EditTarget, in tenant.EditedInstance,
	w editWith) (sidecar.Instance, manifest.File, tenant.EditWritten, string, error) {
	var none sidecar.Instance
	old, err := readAll(ctx, st, t.Key, t.Size)
	if err == nil && (int64(len(old)) != t.Size || tenant.SHA256(sha256.Sum256(old)) != t.SHA256) {
		err = fault.New(fault.Precondition, "an instance's bytes are not those the catalog holds; it is repaired before it is edited")
	}
	if err != nil {
		return none, manifest.File{}, tenant.EditWritten{}, reasonUnreadable, err
	}
	nb := w.file
	if nb == nil {
		if err := receive.CheckRewritable(old, 0); err != nil {
			return none, manifest.File{}, tenant.EditWritten{}, reasonCharset, err
		}
		nb, err = receive.RewriteFile(old, 0, func(ds *dataset.Dataset) error { return applyChanges(ds, w.changes) })
		if err != nil {
			return none, manifest.File{}, tenant.EditWritten{}, reasonUnreadable, err
		}
		if err := e.cfg.Judge.JudgeEdit(ctx, EditedFile{Target: t, File: nb, Set: setTags(w.changes), Removed: removedTags(w.changes),
			FromArchive: w.fromArchive}); err != nil {
			return none, manifest.File{}, tenant.EditWritten{}, reasonRefused, err
		}
	}
	sum := tenant.SHA256(sha256.Sum256(nb))
	size := int64(len(nb))
	key, err := layout.RevisionKey(t.Path, t.UID, in.NewRevision)
	if err != nil {
		return none, manifest.File{}, tenant.EditWritten{}, reasonUnreadable, fault.Wrap(fault.Internal, err, "a revision key cannot be made")
	}
	put, err := st.Put(ctx, key, bytes.NewReader(nb))
	if err == nil && (put.Size != size || tenant.SHA256(put.SHA256) != sum) {
		err = errCopy
	}
	if err == nil {
		n, back, herr := hashOf(ctx, st, key)
		if err = herr; err == nil && (n != size || tenant.SHA256(back) != sum) {
			err = errCopy
		}
	}
	if err != nil {
		return none, manifest.File{}, tenant.EditWritten{}, reasonUnreadable, err
	}
	var was sidecar.Instance
	if err := json.Unmarshal(in.RestoreLine, &was); err != nil {
		return none, manifest.File{}, tenant.EditWritten{}, reasonUnreadable, fault.Wrap(fault.Internal, err, "an instance's line cannot be read back")
	}
	line, err := receive.EditedLine(nb, was, sha256Text(sum), 0)
	if err != nil {
		return none, manifest.File{}, tenant.EditWritten{}, reasonUnreadable, fault.Wrap(fault.Internal, err, "the new revision's line cannot be made")
	}
	line.Key, line.Revision = key, in.NewRevision
	if w.file != nil {
		// A revision a profile made is described by that profile's run, not
		// by the revision it replaces: the version, its checksum, the base
		// in force and the elements no rule named. An edit keeps the old
		// line's, since it changes values and not the profile.
		line.Profile, line.ProfileSHA256, line.BaseProfile = w.profile, w.profileSHA256, w.base
		line.Unnamed, line.UnnamedOver = w.unnamed, w.unnamedOver
	}
	// The checksum the instance was received with stays the catalog's (a
	// file sent again is still the same file); the line names it whenever
	// it is not the archived one, as a rebuild reads it back.
	received := was.ReceivedSHA256
	if received == "" {
		received = was.SHA256
	}
	line.ReceivedSHA256 = ""
	if received != line.SHA256 {
		line.ReceivedSHA256 = received
	}
	file := manifest.File{Key: key, Length: size, Size: size, SHA256: sha256Text(sum), SOPInstanceUID: t.UID}
	// Kept with the publish: an undo that may not be published writes it back.
	kept, err := json.Marshal(line)
	if err != nil {
		return none, manifest.File{}, tenant.EditWritten{}, reasonUnreadable, fault.Wrap(fault.Internal, err, "the new revision's line cannot be kept")
	}
	return line, file, tenant.EditWritten{UID: t.UID, SHA256: sum, Size: size, Line: kept}, "", nil
}

// editWith is how an edit makes each instance's new revision: by applying
// changes to the instance's data set, or, for a profile re-run, by writing
// a file already made and checked (file), whose version the instance's line
// then names (profile) and whose catalog values changes name.
type editWith struct {
	changes     []EditChange
	fromArchive bool
	file        []byte
	profile     string
	// profileSHA256, base, unnamed and unnamedOver are what the line of a
	// revision made by a profile records of the run.
	profileSHA256, base string
	unnamed             []sidecar.UnnamedElement
	unnamedOver         int
}

// applyValues gives a scan header's values those an edit sets.
func applyValues(v *sidecar.Values, set tenant.EditValues) {
	if set.SetDescription {
		v.SeriesDescription = set.SeriesDescription
	}
	if set.SetModality {
		v.Modality = set.Modality
	}
	if set.SetSeriesNumber {
		v.SeriesNumber = nil
		if set.HasSeriesNumber {
			n := set.SeriesNumber
			v.SeriesNumber = &n
		}
	}
}
