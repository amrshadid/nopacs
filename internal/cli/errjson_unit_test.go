package cli

import (
	"encoding/json"
	"strings"
	"testing"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
)

// TestJSONAsked guards how the arguments are read for --json when cobra
// could not read them: the flag alone or with a true value asks, a false or
// malformed value does not, the last one given wins, and nothing after "--"
// counts.
func TestJSONAsked(t *testing.T) {
	for args, want := range map[string]bool{
		"tenant list --json":              true,
		"--json=true tenant list":         true,
		"--json=1 x":                      true,
		"tenant list --json=false":        false,
		"tenant list --json=maybe":        false,
		"tenant list --json --json=false": false,
		"tenant list":                     false,
		"tenant list -- --json":           false,
		"tenant list --jsonx":             false,
	} {
		if got := jsonAsked(strings.Fields(args)); got != want {
			t.Errorf("jsonAsked(%q) = %v, want %v", args, got, want)
		}
	}
}

// TestErrorJSONWithoutErrorInfo guards the error document of an answer
// that carries no ErrorInfo (an older server): the general reason of the
// code it was answered with, and no metadata.
func TestErrorJSONWithoutErrorInfo(t *testing.T) {
	ce := connect.NewError(connect.CodeNotFound, "no storage copy")
	var doc errorDocument
	if jerr := json.Unmarshal(errorJSON("x", fault.Wrap(fault.NotFound, ce, "no storage copy")), &doc); jerr != nil {
		t.Fatal(jerr)
	}
	if e := doc.Error; e.Reason != "NOT_FOUND" || e.Code != "not_found" || e.ExitCode != 4 || e.Metadata != nil {
		t.Errorf("document %+v", doc)
	}
}

// TestErrorJSONMetadata guards that the error document carries the
// ErrorInfo's metadata as the server sent it, for a reason whose case
// gives facts (HELD with the hold's id), and the message it was given.
func TestErrorJSONMetadata(t *testing.T) {
	ce := codes.ToConnect(codes.WithReason(fault.New(fault.Precondition, "a hold covers it"), fault.ReasonHeld, map[string]string{"hold": "hold_1"}, 0))
	err := fault.Wrap(fault.Precondition, ce, ce.Message())
	var doc errorDocument
	if jerr := json.Unmarshal(errorJSON("subject delete: HELD: a hold covers it", err), &doc); jerr != nil {
		t.Fatal(jerr)
	}
	e := doc.Error
	if doc.SchemaVersion != 1 || e.Reason != fault.ReasonHeld || e.Metadata["hold"] != "hold_1" || len(e.Metadata) != 1 ||
		e.Code != "failed_precondition" || e.ExitCode != 5 || e.Message != "subject delete: HELD: a hold covers it" {
		t.Errorf("document %+v", doc)
	}
}
