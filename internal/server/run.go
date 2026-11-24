package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"connectrpc.com/grpchealth/v2"

	"github.com/amrshadid/nopacs/internal/fault"
)

// Start listens on the local socket and, when asked, on the network, and
// serves until Stop. It returns once every listener is open, or the first
// error, having closed what it opened.
func (s *Server) Start() error {
	if s.started {
		return fault.New(fault.Internal, "server: started twice")
	}
	s.started = true
	type opened struct {
		ln    net.Listener
		srv   *http.Server
		tlsOn bool
	}
	var all []opened
	closeAll := func() {
		for _, o := range all {
			_ = o.ln.Close()
		}
	}
	if !s.cfg.NoSocket {
		ln, err := listenSocket(s.cfg.DataDir)
		if err != nil {
			return err
		}
		pl := &peerListener{Listener: ln, self: s.socketUser, peerUser: peerUser, log: s.log}
		srv := s.newHTTPServer(false)
		srv.ConnContext = connContext
		all = append(all, opened{ln: pl, srv: srv})
		s.log.Info("server: listening on the local socket", "path", SocketPath(s.cfg.DataDir), "peer_check", peerCheck, "directory", dirGuard)
	}
	if s.cfg.Listen != "" {
		ln, err := net.Listen("tcp", s.cfg.Listen)
		if err != nil {
			closeAll()
			return fault.Wrap(fault.Unavailable, err, "server: cannot listen on "+s.cfg.Listen+": "+err.Error())
		}
		ln = stallListener{newLimitListener(ln, maxNetworkConns())}
		srv := s.newHTTPServer(s.certs != nil)
		if s.certs != nil {
			// ServeTLS adds the ALPN protocols that srv.Protocols allows (h2,
			// http/1.1) to a copy of this configuration.
			srv.TLSConfig = tlsConfig(s.certs)
		}
		all = append(all, opened{ln: ln, srv: srv, tlsOn: s.certs != nil})
		s.netAddr = ln.Addr()
		s.log.Info("server: listening on the network", "address", ln.Addr().String(), "tls", s.certs != nil, "max_connections", maxNetworkConns())
	}
	for _, o := range all {
		s.servers = append(s.servers, o.srv)
		s.serving.Add(1)
		go func() {
			defer s.serving.Done()
			var err error
			if o.tlsOn {
				err = o.srv.ServeTLS(o.ln, "", "")
			} else {
				err = o.srv.Serve(o.ln)
			}
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				s.serveErrs <- err
			}
		}()
	}
	return nil
}

// newHTTPServer returns an http.Server for one listener with the limits
// above. The local socket and a loopback listener without TLS speak HTTP/2
// without TLS (h2c, with prior knowledge, which gRPC clients use) and
// HTTP/1.1; the standard library has done h2c since Go 1.24
// (http.Protocols.SetUnencryptedHTTP2), so golang.org/x/net is not needed.
// A TLS listener speaks HTTP/2 and HTTP/1.1. There is no read or write
// timeout for a whole request: it would cut a long stream; the deadline of
// each call does that job.
func (s *Server) newHTTPServer(tlsOn bool) *http.Server {
	var p http.Protocols
	p.SetHTTP1(true)
	if tlsOn {
		p.SetHTTP2(true)
	} else {
		p.SetUnencryptedHTTP2(true)
	}
	return &http.Server{
		Handler:           s.handler,
		Protocols:         &p,
		MaxHeaderBytes:    maxHeaderBytes,
		ReadHeaderTimeout: readHeaderTimeout,
		IdleTimeout:       idleTimeout,
		HTTP2: &http.HTTP2Config{
			MaxConcurrentStreams: maxConcurrentStreams,
			PingTimeout:          pingTimeout,
		},
		// "OPTIONS *" is passed to the handler, so it meets the front step
		// like every other request instead of net/http's own answer.
		DisableGeneralOptionsHandler: true,
		BaseContext:                  func(net.Listener) context.Context { return s.baseCtx },
		ErrorLog:                     slog.NewLogLogger(s.log.Handler(), slog.LevelWarn),
	}
}

// Addr returns the network listener's address once Start has opened it,
// or nil; a Listen with port 0 learns its port here.
func (s *Server) Addr() net.Addr { return s.netAddr }

// Errors returns a channel that receives an error when a listener stops
// serving by itself, which a caller treats like a stop signal.
func (s *Server) Errors() <-chan error { return s.serveErrs }

