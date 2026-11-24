package server

import (
	"encoding/binary"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"connectrpc.com/connect/v2"
)

// wireFamily is the protocol a request speaks, as far as the server's own
// answers need to know it: read from the Content-Type header alone, never
// from the path, the HTTP method or the body, so that an answer chosen by it
// cannot tell a caller anything about what the path names.
type wireFamily uint8

const (
	// familyHTTP is any request that is not one of the RPC protocols below:
	// plain HTTP, no content type, an unknown one. Its answer is the JSON
	// error of a Connect unary call, which a plain HTTP client reads too.
	familyHTTP wireFamily = iota
	// familyConnectUnary is a Connect unary call: application/json or
	// application/proto (the Connect protocol, "Unary-Request"). Its error
	// answer is the same JSON as familyHTTP's.
	familyConnectUnary
	// familyGRPC is gRPC: application/grpc, or application/grpc+<codec>
	// (gRPC over HTTP/2, "Requests").
	familyGRPC
	// familyGRPCWeb is gRPC-Web: application/grpc-web, or
	// application/grpc-web+<codec> (gRPC-Web protocol, "Protocol
	// differences vs gRPC over HTTP2"). The text form, grpc-web-text, is not
	// spoken by this server and is familyHTTP.
	familyGRPCWeb
	// familyConnectStream is a Connect streaming call:
	// application/connect+<codec> (the Connect protocol,
	// "Streaming-Request").
	familyConnectStream
)

// wire is a request's protocol family and whether its codec is JSON. The
// codec matters to an answer only in its Content-Type: a client of an
// enveloped protocol refuses an answer whose content type names another
// codec than it asked for, and would report that instead of the error.
type wire struct {
	family wireFamily
	json   bool
}

// wireOf reads the protocol of r from its Content-Type alone. Media types
// are compared without regard to case (RFC 9110 8.3.1) and without their
// parameters. A Connect GET carries no content type (the Connect protocol,
// "Unary-Get-Request"): it is a Connect unary call only when it says so with
// connect=v1 in its query, which changes nothing in a refusal, whose JSON
// both families share.
func wireOf(r *http.Request) wire {
	ct := r.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	ct = strings.ToLower(strings.TrimSpace(ct))
	codec := func(prefix string) (bool, bool) {
		if ct == prefix {
			return false, true
		}
		if rest, ok := strings.CutPrefix(ct, prefix+"+"); ok {
			return rest == "json", true
		}
		return false, false
	}
	if j, ok := codec("application/grpc"); ok {
		return wire{familyGRPC, j}
	}
	if j, ok := codec("application/grpc-web"); ok {
		return wire{familyGRPCWeb, j}
	}
	if rest, ok := strings.CutPrefix(ct, "application/connect+"); ok {
		return wire{familyConnectStream, rest == "json"}
	}
	switch ct {
	case "application/json":
		return wire{familyConnectUnary, true}
	case "application/proto":
		return wire{familyConnectUnary, false}
	case "":
		if r.URL.Query().Get("connect") == "v1" {
			return wire{familyConnectUnary, true}
		}
	}
	return wire{family: familyHTTP}
}

// rpc reports whether w is one of the RPC protocols.
func (w wire) rpc() bool { return w.family != familyHTTP }

// contentType is the Content-Type of an answer in an enveloped protocol: the
// family's, with the JSON codec named when the request used it and the
// protobuf codec otherwise, a fixed small set whatever the request sent.
func (w wire) contentType() string {
	switch w.family {
	case familyGRPC:
		if w.json {
			return "application/grpc+json"
		}
		return "application/grpc"
	case familyGRPCWeb:
		if w.json {
			return "application/grpc-web+json"
		}
		return "application/grpc-web+proto"
	case familyConnectStream:
		if w.json {
			return "application/connect+json"
		}
		return "application/connect+proto"
	}
	return "application/json"
}

