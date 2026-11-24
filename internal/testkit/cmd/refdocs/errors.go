package main

import (
	"fmt"
	"strings"

	connect "connectrpc.com/connect/v2"

	"github.com/amrshadid/nopacs/internal/api/codes"
	"github.com/amrshadid/nopacs/internal/fault"
)

// kindMeanings says in a sentence what each error kind is. renderErrors walks
// every kind the fault package defines and refuses one with no sentence.
var kindMeanings = map[fault.Kind]string{
	fault.Internal:        "A failure of the archive itself, or a cause the caller must not be told about. The message says nothing of the cause; it is in the server's log under the request id.",
	fault.Invalid:         "The request is wrong as written: a malformed name, a missing argument, a value out of range. Sending it again unchanged fails again.",
	fault.Unauthenticated: "No key, or a key that is malformed, unknown, expired, revoked or suspended. All of these get one answer, so a caller cannot tell which.",
	fault.Denied:          "The key is good but holds no grant with the role the method needs. A tenant that does not exist is answered the same way to a caller who may not list tenants.",
	fault.NotFound:        "The thing named does not exist, among the things the caller may see.",
	fault.Conflict:        "The change would collide with what exists, such as a second tenant with the same name.",
	fault.Unavailable:     "A database or a storage the request needs does not answer, or a tenant is not ready. Sending it again later may succeed.",
	fault.Precondition:    "What the request names is in a state that forbids the change: a storage that still holds files, a port the installation has not allotted. It fails again until that state changes.",
	fault.OutOfRange:      "A position that can no longer be answered, such as an event cursor older than the oldest event kept. Start over from a status.",
	fault.Exhausted:       "A bound of the server is reached: a message over the size limit, too many streams or reads open. Send less, or later.",
	fault.DataLoss:        "Archived data is missing or damaged: a file gone from its storage, or bytes that do not match the SHA-256 recorded when they were archived. The message names the scan to mend.",
}

// httpStatus is the HTTP status the Connect protocol carries each code with
// (connectrpc.com/docs/protocol, "Error codes"). The codes package maps kinds
// to codes; the HTTP status of a code is the protocol's, not NoPACs'.
var httpStatus = map[connect.Code]int{
	connect.CodeCanceled:           499,
	connect.CodeUnknown:            500,
	connect.CodeInvalidArgument:    400,
	connect.CodeDeadlineExceeded:   504,
	connect.CodeNotFound:           404,
	connect.CodeAlreadyExists:      409,
	connect.CodePermissionDenied:   403,
	connect.CodeResourceExhausted:  429,
	connect.CodeFailedPrecondition: 400,
	connect.CodeAborted:            409,
	connect.CodeOutOfRange:         400,
	connect.CodeUnimplemented:      501,
	connect.CodeInternal:           500,
	connect.CodeUnavailable:        503,
	connect.CodeDataLoss:           500,
	connect.CodeUnauthenticated:    401,
}

// renderErrors writes the kind table and the table of every code.
func renderErrors(string) (string, error) {
	var b strings.Builder
	b.WriteString("Every failure the API answers has a kind. The kind decides the API code the server answers with, and the\n" +
		"exit status of the `nopacs` command that received it. A caller reads the code; a script reads the exit status.\n" +
		"What each method may answer, and the reasons some kinds carry in an error detail, are in the comments of the\n" +
		"contract in `api/proto/nopacs/v1`.\n\n## Kinds\n\n")
	b.WriteString("| Kind | API code | HTTP status | Exit status | What it means |\n|---|---|---|---|---|\n")
	for k := fault.Internal; ; k++ {
		if strings.HasPrefix(k.String(), "Kind(") {
			break
		}
		meaning, ok := kindMeanings[k]
		if !ok {
			return "", fmt.Errorf("the error kind %q has no sentence in kindMeanings", k)
		}
		code := codes.Code(k)
		status, ok := httpStatus[code]
		if !ok {
			return "", fmt.Errorf("the code %s has no HTTP status in httpStatus", code)
		}
		fmt.Fprintf(&b, "| %s | `%s` | %d | %d | %s |\n", k, strings.ToUpper(code.String()), status, k.ExitCode(), cell(meaning))
	}
	b.WriteString("\n## Every code\n\n" +
		"A client may receive any of the sixteen codes of the protocol. This is the kind each one is taken for, and so the exit\n" +
		"status of `nopacs` for it. A code no kind carries is a failure of the call (exit status 1): it is never shown as the\n" +
		"caller's mistake.\n\n")
	b.WriteString("| API code | HTTP status | Taken for the kind | Exit status |\n|---|---|---|---|\n")
	for c := connect.CodeCanceled; c <= connect.CodeUnauthenticated; c++ {
		status, ok := httpStatus[c]
		if !ok {
			return "", fmt.Errorf("the code %s has no HTTP status in httpStatus", c)
		}
		k := codes.Kind(c)
		fmt.Fprintf(&b, "| `%s` | %d | %s | %d |\n", strings.ToUpper(c.String()), status, k, k.ExitCode())
	}
	return b.String(), nil
}