// Stop stops the server and closes what it was given, in order:
//
//  1. new requests are refused with UNAVAILABLE, and the health service's
//     Check, still answered, says NOT_SERVING;
//  2. the listeners close and the requests in flight have until the drain
//     deadline to finish; then their contexts are canceled and their
//     connections closed, and their handlers have the grace to return;
//  3. a handler still running then (one that ignores its context) is given
//     up on: Stop goes on and its error says how many were abandoned;
//  4. the access package is closed, which writes the audit entries it
//     holds; then the tenant registry, then the master database, each
//     within its own close deadline, enforced even if it ignores its
//     context;
//  5. the lock on the data directory is released.
//
// The whole of Stop is bounded: drain + grace + 3 x close, 29 seconds with
// the defaults. Every step runs even when one before it failed; the errors
// are joined. Stop may be called more than once; later calls return the
// first result.
func (s *Server) Stop() error {
	s.stopOnce.Do(func() { s.stopErr = s.stop() })
	return s.stopErr
}

// stop does Stop's work, once.
func (s *Server) stop() error {
	s.stopMu.Lock()
	s.stopping = true
	s.stopMu.Unlock()
	s.health.SetStatus("", grpchealth.StatusNotServing)

	drain := orDefault(s.cfg.Drain, DefaultDrain)
	grace := orDefault(s.cfg.Grace, StopGrace)
	closeWithin := orDefault(s.cfg.Close, DefaultClose)
	var errs []error

	drainEnd := time.Now().Add(drain)
	ctx, cancel := context.WithDeadline(context.Background(), drainEnd)
	var wg sync.WaitGroup
	for _, srv := range s.servers {
		wg.Go(func() {
			if err := srv.Shutdown(ctx); err != nil {
				// The deadline passed with requests still running: end them.
				s.baseCancel()
				_ = srv.Close()
			}
		})
	}
	wg.Wait()
	cancel()
	drained := make(chan struct{})
	go func() {
		s.inFlight.Wait()
		close(drained)
	}()
	select {
	case <-drained:
	case <-time.After(time.Until(drainEnd)):
	}
	s.baseCancel()
	select {
	case <-drained:
	case <-time.After(grace):
		// A handler that ignores its context would hold Stop, and with it
		// the audit writer, the registry, the master and the lock, for ever.
		// It is given up on; its goroutine stays until the process ends.
		n := s.inFlightN.Load()
		s.log.Error("server: requests did not end after the drain and the grace and were abandoned", "abandoned", n)
		errs = append(errs, fault.New(fault.Internal, fmt.Sprintf("server: %d requests did not end after the drain and the grace and were abandoned", n)))
	}
	s.serving.Wait()

	// Each part gets its own close deadline, so one that uses all of its
	// time does not take the next one's: the access package first (its
	// audit writer may still write to a tenant's log through the
	// registry), then the registry, then the master both of them use. A
	// close that does not return within its deadline, context or not, is
	// given up on in the same way.
	closeEach := func(what string, close func(context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), closeWithin)
		defer cancel()
		done := make(chan error, 1)
		go func() { done <- close(ctx) }()
		var err error
		select {
		case err = <-done:
		case <-time.After(closeWithin + closeWithin/4):
			err = fault.New(fault.Internal, "it did not return within its close deadline and was abandoned")
		}
		if err != nil {
			s.log.Error("server: closing "+what+" failed", "error", err)
			errs = append(errs, fault.Wrap(fault.Internal, err, "server: closing "+what))
		}
	}
	closeEach("the access check and its audit writer", s.access.Close)
	if s.cfg.AfterDrain != nil {
		closeEach("the last step after the drain", s.cfg.AfterDrain)
	}
	if s.cfg.Registry != nil {
		closeEach("the tenant registry", s.cfg.Registry.Close)
	}
	if s.cfg.Master != nil {
		closeEach("the master database", func(context.Context) error { return s.cfg.Master.Close() })
	}
	if err := s.cfg.Lock.Release(); err != nil {
		errs = append(errs, fault.Wrap(fault.Internal, err, "server: releasing the lock"))
	}
	a := s.access.AuditStats()
	s.log.Info("server: stopped", "audit_written", a.Written, "audit_failed", a.Failed,
		"audit_dropped_refusals", a.DroppedRefusals, "audit_dropped_records", a.DroppedRecords,
		"audit_dropped_writes", a.DroppedWrites, "audit_dropped", a.Dropped())
	return errors.Join(errs...)
}
