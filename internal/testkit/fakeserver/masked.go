package fakeserver

import "github.com/amrshadid/nopacs/internal/fault"

// The refusals of a masked key, worded as the real server words them: SetCallerMasked
// makes the caller one, and the handlers that refuse such a key answer these. Only
// the flows are exercised, not the server's rules, which the API's own tests hold.
const (
	// messageMaskedOriginals answers a masked key that asks for a value not in
	// its archived, de-identified form.
	messageMaskedOriginals = "a masked key is never answered an original value"
	// messageMaskedDestination answers a masked key that asks to choose where
	// archived data goes.
	messageMaskedDestination = "a masked key may not choose where archived data goes; a person does"
	// messageMaskedWrite answers a masked key that asks to write values into
	// archived files.
	messageMaskedWrite = "a masked key may not write values into archived files; a person does"
	// messageMaskedBulk answers a masked key that asks to delete in bulk.
	messageMaskedBulk = "a masked key may not delete in bulk; a person does"
)

// refuseMasked returns the refusal with message when the caller is a masked key,
// and nil otherwise. The caller holds s.mu.
func (s *Server) refuseMasked(message string) error {
	if s.masked {
		return fault.New(fault.Denied, message)
	}
	return nil
}
