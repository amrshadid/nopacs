package server_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/server"
)

// uncheckedRoutes sends two requests to every pattern the server has
// registered and returns each pattern whose answers show it is not served
// through the request path: one without a key, over the network listener,
// which must be refused (401) with a request id; and one from the local
// operator, which must be logged by the request path under the route's own
// name and not be the router's answer to a path nothing serves. A route the request path does not serve is either served without
// its check or, as the router does for anything it was not given at New,
// not served at all: both show here. The health service is the one pattern
// meant to answer without a key.
func uncheckedRoutes(t *testing.T, e *env) []string {
	t.Helper()
	hc := e.tlsClient(false)
	local := e.socketClient(false)
	var bad []string
	for _, pattern := range e.srv.Routes() {
		method, path := "POST", pattern
		if m, p, ok := strings.Cut(pattern, " "); ok {
			method, path = m, p
		}
		if path == server.HealthCheckProcedure || path == server.HealthWatchProcedure {
			continue
		}
		if strings.HasSuffix(path, "/") {
			// A subtree: probe a path inside it.
			path += "probe"
		}
		// A pattern route: probe a path of its shape, each wildcard one
		// plain segment.
		path = wildcard.ReplaceAllString(path, "probe")
		do := func(c *http.Client, base string) (*http.Response, string) {
			req, _ := http.NewRequest(method, base+path, strings.NewReader("{}"))
			req.Header.Set("Content-Type", "application/json")
			resp, err := c.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return resp, string(body)
		}
		resp, _ := do(hc, e.netURL)
		if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get(server.RequestIDHeader) == "" {
			bad = append(bad, pattern)
			continue
		}
		name := path
		if strings.Contains(pattern, " ") {
			name = pattern
		}
		resp, body := do(local, "http://local")
		notServed := strings.Contains(body, `"no such method"`) || strings.Contains(body, `"no such route"`)
		if logged := loggedMethod(t, e, resp.Header.Get(server.RequestIDHeader)); logged != name || notServed {
			bad = append(bad, pattern)
		}
	}
	return bad
}

// wildcard is a wildcard of an http.ServeMux pattern, "{name}" or
// "{name...}".
var wildcard = regexp.MustCompile(`\{[a-z_0-9]+(\.\.\.)?\}`)

// loggedMethod returns the method of the "request" log line of the request
// with this id, or "" when there is none.
func loggedMethod(t *testing.T, e *env, id string) string {
	t.Helper()
	for line := range strings.SplitSeq(e.log.String(), "\n") {
		var rec struct {
			Msg       string `json:"msg"`
			RequestID string `json:"request_id"`
			Method    string `json:"method"`
		}
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Msg == "request" && rec.RequestID == id && id != "" {
			return rec.Method
		}
	}
	return ""
}

// TestEveryRouteIsChecked guards that no route skips the request path:
// every pattern registered refuses a request without a key, with a request
// id, and serves the local operator through the request path. Then it
// registers a route outside the request path in each of the two ways one
// could be, and shows the same check catching each, and that neither
// route's handler is ever reached, with a key or without.
func TestEveryRouteIsChecked(t *testing.T) {
	e := newEnv(t, options{})
	routes := e.srv.Routes()
	if len(routes) < 20 {
		t.Fatalf("only %d routes registered: %v", len(routes), routes)
	}
	if bad := uncheckedRoutes(t, e); len(bad) > 0 {
		t.Fatalf("routes not served through the request path: %v", bad)
	}

	t.Run("a plain route outside the path is caught", func(t *testing.T) {
		e := newEnv(t, options{})
		var ran atomic.Int64
		e.srv.HandleUnchecked("GET /debug/open", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			ran.Add(1)
			_, _ = io.WriteString(w, "open to all")
		}))
		if bad := uncheckedRoutes(t, e); len(bad) != 1 || bad[0] != "GET /debug/open" {
			t.Fatalf("the unchecked route was not caught: %v", bad)
		}
		if n := ran.Load(); n != 0 {
			t.Fatalf("the unchecked route's handler ran %d times", n)
		}
	})
	t.Run("a second connect server outside the path is caught", func(t *testing.T) {
		e := newEnv(t, options{})
		var ran atomic.Int64
		e.srv.MountUnchecked(func(s *connect.Server) {
			s.Register(connect.Method{
				Spec: connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/nopacs.test.Open/Call"},
				Handler: func(context.Context, connect.Spec, connect.ServerStream) error {
					ran.Add(1)
					return nil
				},
			})
		})
		bad := uncheckedRoutes(t, e)
		if len(bad) == 0 {
			t.Fatal("the unchecked connect server was not caught")
		}
		for _, p := range bad {
			if !strings.HasPrefix(p, "/nopacs.test.Open/") {
				t.Fatalf("a checked route was reported: %v", bad)
			}
		}
		if n := ran.Load(); n != 0 {
			t.Fatalf("the unchecked connect server's handler ran %d times", n)
		}
	})
}

// TestUnknownMethodIsChecked guards that a method nobody registered still
// passes the request path: without a key it is refused; with the local
// operator it is UNIMPLEMENTED.
func TestUnknownMethodIsChecked(t *testing.T) {
	e := newEnv(t, options{})
	spec := connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: nopacsv1connect.AccessServiceName + "/NoSuchMethod"}
	spec.Procedure = "/" + spec.Procedure
	err := e.network("").raw.CallUnary(e.ctx, spec, &emptyMsg{}, &emptyMsg{})
	if code, _ := codeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("an unknown method without a key: %v", err)
	}
	err = e.local().raw.CallUnary(e.ctx, spec, &emptyMsg{}, &emptyMsg{})
	if code, _ := codeOf(err); code != connect.CodeUnimplemented {
		t.Fatalf("an unknown method for the local operator: %v", err)
	}
	// A call to an unknown service: refused before it is routed without a
	// key, and answered by the server itself for an authenticated caller.
	err = e.network("").raw.CallUnary(e.ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/no.such.Service/Call"}, &emptyMsg{}, &emptyMsg{})
	if code, _ := codeOf(err); code != connect.CodeUnauthenticated {
		t.Fatalf("an unknown service without a key: %v", err)
	}
	err = e.local().raw.CallUnary(e.ctx, connect.Spec{StreamType: connect.StreamTypeUnary, Procedure: "/no.such.Service/Call"}, &emptyMsg{}, &emptyMsg{})
	if code, _ := codeOf(err); code != connect.CodeUnimplemented {
		t.Fatalf("an unknown service for the local operator: %v", err)
	}
}

// emptyMsg is an empty protobuf message for calls whose content does not
// matter.
type emptyMsg = healthStatus
