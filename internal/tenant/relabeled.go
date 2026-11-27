package tenant

import (
	"context"

	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/fault"
)

// ReasonRelabeled is the reason a lookup by labels a committed relabel
// moved carries, as the API contract names it (access.proto, "not found"):
// a client that holds the old labels is told where the data is now, rather
// than that it is gone. fault's table of reasons holds the word.
const ReasonRelabeled = fault.ReasonRelabeled

// MaxRelabelSteps bounds how many relabels one lookup follows, one after
// another (a subject relabeled, then relabeled again).
const MaxRelabelSteps = 64

// Relabeled is where the relabels of a tenant moved a subject, a session or
// a scan named by labels it no longer has.
type Relabeled struct {
	// Correction is the id of the last relabel followed.
	Correction string
	// To is the labels the data has now, at the depth asked: a subject's
	// alone, a session's with its subject's, or all three for a scan.
	To label.Path
}

// RelabelPointers answers where data asked for by old labels is now.
type RelabelPointers interface {
	// RelabeledTo answers where the committed relabels of the tenant (in
	// the state committed, purging or purged; never one prepared or
	// abandoned) moved the labels p, which name a subject (Session and
	// Scan empty), a session (Scan empty) or a scan. Labels are compared
	// exactly, as every lookup by label is. Each relabel that moved p is a
	// start, newest first, and is followed forward through the relabels
	// committed after it, one after another; the first chain whose end the
	// catalog holds now answers, Correction the last relabel of it. At most
	// MaxRelabelSteps relabels are followed in all. It answers false, and
	// the zero Relabeled, when no relabel moved p, or when no chain ends on
	// anything the catalog holds now (a delete hid the data since): there
	// is then nothing to point to, and the caller answers a bare not found.
	// p's labels that are not valid are Invalid.
	RelabeledTo(ctx context.Context, p label.Path) (Relabeled, bool, error)
}

// ValidateRelabelLookup returns an Invalid error unless p names a subject,
// a session or a scan by valid labels: a subject label, then a session
// label only with it, then a scan label only with both.
func ValidateRelabelLookup(p label.Path) error {
	if !label.Valid(p.Subject) {
		return invalid("a subject label passes the label rule")
	}
	if p.Session != "" && !label.Valid(p.Session) {
		return invalid("a session label passes the label rule")
	}
	if p.Scan != "" && (p.Session == "" || !label.Valid(p.Scan)) {
		return invalid("a scan label passes the label rule and comes with a session label")
	}
	return nil
}

// relabelDepth returns how many levels p names: 1 for a subject, 2 for a session,
// 3 for a scan.
func relabelDepth(p label.Path) int {
	switch {
	case p.Scan != "":
		return 3
	case p.Session != "":
		return 2
	}
	return 1
}

// RelabelMoves reports whether a relabel at level ("subject", "session" or
// "scan") whose old labels are old moves the labels p, and returns p as it
// leaves them: the label at level replaced with to. A relabel of a subject
// moves every session and scan under it; one of a session, the session and
// its scans; one of a scan, the scan alone.
func RelabelMoves(level string, old label.Path, to string, p label.Path) (label.Path, bool) {
	depth := relabelDepth(p)
	switch level {
	case "subject":
		if old.Subject == p.Subject {
			p.Subject = to
			return p, true
		}
	case "session":
		if depth >= 2 && old.Subject == p.Subject && old.Session == p.Session {
			p.Session = to
			return p, true
		}
	case "scan":
		if depth == 3 && old == p {
			p.Scan = to
			return p, true
		}
	}
	return p, false
}
