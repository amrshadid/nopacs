package receive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"time"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/manifest"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/frames"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/search"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// scanResult is what archiving one scan decided.
type scanResult struct {
	// kept counts the instances this reception holds in the scan after
	// the run: confirmed now or before.
	kept int
	// dupFiles and dupBytes count the files ignored as the same file the
	// tenant holds.
	dupFiles, dupBytes int64
	// claimed says another reception in flight claims some instances.
	claimed bool
}

// written is one file put on the storage.
type written struct {
	f   tenant.ReceivedFile
	key string
	// size and sum are the size and SHA-256 of the bytes put: the file as
	// received, or its de-identified copy when made.
	size int64
	sum  tenant.SHA256
	// made says the bytes put are the de-identified copy beside the
	// received file in the incoming area (incoming.Area.Derive), which
	// profile made; false, the file as received.
	made    bool
	profile tenant.ProfileRef
	// base is the tenant's base profile the file was de-identified over, zero
	// when it had none (profile.Reception).
	base tenant.ProfileRef
	// pixelRiskAccepted says the image was at risk of burned-in text and a
	// pixel rule of profile accepted it as it is; the catalog and the
	// metadata record it so no sender sends it unless marked
	// (tenant.SendRefusal).
	pixelRiskAccepted bool
	// unnamed and unnamedOver are what the profile's run recorded of the
	// elements no rule named (sidecar.Instance.Unnamed): non-nil, possibly
	// empty, for a file profile de-identified; nil for one archived as it is.
	unnamed     []sidecar.UnnamedElement
	unnamedOver int
}

