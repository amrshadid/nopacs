package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"

	"connectrpc.com/connect/v2"
	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// Call is what the request path tells a handler about the request it is
// serving. A handler normally reads only the Decision, which
// Config.WithDecision puts in its context; CallFrom gives the rest (the
// request id, the credential's kind) to a handler or a test that needs it.
type Call struct {
	// RequestID is the id the server made for the request, the one in the
	// Nopacs-Request-Id response header, the log and the audit entry.
	RequestID string
	// Method is the full procedure name, or the route of a plain HTTP
	// request.
	Method string
	// Credential is what the request presented.
	Credential access.Credential
	// Facts are the request's facts for an audit entry: its method, its
	// remote address and its request id.
	Facts access.Request
	// Decision is what the check allowed. It is the zero Decision for the
	// health service, which is not checked.
	Decision access.Decision

	// unreadable records that the handler's reading of the request's
	// message failed (replayStream), for the reason of the entry pass writes
	// when the request ends without one.
	unreadable atomic.Bool
	// cut is why the handler aborted an answer it had begun (a panic that
	// is http.ErrAbortHandler with its cause), so the request's log line
	// names that failure: pass returns no error when the abort unwinds it.
	cut error
}

// callKey is the context key of a request's Call.
type callKey struct{}

// CallFrom returns the Call of the request ctx belongs to, and false outside
// a request the server is serving.
func CallFrom(ctx context.Context) (*Call, bool) {
	c, ok := ctx.Value(callKey{}).(*Call)
	return c, ok
}

// exchange is what the request path needs to know of one request, whatever
// carried it: an RPC through the Connect interceptor or a plain HTTP request
// through the middleware.
type exchange struct {
	// method is the procedure or the route; it chooses the rule.
	method string
	// metric is the method's name as a metric label: method itself when it
	// is a known route, "unknown" otherwise, so a caller cannot make series
	// without bound.
	metric string
	// authorization is the request's Authorization header.
	authorization string
	// remote is the caller's network address; empty on the local socket.
	remote string
	// rule says what the method needs.
	rule rule
	// readMessage reads the request's first message, for a rule that needs
	// it; nil for a plain HTTP request. The caller keeps the message and
	// hands it to the handler, so it is read once.
	readMessage func() (proto.Message, error)
	// unaudited is set for the one route whose allowed requests write no
	// audit entry by design: the metrics, read every few seconds by a
	// scraper, holding counts and no tenant's content.
	unaudited bool
}

