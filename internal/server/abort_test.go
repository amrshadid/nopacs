package server_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	nopacsv1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/server"
)

// cutAnswer is a panic a handler aborts its answer with: http.ErrAbortHandler
// with the cause of the abort.
type cutAnswer struct{ cause error }

func (c cutAnswer) Error() string   { return "cut: " + c.cause.Error() }
func (c cutAnswer) Unwrap() []error { return []error{http.ErrAbortHandler, c.cause} }

// TestAbortedAnswerIsLoggedAsAFailure guards an answer a handler aborts
// after it began, by a panic that wraps http.ErrAbortHandler with its
// cause: the client sees the body cut short, net/http closes the
// connection quietly (no "panic serving" with a stack in the log), and the
// request's log line names the cause's code and error, never "ok". A
// plain http.ErrAbortHandler is let through as quietly.
func TestAbortedAnswerIsLoggedAsAFailure(t *testing.T) {
	cause := fault.New(fault.DataLoss, "the scan's metadata file is missing from the storage")
	h := func(_ context.Context, w http.ResponseWriter, r *http.Request) error {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 64<<10))
		if r.PathValue("subject") == "plain" {
			panic(http.ErrAbortHandler)
		}
		panic(cutAnswer{cause: cause})
	}
	e := newEnv(t, options{patterns: []server.PatternRoute{
		{Pattern: downloadPattern, Need: readerOnPathTenant, Handler: h},
		{Pattern: subtreePattern, Need: readerOnPathTenant, Handler: server.NotImplemented},
	}})
	e.makeTenants("lab")
	reader := "Bearer " + e.makeKey(nopacsv1.Role_ROLE_READER, &nopacsv1.Scope{Kind: &nopacsv1.Scope_Tenant{Tenant: "lab"}})
	hc := e.tlsClient(false)
	for _, subject := range []string{"cut", "plain"} {
		req, err := http.NewRequest(http.MethodGet, e.netURL+"/tenants/lab/instances/"+subject+"/E01/T1/1.2.3", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", reader)
		resp, err := hc.Do(req)
		if err != nil {
			t.Fatalf("%s: the answer did not begin: %v", subject, err)
		}
		_, rerr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK || rerr == nil {
			t.Errorf("%s: %d, read error %v; want 200 cut short", subject, resp.StatusCode, rerr)
		}
	}
	log := e.log.String()
	var cut, plain string
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, `"duration_ms"`) || !strings.Contains(line, downloadPattern) {
			continue
		}
		if strings.Contains(line, `"code":"data_loss"`) {
			cut = line
		} else {
			plain = line
		}
	}
	if cut == "" || !strings.Contains(cut, "metadata file is missing") {
		t.Errorf("the aborted answer's log line is %q, want data_loss with its cause:\n%s", cut, log)
	}
	if plain == "" || strings.Contains(plain, `"code":"ok"`) {
		t.Errorf("the plainly aborted answer's log line is %q, want a failure:\n%s", plain, log)
	}
	if strings.Contains(log, "panic serving") || strings.Contains(log, "panic in a handler") {
		t.Errorf("an abort was logged as a panic:\n%s", log)
	}
}
