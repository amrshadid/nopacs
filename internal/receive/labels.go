package receive

import (
	"context"
	"errors"
	"slices"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/naming"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// series is the received files of one series of a reception, in order of
// SOP Instance UID.
type series struct {
	uid   string
	files []tenant.ReceivedFile
}

// receivedBySeries returns a reception's received files grouped by series,
// the series in order of UID. Its memory is the files' rows, a few hundred
// bytes each, never their bytes.
func (p *Pipeline) receivedBySeries(ctx context.Context, receptionID string) ([]series, error) {
	by := map[string][]tenant.ReceivedFile{}
	err := p.eachReceived(ctx, receptionID, func(f tenant.ReceivedFile) error {
		by[f.SeriesUID] = append(by[f.SeriesUID], f)
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]series, 0, len(by))
	for uid, files := range by {
		out = append(out, series{uid: uid, files: files})
	}
	slices.SortFunc(out, func(a, b series) int {
		switch {
		case a.uid < b.uid:
			return -1
		case a.uid > b.uid:
			return 1
		}
		return 0
	})
	return out, nil
}

// eachReceived calls fn for each received file of a reception, a page at a
// time.
func (p *Pipeline) eachReceived(ctx context.Context, receptionID string, fn func(tenant.ReceivedFile) error) error {
	page := master.Page{Limit: master.MaxPageLimit}
	for {
		files, next, err := p.store.ReceivedFiles(ctx, receptionID, page)
		if err != nil {
			return err
		}
		for _, f := range files {
			if err := fn(f); err != nil {
				return err
			}
		}
		if next == "" {
			return nil
		}
		page.Cursor = next
	}
}

// catalogLabels returns what the catalog holds for the study and the series,
// asked in bounded batches.
func (p *Pipeline) catalogLabels(ctx context.Context, study string, groups []series) (tenant.CatalogLabels, error) {
	out := tenant.CatalogLabels{Scans: map[string]string{}}
	uids := make([]string, len(groups))
	for i, g := range groups {
		uids[i] = g.uid
	}
	for start := 0; start == 0 || start < len(uids); start += tenant.MaxSeriesLookup {
		part := uids[start:min(start+tenant.MaxSeriesLookup, len(uids))]
		c, err := p.store.CatalogLabels(ctx, study, part)
		if err != nil {
			return out, err
		}
		if c.Session != nil {
			out.Session = c.Session
		}
		for k, v := range c.Scans {
			out.Scans[k] = v
		}
		if len(uids) == 0 {
			break
		}
	}
	return out, nil
}

// ruleFor returns the receiver's naming rule, the default when it has none
// or when the receiver was removed.
func (p *Pipeline) ruleFor(ctx context.Context, receiverName string) (*naming.Rule, error) {
	rcv, err := p.receivers.get(ctx, receiverName)
	if fault.KindOf(err) == fault.NotFound {
		return naming.Default(), nil
	}
	if err != nil {
		return nil, err
	}
	return ruleOf(rcv)
}

// ruleOf returns a receiver's naming rule, the default when it has none,
// with the parts an import receiver's paths give (Receiver.NamingRule).
func ruleOf(rcv tenant.Receiver) (*naming.Rule, error) {
	rule, err := rcv.NamingRule()
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "the receiver's naming rule does not parse")
	}
	return rule, nil
}

// withIdentity returns rec carrying the file's subject identity read from
// its header h under the namespace ns (naming.ReadSubjectIdentity), the
// same read the naming rule makes the subject label from, so the engine
// shifts and hashes by the identity the label is of
// (engine.Reception.IdentityGiven). A header with no identity it can read
// gives an empty key, and the engine refuses every action that needs one.
func withIdentity(rec engine.Reception, h *header.Header, ns string) engine.Reception {
	rec.IdentityGiven, rec.Identity, rec.Namespace = true, "", ns
	if id, err := naming.ReadSubjectIdentity(h, ns); err == nil {
		rec.Identity = string(id.Key())
	}
	return rec
}