// pass is the request path, the one function every routed request goes
// through: the Connect interceptor calls it for every RPC, unary and
// streaming, and the HTTP middleware for every plain HTTP route. The front
// step has already authenticated the caller (or the method is public). In
// order:
//
//  1. the request id: the one the envelope made for this request (it is
//     already in the Nopacs-Request-Id response header), or a new one;
//  2. the credential: the one the front step accepted, so the key is read
//     once per request; were pass reached without it, the credential is
//     read from the request again (the Authorization bearer key, else the
//     local operator when the connection came through the local socket's
//     peer check, else none) and checked in full;
//  3. access.Check with the method's requirement, unless the method is the
//     public health service. For a requirement that depends on the request,
//     the first message is read first; nothing is read for any other;
//  4. a refusal is returned as it is: the access package's fixed message
//     and kind, which the caller turns into the API code;
//  5. the handler, with the Call in its context and any panic recovered.
//
// It logs one line for the request and counts it in the metrics, whatever
// the outcome. The error it returns is the handler's or the check's, not
// yet turned into an API code (see answer).
func (s *Server) pass(ctx context.Context, x exchange, next func(context.Context) error) (err error) {
	start := time.Now()
	id, ok := requestIDFrom(ctx)
	if !ok {
		id = newRequestID()
		ctx = withRequestID(ctx, id)
	}
	call := &Call{RequestID: id, Method: x.method}
	defer func() { s.finish(ctx, x, call, start, err) }()
	// pass accounts for the request's one audit entry however it ends: the
	// check writes a refusal's, the handler or the code below an allowed
	// request's; the health service and an allowed scrape write none.
	defer markAudited(ctx)

	if c, ok := identityFrom(ctx); ok {
		// The front step accepted this caller before the request was
		// routed; its key is not read again.
		call.Credential = c
	} else {
		local, isLocal := localFrom(ctx)
		call.Credential = credential(x.authorization, local, isLocal)
	}
	call.Facts = access.Request{Method: x.method, RemoteAddress: x.remote, RequestID: id}
	o, judged := originFrom(ctx)
	if judged {
		call.Facts.Local, call.Facts.ClientCert = o.local, o.clientCert
	} else if _, ok := localFrom(ctx); ok {
		call.Facts.Local = true
	}

	if !x.rule.public {
		var msg proto.Message
		if x.rule.readsMessage() && x.readMessage != nil {
			// The message may take as long as the body's read bound to
			// arrive; the key the front step accepted is read again by the
			// check that follows, so a key revoked meanwhile is refused.
			call.Credential = call.Credential.Reread()
			m, rerr := x.readMessage()
			if rerr != nil {
				// The message could not be read. The caller is still checked
				// first, against the rule's fallback, so an unauthenticated or
				// unentitled caller is refused before it learns anything
				// about its message.
				// A read that ran out of time has canceled the request's
				// context (net/http does on a failed body read), so the check
				// runs on a context of its own, bounded, or a good key would
				// read as a canceled request instead of being told its
				// message was late.
				req, facts := x.rule.fallback()
				cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), fallbackCheckTimeout)
				d, cerr := s.access.Check(cctx, call.Credential, req, withFacts(call.Facts, facts))
				cancel()
				if cerr != nil {
					// Counted as any refusal is: a malformed message must
					// not be a way around the limits on failed calls. The
					// hold outlives a context the failed read canceled.
					if wctx := context.WithoutCancel(ctx); judged && s.countFailure(wctx, o, call, cerr) {
						s.guard.wait(wctx)
					}
					return cerr
				}
				// The check allowed the caller, whose request then does
				// nothing: its one entry says why.
				late := errors.Is(rerr, os.ErrDeadlineExceeded) || readBoundPassed(ctx)
				reason := reasonUnreadable
				if late {
					reason = reasonTooSlow
				}
				s.access.Record(d, master.OutcomeFailed, reason, access.Facts{})
				if late {
					return errBodyTooSlow
				}
				return rerr
			}
			msg = m
		}
		req, facts := x.rule.need(msg)
		call.Facts = withFacts(call.Facts, facts)
		d, cerr := s.access.Check(ctx, call.Credential, req, call.Facts)
		if cerr != nil {
			if judged && s.countFailure(ctx, o, call, cerr) {
				s.guard.wait(ctx)
			}
			return cerr
		}
		call.Decision = d
		s.at(ctx, pointChecked)
		if s.withDecision != nil {
			ctx = s.withDecision(ctx, d)
		}
		// The check allowed the request: its body may now take as long as the
		// request's deadline allows, and its connection may be kept.
		liftReadBound(ctx)
		markAuthenticated(ctx)
	}

	ctx = context.WithValue(ctx, callKey{}, call)
	if cerr := ctx.Err(); cerr != nil {
		// The caller went away, or the request ran out of time, before its
		// handler started: nothing is done for it.
		err = cerr
	} else {
		err = s.run(ctx, call, next)
	}
	s.at(ctx, pointHandled)
	if !x.rule.public && !call.Decision.Entered() && (!x.unaudited || err != nil) {
		// The check allowed the request and nothing wrote its entry: the
		// handler failed, panicked or was canceled before it recorded one,
		// or succeeded without one. Its one entry is written here, so every
		// checked request has exactly one.
		outcome, reason := master.OutcomeDone, ""
		if err != nil {
			outcome, reason = master.OutcomeFailed, s.unrecordedReason(ctx, call, err)
		}
		s.access.Record(call.Decision, outcome, reason, access.Facts{})
	}
	return err
}

