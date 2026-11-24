package receive

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/deid/check"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// checkHeadroom is what the check after may read past the receiver's
// largest file: the elements the engine writes (the method record, the
// remapped UIDs, a character set) can make a de-identified file a little
// larger than the file received, which the receiver's bound held.
const checkHeadroom = 1 << 20

// prepare makes each file of a scan ready to be put on the storage, by how
// its row says it was recorded (tenant.ReceivedFile.Deidentified), never by
// the receiver's choice now or by what its UIDs look like: a file recorded
// as received goes as it is, unless its receiver de-identifies now, when it
// is held (deid_failed, recorded_as_received: a file in flight at the
// switch is never archived identified under a receiver that de-identifies;
// its release lands it again, remapped and keyed); a file recorded
// de-identified goes de-identified by the profile version the route chosen
// for its reception names, else the one its receiver names now
// (receiverFor), or is held when the receiver names none any more
// (deid_failed, no_profile).
//
// A file de-identified (deidentify) is written beside its original in the
// incoming area, checked by the check after, and only then put, so the
// checksum chain proves the bytes on the storage are the bytes checked. A
// file that fails is returned as a hold record, with the reason the
// failure gives. An error is a failure that says nothing of the file (a
// database or disk that does not answer, the check that cannot read): the
// step is tried again, and runs the files again from their originals,
// writing the same bytes, since the engine reads no clock and no random
// source.
func (p *Pipeline) prepare(ctx context.Context, r tenant.Reception, path label.Path, files []written) ([]written, []tenant.QuarantinedFile, error) {
	rcv, err := p.receiverFor(ctx, r)
	if err != nil {
		return nil, nil, err
	}
	out := make([]written, len(files))
	holds := make([]*tenant.QuarantinedFile, len(files))
	if err := parallel(ctx, len(files), func(ctx context.Context, i int) error {
		wr, q, err := p.prepareOne(ctx, r, rcv, path, files[i])
		out[i], holds[i] = wr, q
		return err
	}); err != nil {
		p.dropMade(out)
		return nil, nil, err
	}
	ready := out[:0:0]
	var held []tenant.QuarantinedFile
	for i := range out {
		if holds[i] != nil {
			held = append(held, *holds[i])
			continue
		}
		ready = append(ready, out[i])
	}
	return ready, held, nil
}

// prepareOne makes one file ready (prepare).
func (p *Pipeline) prepareOne(ctx context.Context, r tenant.Reception, rcv tenant.Receiver, path label.Path, wr written) (written, *tenant.QuarantinedFile, error) {
	wr.size, wr.sum = wr.f.Size, wr.f.SHA256
	hold := func(cause string) (written, *tenant.QuarantinedFile, error) {
		q := p.holdRecord(r, wr.f, tenant.QuarantineDeidFailed, map[string]string{"cause": cause})
		return wr, &q, nil
	}
	switch {
	case !wr.f.Deidentified && rcv.Deidentifies():
		return hold("recorded_as_received")
	case !wr.f.Deidentified:
		// Its bytes are compared with the row's checksum when they are
		// put (putIncoming), before any byte reaches the storage.
		return wr, nil, nil
	case !rcv.Deidentifies():
		return hold("no_profile")
	}
	in, size, err := p.area.Open(wr.f.Incoming)
	if err != nil {
		return wr, nil, err
	}
	defer func() { _ = in.Close() }()
	// The original is de-identified only when it is the file its row
	// recorded: its keyed checksum is the row's.
	keys, err := p.deidKeys(ctx)
	if err != nil {
		return wr, nil, err
	}
	got, n, err := sumAt(in, size)
	if err != nil {
		return wr, nil, err
	}
	if n != wr.f.Size || keyedSum(keys, got) != wr.f.SHA256 {
		return wr, nil, fault.New(fault.Internal, "an incoming copy no longer has the checksum it was received with; it is not de-identified")
	}
	h, err := header.ReadWithin(in, size, p.headerLimit(ctx))
	if err != nil {
		return wr, nil, fault.Wrap(fault.Internal, err, "a recorded file's header cannot be read")
	}
	return p.deidentify(ctx, r, rcv, path, wr, in, h)
}

// sumAt returns the SHA-256 and the length of size bytes of r, read from its
// start without moving any offset of r, so the same handle can be read
// again and given to a writer.
func sumAt(r io.ReaderAt, size int64) (tenant.SHA256, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.NewSectionReader(r, 0, size))
	if err != nil {
		return tenant.SHA256{}, n, fault.Wrap(fault.Unavailable, err, "an incoming file cannot be read")
	}
	var out tenant.SHA256
	copy(out[:], h.Sum(nil))
	return out, n, nil
}