// subjectMismatch is the deid_failed cause of a file whose own subject
// label is not its session's subject (subjectAgrees).
const subjectMismatch = "subject_mismatch"

// subjectAgrees holds a file a receiver de-identifies unless the subject
// label the receiver's rule makes from the file itself is the subject its
// session is filed under (path.Subject), or the subject a chain of
// committed relabels moved that label to. The session's subject was named
// from one file of the study, or by an earlier arrival, and the engine
// shifts and hashes each file by its own subject identity: a file whose
// identity is another (an issuer one file has and another lacks, another
// Patient ID) would put dates moved by two offsets, and two patients'
// files, under one subject. It fails closed: such a file is held,
// deid_failed with cause subject_mismatch; a file whose own label cannot
// be made is held bad_label with its fault. A release runs the check again.
// It returns the reason and details of the hold, or "" when the file
// agrees.
func (p *Pipeline) subjectAgrees(ctx context.Context, rcv tenant.Receiver, path label.Path, f tenant.ReceivedFile, h *header.Header) (tenant.QuarantineReason, map[string]string, error) {
	rule, err := ruleOf(rcv)
	if err != nil {
		return "", nil, err
	}
	var parts map[string]string
	if len(rule.GivenParts()) > 0 {
		if parts, _, err = p.store.ArrivalParts(ctx, f.ReceptionID, f.SOPInstanceUID, f.SHA256); err != nil {
			return "", nil, err
		}
	}
	own, err := rule.SubjectWithParts(h, parts, p.cfg.Hasher)
	if err != nil {
		lf, err := labelFaultOf(err)
		if err != nil {
			return "", nil, err
		}
		return tenant.QuarantineBadLabel, lf.details(), nil
	}
	if own == path.Subject {
		return "", nil, nil
	}
	// A subject a committed relabel renamed keeps its patient's files:
	// their own label is the one it had (tenant.RelabelPointers, which
	// follows only subject relabels for a subject's labels).
	moved, ok, err := p.store.RelabeledTo(ctx, label.Path{Subject: own})
	if err != nil {
		return "", nil, err
	}
	if ok && moved.To.Subject == path.Subject {
		return "", nil, nil
	}
	return tenant.QuarantineDeidFailed, map[string]string{"cause": subjectMismatch}, nil
}

// labelFault is why a file's labels could not be made: the level, the
// attribute when it is one a quarantine record may name, and the fault
// word, all from the closed lists of the quarantine's details.
type labelFault struct {
	level, attribute, fault string
}

// record returns the details of a bad_label record.
func (f *labelFault) details() map[string]string {
	d := map[string]string{"label": f.level, "fault": f.fault}
	if f.attribute != "" {
		d["attribute"] = f.attribute
	}
	return d
}

// labelsOf runs the rule over the header of the first file of g whose bytes
// are in the incoming area, and, for a rule that uses parts a file's path
// gives, over the parts its import arrival keeps (tenant.ImportWork's
// ArrivalParts); a file with none is named without them, and a part the
// rule uses is then missing. A file's fault is a *labelFault; anything
// else (no file readable, a hasher missing) is an error of the server.
func (p *Pipeline) labelsOf(ctx context.Context, rule *naming.Rule, g series) (label.Path, *labelFault, error) {
	var lastErr error
	given := len(rule.GivenParts()) > 0
	for _, f := range g.files {
		h, err := p.readHeader(ctx, f.Incoming)
		if err != nil {
			lastErr = err
			continue
		}
		var parts map[string]string
		if given {
			if parts, _, err = p.store.ArrivalParts(ctx, f.ReceptionID, f.SOPInstanceUID, f.SHA256); err != nil {
				return label.Path{}, nil, err
			}
		}
		path, err := rule.LabelsWithParts(h, parts, p.cfg.Hasher)
		if err == nil {
			return path, nil, nil
		}
		lf, err := labelFaultOf(err)
		return label.Path{}, lf, err
	}
	if lastErr == nil {
		lastErr = fault.New(fault.Internal, "a series has no file")
	}
	return label.Path{}, nil, fault.Wrap(fault.KindOf(lastErr), lastErr, "no file of the series could be read to name it")
}

