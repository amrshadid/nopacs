package deidjob

import (
	"errors"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// errTooLarge is an instance larger than MaxFileBytes: it is not read.
var errTooLarge = errors.New("deidjob: the instance is larger than a unit reads")

// errMissing is an instance whose object neither its storage nor a replica
// in step holds: data loss, as a read of it reports it.
var errMissing = errors.New("deidjob: the instance's file is not on its storage")

// read reads the instance's bytes into memory, each checked against the
// length and archived SHA-256 the catalog holds (checkedread.CopyChecked),
// under one of the server's read slots: from its storage, or from a replica
// in step when that storage cannot serve it (checkedread.Stores.Open). An
// instance past MaxFileBytes is errTooLarge and is not read; one whose
// bytes differ, or that a scrub marked and no replica serves (its primary
// not read at all), is a mismatch (checkedread.Mismatched), a replica's
// recorded as its damage; no slot free is Exhausted.
func (u *unitRun) read(in tenant.ScanInstance) ([]byte, error) {
	limit := int64(MaxFileBytes)
	if n := u.sup.cfg.seams.maxFileBytes; n > 0 {
		limit = n
	}
	obj := checkedread.InstanceObject(in, "", u.scan)
	if err := u.stores.Readable(u.ctx, obj); err != nil {
		// A scrub found its bytes damaged and no replica serves them: data
		// loss, judged as a mismatch.
		return nil, err
	}
	if in.Length > limit || in.Length < 0 {
		return nil, errTooLarge
	}
	release, err := u.sup.cfg.Reads.Take("deid:" + u.t.name)
	if err != nil {
		return nil, err
	}
	defer release()
	o, err := u.stores.Open(u.ctx, obj, in.Offset, in.Length)
	switch {
	case checkedread.StorageFailed(err):
		// The storage itself, not the object: not the instance's loss.
		return nil, err
	case fault.KindOf(err) == fault.NotFound:
		return nil, errMissing
	case err != nil:
		return nil, err
	}
	defer func() { _ = o.Body.Close() }() // only read from; nothing to lose
	out := make([]byte, 0, in.Length)
	sum := in.ArchivedSHA256
	if _, err := checkedread.CopyChecked(o.Body, in.Length, &sum, u.bufs, func(b []byte) error {
		out = append(out, b...)
		return nil
	}); err != nil {
		if checkedread.Mismatched(err) {
			if rerr := u.stores.Mismatch(u.ctx, o, err); rerr != nil {
				u.log.Warn("deid jobs: a replica's bytes found wrong could not be recorded as its damage", "replica", o.Storage, "kind", fault.KindOf(rerr).String())
			}
		}
		return nil, err
	}
	return out, nil
}
