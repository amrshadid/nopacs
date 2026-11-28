package send

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/amrshadid/go-dicom/dicomweb"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// stowPeer is how a stow sender reaches its peer for one unit: a client
// whose every connection goes through resolve, and the credential's
// header.
type stowPeer struct {
	client *dicomweb.Client
	http   *http.Client
	base   *url.URL
	// header and value are the credential's header, empty for none.
	header, value string
	// conn records a connection that failed before a request was sent,
	// and answered says a response came back to the request in flight.
	conn     connRecord
	answered atomic.Bool
}

// MaxStowAnswer bounds the body of a peer's answer to one request: an
// account of up to 1000 instances (max_instances_per_request) is a few
// hundred KiB; a peer that sends more than 4 MiB is not read further.
const MaxStowAnswer = 4 << 20

// answerMark is a transport that marks, on its peer, that a request got a
// response, so a failure to read that response is told from one to send
// the request.
type answerMark struct {
	next http.RoundTripper
	peer *stowPeer
}

// CloseIdleConnections lets the transport's idle connections go.
func (a answerMark) CloseIdleConnections() {
	if c, ok := a.next.(interface{ CloseIdleConnections() }); ok {
		c.CloseIdleConnections()
	}
}

// RoundTrip sends the request and marks the response.
func (a answerMark) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := a.next.RoundTrip(r)
	if err == nil {
		a.peer.answered.Store(true)
	}
	return resp, err
}

// stowPeerOf makes the client of a stow sender: its URL's host resolved
// and checked at each connection, plain http only while the installation
// allows it, TLS verified against the system roots or the sender's CA
// file, redirects never followed (the 3xx is the answer), no proxy, and the
// sender's timeouts. The credential is opened here and held only by the
// client.
func (s *Supervisor) stowPeerOf(ctx context.Context, tenantName string, row tenant.Sender, sealed []byte) (*stowPeer, error) {
	st := row.STOW
	host, port, err := endpoint(row)
	if err != nil {
		return nil, err
	}
	base, err := url.Parse(st.URL)
	if err != nil {
		return nil, fault.Wrap(fault.Internal, err, "a stored stow sender's URL does not parse")
	}
	if base.Scheme == "http" {
		allow, err := s.cfg.Installation.AllowPlainHTTP(ctx)
		if err != nil || !allow {
			return nil, &connectError{fault: tenant.SenderFaultAddressNotAllowed, detail: phrasePlainHTTP, err: err}
		}
	}
	peer := &stowPeer{base: base}
	d := &net.Dialer{Timeout: row.ConnectTimeout}
	tr := &http.Transport{
		Proxy:                 nil,
		DialContext:           s.dialContext(host, port, d, row.ResponseTimeout, &peer.conn),
		TLSHandshakeTimeout:   row.ConnectTimeout,
		ResponseHeaderTimeout: row.ResponseTimeout,
		DisableCompression:    true,
		MaxIdleConnsPerHost:   1,
	}
	if base.Scheme == "https" {
		cfg, err := tlsConfig(s.cfg.TLSDir(tenantName), st.TLS, host)
		if err != nil {
			return nil, err
		}
		tr.DialTLSContext = s.dialTLSContext(host, port, d, cfg, row.ResponseTimeout, &peer.conn)
	}
	peer.http = &http.Client{Transport: answerMark{next: tr, peer: peer},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	opts := []dicomweb.Option{dicomweb.WithHTTPClient(peer.http), dicomweb.WithMaxResponseBytes(MaxStowAnswer)}
	if st.Authentication != tenant.StowAuthNone && st.Authentication != "" {
		if sealed == nil {
			return nil, &connectError{fault: tenant.SenderFaultRejected, detail: phraseNoCredential}
		}
		plain, err := s.cfg.Credential(ctx, tenantName, row.Name, sealed)
		if err != nil {
			return nil, &connectError{fault: tenant.SenderFaultRejected, detail: phraseNoCredential, err: err}
		}
		switch st.Authentication {
		case tenant.StowAuthBasic:
			opts = append(opts, dicomweb.WithBasicAuth(st.Username, plain))
			peer.header, peer.value = "Authorization", basicAuth(st.Username, plain)
		case tenant.StowAuthBearer:
			opts = append(opts, dicomweb.WithBearerToken(plain))
			peer.header, peer.value = "Authorization", "Bearer "+plain
		case tenant.StowAuthHeader:
			opts = append(opts, dicomweb.WithHeader(st.HeaderName, plain))
			peer.header, peer.value = st.HeaderName, plain
		}
	}
	peer.client, err = dicomweb.NewClient(st.URL, opts...)
	if err != nil {
		return nil, &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseUnreachable, err: err}
	}
	return peer, nil
}