// countFailure counts a request the check refused: by its key when the key
// was good and lacked the grant (Denied), by its address when the key was
// no good (Unauthenticated, such as a key revoked since the front step or
// refused by a network rule). It reports whether the refusal is to be
// slowed. A request the check allowed is never counted.
func (s *Server) countFailure(ctx context.Context, o origin, call *Call, err error) bool {
	switch fault.KindOf(err) {
	case fault.Denied:
		return s.failKey(ctx, o, call.Credential.KeyID(), call.Facts)
	case fault.Unauthenticated:
		rules, ok := s.guard.current(ctx)
		return ok && s.failAddress(o, rules, call.Facts)
	}
	return false
}

// The reasons of the entries pass writes for an allowed request that ended
// without one. Each is fixed, so nothing of the request reaches the log.
const (
	reasonUnreadable = "the request's message could not be read"
	reasonTooSlow    = "the request's message did not arrive in time"
	reasonPanic      = "the handler failed (a panic) before it recorded the request"
	reasonDeadline   = "the request's deadline passed before it was recorded"
	reasonCanceled   = "the request was canceled before it was recorded"
	reasonEnded      = "the request ended before it was recorded: "
)

// unrecordedReason names why an allowed request ended without an entry. A
// request whose context has ended was canceled or ran out of time, whatever
// error that caused on the way (a read of its message cut short reads as a
// message that could not be read), so its context is asked first.
func (s *Server) unrecordedReason(ctx context.Context, call *Call, err error) string {
	switch {
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(err, context.Canceled):
		return reasonCanceled
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(err, context.DeadlineExceeded):
		return reasonDeadline
	case errors.Is(err, errPanic):
		return reasonPanic
	case call.unreadable.Load():
		return reasonUnreadable
	}
	return reasonEnded + codeName(s.code(err))
}

// fallbackCheckTimeout bounds the check made after a request's message could
// not be read: two indexed reads of the master, done in milliseconds.
const fallbackCheckTimeout = 5 * time.Second

// errBodyTooSlow answers a request whose message did not arrive within the
// read bound, once its caller has passed the check (a caller who has not is
// refused first): the request was not a well-formed call in time.
var errBodyTooSlow = connect.NewError(connect.CodeInvalidArgument, "the request's message did not arrive in time")

// withFacts adds the audit facts the table gave for a request (its object
// and details) to the facts the server knows (method, remote address,
// request id), which the table cannot change.
func withFacts(base, table access.Request) access.Request {
	base.Object = table.Object
	base.Details = table.Details
	return base
}

// run calls the handler and recovers a panic in it: the panic is logged
// with the request id and the stack, the caller is answered INTERNAL with no
// detail, and the server goes on serving.
func (s *Server) run(ctx context.Context, call *Call, next func(context.Context) error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			if isAbort(p) {
				// net/http's own way to abort a response; it must reach
				// net/http, which closes the connection quietly only for
				// the sentinel itself, so a panic that wraps it with its
				// cause leaves the cause here, for the log line.
				call.cut = errAborted
				if p != any(http.ErrAbortHandler) {
					call.cut, _ = p.(error)
				}
				panic(http.ErrAbortHandler)
			}
			attrs := append([]slog.Attr{
				slog.String("request_id", call.RequestID),
				slog.String("method", call.Method),
				slog.String("stack", string(debug.Stack())),
			}, panicAttrs(p)...)
			s.log.LogAttrs(ctx, slog.LevelError, "panic in a handler", attrs...)
			err = errPanic
		}
	}()
	return next(ctx)
}

// errPanic is the error a recovered panic becomes: Internal, with the fixed
// message, so nothing of the panic reaches the caller.
var errPanic = fault.New(fault.Internal, "internal error")

// errAborted is the failure of an answer its handler aborted after it began
// with http.ErrAbortHandler alone, which names no cause.
var errAborted = fault.New(fault.Unavailable, "the answer was cut before its last byte")