// labelFaultOf returns the fault of a file the naming rule could not name,
// from the rule's error: a *naming.Error of the file's (a value missing,
// empty, too long, of a bad character, not a UID, not matching the
// pattern, not a label), or an error of the server's (no hasher, a hash
// that is not one), which is returned as such.
func labelFaultOf(err error) (*labelFault, error) {
	var ne *naming.Error
	if !errors.As(err, &ne) {
		return nil, fault.Wrap(fault.Internal, err, "the naming rule failed")
	}
	lf := &labelFault{level: ne.Level}
	switch ne.Fault {
	case naming.Missing:
		lf.fault = tenant.FaultMissing
	case naming.Empty:
		lf.fault = tenant.FaultEmpty
	case naming.TooLong:
		lf.fault = tenant.FaultTooLong
	case naming.BadCharacter:
		lf.fault = tenant.FaultBadCharacter
	case naming.BadUID:
		lf.fault = tenant.FaultBadUID
	case naming.NoMatch:
		lf.fault = tenant.FaultNoMatch
	case naming.BadLabel:
		lf.fault = tenant.FaultBadLabel
	default:
		// NoHasher and BadHash are the server's mistakes, never a
		// file's.
		return nil, fault.New(fault.Internal, "the naming rule needs the tenant's hasher: "+ne.Fault.String())
	}
	if _, perr := naming.ParseAttribute(ne.Attribute); perr == nil {
		lf.attribute = ne.Attribute
	}
	return lf, nil
}

// readHeader reads the header of a file in the incoming area, within the
// bound the check that recorded it read it within or a raised one.
func (p *Pipeline) readHeader(ctx context.Context, path string) (*header.Header, error) {
	f, size, err := p.area.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	h, err := header.ReadWithin(f, size, p.headerLimit(ctx))
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "a recorded file's header cannot be read")
	}
	return h, nil
}

// holdRecord returns the quarantine record of a received file of reception
// r. Its id is made from the reception, the UID and the checksum, so one
// file is held at most once by one reception, however many runs of a step
// hold it: the store skips a record it holds.
func (p *Pipeline) holdRecord(r tenant.Reception, f tenant.ReceivedFile, reason tenant.QuarantineReason, details map[string]string) tenant.QuarantinedFile {
	return tenant.QuarantinedFile{
		ID: derivedID("q", "reception", r.ID, f.SOPInstanceUID, f.SHA256.String()), Reason: reason, Details: details,
		Receiver: r.Receiver, ReceptionID: r.ID, Caller: f.Caller, Size: f.Size, SHA256: f.SHA256,
		SOPClassUID: f.SOPClassUID, SOPInstanceUID: f.SOPInstanceUID, StudyUID: f.StudyUID, SeriesUID: f.SeriesUID,
		TransferSyntaxUID: f.TransferSyntaxUID, Incoming: f.Incoming, ContentKept: true, Quarantined: p.now(),
		Deidentified: f.Deidentified,
	}
}

// hold writes quarantine records in bounded batches, under the fence of the
// unit w the worker holds (HoldFilesFenced): a worker whose lease was taken
// over holds nothing and is answered that its lease is lost. A record
// written before (a step run again) is skipped by the store.
func (p *Pipeline) hold(ctx context.Context, w tenant.Work, records []tenant.QuarantinedFile) error {
	fence := w.Fence()
	for start := 0; start < len(records); start += tenant.MaxQuarantineBatch {
		part := records[start:min(start+tenant.MaxQuarantineBatch, len(records))]
		p.at("step.before-hold")
		if _, err := p.store.HoldFiles(ctx, part, &fence); err != nil {
			return err
		}
		p.wake()
	}
	return nil
}