// basicAuth is the value of basic authentication's header (RFC 7617).
func basicAuth(user, password string) string {
	r := &http.Request{Header: http.Header{}}
	r.SetBasicAuth(user, password)
	return r.Header.Get("Authorization")
}

// close lets the client's idle connections go.
func (p *stowPeer) close() { p.http.CloseIdleConnections() }

// probeStow tries a stow sender's peer as TestSender does: a QIDO-RS
// search of its studies (queryStudies), passed only by a 2xx answer
// (probeFailure).
func (s *Supervisor) probeStow(ctx context.Context, tenantName string, p tenant.Pipeline, name string) error {
	row, sealed, err := storedSender(ctx, p, name)
	if err != nil {
		return err
	}
	peer, err := s.stowPeerOf(ctx, tenantName, row, sealed)
	if err != nil {
		return err
	}
	defer peer.close()
	code, err := queryStudies(ctx, peer)
	if err != nil {
		return err
	}
	if ce := probeFailure(code); ce != nil {
		return ce
	}
	return nil
}

// queryStudies sends the service probe of a stow sender's peer and returns
// its HTTP status: GET <url>/studies?limit=1 with Accept
// application/dicom+json, a search for studies (PS3.18 10.6, the
// SearchForStudies transaction), which a DICOMweb service answers 200 with
// at most one match or 204 with none. Unlike an OPTIONS request, which a
// service need not serve (Orthanc answers it 404), it tells a URL that
// names a DICOMweb service from one that names nothing (404). It reads no
// more than 64 KiB of the answer.
func queryStudies(ctx context.Context, peer *stowPeer) (int, error) {
	target := peer.base.JoinPath("studies")
	target.RawQuery = "limit=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return 0, &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseUnreachable, err: err}
	}
	req.Header.Set("Accept", "application/dicom+json")
	if peer.header != "" {
		req.Header.Set(peer.header, peer.value)
	}
	resp, err := peer.http.Do(req)
	if err != nil {
		if ce := peer.conn.take(); ce != nil {
			return 0, ce
		}
		return 0, transportFailure(err)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// probeFailure returns the connectError of a service probe's HTTP status,
// or nil for a 2xx: the failures a request of instances has
// (statusFailure), and any other status, which a DICOMweb service does not
// answer a search for studies with.
func probeFailure(code int) *connectError {
	if ce := statusFailure(code); ce != nil {
		return ce
	}
	if code < 200 || code > 299 {
		return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer}
	}
	return nil
}

// transportFailure returns the connectError of a request that got no
// answer.
func transportFailure(err error) *connectError {
	var ce *connectError
	if errors.As(err, &ce) {
		return ce
	}
	if phrase, ok := tlsFailure(err); ok {
		return &connectError{fault: tenant.SenderFaultTLSFailed, detail: phrase, err: err}
	}
	return &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseUnreachable, err: err}
}

// statusFailure returns the connectError of an HTTP answer that says the
// peer is not usable now (401, 403 and 407: it does not take the
// credential; a redirect; 404: the URL names no DICOMweb service, so a
// mistyped URL fails the sender, never the instances; a 5xx), or nil.
func statusFailure(code int) *connectError {
	switch {
	case code == http.StatusUnauthorized || code == http.StatusForbidden || code == http.StatusProxyAuthRequired:
		return &connectError{fault: tenant.SenderFaultRejected, detail: phraseCredential}
	case code >= 300 && code < 400:
		return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseRedirect}
	case code == http.StatusNotFound:
		return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseNoService}
	case code >= 500:
		return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseServerError}
	}
	return nil
}

// item is one instance of a stow unit's window: sent, and answered, in
// the stream's order, and recorded in that order once answered.
type item struct {
	in  tenant.ScanInstance
	res *result
	// rev is the revision of in's file, set when its transfer starts.
	rev int
	// settle says it is owed to the unit and tried again by name.
	settle bool
}

