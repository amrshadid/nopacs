package server_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"connectrpc.com/connect/v2"
	"connectrpc.com/connect/v2/connecthttp"
	"google.golang.org/genproto/googleapis/rpc/status"
	"google.golang.org/protobuf/proto"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api/codes"
)

// TestLibraryErrorsCarryErrorInfo guards the answers the RPC library writes
// itself, before any interceptor, for an allowed caller whose request it
// cannot take: each keeps the code and HTTP status the library chose and
// gains the code's general google.rpc.ErrorInfo, in the Connect JSON body
// or in grpc-status-details-bin (a trailer for gRPC, a header of a
// trailers-only gRPC-Web answer).
func TestLibraryErrorsCarryErrorInfo(t *testing.T) {
	e := newEnv(t, options{})
	key := e.makeKey(nopacsv1.Role_ROLE_ADMIN, installation())
	frame := func(flag byte, b []byte) []byte {
		return append([]byte{flag, 0, 0, 0, byte(len(b))}, b...) //nolint:gosec // G115: a test's message of a few bytes.
	}
	cases := []struct {
		name, method, path, ct string
		hdr                    map[string]string
		body                   []byte
		status                 int
		code                   connect.Code
		where                  string // "json", "trailer" or "header"
	}{
		{"connect, an unknown Content-Encoding", http.MethodPost, servedProcedure, "application/json",
			map[string]string{"Content-Encoding": "bogus"}, []byte("{}"), http.StatusNotImplemented, connect.CodeUnimplemented, "json"},
		{"connect, a GET without its message", http.MethodGet, servedProcedure + "?connect=v1&encoding=json", "",
			nil, nil, http.StatusBadRequest, connect.CodeInvalidArgument, "json"},
		{"connect, a content type no protocol speaks", http.MethodPost, servedProcedure, "application/xml",
			nil, []byte("<a/>"), http.StatusUnsupportedMediaType, connect.CodeUnknown, "json"},
		{"grpc, an unknown grpc-encoding", http.MethodPost, servedProcedure, "application/grpc",
			map[string]string{"Te": "trailers", "Grpc-Encoding": "bogus"}, frame(1, []byte{1, 2, 3}), http.StatusOK, connect.CodeUnimplemented, "trailer"},
		{"grpc, a codec no protocol speaks", http.MethodPost, servedProcedure, "application/grpc+xml",
			map[string]string{"Te": "trailers"}, frame(0, nil), http.StatusUnsupportedMediaType, connect.CodeUnknown, "json"},
		{"grpc-web, an unknown grpc-encoding", http.MethodPost, servedProcedure, "application/grpc-web+proto",
			map[string]string{"Grpc-Encoding": "bogus"}, frame(1, []byte{1, 2, 3}), http.StatusOK, connect.CodeUnimplemented, "header"},
	}
	hc := e.tlsClient(true)
	for _, c := range cases {
		req, err := http.NewRequest(c.method, e.netURL+c.path, bytes.NewReader(c.body))
		if err != nil {
			t.Fatal(err)
		}
		if c.ct != "" {
			req.Header.Set("Content-Type", c.ct)
		}
		req.Header.Set("Authorization", "Bearer "+key)
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != c.status {
			t.Errorf("%s: HTTP %d, want %d (body %q)", c.name, resp.StatusCode, c.status, body)
			continue
		}
		var ce *connect.Error
		switch c.where {
		case "json":
			var doc struct {
				Code    string `json:"code"`
				Message string `json:"message"`
				Details []struct {
					Type  string `json:"type"`
					Value string `json:"value"`
				} `json:"details"`
			}
			if err := json.Unmarshal(body, &doc); err != nil {
				t.Errorf("%s: body %q is not a Connect error: %v", c.name, body, err)
				continue
			}
			ce = connect.NewError(c.code, doc.Message)
			if doc.Code != c.code.String() {
				t.Errorf("%s: code %q, want %q", c.name, doc.Code, c.code)
			}
			for _, d := range doc.Details {
				v, err := connect.DecodeBinaryHeader(d.Value)
				if err != nil {
					t.Fatalf("%s: detail %q: %v", c.name, d.Value, err)
				}
				ce = ce.WithDetail(&connect.ErrorDetail{Type: d.Type, Value: v})
			}
		default:
			h := resp.Trailer
			if c.where == "header" {
				h = resp.Header
			}
			if got := h.Get("Grpc-Status"); got != strconv.Itoa(int(c.code)) {
				t.Errorf("%s: grpc-status %q, want %d", c.name, got, c.code)
			}
			raw, err := connect.DecodeBinaryHeader(h.Get("Grpc-Status-Details-Bin"))
			var st status.Status
			if err != nil || proto.Unmarshal(raw, &st) != nil {
				t.Errorf("%s: grpc-status-details-bin %q does not decode", c.name, h.Get("Grpc-Status-Details-Bin"))
				continue
			}
			ce = connect.NewError(connect.Code(st.GetCode()), st.GetMessage()) //nolint:gosec // G115: a test's status code.
			for _, a := range st.GetDetails() {
				ce = ce.WithDetail(&connect.ErrorDetail{Type: strings.TrimPrefix(a.GetTypeUrl(), "type.googleapis.com/"), Value: a.GetValue()})
			}
		}
		r, ok := codes.ReasonOfError(ce)
		if !ok || r.Reason != codes.General(c.code) || r.Domain != codes.Domain {
			t.Errorf("%s: error info %+v %v, want %s", c.name, r, ok, codes.General(c.code))
		}
	}
}

