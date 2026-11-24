package scrub

import (
	"crypto/sha256"
	"errors"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// presence is what a read found of an object.
type presence int

// The presences.
const (
	// present is an object read to its end.
	present presence = iota
	// missing is an object the storage does not hold.
	missing
	// notOnline is an object the storage holds in a tier it cannot read
	// at once: neither missing nor damaged (blob.ErrNotOnline).
	notOnline
)

// measured is what one read of an object found: whether it is there, and
// the size and SHA-256 (FIPS 180-4) of its bytes.
type measured struct {
	presence presence
	size     int64
	sum      tenant.SHA256
}

// matches reports whether m is an object of the size and checksum given.
func (m measured) matches(size int64, sum tenant.SHA256) bool {
	return m.presence == present && m.size == size && m.sum == sum
}

// measure reads length bytes of key from offset on storage, -1 for the
// whole object, under one of the server's read slots and paced at the
// tenant's scrub.rate for that storage, and returns their size and SHA-256.
// It only reads: nothing is written to a storage. No slot free is
// Exhausted; a storage that fails otherwise is the error.
func (u *unitRun) measure(store Storage, storage, key string, offset, length int64) (measured, error) {
	release, err := u.sup.cfg.Reads.Take(ReadOwner)
	if err != nil {
		return measured{}, err
	}
	defer release()
	u.reads[key]++
	if f := u.sup.cfg.seams.read; f != nil {
		f(key, u.reads[key])
	}
	body, err := store.Read(u.ctx, key, offset, length)
	switch {
	case errors.Is(err, blob.ErrNotOnline):
		return measured{presence: notOnline}, nil
	case fault.KindOf(err) == fault.NotFound:
		return measured{presence: missing}, nil
	case err != nil:
		return measured{}, err
	}
	defer func() { _ = body.Close() }() // only read from; nothing to lose
	h := sha256.New()
	r := &pacedReader{ctx: u.ctx, r: body, p: u.t.pacer(storage), rate: u.rate}
	if _, err := io.CopyBuffer(h, r, u.buf); err != nil {
		if errors.Is(err, blob.ErrNotOnline) {
			return measured{presence: notOnline}, nil
		}
		return measured{}, err
	}
	return measured{presence: present, size: r.n, sum: tenant.SHA256(h.Sum(nil))}, nil
}

// measureTwice reads an object as measure does and, when it is not of the
// size and checksum given, reads it a second time: a difference is believed
// only when both reads find it, so a read that failed midway or an object
// replaced between the two (a reception writing it again) is not reported.
// It returns the second read when there was one.
func (u *unitRun) measureTwice(store Storage, storage, key string, offset, length, size int64, sum tenant.SHA256) (measured, error) {
	m, err := u.measure(store, storage, key, offset, length)
	if err != nil || m.matches(size, sum) || m.presence == notOnline {
		return m, err
	}
	return u.measure(store, storage, key, offset, length)
}