// sendStow sends a scan's instances after its cursor by STOW-RS, a request
// at a time of at most the sender's instances and bytes, recording the
// answers in the stream's order. The instances owed to the unit are tried
// again first, and those left unaccounted again before the unit ends.
func (u *unitRun) sendStow(scan tenant.JobScan, rec *recorder) outcome {
	ctx := u.ctx
	peer, err := u.sup.stowPeerOf(ctx, u.sr.t.name, u.row, u.sealed)
	if err != nil {
		var ce *connectError
		if errors.As(err, &ce) {
			return u.connectFailed(ce)
		}
		return outcome{temporary: true, reason: reasonConnect}
	}
	defer peer.close()
	var owedQueue []*item
	for _, in := range scan.Owed {
		owedQueue = append(owedQueue, &item{in: in, settle: true})
	}
	if out, stop := u.settleOwed(peer, rec, owedQueue); stop {
		return out
	}
	owedQueue = nil
	it := newInstances(u, scan.Cursor)
	var window []*item
	exhausted := false
	for {
		if out, stop := u.stopping(); stop {
			return out
		}
		// Top the window up to one request's worth of unanswered
		// instances.
		var bytes int64
		var req []*item
		for _, x := range window {
			if x.res == nil {
				req, bytes = append(req, x), bytes+x.in.Size
			}
		}
		for !exhausted && len(req) < u.row.STOW.MaxInstancesPerRequest {
			in, deleted, ok, err := it.next()
			if err != nil {
				return u.readFailure(err)
			}
			if !ok {
				exhausted = true
				break
			}
			x := &item{in: in}
			window = append(window, x)
			if deleted {
				// Counted by the job, deleted since: refused in its place
				// in the stream, never read, and no part of a request.
				r := u.deletedResult()
				x.res = &r
				continue
			}
			if len(req) > 0 && bytes+in.Size > u.row.STOW.MaxRequestBytes {
				break
			}
			req, bytes = append(req, x), bytes+in.Size
		}
		if len(req) == 0 && len(window) == 0 {
			break
		}
		if len(req) > 0 {
			if out := u.stowRequest(peer, req); out != nil {
				if dout, stop := u.drain(&window, rec, &owedQueue); stop {
					return dout
				}
				return *out
			}
		}
		if out, stop := u.drain(&window, rec, &owedQueue); stop {
			return out
		}
		if out, stop := u.checkpoint(rec, true); stop {
			return out
		}
		if len(owedQueue) >= MaxBatch {
			if out, stop := u.settleOwed(peer, rec, owedQueue); stop {
				return out
			}
			owedQueue = nil
		}
	}
	if out, stop := u.checkpoint(rec, true); stop {
		return out
	}
	if out, stop := u.settleOwed(peer, rec, owedQueue); stop {
		return out
	}
	return outcome{}
}

// drain records the answered instances at the front of the window, in
// order, writing a batch whenever one is due, and keeps those owed to try
// again by name. It reports whether the unit must stop, as a write says.
func (u *unitRun) drain(window *[]*item, rec *recorder, owedQueue *[]*item) (outcome, bool) {
	w := *window
	defer func() { *window = w }()
	for len(w) > 0 && w[0].res != nil {
		rec.add(w[0].in, *w[0].res)
		if w[0].res.kind == resultOwed {
			*owedQueue = append(*owedQueue, &item{in: w[0].in, settle: true})
		}
		w = w[1:]
		if out, stop := u.checkpoint(rec, false); stop {
			return out, true
		}
	}
	return outcome{}, false
}

// settleOwed tries the instances owed to the unit once more, after the
// write that made them owed, and records each answer as settling it; one
// left unaccounted again is refused as peer_unaccounted.
func (u *unitRun) settleOwed(peer *stowPeer, rec *recorder, owed []*item) (outcome, bool) {
	for len(owed) > 0 {
		n := min(len(owed), u.row.STOW.MaxInstancesPerRequest, MaxBatch)
		batch := owed[:n]
		var pending []*item
		for _, x := range batch {
			if x.res == nil {
				pending = append(pending, x)
			}
		}
		if len(pending) > 0 {
			if out := u.stowRequest(peer, pending); out != nil {
				return *out, true
			}
		}
		done := 0
		for done < len(batch) && batch[done].res != nil {
			rec.settle(batch[done].in, *batch[done].res)
			done++
		}
		if done == 0 {
			continue
		}
		owed = owed[done:]
		if out, stop := u.checkpoint(rec, true); stop {
			return out, true
		}
	}
	return outcome{}, false
}

// errDamaged marks the read of an instance of a request that found its
// bytes not to be what was archived.
var errDamaged = errors.New("send: an instance's bytes on the storage are not what was archived")

