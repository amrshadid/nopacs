package api

import (
	"strings"
	"testing"
	"time"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// TestJobFailureCodesMatchTheContract guards the mapping of a job failure's
// code: every value the contract's JobFailureCode defines is a code the
// tenant records, and each recorded code is answered as its own value,
// never as UNSPECIFIED, so a client never reads a refusal the server named
// as one it cannot name.
func TestJobFailureCodesMatchTheContract(t *testing.T) {
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	seen := 0
	for n, name := range v1.JobFailureCode_name {
		if v1.JobFailureCode(n) == v1.JobFailureCode_JOB_FAILURE_CODE_UNSPECIFIED {
			continue
		}
		code := tenant.JobFailureCode(strings.ToLower(strings.TrimPrefix(name, "JOB_FAILURE_CODE_")))
		f := tenant.JobFailure{SOPInstanceUID: "2.25.1", Code: code, Time: at}
		if err := f.Validate(); err != nil {
			t.Errorf("the contract's %s is not a code the tenant records: %v", name, err)
			continue
		}
		if got := jobFailureMessage(f).GetCode(); got != v1.JobFailureCode(n) {
			t.Errorf("the code %s is answered as %s, want %s", code, got, name)
		}
		seen++
	}
	if got := jobFailureMessage(tenant.JobFailure{SOPInstanceUID: "2.25.1", Code: tenant.FailureMetaUnreadable, Time: at}).GetCode(); got != v1.JobFailureCode_JOB_FAILURE_CODE_META_UNREADABLE || seen < 7 {
		t.Errorf("meta_unreadable is answered as %s with %d codes seen, want JOB_FAILURE_CODE_META_UNREADABLE of at least 7", got, seen)
	}
}

// TestJobFailureSaysWhenItsStatusIsHTTP guards peer_status_http: a stow
// request refused whole carries its HTTP status and says so; a DICOM status,
// from either kind of peer, does not, so a client never shows 404 as 0194.
func TestJobFailureSaysWhenItsStatusIsHTTP(t *testing.T) {
	at := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	whole := jobFailureMessage(tenant.JobFailure{SOPInstanceUID: "2.25.1", Code: tenant.FailurePeerRefused, PeerStatus: 415,
		Detail: tenant.FailureDetailWholeRequest, Time: at})
	if !whole.GetPeerStatusHttp() || whole.GetPeerStatus() != 415 {
		t.Errorf("a request refused whole is answered %v", whole)
	}
	for _, f := range []tenant.JobFailure{
		{SOPInstanceUID: "2.25.2", Code: tenant.FailurePeerRefused, PeerStatus: 0xA700, Detail: "refused by the peer", Time: at},
		{SOPInstanceUID: "2.25.3", Code: tenant.FailurePeerWarning, PeerStatus: 0xB000, Detail: tenant.FailureDetailWholeRequest, Time: at},
	} {
		if m := jobFailureMessage(f); m.GetPeerStatusHttp() {
			t.Errorf("a DICOM status is answered as HTTP: %v", m)
		}
	}
}