// deidentify de-identifies one file by the profile version ref and checks
// it, in this order:
//
//  1. The engine runs over the file's data set, its bulk values left in the
//     file, and judges its pixels (engine.ApplyImage) within the tenant's
//     deid.pixel-limit. A file the profile cannot process is held
//     (deid_failed, with the engine's fixed cause, pixels_too_large among
//     them; burned_in_risk for an image at risk no pixel rule matches; or
//     profile_refused for a rule whose action is quarantine).
//  2. The de-identified data set must hold the keyed remapping of the
//     received UIDs, which its rows hold; one that does not is held
//     (uid_not_remapped).
//  3. It is written beside the original in the incoming area, its pixel
//     data copied from the original, never held whole (incoming.Derive),
//     filled and decoded a frame at a time when a pixel rule fills it; a
//     frame that does not decode then holds it (deid_failed,
//     pixels_undecodable).
//  3. A label the receiver's naming rule makes from a value as received,
//     not hashed, is searched for the needles: such a rule is refused for a
//     receiver that de-identifies, so this holds only one stored past that
//     refusal (residual_value, the label's level as its path).
//  4. The check after reads the written file, a read of its own, with the
//     engine's needles; a value found is held (residual_value), with the
//     element and the kind of value, never the value, and a file it cannot
//     judge is held too (deid_failed, with its cause).
//
// The file it returns is ready to put: its size and checksum are the
// written file's, and it records ref. Nothing of a value of the file
// reaches a log line, an error or a record.
func (p *Pipeline) deidentify(ctx context.Context, r tenant.Reception, rcv tenant.Receiver, path label.Path, wr written, in *os.File, h *header.Header) (written, *tenant.QuarantinedFile, error) {
	ref := rcv.Profile
	hold := func(reason tenant.QuarantineReason, details map[string]string) (written, *tenant.QuarantinedFile, error) {
		q := p.holdRecord(r, wr.f, reason, details)
		return wr, &q, nil
	}
	if p.cfg.Check == nil {
		return wr, nil, errNoCheck
	}
	// The file's own subject must be its session's: the engine keys its
	// date shift and hashed Patient ID by the file's own identity.
	reason, details, err := p.subjectAgrees(ctx, rcv, path, wr.f, h)
	if err != nil {
		return wr, nil, err
	}
	if reason != "" {
		return hold(reason, details)
	}
	en, err := p.engineFor(ctx, ref)
	if errors.Is(err, errProfileUnusable) {
		return hold(tenant.QuarantineDeidFailed, map[string]string{"cause": "profile_unusable"})
	}
	if err != nil {
		return wr, nil, err
	}
	ds, source, littleEndian, err := h.Dataset(in)
	if err != nil {
		return hold(tenant.QuarantineDeidFailed, map[string]string{"cause": "unreadable"})
	}
	rec, err := en.ApplyImage(ctx, ds, withIdentity(engine.Reception{
		Receiver: r.Receiver, CallingAETitle: wr.f.Caller.CallingAETitle, CalledAETitle: wr.f.Caller.CalledAETitle,
		CharacterSet: h.CharacterSet(),
	}, h, rcv.Namespace), image(h, in, source, p.pixelLimit(ctx)))
	var failure *engine.Failure
	if errors.As(err, &failure) {
		return hold(engineHold(failure))
	}
	if err != nil {
		return wr, nil, fault.Wrap(fault.Unavailable, err, "a file could not be de-identified now")
	}
	if level, kind := labelResidual(rcv, path, rec.Needles); level != "" {
		return hold(tenant.QuarantineResidualValue, map[string]string{"path": level, "kind": kind})
	}
	if dsUID(ds, dsSOPInstanceUID) != wr.f.SOPInstanceUID || dsUID(ds, dsStudyUID) != wr.f.StudyUID || dsUID(ds, dsSeriesUID) != wr.f.SeriesUID {
		return hold(tenant.QuarantineDeidFailed, map[string]string{"cause": "uid_not_remapped"})
	}
	p.at("archive.before-deid")
	made, err := p.area.Derive(wr.f.Incoming, func(w io.WriteSeeker) error {
		return writeDataset(w, ds, writtenSyntax(h.TransferSyntaxUID, ds, rec), source, littleEndian)
	})
	var we errWrite
	if errors.Is(err, engine.ErrPixelsUndecodable) {
		return hold(tenant.QuarantineDeidFailed, pixelHold)
	}
	if errors.As(err, &we) {
		return hold(tenant.QuarantineDeidFailed, map[string]string{"cause": "unwritable"})
	}
	if err != nil {
		return wr, nil, err
	}
	p.at("archive.after-deid")
	wr.made, wr.size, wr.sum, wr.profile = true, made.Size, made.SHA256, ref
	out, size, err := p.area.OpenDerived(wr.f.Incoming)
	if err != nil {
		return wr, nil, err
	}
	defer func() { _ = out.Close() }()
	if size != wr.size {
		return wr, nil, fault.New(fault.Internal, "a de-identified file changed after it was written")
	}
	_, err = p.cfg.Check(rcv.MaxFileBytes+checkHeadroom+grown(rec)).CheckWritten(ctx, out, size, rec.Needles, rec.Written, h.CharacterSet())
	if reason, details, held := checkHold(err); held {
		wr.made = false
		_ = p.area.RemoveDerived(wr.f.Incoming)
		return hold(reason, details)
	} else if err != nil {
		return wr, nil, fault.Wrap(fault.Unavailable, err, "a de-identified file could not be checked now")
	}
	// The bytes the check read, through this handle, are the bytes written
	// and the bytes put: each is compared with the checksum Derive took as
	// it wrote them (here, and before the Put, putIncoming).
	if got, n, err := sumAt(out, size); err != nil {
		return wr, nil, err
	} else if n != made.Size || got != tenant.SHA256(made.SHA256) {
		return wr, nil, fault.New(fault.Internal, "a de-identified file changed while it was checked; it is not archived")
	}
	return wr, nil, nil
}