// stowRequest sends one request of the items given and sets the result of
// each the answer accounts for. An instance whose read finds damage cuts
// the request before its part ends: it is refused, and the instances before
// it, whose fate the cut left unknown, stay unanswered and go in the next
// request. A nil outcome lets the unit go on.
func (u *unitRun) stowRequest(peer *stowPeer, req []*item) *outcome {
	release, err := u.takeSlot()
	if err != nil {
		out := u.readFailure(err)
		return &out
	}
	defer release()
	var opened []*item
	var storageErr error
	var metOut *outcome
	// The body is bounded by its progress (stallConn): a peer that stops
	// reading is cut a response timeout after it stopped. Once the body is
	// sent, the answer, its head and its account, must come whole within
	// the response timeout, so a peer that trickles its answer cannot hold
	// the unit either.
	ctx, cancel := context.WithCancel(u.ctx)
	defer cancel()
	var answerBound *time.Timer
	defer func() {
		if answerBound != nil {
			answerBound.Stop()
		}
	}()
	_ = peer.conn.take() // A failure of an earlier request is not this one's.
	peer.answered.Store(false)
	i := 0
	next := func() (io.Reader, error) {
		for i < len(req) {
			x := req[i]
			i++
			// The instance is met as the archive holds it now, as its
			// transfer starts: deleted since the job was made, it is not
			// read; replaced, its current revision is.
			m, settled, out := u.meet(x.in)
			if out != nil {
				metOut = out
				return nil, errMet
			}
			if settled != nil {
				x.res = settled
				continue
			}
			x.in, x.rev = m.Instance, m.Revision
			if u.copies() {
				// A peer's answer names each instance by the UIDs it was
				// sent with, which a copy changes: a destination profile
				// is a DIMSE sender's only, and the instance is refused,
				// never sent as archived.
				r := refused(tenant.FailureWouldHold, 0, detailCopyStow)
				x.res = &r
				continue
			}
			st, err := openStream(ctx, u.stores, u.scan, x.in, u.bufs, u.log)
			switch {
			case errors.Is(err, errMissing):
				u.dataLoss(detailMissing)
				r := refused(tenant.FailureDamagedInStorage, 0, detailMissing)
				x.res = &r
				continue
			case errors.Is(err, checkedread.ErrMarked):
				u.dataLoss(detailDamaged)
				r := refused(tenant.FailureDamagedInStorage, 0, detailDamaged)
				x.res = &r
				continue
			case err != nil:
				storageErr = err
				return nil, err
			}
			opened = append(opened, x)
			return &partReader{st: st, x: x, r: &paced{ctx: ctx, r: st, rate: &u.sr.rate}, damaged: func(x *item) {
				u.dataLoss(detailDamaged)
				r := refused(tenant.FailureDamagedInStorage, 0, detailDamaged)
				x.res = &r
			}, failed: func(err error) { storageErr = err }}, nil
		}
		if answerBound == nil {
			answerBound = time.AfterFunc(u.row.ResponseTimeout, cancel)
		}
		return nil, io.EOF
	}
	result, err := peer.client.StoreStream(ctx, "", next)
	switch {
	case metOut != nil:
		// The archive's state could not be read, or the unit is ending:
		// the instances not answered stay unanswered.
		return metOut
	case len(opened) == 0:
		// Nothing was sent: every instance asked for was missing.
		if storageErr != nil {
			out := u.readFailure(storageErr)
			return &out
		}
		return nil
	case errors.Is(err, errDamaged):
		// The request was cut at the damaged instance; the others are
		// sent again.
		return nil
	case storageErr != nil:
		out := u.readFailure(storageErr)
		return &out
	case u.ctx.Err() != nil:
		return &outcome{stopped: true}
	}
	out := u.answer(ctx, peer, opened, result, err)
	for _, x := range opened {
		if x.res != nil && (x.res.kind == resultSent || x.res.kind == resultWarned) {
			x.res.superseded = u.supersededByReplace(x.in.SOPInstanceUID, x.rev)
		}
	}
	return out
}

// errMet ends a request whose next instance's state could not be read.
var errMet = errors.New("send: the state of an instance could not be read")