// archiveScan archives the files of one series into its scan. A file is
// written to the storage and read back before its catalog row is committed:
// the catalog never says an instance is held before its bytes on the storage
// were found to be the bytes received.
//
// Order of effects, each safe to repeat or to stop after:
//
//  1. Claim the files' instances (ClaimInstances, under the fence, in
//     batches). The answers decide each file, as the port documents: claimed
//     now or before, written; archived before, already done; the same file,
//     a duplicate; a different file or the UID elsewhere, held; claimed by
//     another reception, left for a later run. A claim run again answers
//     ClaimedBefore or ArchivedBefore and changes nothing. A file claimed
//     before that a held record of this reception keeps (a read-back
//     mismatch of an earlier run) is not written again: it stays held.
//  2. Hold the files the claims decided to hold, under the unit's fence. A
//     record's id is made from the reception and the file, so a run again
//     writes nothing new.
//  3. Under the scan's lock (below), read the catalog's scan and session at
//     the path. When they are another series' scan or another study's
//     session, every file to be written is held as a label conflict and
//     nothing is put: the other scan's objects, sidecar and manifest stay
//     as they are. Otherwise put each file to be written to its layout key, up to archiveParallel
//     at once, every put ended before step 4 starts. A key already holding
//     an object (a run again) is replaced whole by the port's rule, with the
//     same bytes, so the object is what it was.
//  4. Read each one back from the storage, up to archiveParallel at once, as
//     a stream through SHA-256 and
//     compare with the checksum it was received with. A mismatch, or an
//     object that is not there, holds the file with the read-back reason; it
//     keeps its incoming copy and goes no further. Its instance stays claimed
//     by this reception, unconfirmed, until the reception ends, when the
//     store releases every claim the reception did not confirm (by the
//     finish, quarantine or fail step). The object written under its key is
//     one no catalog row explains; it is not deleted (nothing is deleted
//     from a storage without a recorded reason and a checksum proving the
//     copy that replaces it), and a later correct file of the same instance,
//     by a retry or a release, is put to the same key and replaces it whole.
//  5. Put the scan's metadata sidecar, then its manifest, with the files that
//     read back equal, each replaced whole by the port. The manifest is made
//     from the catalog's list of the scan (ScanInstances: every instance
//     confirmed, by any reception) with this reception's files added; the
//     sidecar, which holds headers the catalog does not, from what the
//     storage holds for a scan the catalog holds, every entry under a UID
//     this reception claims, or that the manifest does not list, taken out,
//     and with a scan header made from the catalog's scan and session as
//     the commit leaves them. Before either is put, a scan the catalog
//     holds that a repair holds (tenant.Repairs.ScanRepairHeld) fails the
//     step, which is tried again, so its manifest is never replaced for a
//     commit the hold would refuse.
//  6. Commit the scan's catalog rows with the instances that read back equal
//     (CommitScan, under the fence, at most MaxScanCommit per call).
//
// A crash after 3, 4 or 5 and before 6 leaves objects on the storage that the
// catalog does not list: the data objects of instances this reception still
// claims (nobody else can write those keys, since nobody else holds the
// claim), and a manifest and sidecar that list them. The next run of this
// step, which the unit's lease guarantees, puts and reads back the same
// objects again and commits. If the reception fails instead, its claims are
// released and the objects stay unlisted until the retry of the reception
// writes them again.
//
// The sidecar and manifest are made and replaced, and the scan committed,
// under a lock held by this process per storage and scan, so a reception
// writing a scan sees the commit of another of this process that wrote it
// before, and the two never lose each other's entries. Two processes on one
// tenant (a database with several writers) are not serialized here: the
// catalog then holds every instance, a manifest can miss one confirmed by
// the other process meanwhile, the verify step finds it (scanListed), and
// the scan's next manifest, made from the catalog, lists it.
//
// session is the study's session as the archive step read it before its
// first scan, nil when the catalog had none.
func (p *Pipeline) archiveScan(ctx context.Context, w tenant.Work, r tenant.Reception, store blob.Store, path label.Path, g series, kept map[string]bool,
	session *tenant.SessionLabels) (scanResult, error) {
	var res scanResult
	fence := w.Fence()
	var toWrite []tenant.ReceivedFile
	var holds []tenant.QuarantinedFile
	for start := 0; start < len(g.files); start += tenant.MaxClaimBatch {
		part := g.files[start:min(start+tenant.MaxClaimBatch, len(g.files))]
		claims := make([]tenant.InstanceClaim, len(part))
		for i, f := range part {
			claims[i] = tenant.InstanceClaim{SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID, SHA256: f.SHA256}
		}
		p.at("archive.before-claim")
		answers, err := p.store.ClaimInstances(ctx, r.ID, claims, p.now(), &fence)
		if err != nil {
			return res, err
		}
		p.at("archive.after-claim")
		if len(answers) != len(part) {
			return res, fault.New(fault.Internal, "a claim answered another number of files than it was given")
		}
		marks, err := p.repairs(ctx, part, answers)
		if err != nil {
			return res, err
		}
		for i, d := range answers {
			f := part[i]
			switch d.Kind {
			case tenant.ClaimedNow, tenant.ClaimedBefore:
				if !kept[f.Incoming] {
					toWrite = append(toWrite, f)
				}
			case tenant.ArchivedBefore:
				res.kept++
			case tenant.SameFile:
				if finding, ok := marks[f.SOPInstanceUID]; ok {
					held, err := p.repairHeld(ctx, r, f, finding)
					if err != nil {
						return res, err
					}
					if !held {
						// The same file as an instance a scrub marked
						// damaged: held, not ignored, so a person can
						// repair the instance with it.
						holds = append(holds, p.holdRecord(r, f, tenant.QuarantineRepairsDamaged, map[string]string{"finding": finding}))
						continue
					}
					// Another reception holds these bytes for the repair
					// already: one copy is enough, so this one is a
					// duplicate of it and its bytes go.
				}
				res.dupFiles++
				res.dupBytes += f.Size
			case tenant.DifferentFile:
				q := p.holdRecord(r, f, tenant.QuarantineDifferentFile, nil)
				q.KeptSHA256 = d.HeldSHA256
				holds = append(holds, q)
			case tenant.UIDElsewhere:
				details := map[string]string{}
				if validUID(d.HeldStudyUID) && validUID(d.HeldSeriesUID) {
					details["study_instance_uid"], details["series_instance_uid"] = d.HeldStudyUID, d.HeldSeriesUID
				}
				holds = append(holds, p.holdRecord(r, f, tenant.QuarantineUIDElsewhere, details))
			case tenant.Claimed:
				res.claimed = true
			default:
				return res, fault.New(fault.Internal, "a claim gave an answer this version does not know: "+d.Kind.String())
			}
		}
	}
	if err := p.hold(ctx, w, holds); err != nil {
		return res, err
	}
	if len(toWrite) == 0 {
		return res, nil
	}
	manifestKey, err := layout.ManifestKey(path)
	if err != nil {
		return res, fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	metadataKey, err := layout.MetadataKey(path)
	if err != nil {
		return res, fault.Wrap(fault.Internal, err, "the scan's labels make no key")
	}
	puts := make([]written, len(toWrite))
	for i, f := range toWrite {
		key, err := layout.InstanceKey(path, f.SOPInstanceUID)
		if err != nil {
			return res, fault.Wrap(fault.Internal, err, "an instance makes no key")
		}
		puts[i] = written{f: f, key: key}
	}
	// Every UID this reception claims and writes: its entries already on
	// the storage are not the catalog's (no committed instance has a UID
	// while a claim on it stands), whether its file reads back or not, and
	// whether or not it is held before it is put.
	ours := make(map[string]bool, len(puts))
	for _, wr := range puts {
		ours[wr.f.SOPInstanceUID] = true
	}
	// Each file is made ready to put before the scan is locked: as it was
	// received, or de-identified and checked (prepare); one that cannot be
	// is held now, and goes no further.
	puts, unready, err := p.prepare(ctx, r, path, puts)
	if err != nil {
		return res, err
	}
	defer p.dropMade(puts)
	if err := p.hold(ctx, w, unready); err != nil {
		return res, err
	}
	if len(puts) == 0 {
		return res, nil
	}
	unlock := p.lockScan(r.Storage, manifestKey)
	defer unlock()

	// What the catalog holds at the path, read under the lock so no
	// reception of this process commits the scan meanwhile. Labels that are
	// another study's session or another series' scan (a naming rule that
	// gives two series one label) hold the files before any put, so the
	// other scan's objects, sidecar and manifest are never touched.
	cat, err := p.catalogAt(ctx, path, g.uid, session)
	if err != nil {
		return res, err
	}
	if details := cat.clash(r.StudyUID, g.uid); details != nil {
		return res, p.holdWritten(ctx, w, r, puts, tenant.QuarantineLabelConflict, details)
	}
	// Labels or UIDs a correction not ended holds (a delete in its grace, a
	// relabel's old tree) are not the catalog's to give: a put there would
	// replace the manifest and sidecar the correction still owns, so the
	// files are held before any put, naming the correction.
	if h, err := p.store.HeldByCorrection(ctx, path, r.StudyUID, g.uid); err != nil || h.Correction != "" {
		if err != nil {
			return res, err
		}
		return res, p.holdWritten(ctx, w, r, puts, tenant.QuarantineLabelConflict, map[string]string{"label": h.Level,
			"fault": tenant.FaultHeldByCorrection, "correction": h.Correction})
	}

	// Each instance has a key of its own, so its put waits on no other's;
	// one after another, a series of hundreds of slices took that many
	// storage round trips in a row (two syncs each on a directory, a
	// request each on S3). Every put still ends before any read-back.
	if err := parallel(ctx, len(puts), func(ctx context.Context, i int) error {
		return p.putIncoming(ctx, store, puts[i])
	}); err != nil {
		return res, err
	}

	sums := make([]tenant.SHA256, len(puts))
	errs := make([]error, len(puts))
	if err := parallel(ctx, len(puts), func(ctx context.Context, i int) error {
		p.at("archive.before-readback")
		sums[i], errs[i] = readSum(ctx, store, puts[i].key)
		if errs[i] != nil && fault.KindOf(errs[i]) != fault.NotFound {
			return errs[i]
		}
		return nil
	}); err != nil {
		return res, err
	}
	good := puts[:0:0]
	var mismatched []tenant.QuarantinedFile
	for i, wr := range puts {
		sum, err := sums[i], errs[i]
		switch {
		case fault.KindOf(err) == fault.NotFound:
			mismatched = append(mismatched, p.holdRecord(r, wr.f, tenant.QuarantineReadBackMismatch, map[string]string{"storage": r.Storage}))
		case err != nil:
			return res, err
		case sum != wr.sum:
			mismatched = append(mismatched, p.holdRecord(r, wr.f, tenant.QuarantineReadBackMismatch,
				map[string]string{"storage": r.Storage, "read_sha256": hex.EncodeToString(sum[:])}))
			p.log.Warn("an archived file read back with another checksum; it is held, not committed, and its incoming copy kept",
				"reception", r.ID, "storage", r.Storage)
		default:
			good = append(good, wr)
		}
	}
	p.at("archive.after-readback")
	if err := p.hold(ctx, w, mismatched); err != nil {
		return res, err
	}
	puts = good
	if len(puts) == 0 {
		return res, nil
	}
	files := make([]manifest.File, len(puts))
	for i, wr := range puts {
		files[i] = manifest.File{Key: wr.key, Length: wr.size, Size: wr.size, SHA256: wr.sum.String(), SOPInstanceUID: wr.f.SOPInstanceUID}
	}
	merged, err := p.scanManifest(ctx, g.uid, files)
	if err != nil {
		return res, fault.Wrap(fault.KindOf(err), err, "the scan's manifest cannot be made")
	}
	encoded, err := manifest.Encode(merged)
	if err != nil {
		return res, fault.Wrap(fault.Internal, err, "the scan's manifest cannot be written")
	}
	inManifest := make(map[string]bool, len(merged.Files))
	for _, mf := range merged.Files {
		inManifest[mf.SOPInstanceUID] = true
	}
	// One reading of the clock for the sidecar's times and for every row the
	// commits below make, so a line's held_at and the scan's created and
	// updated times are the catalog's own.
	at := p.now()
	// A repair holding the scan refuses its commit; were the sidecar and
	// manifest put first, the catalog would keep naming the old manifest's
	// digest while the object is the new one (or the repair put the old one
	// back over it). So a hold is looked for before either is written, and
	// the step is tried again once the repair ends; the commit still checks,
	// for a repair recorded between here and there.
	if cat.hasScan {
		held, err := p.store.ScanRepairHeld(ctx, g.uid, at)
		if err != nil {
			return res, err
		}
		if held {
			return res, tenant.ScanRepairingError()
		}
	}
	p.at("archive.before-sidecar")
	var values tenant.ScanValues
	var searched search.Row
	sidecarSum, err := p.putSidecar(ctx, store, metadataKey, cat, g.uid, r, at, puts, func(u string) bool { return !inManifest[u] || ours[u] }, &values, &searched)
	if err != nil {
		return res, err
	}
	p.at("archive.before-manifest")
	if _, err := store.Put(ctx, manifestKey, bytes.NewReader(encoded)); err != nil {
		return res, storageError(err, "the scan's manifest cannot be written")
	}
	p.at("archive.after-manifest")
	manifestSum := tenant.SHA256(sha256.Sum256(encoded))

	for start := 0; start < len(puts); start += tenant.MaxScanCommit {
		part := puts[start:min(start+tenant.MaxScanCommit, len(puts))]
		c := tenant.ScanCommit{
			ReceptionID: r.ID, StudyUID: r.StudyUID, SeriesUID: g.uid,
			SubjectLabel: path.Subject, SessionLabel: path.Session, ScanLabel: path.Scan,
			Storage: r.Storage, ManifestSHA256: manifestSum, SidecarSHA256: sidecarSum, Values: values, Search: searched, At: at, Fence: &fence,
		}
		for _, wr := range part {
			c.Instances = append(c.Instances, tenant.ConfirmedInstance{
				SOPInstanceUID: wr.f.SOPInstanceUID, Key: wr.key, Length: wr.size, Size: wr.size, ArchivedSHA256: wr.sum,
				Profile: wr.profile, PixelRiskAccepted: wr.pixelRiskAccepted && !wr.profile.IsZero(),
			})
		}
		p.at("archive.before-commit")
		_, err := p.store.CommitScan(ctx, c)
		var lc *tenant.LabelConflict
		switch {
		case errors.As(err, &lc):
			details := map[string]string{"label": lc.Label, "fault": lc.Fault}
			if lc.Correction != "" {
				details["correction"] = lc.Correction
			}
			return res, p.holdWritten(ctx, w, r, puts[start:], tenant.QuarantineLabelConflict, details)
		case errors.Is(err, tenant.ErrSeriesElsewhere):
			return res, p.holdWritten(ctx, w, r, puts[start:], tenant.QuarantineUIDElsewhere, nil)
		case err != nil:
			return res, err
		}
		p.wake()
		var bytes int64
		for _, wr := range part {
			bytes += wr.size
		}
		p.meter.archived(p.cfg.Clock.Now(), int64(len(part)), bytes)
		p.at("archive.after-commit")
		res.kept += len(part)
	}
	return res, nil
}

// holdWritten holds files whose commit the catalog refused, or would refuse
// for their labels. Objects already put on the storage stay, unlisted by
// the catalog; nothing is deleted from a storage without a recorded reason,
// and the files keep their incoming copies for a person to decide.
func (p *Pipeline) holdWritten(ctx context.Context, w tenant.Work, r tenant.Reception, puts []written, reason tenant.QuarantineReason, details map[string]string) error {
	records := make([]tenant.QuarantinedFile, len(puts))
	for i, wr := range puts {
		records[i] = p.holdRecord(r, wr.f, reason, details)
	}
	return p.hold(ctx, w, records)
}

// putIncoming puts a file's bytes to its key: its incoming copy as
// received, or the de-identified copy made from it (written.made). It
// checks that what the storage counted is what was received, or what was
// de-identified and checked: a copy changed on the disk since is not
// archived.
func (p *Pipeline) putIncoming(ctx context.Context, store blob.Store, wr written) error {
	in, err := p.openPut(wr)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	// The bytes are compared with the row (the received file's checksum,
	// or the de-identified copy's) before any of them is written: wrong
	// bytes never reach the storage, and the handle compared is the one
	// put. A file recorded de-identified keeps a keyed checksum in its row;
	// its own was compared before it was de-identified (prepareOne), and
	// what is put is the copy, whose checksum is wr.sum.
	sum, n, err := sumAt(in, wr.size+1)
	if err != nil {
		return err
	}
	if n != wr.size || sum != wr.sum {
		return fault.New(fault.Internal, "an incoming copy no longer has the checksum it was received or checked with; nothing of it is archived")
	}
	p.at("archive.before-put")
	got, err := store.Put(ctx, wr.key, io.NewSectionReader(in, 0, wr.size))
	if err != nil {
		return storageError(err, "a file cannot be written to the storage")
	}
	p.at("archive.after-put")
	if got.Size != wr.size || tenant.SHA256(got.SHA256) != wr.sum {
		return fault.New(fault.Internal, "an incoming copy no longer has the checksum it was received or checked with; it is not archived")
	}
	return nil
}

// openPut opens the bytes a file puts: its incoming copy, or the
// de-identified copy made from it.
func (p *Pipeline) openPut(wr written) (*os.File, error) {
	if wr.made {
		f, _, err := p.area.OpenDerived(wr.f.Incoming)
		return f, err
	}
	f, _, err := p.area.Open(wr.f.Incoming)
	return f, err
}

// readManifest reads a scan's manifest from the storage, or an empty one
// when there is none. One that cannot be decoded is an error: it is not
// replaced by a manifest that would drop what it lists.
func readManifest(ctx context.Context, store blob.Store, key string) (manifest.Manifest, error) {
	rc, err := store.Read(ctx, key, 0, -1)
	if fault.KindOf(err) == fault.NotFound {
		return manifest.Manifest{}, nil
	}
	if err != nil {
		return manifest.Manifest{}, storageError(err, "the scan's manifest cannot be read")
	}
	defer func() { _ = rc.Close() }()
	m, err := manifest.Decode(rc)
	if err != nil {
		return manifest.Manifest{}, fault.Wrap(fault.Internal, err, "the scan's manifest on the storage cannot be read; it is left as it is")
	}
	return m, nil
}

// lockScan takes the lock held while a scan's metadata sidecar and manifest
// on a storage are made and replaced, by the archive step and by the pass
// that gives old sidecars their frame tables (frames.Run), and returns its
// release.
func (p *Pipeline) lockScan(storage, manifestKey string) func() {
	if p.cfg.ScanLocks != nil {
		return p.cfg.ScanLocks.Lock(storage, manifestKey)
	}
	return p.scanLocks.lock(storage + "\x00" + manifestKey)
}

// LockScan takes the same lock for another writer of a scan's manifest and
// sidecar in this process (a correction's purge or replace, which rewrites
// them in place), and returns its release: the two never lose each other's
// entries.
func (p *Pipeline) LockScan(storage, manifestKey string) func() {
	return p.lockScan(storage, manifestKey)
}

// putSidecar writes the scan's metadata sidecar: the sidecar the storage
// holds, merged with this reception's instances, streamed to the storage as
// it is made, each instance's header and frame table read from its
// incoming copy only when its line is written. It returns the SHA-256 of
// the bytes put, which the commit keeps with the manifest's, so a replica's
// copy and a read of the file are checked against it. It sets values to what the
// first of those headers says of the scan and its session (scanValues),
// which the commit gives the catalog, so no header is read twice; that
// first line is made before the sidecar's header is written, which holds
// the values the catalog will keep.
//
// The header and the lines kept come from what the catalog holds (c): for
// a scan the catalog does not hold, a sidecar on the storage is one an
// attempt that never committed wrote, and nothing of it is kept, since
// every line it holds is one the manifest does not list.
func (p *Pipeline) putSidecar(ctx context.Context, store blob.Store, key string, c scanCatalog, seriesUID string, r tenant.Reception,
	at time.Time, puts []written, drop func(string) bool, values *tenant.ScanValues, searched *search.Row) (tenant.SHA256, error) {
	var old io.Reader
	if c.hasScan {
		rc, err := store.Read(ctx, key, 0, -1)
		switch {
		case err == nil:
			old = rc
			defer func() { _ = rc.Close() }()
		case fault.KindOf(err) != fault.NotFound:
			return tenant.SHA256{}, storageError(err, "the scan's metadata cannot be read")
		}
	}
	meta := p.sidecarMetaFor(ctx, r, c, seriesUID, at)
	first, h, err := p.sidecarLine(ctx, meta, puts[0])
	if err != nil {
		return tenant.SHA256{}, err
	}
	*values = scanValues(h)
	*searched = p.searchRow(ctx, puts, h)
	meta.withValues(c, *values)
	i := 0
	added := func() (sidecar.Instance, bool, error) {
		if i == len(puts) {
			return sidecar.Instance{}, false, nil
		}
		wr := puts[i]
		i++
		if i == 1 {
			return first, true, nil
		}
		in, _, err := p.sidecarLine(ctx, meta, wr)
		if err != nil {
			return sidecar.Instance{}, false, err
		}
		return in, true, nil
	}
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		_, err := sidecar.MergeScan(pw, old, meta.scan, added, drop)
		_ = pw.CloseWithError(err)
		done <- err
	}()
	w, perr := store.Put(ctx, key, pr)
	_ = pr.CloseWithError(errors.New("the storage stopped reading"))
	merr := <-done
	if merr != nil && !errors.Is(merr, io.ErrClosedPipe) {
		if fault.KindOf(merr) == fault.Internal {
			return tenant.SHA256{}, fault.Wrap(fault.Internal, merr, "the scan's metadata cannot be made")
		}
		return tenant.SHA256{}, merr
	}
	if perr != nil {
		return tenant.SHA256{}, storageError(perr, "the scan's metadata cannot be written")
	}
	return tenant.SHA256(w.SHA256), nil
}

