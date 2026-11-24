package blob

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"time"
)

// ErrNotOnline is in the chain of the Unavailable error a driver returns for
// an object it holds in a tier it cannot read at once (an S3 object in an
// archive storage class answers InvalidObjectState). Such an object is not
// missing and not damaged: a reader answers unavailable and a scrub tallies
// it, and neither reports data loss.
var ErrNotOnline = errors.New("the object is held in a tier the storage cannot read at once")

// Store is one storage: a flat set of objects, each named by a key and
// holding a sequence of bytes. Keys look like paths ("data/s1/a/b/x.dcm"),
// and List can show them one level at a time, but the storage holds objects
// only: there is no directory to make or remove, and a prefix exists exactly
// while some key under it does.
//
// Every method validates the keys and options it is given and returns a
// fault.Invalid error, having touched nothing, for one that is wrong. A Store
// is safe for concurrent use.
type Store interface {
	// Put writes everything r yields to key and returns its size and
	// SHA-256, computed from the bytes as they passed.
	//
	// The object appears whole or not at all: until Put returns nil, no
	// reader sees any of the new bytes, and when it returns nil they are all
	// there and kept. A key that already holds an object is replaced the
	// same way, so a reader sees the old object or the new one and never a
	// mixture. When r fails, ctx ends, or the storage fails midway, Put
	// returns an error and the key holds what it held before (nothing, or
	// the old object). The one exception is a failure after the new object
	// is in place but before it is known to be kept, such as a directory
	// driver's last sync: Put still returns an error, so the caller puts
	// again, and the key may hold the new object, whole. In no case does it
	// hold a part of one. An error from r is returned wrapped, so its kind
	// passes through; an ended ctx gives an error that matches ctx.Err()
	// with errors.Is.
	//
	// Two Puts to one key at the same time both succeed, and the key then
	// holds the bytes of one of them, whole. Which one is not defined.
	Put(ctx context.Context, key string, r io.Reader) (Written, error)

	// Read returns a stream of length bytes of key, starting at offset. A
	// length of -1 means to the end of the object. A range that runs past
	// the end is cut at the end, so offset equal to the size gives an empty
	// stream; an offset past the size, a negative offset, or a length below
	// -1 is Invalid. A key that holds nothing is NotFound.
	//
	// The stream gives the bytes of the object as it was when Read was
	// called, even when a Put replaces it meanwhile. The caller closes it.
	Read(ctx context.Context, key string, offset, length int64) (io.ReadCloser, error)

	// Stat returns what is known of key without reading it, or NotFound.
	Stat(ctx context.Context, key string) (Info, error)

	// List returns one page of the keys that begin with opts.Prefix, in
	// byte order of their UTF-8 text. With opts.Delimiter set to "/", a key
	// that has another "/" after the prefix is not returned itself: the
	// part of it up to and including that "/" is returned once, in
	// Page.Prefixes, so one level can be listed. A page holds at most
	// opts.Limit keys and prefixes together.
	//
	// The order is stable across pages: a page continues exactly after the
	// last entry of the one before, so a key put or deleted behind the
	// point reached is not seen, and one ahead of it is seen as it is when
	// that page is read.
	List(ctx context.Context, opts ListOptions) (Page, error)

	// Delete removes key. Removing a key that holds nothing is not an
	// error. It never removes anything but the one object: a key that is
	// only a prefix of others holds nothing, and deleting it changes
	// nothing.
	Delete(ctx context.Context, key string) error

	// Purge removes key so its bytes are gone from the storage where the
	// storage allows it, and says, for each version of the object it
	// handled, what the storage did (Removal): a store that keeps no
	// versions answers one Removal with no version. It never claims more
	// than the storage reported: a versioned bucket that kept older
	// versions answers hidden, an object lock that refused a version
	// answers retained, and a driver that could not learn what happened
	// answers unknown. A key that holds nothing answers what the store
	// knows of it: no Removal when the store keeps no versions (nothing is
	// there to remove); each older version or delete marker it removed or
	// kept, on a store it knows keeps versions; one Removal, unknown, when
	// it cannot tell. Delete stays for the archive's own clean-up of what it
	// never published; a correction's purge, whose claim a person reads,
	// uses Purge.
	Purge(ctx context.Context, key string) ([]Removal, error)

	// Usage reports the space of the volume the storage is on, where the
	// driver can know it, and says so in Usage.Known when it cannot.
	Usage(ctx context.Context) (Usage, error)

	// Close releases what the Store holds. A stream from Read that is
	// still open may go on being read. Closing twice is harmless; any
	// other call after Close fails as Unavailable.
	Close() error
}

// Disposition is what a storage did with an object version Purge removed.
type Disposition string

