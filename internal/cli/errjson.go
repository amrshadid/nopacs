package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/cli/client"
	"github.com/amrshadid/nopacs/internal/cli/output"
	"github.com/amrshadid/nopacs/internal/fault"
)

// errorDocument is the error --json writes on standard error, on one line,
// so a program reads why a command failed without parsing a sentence. Its
// reason is the server's google.rpc.ErrorInfo reason when the server
// answered (a case such as HELD or NOT_ONLINE, or a code's general reason
// such as NOT_FOUND), and the general reason of the failure's kind when
// the command failed before or without an answer: the same fixed words
// either way, so a program branches on them alone.
type errorDocument struct {
	SchemaVersion int       `json:"schemaVersion"`
	Error         errorBody `json:"error"`
}

// errorBody is the error itself.
type errorBody struct {
	// Message is the line the command writes without --json.
	Message string `json:"message"`
	// ExitCode is the status the command exits with.
	ExitCode int `json:"exitCode"`
	// Code is the API code, as the Connect protocol names it
	// (failed_precondition, ...): the server's when it answered, else the
	// one that carries the failure's kind.
	Code string `json:"code"`
	// Reason and Domain are the google.rpc.ErrorInfo's; Metadata its facts.
	Reason   string            `json:"reason"`
	Domain   string            `json:"domain"`
	Metadata map[string]string `json:"metadata,omitempty"`
	// RetryAfterSeconds is the server's retry delay, rounded up to whole
	// seconds; absent when it gave none.
	RetryAfterSeconds int64 `json:"retryAfterSeconds,omitempty"`
	// RequestID is the server's id of the call; absent when no answer came.
	RequestID string `json:"requestId,omitempty"`
}

// fail writes the error of a failed command: line as the one line
// (a.line), or with --json the errorDocument of err with line as its
// message.
func (a *app) fail(line string, err error) {
	if !a.opts.json {
		a.line(line)
		return
	}
	_, _ = fmt.Fprintf(a.env.Stderr, "%s\n", errorJSON(output.Clean(line), err))
}

// jsonAsked reports whether args ask for --json, as "--json" or
// "--json=<true>" anywhere before a "--" that ends the flags. cobra refuses an
// unknown flag or a missing argument before it sets the flag, so the error
// of a script that asked for JSON would otherwise be the plain line.
func jsonAsked(args []string) bool {
	asked := false
	for _, a := range args {
		if a == "--" {
			break
		}
		if a == "--json" {
			asked = true
			continue
		}
		if v, ok := strings.CutPrefix(a, "--json="); ok {
			b, err := strconv.ParseBool(v)
			asked = err == nil && b
		}
	}
	return asked
}

// errorJSON returns the errorDocument of err, encoded, with message as its
// message.
func errorJSON(message string, err error) []byte {
	kind := fault.KindOf(err)
	body := errorBody{Message: message, ExitCode: kind.ExitCode(), Domain: codes.Domain}
	code := codes.Code(kind)
	if d, ok := client.DetailOf(err); ok {
		code = d.Code
		body.Reason, body.Metadata = d.Reason, d.Metadata
		if d.Retry > 0 {
			body.RetryAfterSeconds = int64(math.Ceil(d.Retry.Seconds()))
		}
	}
	if body.Reason == "" {
		body.Reason = codes.General(code)
	}
	body.Code = code.String()
	var ce *client.CallError
	if errors.As(err, &ce) {
		body.RequestID = ce.RequestID
	}
	b, jerr := json.Marshal(errorDocument{SchemaVersion: output.SchemaVersion, Error: body})
	if jerr != nil {
		// Every field is a string, a number or a map of strings: this
		// cannot fail, and the exit status still tells the kind.
		return []byte(`{"schemaVersion":1,"error":{"message":"the error could not be written as JSON"}}`)
	}
	return b
}
