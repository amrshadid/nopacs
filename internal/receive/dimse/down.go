package dimse

import (
	"errors"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// A listener that cannot start says why with a reason of a receiver's
// health (tenant.Down*) and a phrase of the server's fixed set
// (tenant.Phrase*). The text of the system's or the library's error is
// never the phrase: it may name a path or another program.

// DownError is why a listener could not start, as a receiver's health says
// it: a reason and a fixed phrase. Its cause is kept for the server's log,
// never shown.
type DownError struct {
	// Reason is the health's down reason.
	Reason tenant.ReceiverDownReason
	// Detail is the health's phrase for it.
	Detail string
	// Err is the cause.
	Err error
}

// Error returns the phrase.
func (e *DownError) Error() string { return e.Detail }

// Unwrap returns the cause.
func (e *DownError) Unwrap() error { return e.Err }

// Down returns the reason and the phrase, so a caller that knows nothing of
// this package reads them through an interface.
func (e *DownError) Down() (tenant.ReceiverDownReason, string) { return e.Reason, e.Detail }

// listenError returns the DownError of a port the system did not give.
func listenError(err error) *DownError {
	switch {
	case addrInUse(err):
		return &DownError{Reason: tenant.DownAddressInUse, Detail: tenant.PhraseAddressInUse, Err: err}
	case permissionDenied(err):
		return &DownError{Reason: tenant.DownPermissionDenied, Detail: tenant.PhrasePermissionDenied, Err: err}
	case addrNotAvailable(err):
		return &DownError{Reason: tenant.DownAddressNotAvailable, Detail: tenant.PhraseAddressNotAvailable, Err: err}
	}
	return &DownError{Reason: tenant.DownOther, Detail: tenant.PhraseCouldNotStart, Err: err}
}

// isErrno reports whether err holds one of the errors given.
func isErrno(err error, targets ...error) bool {
	for _, t := range targets {
		if errors.Is(err, t) {
			return true
		}
	}
	return false
}
