package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"

	"github.com/amrshadid/nopacs/internal/fault"
)

// ErrNotTheBytesExpected is in the chain of the error of a PutConfirmed
// whose bytes were not the size or the SHA-256 the caller expected.
var ErrNotTheBytesExpected = errors.New("the bytes to put are not the ones expected")

// PutConfirmed puts what r yields to key, as Store.Put does, only when it is
// exactly size bytes whose SHA-256 is sum: the copy of an object whose
// bytes are known beforehand (a replica's copy, a repair from another
// storage), where a byte changed on the way must never be kept. Otherwise
// it fails as DataLoss, with ErrNotTheBytesExpected in the chain, and the key
// holds what it held before: nothing, or the old object.
//
// The check runs in the stream handed to Put, which fails at the end of
// input, before the storage keeps anything, by the contract of every Put (an
// error of r leaves the key as it was). A stream longer than size fails at
// the first byte past it, without reading the rest. So it holds on any
// driver, and no driver knows of it; Written.Confirmed then says whether
// the storage itself also answered for the checksum of what it kept.
func PutConfirmed(ctx context.Context, s Store, key string, r io.Reader, size int64, sum [sha256.Size]byte) (Written, error) {
	if size < 0 {
		return Written{}, fault.New(fault.Invalid, "the size of the bytes to put is negative")
	}
	w, err := s.Put(ctx, key, &expecting{r: r, h: sha256.New(), left: size, sum: sum})
	if err != nil {
		return Written{}, err
	}
	// Put counted and hashed the same bytes; a driver that says otherwise
	// wrote something else, and it is in place by now. Only a driver that
	// misreports reaches this: what it kept is removed, so no object it
	// cannot vouch for stays under the key (an old object the Put replaced
	// is gone either way), even when ctx has ended.
	if w.Size != size || w.SHA256 != sum {
		msg := "the storage reports writing other bytes than the ones it was given; the object was removed"
		if derr := s.Delete(context.WithoutCancel(ctx), key); derr != nil {
			msg = "the storage reports writing other bytes than the ones it was given, and the object could not be removed"
		}
		return Written{}, fault.Wrap(fault.Internal, ErrNotTheBytesExpected, msg)
	}
	return w, nil
}

// expecting yields r's bytes while they stay within the size expected, and
// in place of r's end gives an error unless they were exactly that size with
// that SHA-256.
type expecting struct {
	r    io.Reader
	h    hash.Hash
	left int64
	sum  [sha256.Size]byte
	err  error
}

func (e *expecting) Read(p []byte) (int, error) {
	if e.err != nil {
		return 0, e.err
	}
	// One byte more than is left is asked, so a longer stream shows itself
	// without the rest being read.
	if int64(len(p)) > e.left+1 {
		p = p[:e.left+1]
	}
	n, err := e.r.Read(p)
	if int64(n) > e.left {
		e.err = mismatch("the bytes to put are more than the size expected")
		return 0, e.err
	}
	e.h.Write(p[:n]) // A hash never returns an error.
	e.left -= int64(n)
	if !errors.Is(err, io.EOF) {
		return n, err
	}
	switch {
	case e.left > 0:
		e.err = mismatch("the bytes to put are fewer than the size expected")
	case [sha256.Size]byte(e.h.Sum(nil)) != e.sum:
		e.err = mismatch("the bytes to put do not have the SHA-256 expected")
	default:
		return n, io.EOF
	}
	// The last bytes are withheld with the error: the store fails either way,
	// and nothing it might have kept ends with them.
	return 0, e.err
}

// mismatch is the error of bytes that are not the ones expected.
func mismatch(msg string) error {
	return fault.Wrap(fault.DataLoss, ErrNotTheBytesExpected, msg)
}
