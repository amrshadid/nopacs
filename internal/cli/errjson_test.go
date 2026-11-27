package cli_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/gen/nopacs/v1/nopacsv1connect"
	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
)

// errorDoc is the error document --json writes, as a program reads it.
type errorDoc struct {
	SchemaVersion int `json:"schemaVersion"`
	Error         struct {
		Message           string            `json:"message"`
		ExitCode          int               `json:"exitCode"`
		Code              string            `json:"code"`
		Reason            string            `json:"reason"`
		Domain            string            `json:"domain"`
		Metadata          map[string]string `json:"metadata"`
		RetryAfterSeconds int64             `json:"retryAfterSeconds"`
		RequestID         string            `json:"requestId"`
	} `json:"error"`
}

// decodeErrorJSON reads the one line --json writes on standard error for a
// failed command, and fails the test unless it is exactly one JSON object
// on one line with no field the document does not name.
func decodeErrorJSON(t *testing.T, stderr string) errorDoc {
	t.Helper()
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Fatalf("standard error is not one line: %q", stderr)
	}
	var doc errorDoc
	dec := json.NewDecoder(strings.NewReader(stderr))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("standard error %q is not the error document: %v", stderr, err)
	}
	if doc.SchemaVersion != 1 {
		t.Errorf("schemaVersion %d, want 1", doc.SchemaVersion)
	}
	return doc
}

// expectJSONError fails t unless res exited with exit, wrote nothing on
// standard output, and wrote on standard error the error document of
// message (the line without --json, less "nopacs: ") and reason.
func expectJSONError(t *testing.T, res result, exit int, message, reason string) {
	t.Helper()
	if res.code != exit || res.stdout != "" {
		t.Errorf("exit %d, standard output %q; want exit %d and nothing", res.code, res.stdout, exit)
	}
	e := decodeErrorJSON(t, res.stderr).Error
	if e.Message != message || e.Reason != reason || e.ExitCode != exit || e.Domain != codes.Domain || e.Code == "" {
		t.Errorf("error document %+v, want message %q, reason %s, exit %d", e, message, reason, exit)
	}
}

// TestJSONErrorOfAFlagCobraRefuses guards that --json is honored when cobra
// refuses the arguments before it reads the flag: an unknown flag with
// --json, or --json=true, before or after it, writes the error document
// (INVALID_ARGUMENT, exit 2) and nothing on standard output; --json=false
// and no --json write the plain line.
func TestJSONErrorOfAFlagCobraRefuses(t *testing.T) {
	r := newRig(t)
	for _, args := range [][]string{
		{"tenant", "list", "--no-such-flag", "--json"},
		{"--json=true", "tenant", "list", "--no-such-flag"},
	} {
		res := r.run(args...)
		if res.code != 2 || res.stdout != "" {
			t.Errorf("%v: exit %d, standard output %q", args, res.code, res.stdout)
		}
		e := decodeErrorJSON(t, res.stderr).Error
		if e.Reason != "INVALID_ARGUMENT" || e.Code != "invalid_argument" || e.ExitCode != 2 || !strings.Contains(e.Message, "no-such-flag") {
			t.Errorf("%v: error document %+v", args, e)
		}
	}
	for _, args := range [][]string{{"tenant", "list", "--no-such-flag", "--json=false"}, {"tenant", "list", "--no-such-flag"}} {
		if res := r.run(args...); res.code != 2 || !strings.HasPrefix(res.stderr, "nopacs: ") {
			t.Errorf("%v: exit %d, standard error %q; want the plain line", args, res.code, res.stderr)
		}
	}
}

// TestJSONErrorCarriesTheServersReason guards the error --json writes when
// the server answered with a reason: one JSON object on standard error and
// nothing on standard output, holding the line's message, the exit status,
// the server's code, the reason and domain of its google.rpc.ErrorInfo, its
// retry delay in whole seconds rounded up, and the request id; for a reason
// that travels only in the details, the same reason. Without --json the
// line is unchanged.
func TestJSONErrorCarriesTheServersReason(t *testing.T) {
	r := replicaRig(t)
	r.fake.FailWithReason(nopacsv1connect.ReplicaServiceBackfillReplicaProcedure, fault.Unavailable, "the storage is switching", fault.ReasonStorageSwitching, 1500*time.Millisecond)
	res := r.run("--json", "replica", "backfill", "lab1", "copy")
	if res.code != 1 || res.stdout != "" {
		t.Fatalf("%+v, want exit 1 and nothing on standard output", res)
	}
	e := decodeErrorJSON(t, res.stderr).Error
	if e.Reason != fault.ReasonStorageSwitching || e.Domain != codes.Domain || e.Code != "unavailable" || e.ExitCode != 1 ||
		e.RetryAfterSeconds != 2 || e.RequestID == "" || !strings.Contains(e.Message, "replica backfill: STORAGE_SWITCHING: the storage is switching (try again in 2s)") {
		t.Errorf("error document %+v", e)
	}

	r = replicaRig(t)
	r.fake.FailWithDetailOnly(nopacsv1connect.ReplicaServiceBackfillReplicaProcedure, fault.Precondition, "a hold covers it", fault.ReasonHeld, 0)
	res = r.run("replica", "backfill", "lab1", "copy", "--json")
	e = decodeErrorJSON(t, res.stderr).Error
	if res.code != 5 || e.Reason != fault.ReasonHeld || e.Code != "failed_precondition" || e.ExitCode != 5 || e.RetryAfterSeconds != 0 {
		t.Errorf("a reason in the details alone: exit %d, document %+v", res.code, e)
	}

	plain := r.run("replica", "backfill", "lab1", "copy")
	if !strings.HasPrefix(plain.stderr, "nopacs: replica backfill: HELD: a hold covers it") || strings.Count(plain.stderr, "\n") != 1 {
		t.Errorf("without --json: %q, want the one line", plain.stderr)
	}
}

// TestJSONErrorWithoutAReason guards the error --json writes when no case
// applies: a server answer with its code's general reason and a usage
// mistake the command line finds itself carry the general reason of their
// code, so every error document has a reason; and a general reason is
// never written into the plain line, which stays as the server worded it.
// An answer with no ErrorInfo at all is TestErrorJSONWithoutErrorInfo's.
func TestJSONErrorWithoutAReason(t *testing.T) {
	r := replicaRig(t)
	r.fake.FailWith(nopacsv1connect.ReplicaServiceBackfillReplicaProcedure, connect.CodeNotFound, "no storage copy")
	e := decodeErrorJSON(t, r.run("--json", "replica", "backfill", "lab1", "copy").stderr).Error
	if e.Reason != "NOT_FOUND" || e.Code != "not_found" || e.ExitCode != 4 || e.Domain != codes.Domain || e.Metadata != nil {
		t.Errorf("an answer with no ErrorInfo: %+v", e)
	}

	r.fake.FailWithDetailOnly(nopacsv1connect.ReplicaServiceBackfillReplicaProcedure, fault.NotFound, "no storage copy", "NOT_FOUND", 0)
	if line := r.run("replica", "backfill", "lab1", "copy").stderr; strings.Contains(line, "NOT_FOUND") || !strings.Contains(line, "no storage copy") {
		t.Errorf("the general reason is written into the line: %q", line)
	}

	res := r.run("--json", "replica", "backfill", "lab1")
	e = decodeErrorJSON(t, res.stderr).Error
	if res.code != 2 || e.Reason != "INVALID_ARGUMENT" || e.Code != "invalid_argument" || e.ExitCode != 2 || e.RequestID != "" {
		t.Errorf("a usage mistake: exit %d, document %+v", res.code, e)
	}
}
