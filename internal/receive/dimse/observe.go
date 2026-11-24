package dimse

import (
	"net"
	"strings"

	"github.com/amrshadid/go-dicom/network"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// observe reports the refusals the library makes outside admission, which
// only its events show:
//
//   - an association accepted with every presentation context rejected
//     (PS3.8 9.3.3.2: abstract syntax or transfer syntaxes not supported),
//     on which nothing can be sent: no_acceptable_context;
//   - an A-ABORT this side sent (PS3.8 9.3.8) because the caller broke the
//     protocol or a bound, or went quiet past the timers of PS3.8 9.1.5:
//     protocol_error. An abort for any other cause is the server's own
//     doing and no fault of the caller's: a handler that panicked is logged
//     as the server's failure, and one made by stopping is not reported;
//   - a TLS handshake that failed before any association was asked for:
//     tls_failed, with the caller's address and no AE titles.
//
// The library calls the handlers on the connection's goroutine; each only
// builds the caller and hands it to Report, which does not block.
func (l *Listener) observe(events *network.EventManager) {
	events.On(network.EVTAssocAccepted, func(e *network.Event) {
		if e.Role != "SCP" {
			return
		}
		for _, c := range e.PresentationContexts() {
			if c.Result == network.PCResultAcceptance {
				return
			}
		}
		l.cfg.Report(tenant.FailureNoAcceptableContext, eventCaller(e))
	})
	events.On(network.EVTPDUSent, func(e *network.Event) {
		if e.PDUType != network.PDUTypeAbort || e.Role != "SCP" || e.LocalAbort == nil {
			return
		}
		switch e.LocalAbort.Cause {
		case network.AbortCauseProtocolViolation, network.AbortCauseTimeout:
			l.cfg.Report(tenant.FailureProtocolError, eventCaller(e))
		case network.AbortCauseHandlerPanic:
			// The phrase only: the panic's text may hold what a file said.
			l.log.Error("receiver: an association was aborted because the server failed while serving it")
		}
	})
	events.On(network.EVTTLSHandshakeFailed, func(e *network.Event) {
		l.cfg.Report(tenant.FailureTLSFailed, eventCaller(e))
	})
}

// eventCaller returns who called, from an event of the association: the AE
// titles when well formed, and the host's address without its port.
func eventCaller(e *network.Event) tenant.GroupCaller {
	c := tenant.GroupCaller{}
	if ae := strings.TrimSpace(e.CallingAE); tenant.ValidateAETitle(ae) == nil {
		c.CallingAETitle = ae
	}
	if ae := strings.TrimSpace(e.CalledAE); tenant.ValidateAETitle(ae) == nil {
		c.CalledAETitle = ae
	}
	if host, _, err := net.SplitHostPort(e.RemoteAddr); err == nil {
		if addr, _ := hostOf(&net.TCPAddr{IP: net.ParseIP(host)}); addr.IsValid() {
			c.Address = addr.String()
		}
	}
	return c
}
