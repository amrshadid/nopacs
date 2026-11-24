// Package readat reads a stored object at offsets (io.ReaderAt) through the
// storage port's ranged reads, for a reader that takes a file in many small
// pieces, such as go-dicom reading a header or walking the items of
// encapsulated pixel data, and must never fetch the whole object to do it.
//
// A read that continues where the bytes held end fetches the next chunk,
// twice as long as the last up to MaxChunk, so a header is a few ranged
// reads of the storage and never one per element; a read that jumps (the
// DICOM reader skipping a value, or encapsulated pixel data item by item)
// fetches only what it asks for, rounded up to MinChunk, so skipping over a
// large file does not read it.
//
// # What this package must never do
//
// It must never read more of an object than the bytes asked for rounded up
// to a chunk, never hold more than one chunk, and never know which driver
// holds the bytes: it reaches them through blob.Store only.
package readat