// answer sets the result of each instance of a request the peer answered,
// as StowSender says (PS3.18 10.5.3), or returns the outcome of a request
// that failed whole.
//
// A request that failed short of an HTTP status is told apart in this
// order: a connection the dial recorded as failed (the client may hand back
// only a closed pipe for it), counted toward the sender's health; an answer
// that came and could not be read (past MaxStowAnswer, or not the account
// PS3.18 10.5.3 describes), counted as a wrong answer; the request's
// deadline; anything else, a connection lost midway.
func (u *unitRun) answer(ctx context.Context, peer *stowPeer, opened []*item, res *dicomweb.StoreResult, err error) *outcome {
	var se *dicomweb.StatusError
	if err != nil && !errors.As(err, &se) {
		if ce := peer.conn.take(); ce != nil {
			out := u.connectFailed(ce)
			return &out
		}
		if ce := transportFailure(err); isDialFailure(err) || ce.fault == tenant.SenderFaultTLSFailed {
			out := u.connectFailed(ce)
			return &out
		}
		if peer.answered.Load() && ctx.Err() == nil {
			out := u.connectFailed(&connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer, err: err})
			return &out
		}
		reason := reasonLost
		var ne net.Error
		if ctx.Err() != nil || (errors.As(err, &ne) && ne.Timeout()) {
			reason = reasonTimeout
		}
		u.log.Info("sender: a STOW-RS request got no answer", "reason", reason)
		return &outcome{temporary: true, reason: reason}
	}
	if se != nil {
		if ce := statusFailure(se.StatusCode); ce != nil {
			out := u.connectFailed(ce)
			return &out
		}
		if se.StatusCode == http.StatusRequestTimeout || se.StatusCode == http.StatusTooManyRequests {
			u.connected()
			return &outcome{temporary: true, reason: reasonPeerFailing}
		}
	}
	u.connected()
	if se != nil && (res == nil || len(res.Stored)+len(res.Failed) == 0) {
		// Refused whole, with no account of each instance.
		for _, x := range opened {
			r := refused(tenant.FailurePeerRefused, int32(se.StatusCode), detailWholeRequest) // #nosec G115 -- an HTTP status.
			x.res = &r
		}
		return nil
	}
	stored := map[string]dicomweb.StoredInstance{}
	failed := map[string]dicomweb.FailedInstance{}
	if res != nil {
		for _, s := range res.Stored {
			stored[strings.TrimSpace(s.SOPInstanceUID)] = s
		}
		for _, f := range res.Failed {
			failed[strings.TrimSpace(f.SOPInstanceUID)] = f
		}
	}
	for _, x := range opened {
		var r result
		if s, ok := stored[x.in.SOPInstanceUID]; ok {
			if s.WarningReason != 0 {
				r = warned(int32(s.WarningReason))
			} else {
				r = sent()
			}
			r.sopClass = safeClass(s.SOPClassUID)
		} else if f, ok := failed[x.in.SOPInstanceUID]; ok {
			detail := detailRefused
			if f.FailureReason == 0 {
				detail = detailWithoutStatus
			}
			r = refused(tenant.FailurePeerRefused, int32(f.FailureReason), detail)
			r.sopClass = safeClass(f.SOPClassUID)
		} else {
			r = owed()
		}
		x.res = &r
	}
	return nil
}

// safeClass returns a SOP Class UID a peer named, when it is a UID a
// failure may record, and empty otherwise: the peer's text is never kept
// when it is not one.
func safeClass(uid string) string {
	if uid = strings.TrimSpace(uid); validUID(uid) {
		return uid
	}
	return ""
}

// isDialFailure reports whether err is a connection that was never made.
func isDialFailure(err error) bool {
	var ce *connectError
	if errors.As(err, &ce) {
		return true
	}
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "dial"
}

// partReader is one instance's part of a request: its checked bytes, and,
// when the read ends early, the instance marked damaged (the request is
// cut with errDamaged) or the storage's failure kept.
type partReader struct {
	st      *stream
	x       *item
	r       io.Reader
	damaged func(*item)
	failed  func(error)
	ended   bool
}

// Read reads the instance's bytes.
func (p *partReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if err != nil && err != io.EOF && !p.ended {
		p.ended = true
		_ = p.st.Close()
		if p.st.damaged() {
			p.damaged(p.x)
			return n, errDamaged
		}
		if fe := p.st.failed(); fe != nil {
			p.failed(fe)
			return n, fault.Wrap(fault.Unavailable, fe, "the storage did not answer")
		}
	}
	return n, err
}

// Close stops the read of the instance.
func (p *partReader) Close() error { return p.st.Close() }
