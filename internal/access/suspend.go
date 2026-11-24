package access

import (
	"context"
	"strconv"

	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/master"
)

// detailAll is the detail of a reinstatement's entries that says whether
// every suspended key was asked for, or keys by id.
const detailAll = "all"

// reasonNotLocal is the audit reason of a reinstatement asked for with a
// key: the caller is told only that it may not.
const reasonNotLocal = "only the local operator reinstates keys"

// ReinstateKeys lets suspended keys work again (access.proto,
// AccessService.ReinstateKeys): the keys named by ids, or every suspended
// key when all is true. Only the local operator may: a key is refused as
// Denied whatever its grants, and the refusal recorded, since after a
// restore every key is suspended and none may lift a suspension, its own or
// another's, before a person on the machine has reviewed them. The master
// checks the request (master.ValidateReinstatement) and writes one entry
// per key reinstated, with its id as the object, in the change's
// transaction; a failure is recorded as every change's is. It returns the
// keys reinstated, with ids only, and how many.
func (a *Access) ReinstateKeys(ctx context.Context, d Decision, ids []string, all bool) ([]master.Key, int64, error) {
	entry, by, err := a.actFor(d, true, "", map[string]string{detailAll: strconv.FormatBool(all)})
	if err != nil {
		return nil, 0, err
	}
	if !d.principal.IsLocalOperator() {
		a.record(entry, master.OutcomeRefused, reasonNotLocal)
		return nil, 0, fault.New(fault.Denied, MessageDenied)
	}
	keys, n, err := a.store.ReinstateKeys(ctx, ids, all, entry, by)
	if err != nil {
		// NotFound names the id the caller gave, so it keeps its message.
		return nil, 0, a.failAfter(entry, err)
	}
	return keys, n, nil
}
