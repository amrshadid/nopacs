package tenanttest

import (
	"strings"
	"testing"

	"github.com/amrshadid/nopacs/internal/fault"
)

// CheckErrorHidesSecrets fails tb when err, an error a driver returned from
// opening or using a tenant's database, carries any of secrets (an address,
// a path, a host, a user, a password, in any form the driver might write
// them) in its caller-safe message (fault.Message) or in its full text
// (Error). The registry shows a driver error's message in a tenant's state
// reason, in the errors it returns and in its log, so a message that holds
// a secret leaks it to every caller; the full text must hold none either,
// since a cause may reach a log a person reads. A driver's tests call it
// with every failure the driver can produce. A nil err fails, since a test
// that meant to produce a failure produced none. Empty secrets are ignored.
func CheckErrorHidesSecrets(tb testing.TB, err error, secrets ...string) {
	tb.Helper()
	if err == nil {
		tb.Error("no error to check: the failure was not produced")
		return
	}
	for _, s := range secrets {
		if s == "" {
			continue
		}
		if strings.Contains(fault.Message(err), s) {
			tb.Errorf("the error's message %q holds %q", fault.Message(err), s)
		}
		if strings.Contains(err.Error(), s) {
			tb.Errorf("the error's text %q holds %q", err.Error(), s)
		}
	}
}
