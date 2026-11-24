// Package codes is the one table that pairs an API error code with a fault
// kind, so the server and the command line cannot disagree about what a code
// means: the server turns a kind into a code, the command line turns the code
// back into a kind and, through fault.Kind.ExitCode, into an exit status.
//
// The table follows the contract's list of error kinds in
// api/proto/nopacs/v1/access.proto, and each pair is one to one:
//
//	INVALID_ARGUMENT     invalid          exit 2
//	UNAUTHENTICATED      unauthenticated  exit 3
//	PERMISSION_DENIED    denied           exit 3
//	NOT_FOUND            not found        exit 4
//	ALREADY_EXISTS       conflict         exit 1
//	UNAVAILABLE          unavailable      exit 1
//	FAILED_PRECONDITION  precondition     exit 5
//	OUT_OF_RANGE         out of range     exit 6
//	RESOURCE_EXHAUSTED   exhausted        exit 7
//	INTERNAL             internal         exit 1
//
// RESOURCE_EXHAUSTED carries every bound of the server the contract names: a
// message over the size limit, and too many streams open at once.
//
// The codes the contract says any method may answer without naming a kind are
// mapped on the receiving side only, because a server never answers them from
// a fault kind: DEADLINE_EXCEEDED and CANCELED are unavailable (exit 1, a
// failure that may pass on a later try). Every other code, UNKNOWN,
// UNIMPLEMENTED, DATA_LOSS and any a newer server adds, is internal: an
// unknown code is a failure, exit 1, and never a usage error or a refusal.
//
// This package must never log or print, and must never put into a message
// anything but the message that fault carries for the caller: an error that
// crosses the API is shown to a person on the other side.
package codes
