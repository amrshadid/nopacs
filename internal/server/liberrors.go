package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"

	"connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/api/codes"
)

// withErrorInfo wraps a procedure's handler from the RPC library so the
// answers the library writes by itself, before any interceptor runs, carry
// the google.rpc.ErrorInfo every other answer carries (codes.Ensure): an
// unknown Content-Encoding or grpc-encoding, a content type no protocol
// speaks (HTTP 415, empty body), a Connect GET without its message. The code
// and the HTTP status the library chose are kept; the code's general reason
// is added, and an answer that has a detail already passes unchanged.
//
//   - A Connect unary error (any status but 200) is held whole, small as it
//     is, and written again by writeHTTPErrorStatus with the details
//     (the Connect protocol, "Unary-Error-Response").
//   - An empty answer of another status (415, 405) is given the Connect
//     error body of the code the Connect protocol reads from that status
//     ("HTTP to Error Code"), keeping the status and its headers.
//   - A gRPC status in the trailers (http.TrailerPrefix), or in the header
//     block of a gRPC-Web trailers-only answer, is given
//     grpc-status-details-bin (a google.rpc.Status, gRPC over HTTP/2,
//     "Responses").
func withErrorInfo(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lw := &libraryWriter{ResponseWriter: w}
		h.ServeHTTP(lw, r)
		lw.finish()
	})
}

// maxHeldError bounds what libraryWriter holds of an answer that is not
// 200: a Connect error is a few hundred bytes, and an answer past the bound
// is passed on as it is rather than held.
const maxHeldError = 64 << 10

// libraryWriter is the writer withErrorInfo gives the library's handler.
type libraryWriter struct {
	http.ResponseWriter
	wrote  bool
	held   bool
	status int
	body   bytes.Buffer
}

// WriteHeader adds the details to a gRPC-Web trailers-only answer, holds
// an answer that is not 200, and passes any other on.
func (w *libraryWriter) WriteHeader(code int) {
	if w.wrote {
		return
	}
	w.wrote = true
	addHeaderDetails(w.Header(), "")
	if code != http.StatusOK {
		w.held, w.status = true, code
		return
	}
	w.ResponseWriter.WriteHeader(code)
}

// Write holds the body of a held answer, up to maxHeldError, and passes any
// other on.
func (w *libraryWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	if !w.held {
		return w.ResponseWriter.Write(b)
	}
	if w.body.Len()+len(b) <= maxHeldError {
		return w.body.Write(b)
	}
	w.release()
	return w.ResponseWriter.Write(b)
}

// Flush sends what is written so far, except a held answer, which is
// written whole when the handler returns.
func (w *libraryWriter) Flush() {
	if w.held {
		return
	}
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

// Unwrap returns the writer below, for http.ResponseController.
func (w *libraryWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// release writes a held answer as the library made it and stops holding.
func (w *libraryWriter) release() {
	w.held = false
	w.ResponseWriter.WriteHeader(w.status)
	_, _ = w.ResponseWriter.Write(w.body.Bytes())
	w.body.Reset()
}

// finish runs when the library's handler has returned: it writes a held
// answer with its details, and adds them to a gRPC status the handler left
// in the header map, which net/http sends after this returns.
func (w *libraryWriter) finish() {
	h := w.Header()
	if !w.wrote {
		addHeaderDetails(h, "")
	}
	addHeaderDetails(h, http.TrailerPrefix)
	if !w.held {
		return
	}
	var e struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Details json.RawMessage `json:"details"`
	}
	switch {
	case w.body.Len() == 0:
		writeHTTPErrorStatus(w.ResponseWriter, connect.NewError(statusCode(w.status), http.StatusText(w.status)), w.status)
	case json.Unmarshal(w.body.Bytes(), &e) == nil && e.Code != "" && len(e.Details) == 0:
		var code connect.Code
		if code.UnmarshalText([]byte(e.Code)) != nil {
			code = connect.CodeUnknown
		}
		writeHTTPErrorStatus(w.ResponseWriter, connect.NewError(code, e.Message), w.status)
	default:
		w.release()
	}
}

// addHeaderDetails adds grpc-status-details-bin under prefix to h when h
// holds a gRPC status other than OK under prefix and no details, built from
// that status and its message.
func addHeaderDetails(h http.Header, prefix string) {
	status := h.Values(prefix + "Grpc-Status")
	if len(status) == 0 || status[0] == "0" || len(h.Values(prefix+"Grpc-Status-Details-Bin")) > 0 {
		return
	}
	n, err := strconv.Atoi(status[0])
	if err != nil || n < 0 || n > int(connect.CodeUnauthenticated) {
		return
	}
	msg := ""
	if m := h.Values(prefix + "Grpc-Message"); len(m) > 0 {
		msg = m[0]
		if d, err := url.PathUnescape(msg); err == nil {
			msg = d
		}
	}
	ce := codes.Ensure(connect.NewError(connect.Code(n), msg)) //nolint:gosec // G115: n is 0 to 16, checked above.
	// The key is set as written, not canonicalized: under the trailer prefix
	// net/http sends what follows the colon as the trailer's name.
	h[prefix+"Grpc-Status-Details-Bin"] = []string{connect.EncodeBinaryHeader(codes.StatusBytes(ce))}
}

// statusCode is the code the Connect protocol reads from an HTTP status
// that comes without an error body ("HTTP to Error Code").
func statusCode(status int) connect.Code {
	switch status {
	case http.StatusBadRequest:
		return connect.CodeInternal
	case http.StatusUnauthorized:
		return connect.CodeUnauthenticated
	case http.StatusForbidden:
		return connect.CodePermissionDenied
	case http.StatusNotFound:
		return connect.CodeUnimplemented
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return connect.CodeUnavailable
	}
	return connect.CodeUnknown
}
