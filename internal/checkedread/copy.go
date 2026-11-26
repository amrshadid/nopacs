package checkedread

import (
	"crypto/sha256"
	"errors"
	"hash"
	"io"

	"github.com/amrshadid/nopacs/internal/tenant"
)

// The ways the bytes of an instance on its storage are found not to be
// what its manifest records. Each is data loss: the catalog holds the
// instance, and its bytes are not passed on as good.
var (
	// ErrShort: the storage holds fewer bytes than the manifest records.
	ErrShort = errors.New("the instance's bytes on the storage are fewer than its manifest records")
	// ErrLong: the storage holds more bytes than the manifest records.
	ErrLong = errors.New("the instance's bytes on the storage are more than its manifest records")
	// ErrChecksum: the bytes have another SHA-256 than the manifest records.
	ErrChecksum = errors.New("the instance's bytes on the storage do not have the SHA-256 its manifest records")
	// ErrMarked: a scrub found the instance's bytes on its storage missing
	// or not what was archived, and marked it (tenant.ScanInstance.Damaged).
	// A reader refuses it before it reads or passes a byte (Marked).
	ErrMarked = errors.New("a scrub found the instance's bytes on its storage missing or not what was archived")
)

// Marked returns ErrMarked for an instance a scrub marked, and nil for any
// other. A marked instance fails as data loss at once rather than after a
// prefix of bytes that are not what was archived; a reader that holds
// Stores asks Stores.Readable instead, which lets a replica in step serve
// it.
func Marked(in tenant.ScanInstance) error {
	if in.Damaged {
		return ErrMarked
	}
	return nil
}

// CopyChecked passes the bytes r yields to emit, in pieces of at most the
// length of each buffer, checking that they are exactly length bytes and,
// when sum is not nil, that their SHA-256 is *sum. The last piece is
// passed only after both checks, so the bytes of an instance that differs
// from its manifest never reach the client whole: ErrShort, ErrLong or
// ErrChecksum is returned instead, with the bytes passed before it
// counted. An error of r or of emit is returned as it is. The two buffers
// take turns: one holds the piece not yet passed while the other is read
// into.
//
// The SHA-256 of a body of more than one piece is computed on its own
// goroutine while the piece before is passed: one SHA-256 runs at about
// the speed of a socket's writes (some 2 GB/s on a core with the SHA
// instructions), so hashing and writing one after the other would halve
// the rate. Neither buffer is read into again before the hash of what it
// held is done, and CopyChecked returns only once the hash has let go of
// both, so a caller may reuse them at once.
func CopyChecked(r io.Reader, length int64, sum *tenant.SHA256, bufs [2][]byte, emit func([]byte) error) (int64, error) {
	var h *pipedHash
	if sum != nil {
		h = newPipedHash(length > int64(len(bufs[0])))
		defer h.stop()
	}
	r = io.LimitReader(r, length+1)
	var (
		read, passed int64
		held         []byte
		turn         int
	)
	for {
		b := bufs[turn]
		if h != nil {
			// b held the piece before held; its hash must be done before
			// it is read into.
			h.waitBehind(1)
		}
		n, err := io.ReadFull(r, b)
		if n > 0 {
			read += int64(n)
			if read > length {
				// Past the length: the piece held, which ends at the
				// length, is not passed, so the body never looks whole.
				return passed, ErrLong
			}
			if h != nil {
				h.write(b[:n])
			}
			if held != nil {
				if err := emit(held); err != nil {
					return passed, err
				}
				passed += int64(len(held))
			}
			held, turn = b[:n], 1-turn
		}
		switch {
		case err == nil:
			continue
		case !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF):
			return passed, err
		}
		// The end of what r holds.
		switch {
		case read < length:
			return passed, ErrShort
		case h != nil && h.sum() != *sum:
			return passed, ErrChecksum
		}
		if held != nil {
			if err := emit(held); err != nil {
				return passed, err
			}
			passed += int64(len(held))
		}
		return passed, nil
	}
}

// pipedHash is the SHA-256 of CopyChecked, computed on the caller's
// goroutine for a body of one piece and on its own for a longer one.
type pipedHash struct {
	h hash.Hash
	// in carries the pieces to the goroutine, done one token per piece
	// hashed, and finished is closed when the goroutine has returned; all
	// three are nil when the hash is computed in line.
	in       chan []byte
	done     chan struct{}
	finished chan struct{}
	// sent and hashed count the pieces given and the pieces done.
	sent, hashed int
}

// newPipedHash returns a hash, with its own goroutine when piped.
func newPipedHash(piped bool) *pipedHash {
	p := &pipedHash{h: sha256.New()}
	if !piped {
		return p
	}
	// At most two pieces are outstanding (waitBehind), so neither channel
	// ever blocks its sender for long, and done never blocks at all.
	p.in, p.done, p.finished = make(chan []byte, 2), make(chan struct{}, 2), make(chan struct{})
	go func() {
		defer close(p.finished)
		for b := range p.in {
			_, _ = p.h.Write(b) // A hash never fails.
			p.done <- struct{}{}
		}
	}()
	return p
}

// write adds b to the hash. When piped, b must not change until the hash
// has gone past it (waitBehind).
func (p *pipedHash) write(b []byte) {
	if p.in == nil {
		_, _ = p.h.Write(b) // A hash never fails.
		return
	}
	p.in <- b
	p.sent++
}

// waitBehind waits until at most n pieces given are not yet hashed.
func (p *pipedHash) waitBehind(n int) {
	for p.in != nil && p.sent-p.hashed > n {
		<-p.done
		p.hashed++
	}
}

// sum returns the SHA-256 of every piece given.
func (p *pipedHash) sum() tenant.SHA256 {
	p.waitBehind(0)
	return tenant.SHA256(p.h.Sum(nil))
}

// stop ends the goroutine and waits for it, so no piece is read after
// CopyChecked returns.
func (p *pipedHash) stop() {
	if p.in == nil {
		return
	}
	close(p.in)
	<-p.finished
	p.in = nil
}

// Mismatched reports whether err is one of CopyChecked's findings that the
// bytes differ from the manifest, an instance a scrub marked (Marked), or a
// file read whole that is not the one the catalog keeps the digest of
// (ErrOtherFile).
func Mismatched(err error) bool {
	return errors.Is(err, ErrShort) || errors.Is(err, ErrLong) || errors.Is(err, ErrChecksum) || errors.Is(err, ErrMarked) ||
		errors.Is(err, ErrOtherFile)
}
