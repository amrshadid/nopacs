package checkedread

import (
	"context"
	"io"

	"github.com/amrshadid/nopacs/internal/blob"
	"github.com/amrshadid/nopacs/internal/fault"
)

// Source is one archived object read in several pieces (the ranges of a
// frame, a header read at offsets): its first Read or Stat chooses the
// storage as Open does, and every later one goes to that same storage, so
// the bytes of one answer never come from two storages and a read never
// turns to a replica midway. A first call that no storage served is kept
// as well: every later call returns its error, so one answer tries each
// storage once (a Stat of the scan's metadata file, then its Read) and a
// replica's missing object is never found twice.
type Source struct {
	s   *Stores
	obj Object
	// chosen is the storage that served the first read; nil before one did.
	chosen *Opened
	// failed is the error of a first call that no storage served.
	failed error
}

// Source returns the object obj, read through s, no storage chosen yet.
func (s *Stores) Source(obj Object) *Source { return &Source{s: s, obj: obj} }

// Read reads length bytes of the object from offset, as blob.Store.Read
// does, from the storage chosen.
func (src *Source) Read(ctx context.Context, offset, length int64) (io.ReadCloser, error) {
	switch {
	case src.chosen != nil:
		return src.chosen.store.Read(ctx, src.obj.Key, offset, length)
	case src.failed != nil:
		return nil, src.failed
	}
	o, err := src.s.Open(ctx, src.obj, offset, length)
	if err != nil {
		src.failed = err
		return nil, err
	}
	src.chosen = &o
	return o.Body, nil
}

// Stat returns what is known of the object, from the storage chosen.
func (src *Source) Stat(ctx context.Context) (blob.Info, error) {
	switch {
	case src.chosen != nil:
		return src.chosen.store.Stat(ctx, src.obj.Key)
	case src.failed != nil:
		return blob.Info{}, src.failed
	}
	info, o, err := src.s.stat(ctx, src.obj, true)
	if err != nil {
		src.failed = err
		return info, err
	}
	src.chosen = &o
	return info, nil
}

// Opened returns the storage that serves the object, and false before a
// read chose one.
func (src *Source) Opened() (Opened, bool) {
	if src.chosen == nil {
		return Opened{}, false
	}
	return *src.chosen, true
}

// Mismatch is Stores.Mismatch for the storage chosen.
func (src *Source) Mismatch(ctx context.Context, err error) error {
	if src.chosen == nil {
		return nil
	}
	return src.s.Mismatch(ctx, *src.chosen, err)
}

// Store returns the object as a blob.Store, for a reader that takes one
// (package readat): it reads and stats the object's key through the
// Source, refuses any other key, and writes nothing.
func (src *Source) Store() blob.Store { return sourceStore{src} }

// sourceStore is Source.Store.
type sourceStore struct{ src *Source }

// errSourceOnly is a call a Source's store does not serve.
var errSourceOnly = fault.New(fault.Invalid, "checkedread: a source reads its one object and writes nothing")

// Read reads the object.
func (s sourceStore) Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error) {
	if key != s.src.obj.Key {
		return nil, errSourceOnly
	}
	return s.src.Read(ctx, offset, length)
}

// Stat stats the object.
func (s sourceStore) Stat(ctx context.Context, key string) (blob.Info, error) {
	if key != s.src.obj.Key {
		return blob.Info{}, errSourceOnly
	}
	return s.src.Stat(ctx)
}

// Put is refused.
func (sourceStore) Put(context.Context, string, io.Reader) (blob.Written, error) {
	return blob.Written{}, errSourceOnly
}

// List is refused.
func (sourceStore) List(context.Context, blob.ListOptions) (blob.Page, error) {
	return blob.Page{}, errSourceOnly
}

// Delete is refused.
func (sourceStore) Delete(context.Context, string) error { return errSourceOnly }

// Purge is refused.
func (sourceStore) Purge(context.Context, string) ([]blob.Removal, error) { return nil, errSourceOnly }

// Usage is refused.
func (sourceStore) Usage(context.Context) (blob.Usage, error) { return blob.Usage{}, errSourceOnly }

// Close closes nothing: the storages are the Stores'.
func (sourceStore) Close() error { return nil }