// writeWireError writes an error answer in the protocol w, all of it built
// here from code and msg, so its status, headers, trailers and body depend on
// w, code and msg and on nothing else. It never reads the request's body.
// Every form carries X-Content-Type-Options: nosniff and, for
// UNAUTHENTICATED, WWW-Authenticate: Bearer (RFC 9110 15.5.2, RFC 6750 3).
// The headers the envelope set before (the request id among them) go out in
// the first header block, so an RPC client sees them as the answer's
// initial metadata. A HEAD request gets the same status and headers without
// the body (RFC 9110 9.3.2); net/http leaves the body out itself.
//
//   - gRPC: HTTP 200 and the Content-Type in the header block, then the
//     status in trailers, grpc-status as the code's number and grpc-message
//     percent-encoded (gRPC over HTTP/2, "Responses": Response-Headers,
//     then Trailers), with no message between. Not Trailers-Only: a client
//     such as grpcio surfaces a Trailers-Only block as trailing metadata
//     only, and the request id must reach a refused caller as initial
//     metadata like any other header. The Trailer header names the two in
//     a fixed order. Over HTTP/1.1 the trailers go after an empty chunked
//     body.
//   - gRPC-Web: HTTP 200 and the Content-Type in the header block, then one
//     trailer frame as the body (the gRPC-Web protocol, "Message framing"):
//     flag 0x80, the length as four bytes big-endian, then
//     "grpc-status: N\r\ngrpc-message: ...\r\n".
//   - Connect streaming: HTTP 200 and one end-of-stream message (the
//     Connect protocol, "Streaming-Response"): flags 0x02, the length as
//     four bytes big-endian, then {"error":{"code":...,"message":...}} in
//     JSON whatever the codec.
//   - Connect unary and anything else: the HTTP status the Connect protocol
//     gives the code ("Error Codes") and {"code":...,"message":...} as
//     application/json (writeHTTPError).
//
// Every form but gRPC's carries an explicit Content-Length, so its framing
// does not depend on the HTTP version.
func writeWireError(w http.ResponseWriter, p wire, code connect.Code, msg string) {
	h := w.Header()
	common := func() {
		h.Set("Content-Type", p.contentType())
		h.Set("X-Content-Type-Options", "nosniff")
		if code == connect.CodeUnauthenticated {
			h.Set("Www-Authenticate", "Bearer")
		}
	}
	switch p.family {
	case familyGRPC:
		common()
		h.Set("Trailer", "Grpc-Status, Grpc-Message")
		w.WriteHeader(http.StatusOK)
		// Declared trailers, set after the header block is written: net/http
		// sends them when the handler returns.
		h.Set("Grpc-Status", strconv.Itoa(int(code)))
		h.Set("Grpc-Message", grpcPercentEncode(msg))
	case familyGRPCWeb:
		body := grpcWebTrailers(code, msg)
		common()
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body) //nolint:gosec // G705: a gRPC-Web trailer frame built here from a code and a fixed message, never HTML.
	case familyConnectStream:
		body := connectEndStream(code, msg)
		common()
		h.Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body) //nolint:gosec // G705: an application/connect+ envelope built here from a code and a fixed message, never HTML.
	default:
		writeHTTPError(w, connect.NewError(code, msg))
	}
}

// grpcWebTrailers returns the gRPC-Web trailer frame carrying code and msg:
// flag 0x80, the length, then the trailers as an HTTP/1 header block with
// lowercase names, in a fixed order.
func grpcWebTrailers(code connect.Code, msg string) []byte {
	block := "grpc-status: " + strconv.Itoa(int(code)) + "\r\ngrpc-message: " + grpcPercentEncode(msg) + "\r\n"
	out := make([]byte, 5, 5+len(block))
	out[0] = 0x80
	binary.BigEndian.PutUint32(out[1:], uint32(len(block))) //nolint:gosec // G115: a fixed block of a few dozen bytes.
	return append(out, block...)
}

// connectEndStream returns the Connect protocol's end-of-stream message
// carrying an error: the envelope (flag 0x02, end of stream) around its JSON.
func connectEndStream(code connect.Code, msg string) []byte {
	type wireErr struct {
		Code    string `json:"code"`
		Message string `json:"message,omitempty"`
	}
	payload, err := json.Marshal(struct {
		Error wireErr `json:"error"`
	}{wireErr{code.String(), msg}})
	if err != nil {
		payload = []byte(`{"error":{"code":"internal"}}`)
	}
	out := make([]byte, 5, 5+len(payload))
	out[0] = 0x02
	binary.BigEndian.PutUint32(out[1:], uint32(len(payload))) //nolint:gosec // G115: a fixed message of a few dozen bytes.
	return append(out, payload...)
}

// grpcPercentEncode encodes a grpc-message value as gRPC over HTTP/2
// requires ("Responses", Status-Message): every byte outside the printable
// ASCII range 0x20-0x7E, and "%" itself, as %XX.
func grpcPercentEncode(msg string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(msg); i++ {
		c := msg[i]
		if c >= 0x20 && c <= 0x7e && c != '%' {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&0x0f])
	}
	return b.String()
}
