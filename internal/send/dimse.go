package send

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/jobmeet"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// maxContexts is the most presentation contexts one association proposes:
// their IDs are the odd numbers 1 to 255 (PS3.8 9.3.2.2).
const maxContexts = 128

// pair is a kind of instance as an association proposes it: a SOP class in
// one transfer syntax.
type pair struct{ class, syntax string }

// dimseSession is the association a DIMSE scan unit sends on, opened at its
// first instance and again when an instance of a kind not yet proposed
// comes, proposing every kind the unit has met, each in the one syntax it is
// stored in (PS3.8 9.3.2.2), so nothing is ever sent in another syntax.
type dimseSession struct {
	u        *unitRun
	scu      *network.SCU
	pairs    []pair
	proposed map[pair]bool
}

// sendDIMSE sends a scan's instances after its cursor by C-STORE, one at a
// time, recording the peer's answers in batches.
func (u *unitRun) sendDIMSE(scan tenant.JobScan, rec *recorder) outcome {
	sess := &dimseSession{u: u, proposed: map[pair]bool{}}
	defer sess.close()
	it := newInstances(u, scan.Cursor)
	for {
		if out, stop := u.stopping(); stop {
			return out
		}
		in, deleted, ok, err := it.next()
		if err != nil {
			return u.readFailure(err)
		}
		if !ok {
			return outcome{}
		}
		var m jobmeet.Meeting
		var settled *result
		if deleted {
			r := u.deletedResult()
			settled = &r
		} else {
			var out *outcome
			if m, settled, out = u.meet(in); out != nil {
				return *out
			}
		}
		if settled != nil {
			rec.add(in, *settled)
		} else {
			res, out := sess.send(m.Instance)
			if out != nil {
				return *out
			}
			if res.kind == resultSent || res.kind == resultWarned {
				res.superseded = u.supersededByReplace(in.SOPInstanceUID, m.Revision)
			}
			in = m.Instance
			rec.add(in, res)
		}
		if out, stop := u.checkpoint(rec, false); stop {
			return out
		}
	}
}

// send sends one instance and returns what became of it, or the outcome
// that ends the unit with it unanswered.
func (s *dimseSession) send(in tenant.ScanInstance) (result, *outcome) {
	u := s.u
	release, err := u.takeSlot()
	if err != nil {
		out := u.readFailure(err)
		return result{}, &out
	}
	defer release()
	st, err := openStream(u.ctx, u.stores, u.scan, in, u.bufs, u.log)
	switch {
	case errors.Is(err, errMissing):
		u.dataLoss(detailMissing)
		return refused(tenant.FailureDamagedInStorage, 0, detailMissing), nil
	case errors.Is(err, checkedread.ErrMarked):
		u.dataLoss(detailDamaged)
		return refused(tenant.FailureDamagedInStorage, 0, detailDamaged), nil
	case err != nil:
		out := u.readFailure(err)
		return result{}, &out
	}
	defer func() { _ = st.Close() }()
	m, ds, err := dataSetOf(st, in.Length, u.prefix)
	if err == nil && m.sopInstance != "" && m.sopInstance != in.SOPInstanceUID {
		err = errMetaDisagrees
	}
	if errors.Is(err, errNoMeta) || errors.Is(err, errMetaDisagrees) {
		// Read to the end, so damage that reached the meta group is
		// reported as damage rather than as a meta that cannot be read.
		_, _ = io.Copy(io.Discard, st)
	}
	if err != nil {
		_ = st.Close()
		return s.readEnded(st, err)
	}
	p := pair{class: m.sopClass, syntax: m.syntax}
	res, out := s.ensure(p)
	if out != nil || res != nil {
		if res != nil {
			res.sopClass, res.syntax = p.class, p.syntax
			return *res, nil
		}
		return result{}, out
	}
	if refusal, ok := s.notAccepted(p); ok {
		return refusal, nil
	}
	// Every PDU of the instance is written within the response timeout of
	// the last fragment the peer took (stallCtx), so a peer that stops
	// reading cuts the association rather than hold the unit, and one that
	// is slow but steady is never cut.
	ictx := newStallCtx(u.ctx, u.row.ResponseTimeout)
	status, err := s.scu.StoreStream(ictx, network.StoreStreamRequest{
		SOPClassUID: m.sopClass, SOPInstanceUID: in.SOPInstanceUID, TransferSyntaxUID: m.syntax,
		DataSet: &progressReader{r: &paced{ctx: u.ctx, r: ds, rate: &u.sr.rate}, ctx: ictx},
	})
	if err != nil {
		// The association is over: aborted for a read that failed, or
		// lost. The next instance opens another.
		s.drop()
		_ = st.Close()
		if st.damaged() || st.failed() != nil {
			return s.readEnded(st, err)
		}
		if u.ctx.Err() != nil {
			return result{}, &outcome{stopped: true}
		}
		reason := reasonLost
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			reason = reasonTimeout
		}
		var te *network.TimeoutError
		if errors.As(err, &te) || ictx.stalled() {
			reason = reasonTimeout
		}
		u.log.Info("sender: the association ended during a C-STORE", "reason", reason)
		return result{}, &outcome{temporary: true, reason: reason}
	}
	res2, temporary := classifyStatus(status)
	if temporary {
		return result{}, &outcome{temporary: true, reason: reasonResources}
	}
	res2.sopClass, res2.syntax = p.class, p.syntax
	return res2, nil
}

