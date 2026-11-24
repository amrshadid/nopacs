package checkedread

import (
	"strconv"
	"sync"
	"time"

	"github.com/amrshadid/nopacs/internal/fault"
)

// The bounds of reading archived bytes.
const (
	// ChunkSize is the most bytes one FileChunk of ReadFiles holds: 1 MiB
	// in this version, under the 3 MiB a client accepts and well under the
	// 4 MB a stock gRPC client takes for a whole message.
	ChunkSize = 1 << 20
	// MaxOpenReads is the most streams of ReadFiles and downloads one
	// caller, a key or the local operator, holds open at once when no
	// Slots.Limits says otherwise: the default of read.per-caller.
	MaxOpenReads = 16
	// MaxServerReads is the most the server holds open at once, for every
	// caller together, when no Slots.Limits says otherwise (the default of
	// read.per-server): each holds a storage's connection or file and two
	// buffers of ChunkSize.
	MaxServerReads = 256
	// StallTimeout ends a download whose client reads nothing for this
	// long, so a client that stalls does not hold a storage's connection
	// for ever.
	StallTimeout = 5 * time.Minute
)

// Slots counts the reads of archived bytes open now, per owner and in
// all. Its zero value is ready to use. One process serves an installation
// (the lock on its data directory), so a count in memory is the bound.
type Slots struct {
	// Limits answers the bounds that apply to the next read: the most one
	// owner, and the whole server, hold open (the settings read.per-caller
	// and read.per-server). It is called at every Take, so it must be
	// cheap: the server's keeps what it read for a second. Nil means
	// MaxOpenReads and MaxServerReads. A bound lowered below what is open
	// takes nothing back: reads open keep their place and only a new read
	// is refused.
	Limits func() (perCaller, perServer int)

	mu      sync.Mutex
	byOwner map[string]int
	total   int
}

// bounds returns the bounds of the next read; a caller's is never above
// the server's.
func (o *Slots) bounds() (perCaller, perServer int) {
	perCaller, perServer = MaxOpenReads, MaxServerReads
	if o.Limits != nil {
		perCaller, perServer = o.Limits()
	}
	return min(perCaller, perServer), perServer
}

// Take counts one more read for owner and returns the function that ends
// it, or an Exhausted error when owner or the server is at its bound. The
// read is refused, never queued: a client keeps a small pool of downloads
// and the one past the bound is told so.
func (o *Slots) Take(owner string) (func(), error) {
	// The bounds are read before the lock, so a slow answer never holds up
	// the release of a read.
	perCaller, perServer := o.bounds()
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.total >= perServer {
		return nil, fault.New(fault.Exhausted, "the server holds as many streams and downloads open as it serves at once; try again later")
	}
	if o.byOwner[owner] >= perCaller {
		return nil, fault.New(fault.Exhausted, "the caller holds "+strconv.Itoa(perCaller)+" streams and downloads open already; end one first")
	}
	if o.byOwner == nil {
		o.byOwner = map[string]int{}
	}
	o.byOwner[owner]++
	o.total++
	var once sync.Once
	return func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if o.byOwner[owner]--; o.byOwner[owner] <= 0 {
				delete(o.byOwner, owner)
			}
			o.total--
		})
	}, nil
}