// sidecarLine returns the sidecar line of the written file wr, read from
// the bytes it put, and the header read.
func (p *Pipeline) sidecarLine(ctx context.Context, meta sidecarMeta, wr written) (sidecar.Instance, *header.Header, error) {
	h, table, sums, err := p.readLine(ctx, wr)
	if err != nil {
		return sidecar.Instance{}, nil, err
	}
	in := p.lineOf(meta, wr)
	in.TransferSyntax = h.TransferSyntaxUID
	// A deflated data set's positions count inflated bytes (PS3.5 A.5), so
	// the sidecar's check would refuse them as outside the file and the scan
	// could never be made; such a file has no place for its pixels.
	if h.HasPixelData && h.TransferSyntaxUID != header.TransferSyntaxDeflated {
		in.PixelData = &sidecar.PixelData{Offset: h.PixelData.Offset, Length: h.PixelData.Length}
		in.Frames, in.FrameSHA256 = table, sums
	}
	if js, err := h.DICOMJSON(); err == nil {
		in.Dataset = js
	}
	return in, h, nil
}

// readLine reads what a file's sidecar line holds from the bytes it put
// (its incoming copy, or the de-identified copy made from it): its header,
// its frame table (frames.Table) and each frame's checksum
// (frames.Checksums), from the same open file, whose bytes are those
// written to the storage, so the table's offsets are places in the
// archived file and the header is what the archive holds, never an
// original a profile changed. The frames' checksums are read in one pass
// with the file's own, which must be the one the bytes were put with: a
// copy that changed since is never given checksums that are not of the
// bytes archived.
func (p *Pipeline) readLine(ctx context.Context, wr written) (*header.Header, [][]sidecar.Range, []string, error) {
	f, err := p.openPut(wr)
	if err != nil {
		return nil, nil, nil, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return nil, nil, nil, fault.Wrap(fault.Unavailable, err, "a recorded file cannot be looked at")
	}
	size, sum := info.Size(), wr.sum
	h, err := header.ReadWithin(f, size, p.headerLimit(ctx))
	if err != nil {
		return nil, nil, nil, fault.Wrap(fault.Internal, err, "a recorded file's header cannot be read")
	}
	table, err := frames.Table(h, f)
	if err != nil {
		return nil, nil, nil, fault.Wrap(fault.Internal, err, "a recorded file's pixel data cannot be read")
	}
	if len(table) == 0 {
		// No frames to checksum: the header read is still of the bytes
		// put only when the file's checksum is theirs.
		whole, n, err := sumAt(f, size)
		if err != nil {
			return nil, nil, nil, err
		}
		if n != wr.size || whole != sum {
			return nil, nil, nil, fault.New(fault.Internal, "a recorded file changed after it was put; its header is not written as the archive's")
		}
		return h, table, nil, nil
	}
	sums, whole, err := frames.Checksums(io.NewSectionReader(f, 0, size), table)
	if err != nil {
		return nil, nil, nil, fault.Wrap(fault.Internal, err, "a recorded file's frames cannot be read")
	}
	if whole != sum {
		return nil, nil, nil, fault.New(fault.Internal, "a recorded file changed after it was received; its frames are not checksummed")
	}
	return h, table, sums, nil
}

