package send

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"time"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The steps of a test of a sender, as the contract's SenderTestStep in
// lowercase without its prefix, in the order they run.
const (
	StepResolve   = "resolve"
	StepConnect   = "connect"
	StepTLS       = "tls"
	StepAssociate = "associate"
	StepRequest   = "request"
	StepRelease   = "release"
)

// The outcomes of a step, as the contract's SenderTestOutcome in lowercase
// without its prefix.
const (
	OutcomePassed  = "passed"
	OutcomeFailed  = "failed"
	OutcomeSkipped = "skipped"
)

// TestStep is the outcome of one step of a test of a sender
// (SenderTestResult).
type TestStep struct {
	// Step and Outcome are the step and how it ended.
	Step, Outcome string
	// Duration is how long it took; zero for a skipped step.
	Duration time.Duration
	// Fault is why a failed step failed; empty otherwise.
	Fault tenant.SenderFault
	// Detail is a phrase from the server's fixed set: an association's
	// rejection reason (PS3.8 9.3.4), a C-ECHO's status in hexadecimal, a
	// stow request's HTTP status. Never the peer's own words.
	Detail string
}

// phraseDeadline is the detail of a step the call's deadline cut short.
const phraseDeadline = "the test's deadline passed before the step finished"

// tester runs the steps of one test in order, each skipped once one
// before it failed.
type tester struct {
	ctx    context.Context
	steps  []TestStep
	failed bool
	// note is the detail a step that passed gives, set by its function.
	note string
}

// run runs step with fn unless a step before failed, and records it.
func (t *tester) run(step string, fn func() error) bool {
	if t.failed {
		t.skip(step)
		return false
	}
	start := time.Now()
	t.note = ""
	err := fn()
	r := TestStep{Step: step, Outcome: OutcomePassed, Duration: time.Since(start), Detail: t.note}
	if err != nil {
		var ce *connectError
		switch {
		case errors.As(err, &ce):
			r.Fault, r.Detail = ce.fault, ce.detail
		default:
			r.Fault, r.Detail = tenant.SenderFaultWrongAnswer, phraseWrongAnswer
		}
		if t.ctx.Err() != nil {
			r.Fault, r.Detail = tenant.SenderFaultUnreachable, phraseDeadline
		}
		r.Outcome = OutcomeFailed
		t.failed = true
	}
	t.steps = append(t.steps, r)
	return err == nil
}

// skip records a step not tried.
func (t *tester) skip(step string) {
	t.steps = append(t.steps, TestStep{Step: step, Outcome: OutcomeSkipped})
}

// TestSender tries the peer of the tenant's sender row as a send would
// reach it, and reports each step (sender.proto, TestSender): resolve and
// check, connect, TLS, then a DIMSE association proposing the Verification
// SOP Class, a C-ECHO and the release, or a stow sender's search of its
// studies (queryStudies). It sends no instance and changes no health; the whole
// test is bounded by ctx. A failure of the server's own is an error.
func (s *Supervisor) TestSender(ctx context.Context, tenantName string, row tenant.Sender) ([]TestStep, error) {
	t := &tester{ctx: ctx}
	host, port, err := endpoint(row)
	if err != nil {
		return nil, err
	}
	var aps []netip.AddrPort
	t.run(StepResolve, func() error {
		if row.Type == tenant.SenderSTOW && isPlain(row) {
			allow, err := s.cfg.Installation.AllowPlainHTTP(ctx)
			if err != nil || !allow {
				return &connectError{fault: tenant.SenderFaultAddressNotAllowed, detail: phrasePlainHTTP, err: err}
			}
		}
		aps, err = s.resolve(ctx, host, port)
		return err
	})
	var conn net.Conn
	t.run(StepConnect, func() error {
		d := &net.Dialer{Timeout: row.ConnectTimeout}
		var last error
		for _, ap := range aps {
			if conn, last = d.DialContext(ctx, "tcp", ap.String()); last == nil {
				return nil
			}
		}
		return &connectError{fault: tenant.SenderFaultUnreachable, detail: phraseUnreachable, err: last}
	})
	tlsOf := row.DIMSE.TLS
	wantTLS := row.Type == tenant.SenderDIMSE && tlsOf != nil
	if row.Type == tenant.SenderSTOW {
		tlsOf, wantTLS = row.STOW.TLS, !isPlain(row)
	}
	if !wantTLS {
		t.skip(StepTLS)
	} else {
		t.run(StepTLS, func() error {
			cfg, err := tlsConfig(s.cfg.TLSDir(tenantName), tlsOf, host)
			if err != nil {
				return err
			}
			hctx, cancel := context.WithTimeout(ctx, row.ConnectTimeout)
			defer cancel()
			if err := tls.Client(conn, cfg).HandshakeContext(hctx); err != nil {
				phrase, ok := tlsFailure(err)
				if !ok {
					phrase = phraseTLSHandshake
				}
				return &connectError{fault: tenant.SenderFaultTLSFailed, detail: phrase, err: err}
			}
			return nil
		})
	}
	if conn != nil {
		_ = conn.Close()
	}
	if row.Type == tenant.SenderSTOW {
		t.skip(StepAssociate)
		t.run(StepRequest, func() error { return s.testStowRequest(ctx, tenantName, row, t) })
		t.skip(StepRelease)
		return t.steps, nil
	}
	s.testDIMSE(ctx, tenantName, row, t)
	return t.steps, nil
}

