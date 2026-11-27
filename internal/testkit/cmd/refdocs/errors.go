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

// reasonMeanings says in a sentence when each reason of fault's table is
// answered. renderErrors walks the table and refuses a reason with no
// sentence.
var reasonMeanings = map[string]string{
	fault.ReasonTenantMoved:      "A change to a tenant an export handed to another installation; it is read only here.",
	fault.ReasonTenantFenced:     "A change to, or a read of the data of, a tenant whose storages name another installation or a later generation; it serves no data here.",
	fault.ReasonHeld:             "A delete of data a hold covers.",
	fault.ReasonRetentionMinimum: "A person's delete of data a retention rule keeps at least until a time to come.",
	fault.ReasonRelabeled:        "A subject, session or scan asked for by a label a relabel moved, asked by a caller who may read the tenant's corrections.",
	fault.ReasonTenantRestoring:  "The tenant is being reconciled after a restore; retry with a backoff.",
	fault.ReasonNotOnline:        "The storage holds the object in a tier it cannot read at once, and no copy in step on another storage is online. A retry delay comes only while a recall runs.",
	fault.ReasonStorageSwitching: "A write to a storage being switched to its replica waits for the swap; it carries a retry delay.",
	fault.ReasonCursorExpired:    "An event cursor before the oldest event kept; start over from a status.",
	fault.ReasonHistoryChanged:   "An event cursor of another history of the tenant's feed, or past its newest event; start over from a status.",
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
	b.WriteString("\n## Reasons\n\n" +
		"Every error the server answers carries a `google.rpc.ErrorInfo` detail with the domain `" + codes.Domain + "` and a\n" +
		"reason, a fixed word a program branches on. A failure that is one of the cases below carries that case's reason,\n" +
		"which also begins its message, and the metadata the case names when it knows them. Any other failure carries\n" +
		"its code's general reason, the code's name above (`NOT_FOUND`, `UNAVAILABLE`, ...), which says no more than the\n" +
		"code. A reason a client does not know is one added later: it treats it as its code alone. How to read them, and\n" +
		"the retry delays some carry: [Reasons and retry delays](../api/errors.md).\n\n")
	b.WriteString("| Reason | Kind | API code | Metadata | When |\n|---|---|---|---|---|\n")
	for _, c := range fault.Cases() {
		meaning, ok := reasonMeanings[c.Reason]
		if !ok {
			return "", fmt.Errorf("the reason %s has no sentence in reasonMeanings", c.Reason)
		}
		meta := "none"
		if len(c.Metadata) > 0 {
			meta = "`" + strings.Join(c.Metadata, "`, `") + "`"
		}
		fmt.Fprintf(&b, "| `%s` | %s | `%s` | %s | %s |\n", c.Reason, c.Kind, codes.General(codes.Code(c.Kind)), meta, cell(meaning))
	}
	return b.String(), nil
}