// repairs returns, of the files a claim answered as the same file as an
// instance held, those whose instance a scrub marked damaged or missing,
// each with the id of the finding that marked it: the same SOP Instance UID
// and received checksum, a copy of what was archived, which is held as one
// that repairs the instance rather than ignored as a duplicate. It asks the
// catalog only when the claim answered a same file.
func (p *Pipeline) repairs(ctx context.Context, files []tenant.ReceivedFile, answers []tenant.Duplicate) (map[string]string, error) {
	var uids []string
	for i, d := range answers {
		if d.Kind == tenant.SameFile {
			uids = append(uids, files[i].SOPInstanceUID)
		}
	}
	if len(uids) == 0 {
		return nil, nil
	}
	return p.store.DamagedInstances(ctx, uids)
}

// repairHeld reports whether a reception other than r holds, for the
// repair of the instance the finding marked, a file of f's SOP Instance UID
// and received checksum: one held copy per finding and checksum is kept,
// however many times the same bytes are sent again. A record of r itself
// (a step run again) is not another's: r's own hold is written again,
// which the store skips.
func (p *Pipeline) repairHeld(ctx context.Context, r tenant.Reception, f tenant.ReceivedFile, finding string) (bool, error) {
	filter := tenant.QuarantineFilter{State: tenant.QuarantineHeld, Reason: tenant.QuarantineRepairsDamaged, SOPInstanceUID: f.SOPInstanceUID}
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		held, next, err := p.store.ListQuarantined(ctx, filter, page)
		if err != nil {
			return false, err
		}
		for _, q := range held {
			if q.ReceptionID != r.ID && q.SHA256 == f.SHA256 && q.Details["finding"] == finding && q.ContentKept {
				return true, nil
			}
		}
		if next == "" {
			return false, nil
		}
		page.Cursor = next
	}
}

// storageError keeps a storage's failure with its kind, and a message for
// the log and the unit's last error that says which write failed.
func storageError(err error, message string) error {
	k := fault.KindOf(err)
	if k == fault.Internal {
		k = fault.Unavailable
	}
	return fault.Wrap(k, err, message+": "+safe(err))
}

// scanValues returns what a file's header says of its scan and session, as
// the catalog keeps it (tenant.ScanValuesOf of header.CatalogText): one
// absent or not of its kind is left empty.
func scanValues(h *header.Header) tenant.ScanValues {
	return tenant.ScanValuesOf(h.CatalogText())
}