// testDIMSE runs the association, the C-ECHO and the release of a test of
// a DIMSE sender, its host resolved and checked again as every association
// is; the release is tried whenever an association was made.
func (s *Supervisor) testDIMSE(ctx context.Context, tenantName string, row tenant.Sender, t *tester) {
	var scu *network.SCU
	t.run(StepAssociate, func() error {
		var err error
		scu, err = s.associate(ctx, tenantName, row, []network.PresentationContextItem{{
			ID: 1, AbstractSyntax: network.VerificationSOPClassUID, TransferSyntaxes: []string{network.ImplicitVRLittleEndianUID},
		}})
		if err != nil {
			return err
		}
		if len(scu.Association().AcceptedContexts()) == 0 {
			return &connectError{fault: tenant.SenderFaultNotAccepted, detail: phraseNoContexts}
		}
		return nil
	})
	t.run(StepRequest, func() error {
		err := scu.Echo(ctx)
		var de *network.DIMSEError
		switch {
		case err == nil:
			return nil
		case errors.As(err, &de):
			return &connectError{fault: tenant.SenderFaultNotAccepted, detail: fmt.Sprintf("C-ECHO status %04X", de.Status), err: err}
		}
		return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer, err: err}
	})
	if scu == nil {
		t.skip(StepRelease)
		return
	}
	failed := t.failed
	t.failed = false
	t.run(StepRelease, func() error {
		rctx, cancel := context.WithTimeout(ctx, row.ConnectTimeout)
		defer cancel()
		if err := scu.Release(rctx); err != nil {
			_ = scu.Abort(rctx)
			return &connectError{fault: tenant.SenderFaultWrongAnswer, detail: phraseWrongAnswer, err: err}
		}
		return nil
	})
	t.failed = t.failed || failed
}

// testStowRequest sends a stow sender's service probe, a QIDO-RS search
// of its studies (queryStudies), passed only by a 2xx answer
// (probeFailure), and names the status in the step's detail: "HTTP 200",
// or the status and why it failed, such as "HTTP 404: the URL does not
// name a DICOMweb service".
func (s *Supervisor) testStowRequest(ctx context.Context, tenantName string, row tenant.Sender, t *tester) error {
	st, err := s.cfg.Tenants.Store(tenantName)
	if err != nil {
		return fault.Wrap(fault.Unavailable, err, "the tenant is not ready")
	}
	p, ok := st.(tenant.Pipeline)
	if !ok {
		return fault.New(fault.Internal, "the tenant's database keeps no senders")
	}
	// The URL and the credential as stored together, never the
	// credential of one row with the URL of another.
	stored, sealed, err := storedSender(ctx, p, row.Name)
	if err != nil {
		return err
	}
	peer, err := s.stowPeerOf(ctx, tenantName, stored, sealed)
	if err != nil {
		return err
	}
	defer peer.close()
	code, err := queryStudies(ctx, peer)
	if err != nil {
		return err
	}
	detail := "HTTP " + strconv.Itoa(code)
	if ce := probeFailure(code); ce != nil {
		ce.detail = detail + ": " + ce.detail
		return ce
	}
	t.note = detail
	return nil
}

// endpoint returns the host and port a sender connects to: a DIMSE peer's,
// or its stow URL's host with the URL's port (443 or 80 when it names
// none).
func endpoint(row tenant.Sender) (string, int, error) {
	if row.Type != tenant.SenderSTOW {
		return row.DIMSE.Host, row.DIMSE.Port, nil
	}
	u, err := url.Parse(row.STOW.URL)
	if err != nil || u.Hostname() == "" {
		return "", 0, fault.New(fault.Internal, "a stored stow sender's URL does not parse")
	}
	port := 443
	if u.Scheme == "http" {
		port = 80
	}
	if v := u.Port(); v != "" {
		if port, err = strconv.Atoi(v); err != nil {
			return "", 0, fault.New(fault.Internal, "a stored stow sender's URL has a port that is not a number")
		}
	}
	return u.Hostname(), port, nil
}

// isPlain reports whether a stow sender's URL is plain http.
func isPlain(row tenant.Sender) bool {
	u, err := url.Parse(row.STOW.URL)
	return err == nil && u.Scheme == "http"
}