// The dispositions, from the strongest claim to the weakest.
const (
	// Removed: the bytes are gone from this storage (a file unlinked, an
	// object deleted from an unversioned bucket, a version deleted by its
	// version id).
	Removed Disposition = "removed"
	// Hidden: the key answers not found, but the bucket keeps older
	// versions of the bytes.
	Hidden Disposition = "hidden"
	// Retained: the storage's own retention (S3 object lock) refused to
	// remove the version.
	Retained Disposition = "retained"
	// Unknown: the driver could not learn what the storage did.
	Unknown Disposition = "unknown"
)

// Removal is what a storage did with one version of an object.
type Removal struct {
	// Version is the version's id on a versioned bucket, "" otherwise.
	Version string
	// Disposition is what the storage did.
	Disposition Disposition
	// RetainUntil is when the retention that kept a Retained version
	// ends, when the storage said; zero otherwise.
	RetainUntil time.Time
}

// PurgeExpecter is a Store that can say before any purge what its Purge
// will do with an object, as a delete's preview shows it: the disposition
// and a short phrase why.
type PurgeExpecter interface {
	PurgeExpected(ctx context.Context) (Disposition, string)
}

// VersionPurger is a Store that keeps versions of an object and can remove
// them one by one. A correction that rewrites an object in place (a scan's
// manifest and metadata after one instance is deleted) leaves the older
// versions holding what it removed; PurgeOlder removes them. A version a
// storage's retention kept is tried again after its retention ends, by
// PurgeVersion, which never touches another version of the key.
type VersionPurger interface {
	// PurgeOlder removes every version and delete marker of key but the
	// current one, and says what the storage did with each.
	PurgeOlder(ctx context.Context, key string) ([]Removal, error)
	// PurgeVersion removes one version of key by its id, and says what the
	// storage did; a version no longer there answers removed.
	PurgeVersion(ctx context.Context, key, version string) (Removal, error)
}

// DirPruner is a Store whose keys are paths of directories that stay after
// their last object is removed (a directory storage). RemoveEmptyDirs
// removes the directory of dir, every directory beneath it, and then each
// directory above it up to the top level, when and only when it holds
// nothing: a directory named for a subject a purge removed would otherwise
// still name it.
type DirPruner interface {
	RemoveEmptyDirs(ctx context.Context, dir string) error
}

// Written is what a Put wrote: its size in bytes and the SHA-256 of its
// bytes, both counted as they passed.
type Written struct {
	// Size is the number of bytes written.
	Size int64
	// SHA256 is the SHA-256 of the bytes written.
	SHA256 [sha256.Size]byte
}

// Info is what Stat knows of an object without reading it.
type Info struct {
	// Size is the object's size in bytes.
	Size int64
}

// ListOptions says which keys List returns and from where.
type ListOptions struct {
	// Prefix is the text every key returned begins with. It need not end at
	// a "/": "data/s1" matches "data/s1/x" and "data/s10/x" alike. Empty
	// means every key. It is checked by CheckPrefix.
	Prefix string
	// Delimiter is "" to list every key under Prefix, or "/" to list one
	// level, with deeper keys folded into Page.Prefixes. No other value is
	// accepted.
	Delimiter string
	// Token is Page.Next of the page before, to continue from it, or empty
	// for the first page. It is valid only with the same Prefix and
	// Delimiter it was returned for, and it means nothing to another
	// driver.
	Token string
	// Limit is the most entries a page holds, keys and prefixes together:
	// from 1 to MaxListLimit, or 0 for MaxListLimit.
	Limit int
}

// MaxListLimit is the largest page List returns. It is the largest page an
// S3 listing returns (docs.aws.amazon.com/AmazonS3/latest/API/API_ListObjectsV2.html),
// so a page here is never more than one request there.
const MaxListLimit = 1000

// Page is one page of a listing.
type Page struct {
	// Keys are the keys on this page, in byte order.
	Keys []string
	// Prefixes are, with a delimiter, the distinct prefixes that end at the
	// first "/" after ListOptions.Prefix, each ending with "/", in byte
	// order. A prefix sorts among the keys as its text does, and the page's
	// limit counts both.
	Prefixes []string
	// Next is the Token for the following page, or empty when this page
	// holds the last entry. A driver never returns a non-empty Next with
	// nothing after it.
	Next string
}

// Usage is the space of the volume a storage is on.
type Usage struct {
	// Known says whether the driver could find the numbers below. A storage
	// that has no answer, such as an S3 bucket, reports false, and the
	// fields are zero; what it holds is then counted from the catalog.
	Known bool
	// Total is the volume's size in bytes.
	Total int64
	// Used is the bytes in use on the volume, by this storage and anything
	// else that shares it.
	Used int64
	// Free is the bytes this process could still write.
	Free int64
}
