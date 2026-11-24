package server

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"runtime/debug"
	"slices"
	"strconv"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/fault"
)

// RequestIDHeader is the response header that carries the request's id
// (access.proto, "The request id"). A request header of the same name is
// deleted unread: the client cannot choose the id.
const RequestIDHeader = "Nopacs-Request-Id"

// The headers by which a client gives its own deadline: gRPC and gRPC-Web
// send grpc-timeout, the Connect protocol connect-timeout-ms. The RPC
// library applies either, inside the maximum; the envelope applies the
// default only when neither is there.
const (
	grpcTimeoutHeader    = "Grpc-Timeout"
	connectTimeoutHeader = "Connect-Timeout-Ms"
)

// isAbort reports whether a recovered panic is net/http's sentinel for
// aborting a response, which is let through every recover so net/http
// sees it and closes the connection quietly.
func isAbort(p any) bool {
	err, ok := p.(error)
	return ok && errors.Is(err, http.ErrAbortHandler)
}

// requestIDKey is the context key of a request's id.
type requestIDKey struct{}

// withRequestID returns ctx carrying a request id.
func withRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// requestIDFrom returns the request id ctx carries.
func requestIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(requestIDKey{}).(string)
	return id, ok && id != ""
}

// idEncoding writes a request id in lowercase base32 without padding: only
// letters and digits, so it is safe in a header, a log and an audit entry.
var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// newRequestID returns a new request id: 80 random bits, 16 characters. The
// contract asks for uniqueness, not secrecy; at 80 bits two ids collide
// once in about 2^40 requests, beyond what an installation serves.
func newRequestID() string {
	var b [10]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails (Go 1.24 and later).
	return idEncoding.EncodeToString(b[:])
}

// NewID returns a new id for an audit entry, of the form access.Config's
// NewID needs: 26 characters of a-z and 2-7, 128 random bits, so ids made by
// any number of servers on one master do not collide.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return idEncoding.EncodeToString(b[:])
}

// envelope is the outermost handler of every listener: it runs for every
// request before the front step and any route, so what it does holds for
// the front step's refusals and for answers the RPC library gives before the
// request path runs (an unsupported content type, a method other than POST,
// an unknown compression, a malformed timeout header) as much as for any
// route. In order:
//
//   - it deletes a client's own Nopacs-Request-Id, makes the request's id
//     and sets it on the response, so every answer carries it, success,
//     refusal, error or panic alike;
//   - it sets the CORS headers for a page whose origin the installation
//     allows, and Vary: Origin on every answer (cors.go);
//   - it refuses new work once the server is stopping, with UNAVAILABLE,
//     except the health service's Check, which answers NOT_SERVING then;
//   - it counts the request in flight, for the drain on stop;
//   - it answers a CORS preflight from an allowed origin, which goes no
//     further; one from any other origin goes on, and is refused for its
//     missing key like any request;
//   - it bounds the request's time: the client's deadline when it sent one,
//     otherwise the default, and never more than the maximum; a long
//     method (Config.LongMethods) has none of these, and its response is
//     bound by the write stall timeout instead;
//   - it bounds the time to read the request's body until the check has
//     allowed the request (BodyReadTimeout);
//   - it recovers a panic that escaped the request path (a handler's panic
//     is recovered inside it, in pass), logs it with the id and a stack, and
//     answers INTERNAL;
//   - it logs and counts an answer neither the front step nor the request
//     path did (one the RPC library gave before the path ran), so every
//     answer that carries a request id has a log line with that id and a
//     count in the metrics.
func (s *Server) envelope(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		r.Header.Del(RequestIDHeader)
		id := newRequestID()
		w.Header().Set(RequestIDHeader, id)
		// Before anything can answer, so every answer to an allowed page,
		// a refusal or a stop included, is one its page can read.
		corsAllowed := s.cors.decorate(w.Header(), r)
		mark := &pathMark{}
		// "PRI * HTTP/2.0" sent on an HTTP/1 connection (the HTTP/2 preface
		// where none is spoken) names HTTP/2 but is answered over HTTP/1.
		sw := &statusWriter{ResponseWriter: w, mark: mark, http1: r.ProtoMajor == 1 || r.Method == "PRI"}
		defer s.logTransport(r, sw, mark, id, start)

		if !s.admit(r) {
			s.writeAnyError(sw, r, connect.NewError(connect.CodeUnavailable, "the server is stopping"))
			return
		}
		defer s.leave()
		end := s.metrics.Begin()
		defer end()
		if corsAllowed && isPreflight(r) {
			answerPreflight(sw)
			return
		}

		ctx := context.WithValue(withRequestID(r.Context(), id), pathMarkKey{}, mark)
		if s.isLong(r) {
			// A long method has no deadline of its own (a subject's instances
			// may take hours); a client that stops reading is cut by the
			// write stall bound instead.
			sw.stall = s.stall
		} else {
			// The maximum always, so a client cannot hold a handler, a
			// connection and a goroutine longer by asking for a longer
			// deadline; the RPC library applies a shorter one the client sends
			// inside it.
			var cancelMax context.CancelFunc
			ctx, cancelMax = context.WithTimeout(ctx, s.maxDeadline)
			defer cancelMax()
			if r.Header.Get(grpcTimeoutHeader) == "" && r.Header.Get(connectTimeoutHeader) == "" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, s.deadline)
				defer cancel()
			}
		}
		// The body must arrive within the read bound until the check has let
		// the request through; pass lifts the bound then. A refused request's
		// body is never waited for longer.
		rc := http.NewResponseController(w)
		bound := readBound{end: time.Now().Add(s.bodyRead), lift: func() { _ = rc.SetReadDeadline(time.Time{}) }}
		_ = rc.SetReadDeadline(bound.end)
		ctx = context.WithValue(ctx, readBoundKey{}, bound)

		defer func() {
			if p := recover(); p != nil {
				if isAbort(p) {
					// The sentinel itself: net/http closes the connection
					// quietly only for it, not for a panic that wraps it.
					panic(http.ErrAbortHandler)
				}
				attrs := append([]slog.Attr{
					slog.String("request_id", id),
					slog.String("path", s.loggedPath(r)),
					slog.String("stack", string(debug.Stack())),
				}, panicAttrs(p)...)
				s.log.LogAttrs(ctx, slog.LevelError, "panic outside a handler", attrs...)
				s.writeAnyError(sw, r, connect.NewError(connect.CodeInternal, "internal error"))
			}
		}()
		next.ServeHTTP(sw, r.WithContext(ctx))
	})
}

