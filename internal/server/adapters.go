package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/obs/redact"
)

// errNoSuchMethod answers a procedure the table does not know, after the
// check. The router dispatches only the procedures recorded at New, each of
// which has a requirement, so this is a guard that should never be reached:
// were a procedure to arrive here unknown (registered on the connect.Server
// after New, say), it must never be served.
var errNoSuchMethod = connect.NewError(connect.CodeUnimplemented, noSuchMethod)

// interceptor is the Connect server interceptor: every RPC the server's
// connect.Server dispatches, unary and streaming, goes through it and so
// through pass. It is installed when the connect.Server is made
// (buildRoutes), so a method registered on it cannot skip it. The caller was
// authenticated by the front step before the call was routed (or the method
// is public); pass takes the credential front accepted from the context.
// A procedure the table does not know is checked as any authenticated
// caller's and then answered UNIMPLEMENTED: its handler is never called.
func (s *Server) interceptor(next connect.ServerFunc) connect.ServerFunc {
	return func(ctx context.Context, spec connect.Spec, stream connect.ServerStream) error {
		info, _ := connect.CallInfoForServerContext(ctx)
		x := exchange{method: spec.Procedure, metric: spec.Procedure}
		if info != nil {
			x.authorization = info.RequestHeader().Get("Authorization")
			x.remote = remoteOf(ctx, info.PeerAddr)
		}
		handler := func(ctx context.Context, st connect.ServerStream) error { return next(ctx, spec, st) }
		if r, known := s.ruleFor(spec.Procedure); known {
			x.rule = r
		} else {
			// The procedure comes from the request, so it is recorded only
			// when it has the shape of one (methodName), and counted under
			// one label.
			x.rule = on(access.Authenticated())
			x.method = methodName(spec.Procedure)
			x.metric = unknownMetric
			handler = func(context.Context, connect.ServerStream) error { return errNoSuchMethod }
		}
		replay := &replayStream{ServerStream: stream, s: s, ctx: ctx, method: x.method}
		if info != nil {
			replay.codec = info.Codec
		}
		x.readMessage = func() (proto.Message, error) {
			m := x.rule.newMsg()
			if err := stream.Receive(m); err != nil {
				return nil, err
			}
			// Kept for the handler, and not logged: nothing from a request
			// is logged before the check has allowed it.
			replay.peeked = m
			return m, nil
		}
		err := s.pass(ctx, x, func(ctx context.Context) error {
			replay.ctx = ctx
			return handler(ctx, replay)
		})
		if err == nil {
			return nil
		}
		ce := s.answer(err)
		if ce.Code() == connect.CodeUnauthenticated && info != nil {
			// RFC 9110 15.5.2: a 401 names the scheme the caller should use.
			info.ResponseHeader().Set("Www-Authenticate", "Bearer")
		}
		return ce
	}
}

// replayStream hands the handler the message the check already read, as
// its first Receive, so a message is read from the wire once; it logs each
// message at debug level, redacted, once the check has allowed the request;
// and it refuses to send an answer over the size limit in the request's
// encoding.
type replayStream struct {
	connect.ServerStream
	s      *Server
	ctx    context.Context
	method string
	codec  string
	peeked proto.Message
}

// Receive returns the message the check read, once, then reads from the
// stream.
func (r *replayStream) Receive(msg any) error {
	if r.peeked != nil {
		dst, ok := msg.(proto.Message)
		if !ok {
			return connect.NewError(connect.CodeInternal, "internal error")
		}
		proto.Reset(dst)
		proto.Merge(dst, r.peeked)
		r.peeked = nil
		r.logMessage("message received", dst)
		return nil
	}
	r.s.at(r.ctx, pointReceive)
	if err := r.ServerStream.Receive(msg); err != nil {
		if !errors.Is(err, io.EOF) {
			if c, ok := CallFrom(r.ctx); ok {
				c.unreadable.Store(true)
			}
		}
		return err
	}
	r.logMessage("message received", msg)
	return nil
}

// jsonCodec is the name of the RPC library's JSON codec.
const jsonCodec = "json"