// readEnded returns what became of an instance whose read ended before it
// was sent whole, once its copy has ended: refused as damaged in storage
// for a mismatch, refused as meta_unreadable for a meta group (PS3.10 7.1)
// that cannot be read or that names another instance than its data set,
// and the unit failed for now for a storage that did not answer.
func (s *dimseSession) readEnded(st *stream, err error) (result, *outcome) {
	u := s.u
	switch {
	case st.damaged():
		u.dataLoss(detailDamaged)
		return refused(tenant.FailureDamagedInStorage, 0, detailDamaged), nil
	case errors.Is(err, errNoMeta):
		// The bytes are the archived ones (the checksum is not what
		// failed): the file is refused for what it holds, not as data
		// loss.
		u.log.Info("sender: an instance is not sent: its file meta information cannot be read")
		return refused(tenant.FailureMetaUnreadable, 0, detailNoMeta), nil
	case errors.Is(err, errMetaDisagrees):
		u.log.Info("sender: an instance is not sent: its file meta information and its data set disagree")
		return refused(tenant.FailureMetaUnreadable, 0, detailMetaDisagrees), nil
	}
	if fe := st.failed(); fe != nil {
		err = fe
	}
	out := u.readFailure(err)
	return result{}, &out
}

// classifyStatus reads a C-STORE response's status as the contract says
// (PS3.4 B.2.3, PS3.7 Annex C): 0000 sent; B000, B006 and B007 sent with a
// warning; A7xx (out of resources) a temporary failure of the unit; any
// other refuses the instance with it.
func classifyStatus(status uint16) (result, bool) {
	switch {
	case status == 0x0000:
		return sent(), false
	case status == 0xB000 || status == 0xB006 || status == 0xB007:
		return warned(int32(status)), false
	case status&0xFF00 == 0xA700:
		return result{}, true
	}
	return refused(tenant.FailurePeerRefused, int32(status), detailRefused), false
}