// readBound is the bound on reading a request's body before the check:
// when it ends, and how to lift it once the check has allowed the request.
type readBound struct {
	end  time.Time
	lift func()
}

// readBoundKey is the context key of a request's readBound.
type readBoundKey struct{}

// liftReadBound lifts the read bound the envelope set on the request's body.
func liftReadBound(ctx context.Context) {
	if b, ok := ctx.Value(readBoundKey{}).(readBound); ok {
		b.lift()
	}
}

// readBoundPassed reports whether the read bound of ctx's request has run
// out: a read that failed then failed because of it.
func readBoundPassed(ctx context.Context) bool {
	b, ok := ctx.Value(readBoundKey{}).(readBound)
	return ok && !time.Now().Before(b.end)
}

// pathMark records what the request path learned of the request: that it
// logged and counted it (the front step and the router mark it too, for the
// answers they give), so the envelope does not do it a second time and the
// front step knows a check, or an answer of its own, took the request up;
// and that its caller was allowed, so its HTTP/1.1 connection may stay open
// for the next request.
type pathMark struct {
	done atomic.Bool
	keep atomic.Bool
	// audited records that the request's one audit entry was written, or
	// that the request is one deliberately not audited, so the front step
	// writes none of its own.
	audited atomic.Bool
}

// markAudited records, for the front step, that the request's audit entry
// is accounted for.
func markAudited(ctx context.Context) {
	if m, ok := ctx.Value(pathMarkKey{}).(*pathMark); ok {
		m.audited.Store(true)
	}
}

// pathMarkKey is the context key of a request's pathMark.
type pathMarkKey struct{}

// markPath records, for the envelope, that the request path has logged the
// request.
func markPath(ctx context.Context) {
	if m, ok := ctx.Value(pathMarkKey{}).(*pathMark); ok {
		m.done.Store(true)
	}
}

// markAuthenticated records, for the envelope, that the request's caller
// was authenticated (a key, or the local operator), so its connection may be
// kept for another request.
func markAuthenticated(ctx context.Context) {
	if m, ok := ctx.Value(pathMarkKey{}).(*pathMark); ok {
		m.keep.Store(true)
	}
}

// transportMethod is the method label of an answer given before the request
// path ran: one fixed label, whatever the path asked for, so a caller cannot
// make metric series without bound.
const transportMethod = "transport"

// maxLoggedPath bounds a path or a procedure as the log writes it: the
// audit entry holds 256 bytes of it too, and a 64 KiB path in every log
// line would let a caller fill the log.
const maxLoggedPath = 256

// logTransport logs and counts an answer the request path did not handle:
// one the RPC library or net/http gave before the path ran, or one the
// envelope gave itself. Its code is the HTTP status, from the fixed set of
// statuses.
func (s *Server) logTransport(r *http.Request, sw *statusWriter, mark *pathMark, id string, start time.Time) {
	if mark.done.Load() {
		return
	}
	status := sw.Status()
	took := time.Since(start)
	code := "http_" + strconv.Itoa(status)
	s.metrics.Observe(transportMethod, code, took)
	attrs := []slog.Attr{
		slog.String("request_id", id),
		slog.String("method", transportMethod),
		slog.String("path", s.loggedPath(r)),
		slog.Int("http_status", status),
		slog.String("code", code),
		slog.Float64("duration_ms", float64(took.Microseconds())/1000),
	}
	if _, local := localFrom(r.Context()); !local {
		attrs = append(attrs, slog.String("remote", r.RemoteAddr))
	}
	s.log.LogAttrs(r.Context(), slog.LevelInfo, "request", attrs...)
}