// finish logs the request's line and counts it in the metrics.
func (s *Server) finish(ctx context.Context, x exchange, call *Call, start time.Time, err error) {
	markPath(ctx)
	if err == nil && call.cut != nil {
		// The handler aborted its answer after it began: a failure, whatever
		// pass returned while the abort unwound it.
		err = call.cut
	}
	took := time.Since(start)
	code := codeName(s.code(err))
	s.metrics.Observe(x.metric, code, took)

	attrs := []slog.Attr{
		slog.String("request_id", call.RequestID),
		slog.String("method", x.method),
		slog.String("code", code),
		slog.Float64("duration_ms", float64(took.Microseconds())/1000),
	}
	if x.remote != "" {
		attrs = append(attrs, slog.String("remote", x.remote))
	}
	p := call.Decision.Principal()
	switch {
	case p.IsLocalOperator():
		attrs = append(attrs, slog.String("caller", "local:"+p.LocalUser()))
	case p.KeyID() != "":
		attrs = append(attrs, slog.String("caller", "key:"+p.KeyID()))
	}
	level := slog.LevelInfo
	if err != nil {
		attrs = append(attrs, slog.String("kind", s.kindOf(err)))
		// The cause of a fault of the server goes to the log, never to the
		// caller. A fault's Error carries its cause, which never holds a
		// secret (package fault, and the access package's rule).
		switch c := s.code(err); c {
		case connect.CodeInternal, connect.CodeUnknown, connect.CodeDataLoss, connect.CodeUnavailable:
			level = slog.LevelError
			attrs = append(attrs, slog.String("error", err.Error()))
		}
	}
	s.log.LogAttrs(ctx, level, "request", attrs...)
}

// kindOf names an error's kind for the log line: the fault kind, or the API
// code of an error the RPC library made.
func (s *Server) kindOf(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Code().String()
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return "context"
	}
	return fault.KindOf(err).String()
}

// code returns the API code the caller is answered with for err.
func (s *Server) code(err error) connect.Code {
	if err == nil {
		return 0
	}
	return s.answer(err).Code()
}

// codeName names a code as a log field and a metric label: "ok" for
// success.
func codeName(c connect.Code) string {
	if c == 0 {
		return "ok"
	}
	return c.String()
}

// answer turns an error from the request path into what the caller is sent.
// An error the RPC library made (a message over the limit, a malformed
// message, an unknown method) keeps its code and message. A deadline or a
// cancellation is DEADLINE_EXCEEDED or CANCELED. Any other error is mapped
// by its fault kind through codes.ToConnect, which shows the caller only
// the error's message for the caller (fault.Message) and never its cause.
// Every answer carries a google.rpc.ErrorInfo detail (codes.Ensure): the
// contract's reason when a handler marked one or the reason begins the
// message, and the code's general reason otherwise, the library's own
// errors among them. A plain HTTP route's JSON body carries the same
// details (writeHTTPError).
func (s *Server) answer(err error) *connect.Error {
	var ce *connect.Error
	switch {
	case errors.As(err, &ce):
		return codes.Ensure(ce)
	case errors.Is(err, context.DeadlineExceeded):
		return codes.Ensure(connect.NewError(connect.CodeDeadlineExceeded, "the request's deadline passed"))
	case errors.Is(err, context.Canceled):
		return codes.Ensure(connect.NewError(connect.CodeCanceled, "the request was canceled"))
	}
	return codes.ToConnect(err)
}

// credential reads what a request presents. A bearer key in the
// Authorization header is the credential wherever it comes from, so a key
// can be tried on the local socket too. Without one, a request that came
// through the local socket's peer check is the local operator; any other is
// a request with no key. A header that is not a bearer key (another scheme,
// a key without the scheme, the scheme with nothing after it) is a malformed
// key: the check refuses it like any other bad credential, so a caller learns
// nothing from the form of its header, and the header itself never reaches
// the check.
// malformedKey stands for every Authorization header that is not a bearer
// key: not empty, so it is not "no key", and not of a key's shape.
const malformedKey = "malformed"

func credential(authorization string, local localPeer, isLocal bool) access.Credential {
	if authorization != "" {
		scheme, token, found := strings.Cut(strings.TrimSpace(authorization), " ")
		if found && strings.EqualFold(scheme, "Bearer") {
			if t := strings.TrimSpace(token); t != "" {
				return access.APIKey(t)
			}
		}
		return access.APIKey(malformedKey)
	}
	if isLocal {
		return access.LocalOperator(local.user)
	}
	return access.APIKey("")
}
