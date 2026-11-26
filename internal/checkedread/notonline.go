package checkedread

import (
	"context"
	"errors"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// The words of RecallState a read not online is answered with (access.proto,
// NOT_ONLINE's "recall_state"): RecallNone when no recall of the object is
// asked, or the one asked has ended online and the store holds the object
// cold again; otherwise the recall's own state.
const (
	RecallNone       = "none"
	RecallRequested  = string(tenant.RecallRequested)
	RecallInProgress = string(tenant.RecallInProgress)
	RecallFailed     = string(tenant.RecallFailed)
)

// NotOnline is in the chain of the error of a read whose object no storage
// could serve because its own storage holds it in a tier it cannot read at
// once (blob.ErrNotOnline, also in the chain), with the state of the
// object's recall as the tenant's database holds it, so the answer can say
// whether waiting brings the bytes back. A reader's request starts no
// recall: a reader's key is not a lever on the site's bill.
type NotOnline struct {
	// RecallState is one of RecallNone, RecallRequested, RecallInProgress
	// and RecallFailed.
	RecallState string
	// Err is the storage's error, which holds blob.ErrNotOnline.
	Err error
}

// Error returns the storage's error's text.
func (e *NotOnline) Error() string { return e.Err.Error() }

// Unwrap returns the storage's error, so its kind and blob.ErrNotOnline
// pass through.
func (e *NotOnline) Unwrap() error { return e.Err }

// Waiting reports whether a recall of the object is requested or in
// progress: waiting then brings it back, and the answer carries a retry
// delay.
func (e *NotOnline) Waiting() bool {
	return e.RecallState == RecallRequested || e.RecallState == RecallInProgress
}

// notOnline returns err with the state of the object's recall in its chain
// when err is a storage holding obj not online (its own storage, or a
// replica in step, coldCopy) and the tenant's database answers; otherwise
// err as it is. The database is asked only on this path, a read that
// failed.
func (s *Stores) notOnline(ctx context.Context, obj Object, err error) error {
	if err == nil || s.recalls == nil || !errors.Is(err, blob.ErrNotOnline) {
		return err
	}
	var already *NotOnline
	if errors.As(err, &already) {
		return err
	}
	storage := obj.Storage
	var cold *coldCopy
	if errors.As(err, &cold) {
		storage = cold.storage
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recordTimeout)
	defer cancel()
	r, found, rerr := s.recalls.Recall(cctx, storage, obj.Key)
	if rerr != nil {
		return err
	}
	state := RecallNone
	if found {
		switch r.State {
		case tenant.RecallRequested, tenant.RecallInProgress, tenant.RecallFailed:
			state = string(r.State)
		}
	}
	return &NotOnline{RecallState: state, Err: err}
}