// statusWriter is the response as the envelope sees it: it records the
// status written, so an answer the request path did not handle can be
// logged with it; and it closes an HTTP/1.1 connection after an answer to a
// caller that was never allowed by a check (a refusal, the public health
// check, an answer the RPC library gave before the check), so a caller with no key
// cannot keep idle connections, and the file descriptors they cost, for the
// idle timeout. Flush and the response controller's calls pass through to
// the connection's writer, which streams and read deadlines need.
type statusWriter struct {
	http.ResponseWriter
	mark   *pathMark
	http1  bool
	status atomic.Int64
	// stall, when set, is how long one Write or Flush may take: the write
	// deadline is moved to now plus stall before each, so a client that
	// reads nothing ends the response instead of holding it.
	stall time.Duration
}

// armStall moves the write deadline for a long method's next write.
func (w *statusWriter) armStall() {
	if w.stall > 0 {
		_ = http.NewResponseController(w.ResponseWriter).SetWriteDeadline(time.Now().Add(w.stall))
	}
}

// beforeHeaders runs once, before the response's headers are sent.
func (w *statusWriter) beforeHeaders(code int) {
	if !w.status.CompareAndSwap(0, int64(code)) {
		return
	}
	if w.http1 && !w.mark.keep.Load() {
		w.ResponseWriter.Header().Set("Connection", "close")
	}
}

// WriteHeader records the status and writes it.
func (w *statusWriter) WriteHeader(code int) {
	w.beforeHeaders(code)
	w.armStall()
	w.ResponseWriter.WriteHeader(code)
}

// Write records the implied 200 when no status was written, and writes.
func (w *statusWriter) Write(b []byte) (int, error) {
	w.beforeHeaders(http.StatusOK)
	w.armStall()
	return w.ResponseWriter.Write(b)
}

// ReadFrom records the implied 200 when no status was written, and copies
// src to the connection through the connection's own ReadFrom when it has
// one, so a file reaches the socket by the system's copy (net/http passes
// it to sendfile on a plain HTTP/1.1 connection) instead of through this
// process. Without this method io.Copy would see only Write.
func (w *statusWriter) ReadFrom(src io.Reader) (int64, error) {
	w.beforeHeaders(http.StatusOK)
	w.armStall()
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(src)
	}
	return io.Copy(struct{ io.Writer }{w.ResponseWriter}, src)
}

// Flush sends what is buffered, the headers first when none were sent.
func (w *statusWriter) Flush() {
	w.beforeHeaders(http.StatusOK)
	w.armStall()
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap returns the connection's writer, for http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Status returns the status written, or 200 when none was: net/http writes
// 200 for a handler that wrote nothing.
func (w *statusWriter) Status() int {
	if s := w.status.Load(); s != 0 {
		return int(s)
	}
	return http.StatusOK
}

// admit counts a request in flight unless the server is stopping. While it
// stops, only the health service's Check is let in, so a load balancer sees
// NOT_SERVING and takes the server out of its pool. The check and the count
// are under one lock with Stop's switch, so no other request is admitted
// after Stop has begun waiting for those in flight.
func (s *Server) admit(r *http.Request) bool {
	s.stopMu.RLock()
	defer s.stopMu.RUnlock()
	if s.stopping && !isPath(r, HealthCheckProcedure) {
		return false
	}
	s.inFlight.Add(1)
	s.inFlightN.Add(1)
	return true
}

// leave ends an admitted request's count.
func (s *Server) leave() {
	s.inFlightN.Add(-1)
	s.inFlight.Done()
}

// cut returns s cut to at most n bytes, at a character's boundary.
func cut(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// isLong reports whether r is a call of a long method: a procedure named by
// its exact path, or a request a long pattern route serves.
func (s *Server) isLong(r *http.Request) bool {
	if len(s.longProcs) == 0 && len(s.longPatterns) == 0 {
		return false
	}
	if s.longProcs[r.URL.Path] {
		return true
	}
	if len(s.longPatterns) == 0 {
		return false
	}
	m, ok := s.matchPattern(r)
	return ok && s.longPatterns[m.route.Pattern]
}

// setLongMethods reads Config.LongMethods against the routes built, and
// refuses a name that is neither a served procedure nor a pattern route.
func (s *Server) setLongMethods() error {
	s.longProcs, s.longPatterns = map[string]bool{}, map[string]bool{}
	for _, name := range s.cfg.LongMethods {
		switch {
		case s.procs[name] != nil:
			s.longProcs[name] = true
		case slices.ContainsFunc(s.patterns, func(p *patternRoute) bool { return p.Pattern == name }):
			s.longPatterns[name] = true
		default:
			return fault.New(fault.Internal, "server: the long method "+name+" is neither a served procedure nor a pattern route")
		}
	}
	return nil
}