// Send logs the message, redacted, and sends it, unless its encoding is
// over the limit. The RPC library's own send limit counts the compressed
// bytes, so a large answer that compresses well would pass it and then be
// refused by a stock gRPC client, whose limit counts the bytes after
// decompression. The size is the encoding's: the binary size for protobuf,
// and for JSON the JSON form's, which can be several times larger (a byte of
// a string may be written as six); the JSON form is measured only when the
// binary size is within that factor of the limit.
func (r *replayStream) Send(msg any) error {
	if m, ok := msg.(proto.Message); ok {
		size := proto.Size(m)
		if r.codec == jsonCodec && size > MaxMessageBytes/8 {
			if b, err := protojson.Marshal(m); err == nil {
				size = len(b)
			}
		}
		if size > MaxMessageBytes {
			return connect.NewError(connect.CodeResourceExhausted, "the answer is larger than the 4 MiB limit")
		}
	}
	r.logMessage("message sent", msg)
	return r.ServerStream.Send(msg)
}

// logMessage writes a message at debug level, through the redactor. It is
// the only place the server logs a request's or a response's content, and
// it runs only once the check has allowed the request.
func (r *replayStream) logMessage(what string, msg any) {
	m, ok := msg.(proto.Message)
	if !ok || !r.s.log.Enabled(r.ctx, slog.LevelDebug) {
		return
	}
	id, _ := requestIDFrom(r.ctx)
	r.s.log.LogAttrs(r.ctx, slog.LevelDebug, what,
		slog.String("request_id", id),
		slog.String("method", r.method),
		slog.Any("message", redact.LogValue(m)))
}

// HTTPHandler serves a plain HTTP route once the request path has checked
// it. The Call is in ctx. An error it returns is answered by the server in
// the same JSON form as a Connect error, with the matching HTTP status; it
// returns nil when it wrote its own answer.
type HTTPHandler func(ctx context.Context, w http.ResponseWriter, r *http.Request) error

// HTTPRoute is a plain HTTP route: a pattern of http.ServeMux, the
// requirement a caller must meet, and the handler.
type HTTPRoute struct {
	// Pattern is an http.ServeMux pattern, such as "GET /metrics".
	Pattern string
	// Need is what the caller must hold. A plain HTTP route is never
	// public: the zero Requirement, which nobody meets, refuses everyone.
	Need access.Requirement
	// Handler serves the request.
	Handler HTTPHandler
}

// middleware wraps a plain HTTP route in the request path: the route's
// requirement is checked by pass, with the credential the front step
// accepted, before its handler runs.
func (s *Server) middleware(route HTTPRoute) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		x := exchange{
			method:        route.Pattern,
			metric:        route.Pattern,
			authorization: r.Header.Get("Authorization"),
			remote:        remoteOf(r.Context(), r.RemoteAddr),
			rule:          on(route.Need),
			unaudited:     route.Pattern == MetricsPattern,
		}
		err := s.pass(r.Context(), x, func(ctx context.Context) error {
			return route.Handler(ctx, w, r.WithContext(ctx))
		})
		if err != nil {
			s.writeAnyError(w, r, s.answer(err))
		}
	})
}

// httpStatus is the HTTP status of each API code, as the Connect protocol
// gives it for a unary call, so a plain HTTP route and an RPC answer the
// same refusal with the same status.
var httpStatus = map[connect.Code]int{
	connect.CodeCanceled:           499,
	connect.CodeUnknown:            http.StatusInternalServerError,
	connect.CodeInvalidArgument:    http.StatusBadRequest,
	connect.CodeDeadlineExceeded:   http.StatusGatewayTimeout,
	connect.CodeNotFound:           http.StatusNotFound,
	connect.CodeAlreadyExists:      http.StatusConflict,
	connect.CodePermissionDenied:   http.StatusForbidden,
	connect.CodeResourceExhausted:  http.StatusTooManyRequests,
	connect.CodeFailedPrecondition: http.StatusBadRequest,
	connect.CodeAborted:            http.StatusConflict,
	connect.CodeOutOfRange:         http.StatusBadRequest,
	connect.CodeUnimplemented:      http.StatusNotImplemented,
	connect.CodeInternal:           http.StatusInternalServerError,
	connect.CodeUnavailable:        http.StatusServiceUnavailable,
	connect.CodeDataLoss:           http.StatusInternalServerError,
	connect.CodeUnauthenticated:    http.StatusUnauthorized,
}

// wireDetail is one error detail in the Connect protocol's JSON: the
// message's full type name and its protobuf bytes in base64 without padding
// (the Connect protocol, "Error Details").
type wireDetail struct {
	Type  string `json:"type"`
	Value string `json:"value"`
}

// wireDetails returns the details of ce in the Connect protocol's JSON
// form, or nil for none.
func wireDetails(ce *connect.Error) []wireDetail {
	var out []wireDetail
	for _, d := range ce.Details() {
		out = append(out, wireDetail{Type: d.Type, Value: connect.EncodeBinaryHeader(d.Value)})
	}
	return out
}

