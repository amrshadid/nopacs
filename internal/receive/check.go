package receive

import (
	"context"
	"errors"
	"slices"
	"strconv"

	"github.com/amrshadid/nopacs/internal/core/uid"
	"github.com/amrshadid/nopacs/internal/deid/engine"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/receive/header"
	"github.com/amrshadid/nopacs/internal/receive/incoming"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// landed is a file in the incoming area waiting to be recorded or held: one
// a source just handed in, one a restart found with no row, or one a release
// put back in line.
type landed struct {
	// file is where it is and what it holds.
	file incoming.File
	// receiver names the receiver whose rules it is checked by.
	receiver string
	// caller, association and the told UIDs are what its source said;
	// empty for a file a restart found.
	caller      tenant.Caller
	association string
	told        Arrival
	// releasedFrom is the quarantine record whose release put it back in
	// line, empty otherwise.
	releasedFrom string
}

// decision is what the check of a landed file decided: a received file to
// record, or a record to hold. Exactly one is set.
type decision struct {
	received *tenant.ReceivedFile
	hold     *tenant.QuarantinedFile
}

// check reads a landed file's header and decides whether it is recorded as
// received or held, and with which reason: not DICOM, a SOP class the
// receiver does not take, a placing UID missing or not well formed; and a
// file whose release put it back in line, its released record made
// de-identified, at a receiver that archives as received (deid_failed,
// no_profile). A file that passes is given its reception, opened if need
// be. An error is a failure to read the file or to reach the database,
// which decides nothing: the file stays landed and is checked again.
func (p *Pipeline) check(ctx context.Context, l landed) (decision, error) {
	rcv, err := p.receivers.get(ctx, l.receiver)
	if err != nil {
		return decision{}, err
	}
	// At a receiver that de-identifies, every row this file gets holds its
	// UIDs remapped and its checksum keyed (protect, below), so no list of
	// the tenant holds what the original could be matched by. Without the
	// keys the file waits, landed and owed: it is never recorded as
	// received.
	var keys engine.Keys
	deidentifies := rcv.Deidentifies()
	// A file a release put back in line: the released record says whether
	// it was made de-identified, and what the release named.
	var released tenant.QuarantinedFile
	if l.releasedFrom != "" {
		if released, err = p.store.QuarantinedFile(ctx, l.releasedFrom); err != nil {
			return decision{}, err
		}
	}
	if deidentifies || released.Deidentified {
		if keys, err = p.deidKeys(ctx); err != nil {
			return decision{}, err
		}
	}
	holdAtReceive := func(l landed, reason tenant.QuarantineReason, details map[string]string, h *header.Header) decision {
		d := p.holdAtReceive(l, reason, details, h)
		if deidentifies || released.Deidentified {
			protect(d.hold, keys)
		}
		return d
	}
	if released.Deidentified && !deidentifies {
		// A file held at a receiver that de-identifies is never archived
		// as received: released through a receiver that archives as
		// received (which the store refuses, so here one changed between
		// the release and this landing), it is held again, its record
		// protected as the one it was released from, and released once a
		// receiver that de-identifies is named.
		return holdAtReceive(l, tenant.QuarantineDeidFailed, map[string]string{"cause": "no_profile"}, nil), nil
	}
	f, size, err := p.area.Open(l.file.Path)
	if err != nil {
		return decision{}, err
	}
	h, err := header.ReadWithin(f, size, p.headerLimit(ctx))
	_ = f.Close()
	var nd *header.NotDICOMError
	var big *header.TooLargeError
	if errors.As(err, &big) {
		// A header past the bound is held with its bytes kept, never
		// thrown away: it is often a file worth a person's look (an
		// enhanced multi-frame object with very many frames, PS3.3
		// C.7.6.16), released once the installation raises
		// receiver.header-limit. The bound it was read within is in the
		// details.
		return holdAtReceive(l, tenant.QuarantineHeaderTooLarge, map[string]string{"limit": strconv.FormatInt(big.Limit, 10)}, nil), nil
	}
	if errors.As(err, &nd) {
		return holdAtReceive(l, tenant.QuarantineNotDICOM, map[string]string{"offset": strconv.FormatInt(nd.Offset, 10)}, nil), nil
	}
	if err != nil {
		return decision{}, fault.Wrap(fault.Unavailable, err, "a received file's header cannot be read")
	}
	if reason, details := headerHold(h, deidentifies); reason != "" {
		return holdAtReceive(l, reason, details, h), nil
	}
	if a := toldOtherwise(l.told, h); a != "" {
		return holdAtReceive(l, tenant.QuarantineUIDMismatch, map[string]string{"attribute": string(a)}, h), nil
	}
	if reason, details := classHold(rcv, h); reason != "" {
		return holdAtReceive(l, reason, details, h), nil
	}
	class, _ := h.UID(header.SOPClassUID)
	study, _ := h.UID(header.StudyUID)
	inst, _ := h.UID(header.SOPInstanceUID)
	series, _ := h.UID(header.SeriesUID)
	sum := tenant.SHA256(l.file.SHA256)
	if deidentifies {
		study, series, inst = remapPlacingUID(keys, study), remapPlacingUID(keys, series), remapPlacingUID(keys, inst)
		sum = keyedSum(keys, sum)
	}
	receptionID, err := p.receptionFor(ctx, l.receiver, study)
	if err != nil {
		return decision{}, err
	}
	rf := &tenant.ReceivedFile{
		ReceptionID: receptionID, SOPInstanceUID: inst, SeriesUID: series, StudyUID: study,
		SOPClassUID: class, TransferSyntaxUID: h.TransferSyntaxUID, Size: l.file.Size,
		SHA256: sum, Incoming: l.file.Path, Caller: l.caller, Association: l.association,
		Released: l.releasedFrom != "", Deidentified: deidentifies,
	}
	// A release that named a storage or a profile version, of a file that
	// joined no reception and lands again, still names them: the
	// reception the file joins now keeps them.
	rf.ReleasedStorage, rf.ReleasedProfile = released.ReleaseStorage, released.ReleaseProfile
	return decision{received: rf}, nil
}

// headerHold returns why a receiver holds a file by its header alone, and
// the record's details, or "" when it does not: a transfer syntax that is
// not a UID (not_dicom); a placing UID missing, empty or not well formed
// (bad_uid); and at a receiver that de-identifies, a placing UID under the
// standard's own root (bad_uid), which is no patient's study, series or
// instance, held so the rows never need an exception that would let an
// original UID in. A trial run of a profile asks the same.
func headerHold(h *header.Header, deidentifies bool) (tenant.QuarantineReason, map[string]string) {
	if uid.Check(h.TransferSyntaxUID) != nil {
		return tenant.QuarantineNotDICOM, map[string]string{"offset": "132"}
	}
	for _, a := range []header.Attribute{header.SOPInstanceUID, header.SOPClassUID, header.StudyUID, header.SeriesUID} {
		v, present := h.UID(a)
		word := ""
		switch {
		case !present:
			word = tenant.FaultMissing
		case v == "":
			word = tenant.FaultEmpty
		case uid.Check(v) != nil:
			word = tenant.FaultBadUID
		}
		if word != "" {
			return tenant.QuarantineBadUID, map[string]string{"attribute": string(a), "fault": word}
		}
	}
	if deidentifies {
		for _, a := range []header.Attribute{header.SOPInstanceUID, header.StudyUID, header.SeriesUID} {
			if v, _ := h.UID(a); underStandardRoot(v) {
				return tenant.QuarantineBadUID, map[string]string{"attribute": string(a), "fault": tenant.FaultBadUID}
			}
		}
	}
	return "", nil
}

// classHold returns sop_class_not_accepted, with the class, for a file of
// a SOP class the receiver does not list, or "". An empty list is the
// server's default set, which the listener applies when it negotiates
// (PS3.7 D.3.3.2); a file that reached the check under it was negotiated,
// so every class is taken.
func classHold(rcv tenant.Receiver, h *header.Header) (tenant.QuarantineReason, map[string]string) {
	class, _ := h.UID(header.SOPClassUID)
	if list := rcv.DIMSE.SOPClasses; len(list) > 0 && !slices.Contains(list, class) {
		return tenant.QuarantineSOPClassNotAccepted, map[string]string{"sop_class_uid": class}
	}
	return "", nil
}

// toldOtherwise returns the first placing attribute whose value in the
// whole header h differs from what the source read from the file's first
// bytes (Arrival.StudyUID), or "" when they agree or the source read
// none. A source that read nothing of the file (a C-STORE's Affected SOP
// Instance UID comes from its command, not its data set) is not compared.
func toldOtherwise(told Arrival, h *header.Header) header.Attribute {
	if told.StudyUID == "" && told.SeriesUID == "" {
		return ""
	}
	for _, c := range []struct {
		a    header.Attribute
		told string
	}{
		{header.SOPClassUID, told.SOPClassUID}, {header.SOPInstanceUID, told.SOPInstanceUID},
		{header.StudyUID, told.StudyUID}, {header.SeriesUID, told.SeriesUID},
	} {
		if v, _ := h.UID(c.a); v != c.told {
			return c.a
		}
	}
	return ""
}

// holdAtReceive returns the record of a file held by the receiving side,
// with the UIDs it could read (from its header when it has one, else what
// its source was told, each only when well formed). Its id is made from the
// file's id, so the same file checked again after a crash makes the same
// record, which the store skips; for a file a release put back in line, from
// the released record's id, so its new record is not taken for the old one.
func (p *Pipeline) holdAtReceive(l landed, reason tenant.QuarantineReason, details map[string]string, h *header.Header) decision {
	id := derivedID("q", "receive", l.file.ID)
	if l.releasedFrom != "" {
		id = derivedID("q", "release", l.releasedFrom)
	}
	q := &tenant.QuarantinedFile{
		ID: id, Reason: reason, Details: details, Receiver: l.receiver,
		Caller: l.caller, Size: l.file.Size, SHA256: l.file.SHA256, Incoming: l.file.Path, ContentKept: true,
		Quarantined: p.now(), ReleasedFrom: l.releasedFrom,
	}
	valid := func(s string) string {
		if uid.Valid(s) {
			return s
		}
		return ""
	}
	if h != nil {
		v, _ := h.UID(header.SOPClassUID)
		q.SOPClassUID = valid(v)
		v, _ = h.UID(header.SOPInstanceUID)
		q.SOPInstanceUID = valid(v)
		v, _ = h.UID(header.StudyUID)
		q.StudyUID = valid(v)
		v, _ = h.UID(header.SeriesUID)
		q.SeriesUID = valid(v)
		q.TransferSyntaxUID = valid(h.TransferSyntaxUID)
	} else {
		q.SOPClassUID = valid(l.told.SOPClassUID)
		q.SOPInstanceUID = valid(l.told.SOPInstanceUID)
		q.TransferSyntaxUID = valid(l.told.TransferSyntaxUID)
	}
	return decision{hold: q}
}

// receptionFor returns the reception the study's files at the receiver go
// into now: the one cached, or the one the store finds or opens. A reception
// it opens is given its quiet unit at once.
func (p *Pipeline) receptionFor(ctx context.Context, receiver, study string) (string, error) {
	key := receptionKey{receiver, study}
	if id, ok := p.receptions.get(key); ok {
		return id, nil
	}
	now := p.now()
	r, opened, err := p.store.OpenReception(ctx, receiver, study, newID(), now)
	if err != nil {
		return "", err
	}
	if opened {
		p.wake()
	}
	if err := p.ensureQuiet(ctx, r); err != nil {
		return "", err
	}
	p.receptions.put(key, r.ID)
	return r.ID, nil
}
