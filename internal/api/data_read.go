package api

import (
	"context"
	"errors"
	"maps"
	"time"

	"github.com/amrshadid/nopacs/internal/access"
	"github.com/amrshadid/nopacs/internal/checkedread"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// What the reading of archived bytes shares between ReadFiles, the plain
// HTTP download and the DICOMweb retrievals: the recheck of the caller's key
// while a read is open and the audit details. The checked copy, the bound of
// open reads and the opening of storages live in package checkedread, which
// a sender reads through as well.

// The bounds of reading archived bytes (data.proto, "Limits"), as
// checkedread holds them.
const (
	// ChunkSize is the most bytes one FileChunk of ReadFiles holds.
	ChunkSize = checkedread.ChunkSize
	// MaxOpenReads is the most streams of ReadFiles and downloads one
	// caller holds open at once.
	MaxOpenReads = checkedread.MaxOpenReads
	// MaxServerReads is the most the server holds open at once.
	MaxServerReads = checkedread.MaxServerReads
	// StallTimeout ends a download whose client reads nothing for this
	// long.
	StallTimeout = checkedread.StallTimeout
)

// httpChunk is the most bytes a download reads from the storage and writes
// to the connection at once: a quarter of ChunkSize, so a download holds
// half a MiB of buffers, and each write is large enough that a gigabyte
// costs a few thousand system calls.
const httpChunk = ChunkSize / 4

// watchKey returns ctx canceled, with errRefusedWhileOpen as its cause,
// once the caller's key or grant may no longer read, checked again every
// RecheckInterval, and the function that ends the checks. A long read is
// then ended with the answer a new request would get, which Confirm
// records. The function returns once the checking goroutine has, so no
// check of a request runs after its handler returned.
func (c *core) watchKey(ctx context.Context, d access.Decision) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	ended := make(chan struct{})
	go func() {
		defer close(ended)
		every := RecheckInterval
		if c.seams.recheck > 0 {
			every = c.seams.recheck
		}
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if _, err := c.access.Confirm(ctx, d); err != nil {
					cancel(errRefusedWhileOpen{err})
					return
				}
			}
		}
	}()
	return ctx, func() {
		cancel(nil)
		<-ended
	}
}

// errRefusedWhileOpen is the cause of a read or a stream ended because its
// caller's key or grant may no longer read, with the refusal.
type errRefusedWhileOpen struct{ err error }

// Error returns the refusal's message.
func (e errRefusedWhileOpen) Error() string { return e.err.Error() }

// refusedWhileOpen returns the refusal that ended ctx, when the caller's
// key or grant was found unable to read while the read was open.
func refusedWhileOpen(ctx context.Context) (error, bool) {
	var refused errRefusedWhileOpen
	if errors.As(context.Cause(ctx), &refused) {
		return refused.err, true
	}
	return nil, false
}

// newReadStores returns the storages of the tenant named name, none open
// yet.
func (c *core) newReadStores(p tenant.Pipeline, name string) *checkedread.Stores {
	return checkedread.NewStores(c.opener(), func(ctx context.Context, n string) (string, error) {
		row, err := c.master.TenantByName(ctx, n)
		return row.ID, err
	}, p, name)
}

// withDetails returns base with the pairs of more added, base unchanged.
// An audit entry's facts replace the check's details as a whole, so a
// later entry of a request carries its request's details again.
func withDetails(base map[string]string, more map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(more))
	maps.Copy(out, base)
	maps.Copy(out, more)
	return out
}
