package sqlkit

import (
	"strings"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// ReclaimKinds checks the kinds of tenant.WorkReclaim.ReclaimWorkOfKinds
// and returns the one statement both drivers run for it, with param(i)
// written for its i-th parameter (from 1), and the kinds as the parameters
// that follow the two times it takes first: due, then updated, both the
// reclaim's time. A kind named twice is one parameter. The statement's
// length follows the kinds, so a driver writes its own parameters here
// rather than through Rebind, which keeps a fixed set of statements.
func ReclaimKinds(kinds []string, param func(i int) string) (string, []any, error) {
	if err := tenant.ValidateReclaimKinds(kinds); err != nil {
		return "", nil, err
	}
	seen := make(map[string]bool, len(kinds))
	args := make([]any, 0, len(kinds))
	var b strings.Builder
	b.WriteString("UPDATE work SET state = 'queued', owner = '', due = ")
	b.WriteString(param(1))
	b.WriteString(", updated = ")
	b.WriteString(param(2))
	b.WriteString(" WHERE state = 'leased' AND kind IN (")
	for _, k := range kinds {
		if seen[k] {
			continue
		}
		seen[k] = true
		if len(args) > 0 {
			b.WriteString(", ")
		}
		args = append(args, k)
		b.WriteString(param(2 + len(args)))
	}
	b.WriteString(")")
	return b.String(), args, nil
}