// engineHold returns the reason and the details a file the engine refused
// is held with: profile_refused, with the rule, for a rule whose action is
// quarantine; burned_in_risk, with why, for an image at risk no pixel rule
// matches; deid_failed, with the engine's fixed cause, the element and
// the rule, for any other.
func engineHold(failure *engine.Failure) (tenant.QuarantineReason, map[string]string) {
	switch failure.Cause {
	case engine.CauseRuleQuarantine:
		return tenant.QuarantineProfileRefused, ruleDetail(nil, failure.Rule)
	case engine.CauseBurnedInRisk:
		return burnedInHold(failure)
	}
	details := map[string]string{"cause": string(failure.Cause)}
	if failure.Path != "" {
		details["path"] = failure.Path
	}
	return tenant.QuarantineDeidFailed, ruleDetail(details, failure.Rule)
}

// checkHold returns the reason and the details a file is held with for
// the check after's err, and true; or false when err says nothing of the
// file (nil, or a failure to check it now). A value found is
// residual_value, with its kind and element, never the value; a file the
// check cannot judge is deid_failed, with the check's cause.
func checkHold(err error) (tenant.QuarantineReason, map[string]string, bool) {
	var residual *check.Residual
	var refusal *check.Refusal
	switch {
	case errors.As(err, &residual):
		details := map[string]string{"kind": string(residual.First.Kind)}
		if tenant.ValidElementPath(residual.First.Path) {
			details["path"] = residual.First.Path
		}
		return tenant.QuarantineResidualValue, details, true
	case errors.As(err, &refusal):
		return tenant.QuarantineDeidFailed, map[string]string{"cause": refusal.Detail()}, true
	}
	return "", nil, false
}

// ruleDetail adds a rule's name to details, when it has the form a
// record's detail takes, and returns them.
func ruleDetail(details map[string]string, rule string) map[string]string {
	if details == nil {
		details = map[string]string{}
	}
	if tenant.ValidRuleName(rule) {
		details["rule"] = rule
	}
	return details
}

// dropMade removes the de-identified copies the files ready to put were
// written to, once they are put and committed or the step gave up. One a
// failure leaves is made again by the next run, or removed with its
// original, or at the next start.
func (p *Pipeline) dropMade(files []written) {
	for _, wr := range files {
		if wr.made {
			if err := p.area.RemoveDerived(wr.f.Incoming); err != nil {
				p.log.Warn("a de-identified copy could not be removed; it goes with its original or at the next start", "reason", safe(err))
			}
		}
	}
}

// minLabelNeedle is the shortest removed value, or part of a name, looked
// for in a label: shorter ones are found in ordinary words.
const minLabelNeedle = 3

// labelResidual returns the level of the first label of path that holds a
// value the profile removed, and the kind of that value, or "" when none
// does. Only a receiver whose naming rule puts a value into a label as
// received is searched (tenant.Receiver.PlainPlaceholders): a hashed label
// is base32 of a keyed hash and holds no value, and a chance match in it
// would hold a file for nothing. Letters are compared in any case, and a
// person's name by each of its parts too.
func labelResidual(rcv tenant.Receiver, path label.Path, needles engine.Needles) (level, kind string) {
	if len(rcv.PlainPlaceholders()) == 0 {
		return "", ""
	}
	for _, l := range []struct{ level, label string }{{"subject", path.Subject}, {"session", path.Session}, {"scan", path.Scan}} {
		low := strings.ToLower(l.label)
		for _, n := range needles {
			forms := []string{n.Value}
			if n.Kind == engine.NeedleName {
				forms = append(forms, strings.FieldsFunc(n.Value, func(r rune) bool { return r == '^' || r == ' ' || r == '=' })...)
			}
			for _, f := range forms {
				if len(f) >= minLabelNeedle && strings.Contains(low, strings.ToLower(f)) {
					return l.level, residualKindOf(n.Kind)
				}
			}
		}
	}
	return "", ""
}

// residualKindOf is the word a residual_value record names a kind of value
// by, as the check after names it.
func residualKindOf(k engine.NeedleKind) string {
	switch k {
	case engine.NeedleName:
		return string(check.KindName)
	case engine.NeedleDate:
		return string(check.KindDate)
	case engine.NeedleUID:
		return string(check.KindUID)
	}
	return string(check.KindIdentifier)
}