// writeHTTPError writes a Connect-shaped error, {"code": ..., "message":
// ..., "details": [...]}, with its HTTP status (the Connect protocol,
// "Unary-Error-Response"). The details are ce's after codes.Ensure, so a
// plain HTTP answer carries the same google.rpc.ErrorInfo an RPC does.
func writeHTTPError(w http.ResponseWriter, ce *connect.Error) {
	status, ok := httpStatus[ce.Code()]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeHTTPErrorStatus(w, ce, status)
}

// writeHTTPErrorStatus is writeHTTPError with the HTTP status given, for an
// answer whose status the RPC library chose (withErrorInfo).
func writeHTTPErrorStatus(w http.ResponseWriter, ce *connect.Error, status int) {
	ce = codes.Ensure(ce)
	body, err := json.Marshal(struct {
		Code    string       `json:"code"`
		Message string       `json:"message,omitempty"`
		Details []wireDetail `json:"details,omitempty"`
	}{ce.Code().String(), ce.Message(), wireDetails(ce)})
	if err != nil {
		body = []byte(`{"code":"internal"}`)
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	h.Set("X-Content-Type-Options", "nosniff")
	if status == http.StatusUnauthorized {
		h.Set("Www-Authenticate", "Bearer")
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// errorWriter writes an error in whichever protocol a request speaks
// (Connect, gRPC, gRPC-Web), for the answers the envelope gives itself (a
// server that is stopping, a panic outside a handler) and for a plain
// route's error after its check. The answers of the front step and of the
// router (a refusal of a caller not authenticated, a path nothing serves)
// are written by writeWireError instead, whose output is fixed.
var errorWriter = connecthttp.NewErrorWriter()

// writeAnyError answers r with ce in its own protocol, or as JSON for a
// request that is not an RPC. A 401 names the Bearer scheme (RFC 9110
// 15.5.2) in either form.
func (s *Server) writeAnyError(w http.ResponseWriter, r *http.Request, ce *connect.Error) {
	ce = codes.Ensure(ce)
	if ce.Code() == connect.CodeUnauthenticated {
		w.Header().Set("Www-Authenticate", "Bearer")
	}
	if errorWriter.IsSupported(r) {
		if err := errorWriter.Write(w, r, ce); err != nil && !errors.Is(err, context.Canceled) {
			s.log.Debug("an error answer could not be written", "error", err)
		}
		return
	}
	writeHTTPError(w, ce)
}

// remoteOf returns the caller's address for the log and the audit entry:
// the one the front step judged (originOf), which is a trusted proxy's
// forwarded client when there is one; empty on the local socket, where the
// "address" names no machine; addr when the front step did not run.
func remoteOf(ctx context.Context, addr string) string {
	if o, ok := originFrom(ctx); ok {
		return o.remote
	}
	if _, ok := localFrom(ctx); ok {
		return ""
	}
	return addr
}

// maxMethodName is the longest procedure an audit entry and the log record
// as asked: room for any method of the contract, and far less than a path
// may be.
const maxMethodName = 256

// unnamedMethod is what is recorded for a path that is not the name of a
// procedure: "/", one fixed name, whatever the path was.
const unnamedMethod = "/"

// methodName returns the procedure a request named, as an audit entry and
// the log record it: p itself when it has the shape of a protobuf method's
// full name, "/package.Service/Method", each part an identifier of ASCII
// letters, digits and "_" not starting with a digit, and is at most
// maxMethodName bytes; unnamedMethod for anything else. The path is
// whatever a stranger sent, so nothing else of it is kept: no control
// character, no unbounded length.
func methodName(p string) string {
	if len(p) > maxMethodName || len(p) < 4 || p[0] != '/' {
		return unnamedMethod
	}
	slash := strings.LastIndexByte(p, '/')
	if slash == 0 || !fullName(p[1:slash]) || !identifier(p[slash+1:]) {
		return unnamedMethod
	}
	return p
}

// fullName reports whether s is a protobuf full name: identifiers joined by
// ".".
func fullName(s string) bool {
	for part := range strings.SplitSeq(s, ".") {
		if !identifier(part) {
			return false
		}
	}
	return true
}

// identifier reports whether s is a protobuf identifier: ASCII letters,
// digits and "_", not starting with a digit, and not empty.
func identifier(s string) bool {
	if s == "" || (s[0] >= '0' && s[0] <= '9') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !identByte(s[i]) {
			return false
		}
	}
	return true
}

// identByte reports whether c may be in a protobuf identifier.
func identByte(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}
