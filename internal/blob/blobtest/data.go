package blobtest

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
)

// Pattern returns a reader of n bytes that are the same for the same seed
// and look random, so a test can write an object of any size without
// holding it, and check it later by making the same stream again. The bytes
// are not secret and must not be used as such.
func Pattern(seed uint64, n int64) io.Reader {
	var key [32]byte
	binary.LittleEndian.PutUint64(key[:], seed)
	return &pattern{c: rand.NewChaCha8(key), left: n}
}

// PatternBytes returns the bytes Pattern(seed, n) yields.
func PatternBytes(seed uint64, n int) []byte {
	b := make([]byte, n)
	if _, err := io.ReadFull(Pattern(seed, int64(n)), b); err != nil {
		panic(err) // A pattern always yields n bytes; anything else is a bug here.
	}
	return b
}

// PatternSum returns the SHA-256 of the bytes Pattern(seed, n) yields.
func PatternSum(seed uint64, n int64) [sha256.Size]byte {
	h := sha256.New()
	if _, err := io.Copy(h, Pattern(seed, n)); err != nil {
		panic(err) // A hash never fails to write.
	}
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

type pattern struct {
	c    *rand.ChaCha8
	left int64
}

func (p *pattern) Read(b []byte) (int, error) {
	if p.left == 0 {
		return 0, io.EOF
	}
	if int64(len(b)) > p.left {
		b = b[:p.left]
	}
	n, _ := p.c.Read(b) // ChaCha8.Read always fills b and returns a nil error.
	p.left -= int64(n)
	return n, nil
}

// errBroken is what a failing reader in the suite returns, so a test can
// see it come back through Put.
var errBroken = errors.New("the reader broke on purpose")

// failAfter yields n bytes of r, then fails with errBroken.
type failAfter struct {
	r io.Reader
	n int64
}

func (f *failAfter) Read(b []byte) (int, error) {
	if f.n <= 0 {
		return 0, errBroken
	}
	if int64(len(b)) > f.n {
		b = b[:f.n]
	}
	n, err := f.r.Read(b)
	f.n -= int64(n)
	return n, err
}

// cancelAfter yields r, and calls cancel once n bytes have passed, going on
// yielding afterwards: Put must notice the context, not an end of input.
type cancelAfter struct {
	r      io.Reader
	n      int64
	cancel func()
}

func (c *cancelAfter) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n -= int64(n)
	if c.n <= 0 {
		c.cancel()
	}
	return n, err
}

// untouched fails the test, through the flag, if it is ever read: Put of a
// refused key must not consume its input.
type untouched struct{ read bool }

func (u *untouched) Read([]byte) (int, error) {
	u.read = true
	return 0, io.EOF
}
