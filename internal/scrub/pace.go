package scrub

import (
	"context"
	"io"

	"github.com/amrshadid/nopacs/internal/pace"
)

// pacer holds the reads of one storage to scrub.rate, whichever unit reads
// it (pace.Pacer), so a scrub leaves the rest of the storage's bandwidth to
// reception and reads.
type pacer = pace.Pacer

// maxBurst is how far behind the clock a pacer's reservations may fall, so
// a storage left idle does not bank an unbounded burst.
const maxBurst = pace.MaxBurst

// pacedReader is a reader whose pieces are paced.
type pacedReader struct {
	ctx  context.Context
	r    io.Reader
	p    *pacer
	rate int64
	// n counts the bytes read.
	n int64
}

// Read implements io.Reader.
func (r *pacedReader) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.n += int64(n)
	if perr := r.p.Take(r.ctx, n, r.rate); perr != nil && err == nil {
		err = perr
	}
	return n, err
}
