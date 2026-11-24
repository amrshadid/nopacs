//go:build interrupt

package interrupt

import (
	"fmt"
	"os"
)

// steps are every step a test build stops at, named scope:kind:step, each
// with what is durable when the process ends there. A correction's steps
// are named by its kind (delete, relabel, replace); a rebuild's by its mode
// (compare, replace). Where a step is reached more than once in one run
// (an object of a purge or of a relabel's copy, a scan of a rebuild), the
// process ends at the first.
var steps = map[string]string{
	"correction:delete:prepared":          "the correction's row prepared, with its operation id and audit entry; no record written",
	"correction:delete:records":           "the signed records written, state prepared, on each storage the target touches; nothing hidden",
	"correction:delete:committed":         "the publishing transaction committed (rows hidden, totals moved, row committed); the records still say prepared",
	"correction:delete:undone":            "the row undone and the rows shown again; the records still say committed",
	"correction:delete:purge-started":     "the row purging and its records rewritten purging; nothing removed",
	"correction:delete:purge-object":      "the first object removed from its storage; what was removed not yet settled in the database",
	"correction:delete:purge-meta":        "an instance delete's scan manifest and metadata written again without the instance; the catalog not yet told",
	"correction:delete:purge-objects":     "every object removed and settled; the rows not yet removed",
	"correction:delete:purged":            "the rows removed and the row purged in one transaction; the records still say purging",
	"correction:relabel:prepared":         "the correction's row prepared; no record written, nothing copied",
	"correction:relabel:records":          "the signed records written, state prepared; nothing copied",
	"correction:relabel:relabel-object":   "the first object copied to its key under the new labels and read back; nothing published",
	"correction:relabel:relabel-metadata": "a scan's metadata written under the new labels; nothing published",
	"correction:relabel:relabel-manifest": "a scan's manifest written under the new labels; nothing published",
	"correction:relabel:copied":           "every scan copied and the count recorded; nothing published",
	"correction:relabel:committed":        "the one publishing transaction committed (the whole target under the new labels); the records still say prepared",
	"correction:relabel:purge-started":    "the row purging and its records rewritten purging; the old tree whole",
	"correction:relabel:purge-object":     "the first object of the old tree removed; not yet settled in the database",
	"correction:relabel:purge-objects":    "every object of the old tree removed and settled",
	"correction:relabel:purged":           "the row purged; the records still say purging",
	"correction:replace:prepared":         "the correction's row prepared; no record written, no new object",
	"correction:replace:records":          "the signed records written, state prepared; no new object",
	"correction:replace:object":           "the new revision written at its own key and read back; manifest and metadata name the old",
	"correction:replace:meta":             "the scan's manifest and metadata written naming the new revision; the catalog still serves the old",
	"correction:replace:committed":        "the publishing transaction committed (the catalog serves the new revision); the records still say prepared",
	"correction:replace:purge-started":    "the row purging and its records rewritten purging; the old revision whole",
	"correction:replace:purge-object":     "the old revision's object removed; not yet settled in the database",
	"correction:replace:purge-objects":    "every object of the old revision removed and settled",
	"correction:replace:purged":           "the row purged; the records still say purging",
	"rebuild:compare:scan":                "the first scan written into the shadow catalog, its cursor past it",
	"rebuild:compare:built":               "the shadow catalog complete and recorded so",
	"rebuild:compare:compared":            "the differences recorded and the job ended; the shadow not yet dropped",
	"rebuild:compare:dropped":             "the shadow dropped; the job's unit not yet completed",
	"rebuild:replace:scan":                "the first scan written into the shadow catalog, its cursor past it; the catalog in use untouched",
	"rebuild:replace:built":               "the shadow catalog complete and recorded so; the catalog in use untouched",
	"rebuild:replace:activated":           "the activation committed: the shadow copied into the catalog in use in one transaction and the job ended; the shadow not yet dropped",
	"rebuild:replace:dropped":             "the shadow dropped; the job's unit not yet completed",
}

// target is the step this process stops at, read once when the package is
// initialized. It is a package variable, set here and never by a test: a
// hook that exists only in a tagged build has no constructor to be handed
// to, since the code that reaches the steps is built the same with the tag
// or without it.
var target = load()

// load reads Env and ends the process when it names no known step: a
// misspelled step would otherwise let a run go on with no kill and pass.
func load() string {
	t := os.Getenv(Env)
	if t == "" {
		return ""
	}
	if _, ok := steps[t]; !ok {
		_, _ = fmt.Fprintf(os.Stderr, "interrupt: %s names no step this build knows: %q\n", Env, t)
		os.Exit(2)
	}
	return t
}

// At ends the process with Status when scope:kind:step is the step named by
// Env: the caller has just made that step's write durable.
func At(scope, kind, step string) {
	if target == "" || target != scope+":"+kind+":"+step {
		return
	}
	_, _ = fmt.Fprintf(os.Stderr, "interrupt: stopped at %s\n", target)
	os.Exit(Status)
}
