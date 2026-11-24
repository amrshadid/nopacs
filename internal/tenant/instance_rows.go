package tenant

import (
	"github.com/amrshadid/nopacs/internal/core/label"
	"github.com/amrshadid/nopacs/internal/core/layout"
	"github.com/amrshadid/nopacs/internal/core/reception"
)

// What a store keeps of a held instance is what no other row gives: a
// store keeps neither the storage, which is its scan's, nor its key while
// that is the layout's key of its scan and UID, the only key a commit is
// given (ScanCommit.Validate). At ten million instances the key alone was
// about 1.2 GB of rows, a hundred and twenty bytes an instance made again
// from four strings the read has at hand.

// InstanceKey returns an instance's key on its storage from what a store
// keeps of it: stored when a store kept a key, which only a row from before
// this rule holds, and otherwise the layout's key of the scan at p and the
// instance's UID.
func InstanceKey(p label.Path, sopInstanceUID, stored string) (string, error) {
	if stored != "" {
		return stored, nil
	}
	return layout.InstanceKey(p, sopInstanceUID)
}

// KeepsReceivedFiles reports whether a reception that ended in state keeps
// its received files. A failed one does, for its retry, which runs them
// again, and its discard, which lists their places. One done or quarantined
// does not: each of its files is then an instance the catalog holds, a
// duplicate of one, or a quarantine record that keeps its own copy of the
// file's row and place, and a restart reads only the files of receptions in
// progress and of failed ones. A store removes them in the transaction of
// the step that ends the reception, so they cost nothing once it ends; at
// ten million instances they were 40 percent of a tenant's database.
func KeepsReceivedFiles(state reception.State) bool {
	return !state.Ended() || state == reception.Failed
}
