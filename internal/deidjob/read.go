package deidjob

import (
	"errors"

	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// errTooLarge is an instance larger than MaxFileBytes: it is not read.
var errTooLarge = errors.New("deidjob: the instance is larger than a unit reads")

// errMissing is an instance whose object the storage does not hold: data
// loss, as a read of it reports it.
var errMissing = errors.New("deidjob: the instance's file is not on its storage")

// read reads the instance's bytes into memory, each checked against the
// length and archived SHA-256 the catalog holds (checkedread.CopyChecked),
// under one of the server's read slots. An instance past MaxFileBytes is
// errTooLarge and is not read; one whose bytes differ, or that a scrub
// marked (not read at all), is a mismatch (checkedread.Mismatched); no slot
// free is Exhausted.
func (u *unitRun) read(in tenant.ScanInstance) ([]byte, error) {
	limit := int64(MaxFileBytes)
	if n := u.sup.cfg.seams.maxFileBytes; n > 0 {
		limit = n
	}
	if err := checkedread.Marked(in); err != nil {
		// A scrub found its bytes damaged: data loss, judged as a mismatch.
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
	store, err := u.stores.Get(u.ctx, in.Storage)
	if err != nil {
		return nil, err
	}
	body, err := store.Read(u.ctx, in.Key, in.Offset, in.Length)
	switch {
	case fault.KindOf(err) == fault.NotFound:
		return nil, errMissing
	case err != nil:
		return nil, err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	out := make([]byte, 0, in.Length)
	sum := in.ArchivedSHA256
	if _, err := checkedread.CopyChecked(body, in.Length, &sum, u.bufs, func(b []byte) error {
		out = append(out, b...)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}
