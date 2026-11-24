package place

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sync"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/blob/s3"
	"github.com/amrshadid/nopacs/internal/fault"
	"github.com/amrshadid/nopacs/internal/tenant"
)

// Cleanups makes the removal of what a crash left in a storage run once in
// a process: at the first open to write of each storage's place (its
// directory, or its bucket and prefix at its endpoint), and never again
// while the process runs. Every later open to write of the same place, by
// any user of the process sharing it, opens beside the first (OpenBeside).
//
// A directory storage's Open removes every file in its temporary directory
// (dir.Open), and an S3 storage's Open aborts old unfinished uploads. Run
// by a second store of the same storage while the first writes, the
// directory's removal takes the temporary file of a Put in progress, which
// then fails at its rename, or the probe file of an open in progress, which
// then fails as a storage that cannot be written. One process opens a
// storage from several users (the reception pipeline, which reopens when a
// storage's row changes and restarts with each pause; a tenant's
// corrections; an owner record; a storage test), each at its own time, so
// no open but the first can know that nothing of the process writes there.
// Once is enough: what a crash leaves was left by a process that has ended,
// and a Put that fails in this one removes its own temporary file.
//
// A directory is known by its identity (os.SameFile: device and inode on
// Unix, volume and file index on Windows), not by how its path is spelled,
// so a root spelled otherwise (another case on a file system that ignores
// it, another link to it) is still the same place.
//
// The zero value is ready; one value is shared by every opener of the
// process that writes, and must not be copied after first use.
type Cleanups struct {
	mu     sync.Mutex
	places []*cleanup
}

// placeID is where a storage is: a directory's identity, or an S3 storage's
// endpoint, bucket and prefix.
type placeID struct {
	dir fs.FileInfo
	s3  string
}

// same reports whether a and b are one place.
func (a placeID) same(b placeID) bool {
	if a.dir != nil || b.dir != nil {
		return a.dir != nil && b.dir != nil && os.SameFile(a.dir, b.dir)
	}
	return a.s3 == b.s3
}

// cleanup is the state of one storage's place.
type cleanup struct {
	id placeID
	// done is set once an open that removed all a crash left there has
	// succeeded.
	done bool
	// running is closed when the open that removes it ends; nil while none
	// runs.
	running chan struct{}
}

// claim reports whether the caller's open is the one to remove what a crash
// left at id: true when none has succeeded there and none runs, and the
// caller must then call finish with the entry returned. While another runs
// it waits for it to end, since the files that one removes may be about to
// include the caller's first Put.
func (c *Cleanups) claim(ctx context.Context, id placeID) (*cleanup, bool, error) {
	for {
		c.mu.Lock()
		var e *cleanup
		for _, p := range c.places {
			if p.id.same(id) {
				e = p
				break
			}
		}
		if e == nil {
			e = &cleanup{id: id}
			c.places = append(c.places, e)
		}
		if e.done {
			c.mu.Unlock()
			return e, false, nil
		}
		if e.running == nil {
			e.running = make(chan struct{})
			c.mu.Unlock()
			return e, true, nil
		}
		wait := e.running
		c.mu.Unlock()
		select {
		case <-wait:
		case <-ctx.Done():
			return nil, false, fault.Wrap(fault.Unavailable, ctx.Err(), "the storage's open was canceled while another open of it removed what a crash left")
		}
	}
}

// finish ends the claim of e; cleaned says whether the open succeeded and
// removed all a crash left, so that a failed one (a directory not mounted
// yet, an S3 listing refused or cut at its bound) leaves the removal to the
// next open.
func (c *Cleanups) finish(e *cleanup, cleaned bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.done = e.done || cleaned
	close(e.running)
	e.running = nil
}

// placeOf returns where a resolved storage s is, the same for every opener
// of the process: the directory's identity, or the endpoint, bucket and
// prefix. No error names the path.
func placeOf(s tenant.Storage, at resolved) (placeID, error) {
	if s.Type != tenant.StorageDirectory {
		return placeID{s3: at.endpoint.URL + "\x00" + s.S3.Bucket + "\x00" + s.S3.Prefix}, nil
	}
	info, err := os.Stat(at.dir.Path())
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return placeID{}, fault.New(fault.NotFound, "the storage's directory is not there")
	case err != nil:
		return placeID{}, fault.New(fault.Unavailable, "the storage's directory cannot be examined")
	}
	return placeID{dir: info}, nil
}

// sweptWhole reports whether bs, just opened by an open that removes what a
// crash left, removed all of it. A directory store removes every file in
// its temporary directory or fails its open. An S3 store's cleanup does not
// fail its open: it is whole only when it listed every upload under the
// prefix and aborted each old one (s3.CleanupReport.Whole), and otherwise
// the next open sweeps again.
func sweptWhole(bs blob.Store) bool {
	n, ok := bs.(interface{ Notes() s3.Notes })
	if !ok {
		return true
	}
	return n.Notes().Cleanup.Whole()
}
