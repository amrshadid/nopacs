package output

import (
	"strings"

	v1 "github.com/amrshadid/nopacs/gen/nopacs/v1"
)

// QuarantineWarning words the warning a tenant's quarantine stands under, for
// a status: which setting is passed and by how much. It answers "" when no
// warning stands (or the server sent none), so a caller shows a line only when
// there is something to say. The commands and the interactive program share it,
// so a warning is never worded two ways.
func QuarantineWarning(q *v1.QuarantineWarnings) string {
	var parts []string
	if q.GetAgePassed() {
		parts = append(parts, "the oldest file has been held "+Span(q.GetOldestAge().AsDuration())+", past quarantine.warn-age")
	}
	if q.GetBytesPassed() {
		parts = append(parts, Human(q.GetHeldBytes())+" held, past quarantine.warn-bytes")
	}
	if len(parts) == 0 {
		return ""
	}
	return "WARNING: " + strings.Join(parts, "; ") + ". Release or reject what is held; nothing is removed unless a quarantine retention rule says so"
}
