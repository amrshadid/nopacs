package receive

import (
	"context"
	"io"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/sidecar"
	"github.com/amrshadid/nopacs/internal/tenant"
	"github.com/amrshadid/nopacs/internal/version"
)

// ReplaceFile is the file a replace puts in place of an archived instance,
// made from a held quarantined file as its release would make it
// (correction.proto, Replace): the file as received, or its copy
// de-identified by the profile version of the receiver it arrived at.
type ReplaceFile struct {
	// Storage and Path are where the tenant holds the instance's scan now,
	// found by the file's study and series.
	Storage string
	Path    label.Path
	// Open opens the bytes to put; each call gives the same bytes.
	Open func() (io.ReadCloser, error)
	// Size and SHA256 are those bytes'.
	Size   int64
	SHA256 tenant.SHA256
	// Line is the line of the scan's metadata for those bytes, as the
	// archive step makes one: their header, frame table and data set, the
	// profile version that made them, the version of this build. The
	// replace sets its key, revision, size and checksums, and its held
	// time to the one the instance's current line holds.
	Line sidecar.Instance
	// Done lets the pipeline go on with its incoming area and removes the
	// de-identified copy. The caller calls it once the replace has
	// answered, and reads nothing through Open after.
	Done func()
}

// ReplaceSource makes the file a replace from the held quarantined file q
// puts. It is admitted as a decision is (the incoming area is not closed
// under it until Done), and refuses, changing nothing:
//
//   - a file not held, of another reason than different file or repairs
//     damaged, or whose content was not kept (precondition);
//   - a file whose receiver has been removed (precondition), since its
//     profile version decides what leaves the file;
//   - a file recorded as received at a receiver that de-identifies now, or
//     recorded de-identified at one that no longer does (precondition): a
//     replace never archives a file identified under a receiver that
//     de-identifies, and has no version to de-identify one by;
//   - a file whose bytes no longer have the checksum it was held with
//     (precondition);
//   - a file its de-identification holds (precondition, with the reason
//     the pipeline would hold it for): it stays held.
//
// Nothing of a value of the file reaches an error.
func (p *Pipeline) ReplaceSource(ctx context.Context, q tenant.QuarantinedFile) (ReplaceFile, error) {
	leave, err := p.enterDecision()
	if err != nil {
		return ReplaceFile{}, err
	}
	out, err := p.replaceSource(ctx, q)
	if err != nil {
		leave()
		return ReplaceFile{}, err
	}
	done := out.Done
	out.Done = func() {
		if done != nil {
			done()
		}
		leave()
	}
	return out, nil
}