// ensure makes sure an association is open that proposed p, opening one
// that proposes every kind met so far when it is not. It returns a refusal
// of the instance when p cannot be proposed, or the outcome that ends the
// unit when the connection failed.
func (s *dimseSession) ensure(p pair) (*result, *outcome) {
	if s.scu != nil && s.proposed[p] {
		return nil, nil
	}
	if !s.proposed[p] {
		if len(s.pairs) >= maxContexts {
			r := refused(tenant.FailureSyntaxNotAccepted, 0, detailTooManyKinds)
			return &r, nil
		}
		s.pairs = append(s.pairs, p)
		s.proposed[p] = true
	}
	s.close()
	scu, err := s.u.sup.associate(s.u.ctx, s.u.sr.t.name, s.u.row, s.storeContexts())
	if err != nil {
		if s.u.ctx.Err() != nil {
			return nil, &outcome{stopped: true}
		}
		var ce *connectError
		if !errors.As(err, &ce) {
			ce = &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer, err: err}
		}
		out := s.u.connectFailed(ce)
		return nil, &out
	}
	s.u.connected()
	s.scu = scu
	return nil, nil
}

// storeContexts returns the presentation contexts of every kind met.
func (s *dimseSession) storeContexts() []network.PresentationContextItem {
	out := make([]network.PresentationContextItem, len(s.pairs))
	for i, p := range s.pairs {
		out[i] = network.PresentationContextItem{ID: byte(2*i + 1), AbstractSyntax: p.class, TransferSyntaxes: []string{p.syntax}} // #nosec G115 -- at most maxContexts.
	}
	return out
}

// notAccepted returns the refusal of an instance of kind p when the peer
// accepted no context with exactly that class and syntax: the class not
// accepted when the peer took the class in no syntax and refused it as an
// abstract syntax it does not support, the syntax not accepted otherwise
// (PS3.8 9.3.3.2).
func (s *dimseSession) notAccepted(p pair) (result, bool) {
	assoc := s.scu.Association()
	if assoc == nil {
		return refused(tenant.FailureSyntaxNotAccepted, 0, detailSyntax), true
	}
	classTaken := false
	for _, pc := range assoc.AcceptedContexts() {
		if pc.AbstractSyntax != p.class {
			continue
		}
		if pc.TransferSyntax == p.syntax {
			return result{}, false
		}
		classTaken = true
	}
	r := refused(tenant.FailureSyntaxNotAccepted, 0, detailSyntax)
	if !classTaken {
		for _, pc := range assoc.RefusedContexts() {
			if pc.AbstractSyntax == p.class && pc.Result == network.PCResultAbstractSyntaxNotSupported {
				r = refused(tenant.FailureClassNotAccepted, 0, detailClass)
				break
			}
		}
	}
	r.sopClass, r.syntax = p.class, p.syntax
	return r, true
}

// drop forgets an association that is over, closing its connection.
func (s *dimseSession) drop() {
	if s.scu == nil {
		return
	}
	_ = s.scu.Abort(context.WithoutCancel(s.u.ctx)) // Already over; closes what is left.
	s.scu = nil
}

// close releases the open association, if any (A-RELEASE, PS3.8 7.2).
func (s *dimseSession) close() {
	if s.scu == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(s.u.ctx), s.u.row.ConnectTimeout)
	defer cancel()
	if err := s.scu.Release(ctx); err != nil {
		_ = s.scu.Abort(ctx)
	}
	s.scu = nil
}

