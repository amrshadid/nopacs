package dimse

import (
	"time"

	"github.com/amrshadid/nopacs/internal/receive"
)

// SetAnyPort makes l listen on a port the system picks, so tests never
// contend for one.
func SetAnyPort(l *Listener) { l.seams.anyPort = true }

// SetNow sets the clock a certificate's validity is read against.
func SetNow(l *Listener, now func() time.Time) { l.seams.now = now }

// SetBeforeAccept sets what runs in a store just before the Sink is asked.
func SetBeforeAccept(l *Listener, f func()) { l.seams.beforeAccept = f }

// StatusOf exposes the mapping of an outcome to a C-STORE status.
func StatusOf(out receive.Outcome, werr error) uint16 { return statusOf(out, werr) }
