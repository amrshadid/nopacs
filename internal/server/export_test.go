package server

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"

	"github.com/amrshadid/nopacs/internal/access"
)

// HandleUnchecked registers h on the server's mux without the request
// path: the route the route test must catch. Only tests can do this.
func (s *Server) HandleUnchecked(pattern string, h http.Handler) { s.mux.Handle(pattern, h) }

// MountUnchecked mounts a second connect.Server built without the
// interceptor: the other way a route could skip the check.
func (s *Server) MountUnchecked(register func(*connect.Server)) {
	rpc := connect.NewServer()
	register(rpc)
	connecthttp.Mount(s.mux, rpc)
}

// CredentialOf is the credential the request path reads from an
// Authorization header on a network connection.
func CredentialOf(authorization string) access.Credential {
	return credential(authorization, localPeer{}, false)
}

// SetSocketUser replaces the user a local socket peer must be, so a test
// run by one user can show that a peer who is not that user is refused.
func (s *Server) SetSocketUser(u string) { s.socketUser = u }

// SetHook sets a function the server calls at the points of a request named
// by the point constants ("front", "checked", "receive", "handled"), so a test can stop a request at
// each. It must be set before Start.
func (s *Server) SetHook(h func(ctx context.Context, point string)) { s.hook = h }

// SetGuardClock replaces the clock the limits on failed calls read and the
// time a slowed refusal is held, so a test moves past a window or a block
// without sleeping. It must be set before Start.
func (s *Server) SetGuardClock(now func() time.Time, delay time.Duration) {
	s.guard.now, s.guard.delay = now, delay
}

// GuardHolds is how many refusals the server has held for the failure
// delay since it started.
func (s *Server) GuardHolds() int64 { return s.guard.held.Load() }

// SetStallTimeout replaces the write stall bound of long methods, so a test
// of a client that stops reading does not wait minutes. It must be set
// before Start.
func (s *Server) SetStallTimeout(d time.Duration) { s.stall = d }

// GuardRules returns the front step's reading of the access settings, as a
// server made with read and the clock now holds it, so a test can call it
// as the requests do: with their contexts, at the same time.
func GuardRules(read func(ctx context.Context) (AccessRules, error), now func() time.Time) func(ctx context.Context) (AccessRules, bool) {
	g := newGuard(read, slog.New(slog.DiscardHandler))
	g.now = now
	return g.current
}

// SetCertClock replaces the clock by which the network listener times its
// next look at its certificate files, so a test moves past the recheck
// without sleeping. It must be set before Start; it does nothing without
// TLS.
func (s *Server) SetCertClock(now func() time.Time) {
	if s.certs != nil {
		s.certs.now = now
	}
}
