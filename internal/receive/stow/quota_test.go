package stow

import (
	"context"
	"io"
	"net/http"
	"sync"
	"testing"

	"github.com/amrshadid/nopacs/internal/receive"
)

// quotaSink stands for a pipeline whose tenant has room for room more
// files: it keeps those, and refuses each one after as the pipeline does
// at the tenant's storage quota.
type quotaSink struct {
	mu   sync.Mutex
	room int
}

// Accept implements receive.Sink.
func (s *quotaSink) Accept(_ context.Context, a receive.Arrival) receive.Outcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.room == 0 {
		return receive.Outcome{Status: receive.Refused, Reason: receive.ReasonOverQuota}
	}
	if _, err := io.Copy(io.Discard, a.Body); err != nil {
		return receive.Outcome{Status: receive.Refused, Reason: "not kept"}
	}
	s.room--
	return receive.Outcome{Status: receive.Received}
}

// TestServeAtTheQuotaIs507 guards the answer of a tenant at its storage
// quota: a request none of whose parts was stored for it is answered 507
// (Insufficient Storage), each part listed with A700H (out of resources),
// a temporary failure the client sends again; a request some of whose parts
// were stored before the bound is answered 202 as any part by part, the
// rest listed with A700H.
func TestServeAtTheQuotaIs507(t *testing.T) {
	sink := &quotaSink{room: 1}
	r := newRig(t, 1<<20, 1<<24, func(c *Config) { c.Sink = sink })
	body, ct := multipartBody(t, dicomParts(corpus(t, "ct5n-2062.dcm"), corpus(t, "ct5n-2392.dcm")))
	w, res := r.serve("", body, ct)
	a := readAnswer(t, w.Body.Bytes())
	if w.Code != http.StatusAccepted || res.Stored != 1 || res.Refused != 1 || len(a.failed) != 1 || a.failed[0].reason != ReasonOutOfResources {
		t.Errorf("a request that reached the quota part way: %d %+v %+v, want 202 with one part A700H", w.Code, res, a)
	}
	body, ct = multipartBody(t, dicomParts(corpus(t, "ct5n-2693.dcm"), corpus(t, "ct5n-3023.dcm")))
	w, res = r.serve("", body, ct)
	a = readAnswer(t, w.Body.Bytes())
	if w.Code != http.StatusInsufficientStorage || res.Stored != 0 || res.Refused != 2 || len(a.failed) != 2 ||
		a.failed[0].reason != ReasonOutOfResources || a.failed[1].reason != ReasonOutOfResources {
		t.Errorf("a request at the quota: %d %+v %+v, want 507 with both parts A700H", w.Code, res, a)
	}
}