// associate opens an association with a DIMSE sender's peer proposing
// contexts: its host resolved and checked (resolve), each allowed address
// tried in turn while the connection itself fails, TLS when the sender has
// it, the sender's AE titles and timeouts. A failure is a *connectError.
//
// The request carries no deadline of its own: go-dicom sets a write
// deadline on the connection from the context of each PDU it writes and
// never clears it, so a deadline here would cut the association later;
// the connection is bounded by NetworkTimeout and the answer by ARTIM
// (PS3.8 9.1.5), both the sender's connect timeout.
func (s *Supervisor) associate(ctx context.Context, tenantName string, row tenant.Sender, contexts []network.PresentationContextItem) (*network.SCU, error) {
	d := row.DIMSE
	aps, err := s.resolve(ctx, d.Host, d.Port)
	if err != nil {
		return nil, err
	}
	var tlsc *network.TLSConfig
	if d.TLS != nil {
		cfg, err := tlsConfig(s.cfg.TLSDir(tenantName), d.TLS, d.Host)
		if err != nil {
			return nil, err
		}
		tlsc = &network.TLSConfig{Config: cfg}
	}
	var last error
	for _, ap := range aps {
		scu := network.NewSCU(network.SCUConfig{
			CallingAE: d.CallingAETitle, CalledAE: d.CalledAETitle, Address: ap.String(), TLS: tlsc,
			Network: network.NetworkConfig{ARTIMTimeout: row.ConnectTimeout, DIMSETimeout: row.ResponseTimeout, NetworkTimeout: row.ConnectTimeout},
			// The library's lines may quote what the peer sent; the
			// supervisor logs its own, in fixed words.
			Logger: slog.New(slog.DiscardHandler),
		})
		err := associateWithoutDeadline(ctx, scu, contexts)
		if err == nil {
			return scu, nil
		}
		abortCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), row.ConnectTimeout)
		_ = scu.Abort(abortCtx)
		cancel()
		last = associateFailure(err)
		var ce *connectError
		if !errors.As(last, &ce) || ce.fault != tenant.SenderFaultUnreachable || ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

// associateWithoutDeadline associates with ctx's cancellation but not its
// deadline, which go-dicom would leave on the connection.
func associateWithoutDeadline(ctx context.Context, scu *network.SCU, contexts []network.PresentationContextItem) error {
	actx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	return scu.Associate(actx, contexts)
}

// probeDIMSE tries a DIMSE sender's peer as TestSender does: an
// association proposing the Verification SOP Class (PS3.4 Annex A), a
// C-ECHO, and the release.
func (s *Supervisor) probeDIMSE(ctx context.Context, tenantName string, row tenant.Sender) error {
	scu, err := s.associate(ctx, tenantName, row, []network.PresentationContextItem{{
		ID: 1, AbstractSyntax: network.VerificationSOPClassUID, TransferSyntaxes: []string{network.ImplicitVRLittleEndianUID},
	}})
	if err != nil {
		return err
	}
	defer func() { _ = scu.Release(context.WithoutCancel(ctx)) }()
	if err := scu.Echo(ctx); err != nil {
		return &connectError{fault: tenant.SenderFaultNotAccepted, detail: phraseEchoNotAccepted, err: err}
	}
	return nil
}

// associateFailure returns the connectError of an association that could
// not be opened, in the contract's words (SenderFault).
func associateFailure(err error) error {
	var ce *connectError
	if errors.As(err, &ce) {
		return ce
	}
	if phrase, ok := tlsFailure(err); ok {
		return &connectError{fault: tenant.SenderFaultTLSFailed, detail: phrase, err: err}
	}
	var ae *network.AssociationError
	if errors.As(err, &ae) {
		switch ae.Code {
		case "ASSOCIATION_REJECTED":
			return &connectError{fault: tenant.SenderFaultRejected, detail: rejectPhrase(ae.Source, ae.Reason), err: err}
		case "ABORTED":
			return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseAborted, err: err}
		}
		return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer, err: err}
	}
	var pe *network.PDUError
	if errors.As(err, &pe) {
		return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer, err: err}
	}
	return &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseUnreachable, err: err}
}

// rejectPhrase names an A-ASSOCIATE-RJ's reason in the words of PS3.8
// 9.3.4 (Table 9-21), by its source and reason.
func rejectPhrase(source, reason byte) string {
	switch {
	case source == 1 && reason == 2:
		return "application-context-name-not-supported"
	case source == 1 && reason == 3:
		return "calling-AE-title-not-recognized"
	case source == 1 && reason == 7:
		return "called-AE-title-not-recognized"
	case source == 2 && reason == 2:
		return "protocol-version-not-supported"
	case source == 3 && reason == 1:
		return "temporary-congestion"
	case source == 3 && reason == 2:
		return "local-limit-exceeded"
	case reason == 1:
		return "no-reason-given"
	}
	return "rejected, reason " + strconv.Itoa(int(reason))
}