// TestRefusalsCarryErrorInfo guards that the server's own answers carry
// the google.rpc.ErrorInfo detail every answer promises, in each protocol a
// stock client speaks, as that client decodes it: a key the front step
// does not know (UNAUTHENTICATED, written by the front step itself, before
// a call is routed) and a known key with no grant (PERMISSION_DENIED,
// answered by the check on the request path). Each carries its code's
// general reason, in the domain "nopacs", and its message unchanged.
func TestRefusalsCarryErrorInfo(t *testing.T) {
	e := newEnv(t, options{})
	nogrant := e.makeKey(nopacsv1.Role_ROLE_UNSPECIFIED, nil)
	protocols := map[string][]connecthttp.Option{
		"connect json":  {connecthttp.WithProtoJSON()},
		"connect proto": nil,
		"grpc":          {connecthttp.WithGRPC()},
		"grpc-web":      {connecthttp.WithGRPCWeb()},
	}
	for name, opts := range protocols {
		for _, c := range []struct {
			who, auth, reason string
			code              connect.Code
		}{
			{"unknown key", "Bearer nopacs_00000000000000000000000000_notakey", "UNAUTHENTICATED", connect.CodeUnauthenticated},
			{"no grant", "Bearer " + nogrant, "PERMISSION_DENIED", connect.CodePermissionDenied},
		} {
			cl := connect.NewClient(connecthttp.NewTransport(e.tlsClient(true), e.netURL, opts...), bearer(c.auth))
			_, err := nopacsv1connect.NewAccessServiceClient(cl).ListKeys(e.ctx, &nopacsv1.ListKeysRequest{})
			var ce *connect.Error
			if !errors.As(err, &ce) || ce.Code() != c.code {
				t.Errorf("%s, %s: %v, want %v", name, c.who, err, c.code)
				continue
			}
			r, ok := codes.ReasonOfError(ce)
			if !ok || r.Reason != c.reason || r.Domain != codes.Domain || len(r.Metadata) != 0 {
				t.Errorf("%s, %s: error info %+v %v (details %v), want %s in %s", name, c.who, r, ok, ce.Details(), c.reason, codes.Domain)
			}
			if ce.Message() == "" || ce.Message() == c.reason {
				t.Errorf("%s, %s: message %q, want the server's words", name, c.who, ce.Message())
			}
		}
	}
}
