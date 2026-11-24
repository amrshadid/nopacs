package send

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// stepsOf writes a test's steps as step=outcome, with the fault of a
// failed one.
func stepsOf(steps []TestStep) string {
	var b strings.Builder
	for _, s := range steps {
		b.WriteString(s.Step + "=" + s.Outcome)
		if s.Fault != "" {
			b.WriteString("(" + string(s.Fault) + ")")
		}
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// TestTestSender guards TestSender's steps (sender.proto): a DIMSE peer
// passes resolve, connect, the association, the C-ECHO and the release,
// with TLS skipped; one that listens nowhere fails at connect and skips the
// rest; a stow peer passes the search for studies (GET <url>/studies?limit=1
// asking for DICOM JSON) with its HTTP status in the detail; one that
// refuses the credential fails it as rejected; a URL that names no
// DICOMweb service (404), one that answers a redirect, and one that
// answers a server error each fail it, saying why. The health of the
// sender is not changed.
func TestTestSender(t *testing.T) {
	f := newFixture(t)
	f.inst.plain = true
	peer := &dimsePeer{}
	up := f.addDIMSE("up", startPeer(t, peer))
	down := f.addDIMSE("down", freePort(t))
	web := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/moved/studies":
			http.Redirect(w, r, "/dicom-web/studies", http.StatusMovedPermanently)
			return
		case "/broken/studies":
			http.Error(w, "down", http.StatusBadGateway)
			return
		}
		if r.Method != http.MethodGet || r.URL.Path != "/dicom-web/studies" || r.URL.RawQuery != "limit=1" ||
			r.Header.Get("Accept") != "application/dicom+json" {
			http.Error(w, "no", http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer token-xyz" {
			http.Error(w, "who", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(web.Close)
	stow := f.addStow("stow", web.URL+"/dicom-web")
	wrong := f.addStow("wrong", web.URL+"/dicom-web", func(s *tenant.Sender) { s.STOW.SealedCredential = []byte("other") })
	typo := f.addStow("typo", web.URL+"/dicom-wbe")
	moved := f.addStow("moved", web.URL+"/moved")
	broken := f.addStow("broken", web.URL+"/broken")
	s := f.start(f.config())
	// The tenant's first reading registers its senders (after it gives
	// back the send units a stopped process left, a write on the tenant's
	// database); a health read before it has no sender to answer for, so
	// "a test does not change the health" is only checked once it has one.
	waitFor(t, 10*time.Second, "the tenant's first reading of its senders", func() bool {
		_, okUp := s.Health("lab", "up")
		_, okDown := s.Health("lab", "down")
		return okUp && okDown
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, tc := range []struct {
		row    tenant.Sender
		want   string
		detail string
	}{
		{up, "resolve=passed connect=passed tls=skipped associate=passed request=passed release=passed", ""},
		{down, "resolve=passed connect=failed(unreachable) tls=skipped associate=skipped request=skipped release=skipped", ""},
		{stow, "resolve=passed connect=passed tls=skipped associate=skipped request=passed release=skipped", "HTTP 200"},
		{wrong, "resolve=passed connect=passed tls=skipped associate=skipped request=failed(rejected) release=skipped",
			"HTTP 401: " + phraseCredential},
		{typo, "resolve=passed connect=passed tls=skipped associate=skipped request=failed(wrong_answer) release=skipped",
			"HTTP 404: the URL does not name a DICOMweb service"},
		{moved, "resolve=passed connect=passed tls=skipped associate=skipped request=failed(wrong_answer) release=skipped",
			"HTTP 301: " + phraseRedirect},
		{broken, "resolve=passed connect=passed tls=skipped associate=skipped request=failed(wrong_answer) release=skipped",
			"HTTP 502: " + phraseServerError},
	} {
		steps, err := s.TestSender(ctx, "lab", tc.row)
		if err != nil {
			t.Fatalf("%s: %v", tc.row.Name, err)
		}
		if got := stepsOf(steps); got != tc.want {
			t.Errorf("%s: steps %s, want %s", tc.row.Name, got, tc.want)
		}
		if tc.detail != "" && steps[4].Detail != tc.detail {
			t.Errorf("%s: the request's detail %q, want %q", tc.row.Name, steps[4].Detail, tc.detail)
		}
	}
	if peer.arrivals() != 0 {
		t.Errorf("a test sent %d instances", peer.arrivals())
	}
	for _, name := range []string{"up", "down"} {
		if h, ok := s.Health("lab", name); !ok || h.State != tenant.SenderUnknown {
			t.Errorf("%s: health %+v after a test, want unknown", name, h)
		}
	}
}
