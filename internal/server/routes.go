package server

import (
	"net/http"
	"slices"
	"sync"
)

// routeMux is the server's http.ServeMux with a record of every pattern
// registered on it. Only two things register on it, both in this package
// and both through the request path: connecthttp.Mount of the one
// connect.Server built with the interceptor (buildRoutes), and handleHTTP,
// which wraps a plain route in the middleware. The record lets the route
// test send an unauthenticated request to every pattern there is and fail
// on any that answers without refusing.
type routeMux struct {
	mux *http.ServeMux

	mu       sync.Mutex
	patterns []string
}

// newRouteMux returns an empty routeMux.
func newRouteMux() *routeMux { return &routeMux{mux: http.NewServeMux()} }

// Handle registers h for pattern and records the pattern. It satisfies
// connecthttp.ServeMux.
func (m *routeMux) Handle(pattern string, h http.Handler) {
	m.mux.Handle(pattern, h)
	m.mu.Lock()
	m.patterns = append(m.patterns, pattern)
	m.mu.Unlock()
}

// ServeHTTP serves a request with the route that matches it.
func (m *routeMux) ServeHTTP(w http.ResponseWriter, r *http.Request) { m.mux.ServeHTTP(w, r) }

// Patterns returns every pattern registered, in order.
func (m *routeMux) Patterns() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.patterns)
}

// Routes returns every pattern registered on the server, for the route
// test and for a person checking what a server serves.
func (s *Server) Routes() []string { return s.mux.Patterns() }