// replaceSource is ReplaceSource inside its decision.
func (p *Pipeline) replaceSource(ctx context.Context, q tenant.QuarantinedFile) (ReplaceFile, error) {
	switch {
	case q.State != tenant.QuarantineHeld:
		return ReplaceFile{}, fault.New(fault.Precondition, "the quarantined file is not held")
	case q.Reason != tenant.QuarantineDifferentFile && q.Reason != tenant.QuarantineRepairsDamaged:
		return ReplaceFile{}, fault.New(fault.Precondition, "only a file quarantined as a different file or as repairing a damaged instance replaces one")
	case !q.ContentKept || q.Incoming == "":
		return ReplaceFile{}, fault.New(fault.Precondition, "the quarantined file's content was not kept")
	}
	rcv, err := p.receivers.get(ctx, q.Receiver)
	if fault.KindOf(err) == fault.NotFound {
		return ReplaceFile{}, fault.New(fault.Precondition, "the receiver the file arrived at has been removed")
	}
	if err != nil {
		return ReplaceFile{}, err
	}
	switch {
	case !q.Deidentified && rcv.Deidentifies():
		return ReplaceFile{}, fault.New(fault.Precondition, "the file was held as received, and its receiver de-identifies now; release it first so it is recorded again")
	case q.Deidentified && !rcv.Deidentifies():
		return ReplaceFile{}, fault.New(fault.Precondition, "the file was held at a receiver that de-identified, and it names no profile version now")
	}
	in, size, err := p.area.Open(q.Incoming)
	if err != nil {
		return ReplaceFile{}, err
	}
	defer func() { _ = in.Close() }()
	got, n, err := sumAt(in, size)
	if err != nil {
		return ReplaceFile{}, err
	}
	want := got
	if q.Deidentified {
		keys, err := p.deidKeys(ctx)
		if err != nil {
			return ReplaceFile{}, err
		}
		want = keyedSum(keys, got)
	}
	if n != q.Size || want != q.SHA256 {
		return ReplaceFile{}, fault.New(fault.Precondition, "the quarantined file no longer has the checksum it was held with")
	}
	wr := written{f: tenant.ReceivedFile{
		ReceptionID: q.ReceptionID, SOPInstanceUID: q.SOPInstanceUID, SeriesUID: q.SeriesUID, StudyUID: q.StudyUID,
		SOPClassUID: q.SOPClassUID, TransferSyntaxUID: q.TransferSyntaxUID, Size: q.Size, SHA256: q.SHA256,
		Incoming: q.Incoming, Caller: q.Caller, Released: true, Deidentified: q.Deidentified,
	}, size: n, sum: got}
	path, storage, err := p.archivedPath(ctx, q)
	if err != nil {
		return ReplaceFile{}, err
	}
	if q.Deidentified {
		h, err := header.ReadWithin(in, size, p.headerLimit(ctx))
		if err != nil {
			return ReplaceFile{}, fault.New(fault.Precondition, "the quarantined file's header cannot be read, so it cannot be de-identified")
		}
		made, hold, err := p.deidentify(ctx, tenant.Reception{ID: q.ReceptionID, Receiver: q.Receiver}, rcv, path, wr, in, h)
		if err != nil {
			return ReplaceFile{}, err
		}
		if hold != nil {
			return ReplaceFile{}, fault.New(fault.Precondition, "the quarantined file's de-identification fails ("+string(hold.Reason)+holdCause(hold)+"); it stays held")
		}
		wr = made
	}
	line, err := p.replaceLine(ctx, q, wr)
	if err != nil {
		if wr.made {
			_ = p.area.RemoveDerived(q.Incoming)
		}
		return ReplaceFile{}, err
	}
	out := ReplaceFile{Storage: storage, Path: path, Size: wr.size, SHA256: wr.sum, Line: line, Open: func() (io.ReadCloser, error) {
		f, err := p.openPut(wr)
		if err != nil {
			return nil, err
		}
		return f, nil
	}}
	if wr.made {
		out.Done = func() {
			if err := p.area.RemoveDerived(q.Incoming); err != nil {
				p.log.Warn("a replace's de-identified copy could not be removed; it goes with its original or at the next start", "reason", safe(err))
			}
		}
	}
	return out, nil
}

// archivedPath returns the labels and the storage of the scan the tenant
// holds q's series in: where the replace puts the new bytes, and what a
// naming rule's residual check reads.
func (p *Pipeline) archivedPath(ctx context.Context, q tenant.QuarantinedFile) (label.Path, string, error) {
	labels, err := p.store.CatalogLabels(ctx, q.StudyUID, []string{q.SeriesUID})
	if err != nil {
		return label.Path{}, "", err
	}
	sc, ok := labels.Scans[q.SeriesUID]
	if labels.Session == nil || !ok {
		return label.Path{}, "", fault.New(fault.Precondition, "the tenant no longer holds the instance the file would replace in that study and series")
	}
	return label.Path{Subject: labels.Session.SubjectLabel, Session: labels.Session.SessionLabel, Scan: sc}, labels.Session.Storage, nil
}

// replaceLine returns the metadata line of the bytes wr puts, as the
// archive step makes one (sidecarLine), for a file a replace archives.
func (p *Pipeline) replaceLine(ctx context.Context, q tenant.QuarantinedFile, wr written) (sidecar.Instance, error) {
	meta := sidecarMeta{reception: q.ReceptionID, at: p.now(), version: sidecar.SafeVersion(version.Get().Version)}
	in, _, err := p.sidecarLine(ctx, meta, wr)
	return in, err
}

// holdCause returns ", <cause>" for a hold record that names one.
func holdCause(q *tenant.QuarantinedFile) string {
	if c := q.Details["cause"]; c != "" {
		return ", " + c
	}
	return ""
}
